// Package agent is the harness core loop, modeled after pi-agent-core:
// a turn loop that streams model output, executes tool calls, and emits
// events at every step so extensions can observe and shape behavior.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/NanQiaoMi/auto-harness/contextmgr"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// DefaultSystemPrompt is used when Config.SystemPrompt is empty.
const DefaultSystemPrompt = `You are a coding agent running inside the auto-harness framework.
You help the user with software tasks by using tools to inspect and modify files,
run commands, and report results.
Be concise and precise. Prefer dedicated file tools over shell commands.
When you are done, reply with a short summary of what changed.`

// Config configures an Agent run loop.
type Config struct {
	Provider     llm.Provider
	Model        string
	SystemPrompt string
	// AgentID identifies this agent in events. The primary agent leaves
	// it empty; a delegated agent receives the orchestrator task ID.
	AgentID string
	// ParentAgentID identifies the delegated agent that started this
	// one, when applicable.
	ParentAgentID string
	// Workspace is the working tree this agent's path tools use.
	Workspace string
	// MaxTokens bounds one completion (provider default when 0).
	MaxTokens int
	// ContextWindow overrides the model's context window for request
	// budgeting. Zero uses the model metadata or the context manager's
	// default.
	ContextWindow int
	// ContextBudget resolves the context window and maximum output for
	// the current model. It lets a model switch update the request
	// budget without rebuilding the agent.
	ContextBudget func(modelID string) (contextWindow, maxOutput int)
	// MaxTurns caps loop iterations (default 25). It is a safety net,
	// not a substitute for the model deciding to stop.
	MaxTurns    int
	Temperature *float64
	Thinking    bool
	// Resolver supplies a provider for a model ID. When set, the agent
	// asks it for a provider on every turn, so switching models can
	// switch providers too (see llm.Registry.ProviderFor).
	Resolver func(modelID string) (llm.Provider, error)
	// MaxRetries bounds how many times a failed model request is tried
	// again. Zero means the default (3); a negative value disables
	// retries.
	MaxRetries int
	// RetryBaseDelay is the first backoff delay (default 1s). Each
	// further attempt doubles it.
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps one backoff delay (default 60s).
	RetryMaxDelay time.Duration
}

// Checkpointer creates a rollback point before a mutating tool runs.
// Implementations may treat a non-repository workspace as a no-op, but
// an error vetoes the tool call: losing the rollback guarantee must be
// visible rather than silent.
type Checkpointer interface {
	BeforeTool(ctx context.Context, toolName string, args json.RawMessage) error
}

// Result reports the outcome of one Run.
type Result struct {
	// Final is the last assistant text message.
	Final string
	// Turns counts model completions in this run.
	Turns int
	// Usage accumulates token accounting across the run.
	Usage llm.Usage
	// Cancelled reports whether a hook cancelled the run.
	Cancelled bool
	// Queued counts the follow-up messages this run drained from the
	// queue.
	Queued int
	// Continued reports that a settle handler asked for another run.
	Continued bool
}

// Agent runs the tool-calling loop over a conversation history,
// optionally mirrored to a persistent Session.
type Agent struct {
	cfg   Config
	Tools *tools.Registry
	Bus   *ext.Bus
	// Session, when non-nil, receives every message appended to history.
	Session *session.Session
	// Logf receives non-fatal diagnostics (hook errors, missing tools).
	Logf func(format string, args ...any)
	// Compactor compresses history on Compact when no hook provides
	// replacement messages (see NewSummarizer).
	Compactor Compactor
	// Context sizes and compresses every provider request. It is
	// optional: nil keeps the old behavior.
	Context contextmgr.Builder
	// ContextPolicy, when set, wraps Context before each use.
	//
	// It is how the extension API layers its registered policies over
	// the kernel's builder. It is read on every request rather than
	// captured once, so a hot reload that adds or drops a policy takes
	// effect without rebuilding the agent.
	ContextPolicy func(contextmgr.Builder) contextmgr.Builder
	// Checkpointer snapshots the workspace before mutating tools.
	Checkpointer Checkpointer

	history []llm.Message

	// queued holds messages a caller submitted while a run was in
	// progress. Run drains them between turns, so a follow-up typed
	// mid-run becomes the next thing the model sees rather than an
	// error or a lost line.
	queued []string
	// running reports whether a Run is in progress.
	running bool

	// mu guards the history, the configuration and the queue.
	//
	// A run appends to the history while an outside caller may read
	// it: the RPC server answers "get_messages" while the model is
	// still working, and "set_model" may switch the model mid-run.
	mu sync.Mutex
}

// Compactor turns a long history into a shorter replacement history.
type Compactor func(ctx context.Context, msgs []llm.Message) ([]llm.Message, error)

// NewSummarizer returns a Compactor that replaces the history with a
// single message holding an LLM-generated summary of the conversation.
func NewSummarizer(provider llm.Provider, model string) Compactor {
	return func(ctx context.Context, msgs []llm.Message) ([]llm.Message, error) {
		summary, err := contextmgr.SummarizeOnce(ctx, provider, model, msgs, 2048)
		if err != nil {
			return nil, fmt.Errorf("agent: summarizer: %w", err)
		}
		return []llm.Message{contextmgr.SummaryMessage(summary)}, nil
	}
}

// New creates an Agent. bus and registry must not be nil.
func New(cfg Config, bus *ext.Bus, registry *tools.Registry) *Agent {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 25
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = DefaultSystemPrompt
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if cfg.RetryMaxDelay <= 0 {
		cfg.RetryMaxDelay = DefaultRetryMaxDelay
	}
	return &Agent{cfg: cfg, Bus: bus, Tools: registry}
}

// Config returns the agent's effective configuration.
func (a *Agent) Config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SetModel switches the model for subsequent runs. When a resolver is
// installed it also refreshes the provider immediately, so status APIs
// report the same route the next run will use.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	a.cfg.Model = model
	resolver := a.cfg.Resolver
	a.mu.Unlock()
	if resolver == nil {
		return
	}
	provider, err := resolver(model)
	if err != nil {
		a.mu.Lock()
		if a.cfg.Model == model {
			a.cfg.Provider = nil
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	if a.cfg.Model == model {
		a.cfg.Provider = provider
	}
	a.mu.Unlock()
}

// SetThinking turns extended thinking on or off for subsequent runs.
func (a *Agent) SetThinking(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Thinking = on
}

// SetResolver installs a per-model provider resolver, enabling model
// switches that cross providers.
func (a *Agent) SetResolver(fn func(modelID string) (llm.Provider, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Resolver = fn
}

// SetContextManager installs the request-level context policy.
func (a *Agent) SetContextManager(builder contextmgr.Builder) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Context = builder
}

// Provider returns the provider currently in use.
func (a *Agent) Provider() llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Provider
}

// providerFor returns the provider for the current model: the resolver
// wins when configured (so a model switch can cross providers), otherwise
// the fixed provider.
func (a *Agent) providerFor() (llm.Provider, error) {
	a.mu.Lock()
	resolver := a.cfg.Resolver
	model := a.cfg.Model
	provider := a.cfg.Provider
	a.mu.Unlock()

	if resolver != nil {
		p, err := resolver(model)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	if provider == nil {
		return nil, fmt.Errorf("agent: no provider configured")
	}
	return provider, nil
}

// Submit hands the agent a message to process after the current turn.
//
// It is how a follow-up typed while the model is working gets in: the
// message is queued rather than rejected, and Run picks it up between
// turns so it lands where a user message belongs — after the assistant
// message and its tool results, never between a tool call and its
// result. When no run is in progress the message starts one.
//
// The bool result reports whether a run was already in progress, so a
// caller can tell "queued" from "started".
func (a *Agent) Submit(ctx context.Context, text string) (queued bool, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return false, fmt.Errorf("agent: an empty message cannot be submitted")
	}
	a.mu.Lock()
	if a.running {
		a.queued = append(a.queued, text)
		a.mu.Unlock()
		return true, nil
	}
	a.mu.Unlock()

	_, err = a.Run(ctx, text)
	return false, err
}

// Enqueue queues a message for the next turn, without starting a run
// of its own.
//
// It is for callers that already know a run is active: the RPC server
// tracks that itself, and using Submit there would race with the
// goroutine that has not entered Run yet. A message queued just before
// a run starts is picked up by that run, so the small window between
// "a run was accepted" and "the run began" does not lose input.
func (a *Agent) Enqueue(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("agent: an empty message cannot be queued")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queued = append(a.queued, text)
	return nil
}

// Queue returns the messages waiting to be processed.
func (a *Agent) Queue() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.queued...)
}

// Running reports whether a run is in progress.
func (a *Agent) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// takeQueued removes and returns the queued messages.
func (a *Agent) takeQueued() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.queued
	a.queued = nil
	return out
}

// History returns the current conversation history.
func (a *Agent) History() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]llm.Message, len(a.history))
	copy(out, a.history)
	return out
}

// SetHistory replaces the conversation history (e.g. after resuming a
// session). It does not touch the session file.
func (a *Agent) SetHistory(msgs []llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append([]llm.Message(nil), msgs...)
}

// ReloadHistory replaces the history with the current branch of the
// session tree. It is called after a branch switch (/tree) so the
// model sees the branch the user selected.
func (a *Agent) ReloadHistory() {
	if a.Session == nil {
		return
	}
	a.SetHistory(a.Session.Tree().Messages())
}

// maxSettleRounds bounds how many times a settle handler may ask for
// another run. It is a safety net: a handler that always asks would
// keep the agent busy forever and never hand the prompt back.
const maxSettleRounds = 8

// Run executes one user turn: the input is appended to history, then the
// model is called repeatedly (with tool executions in between) until it
// stops requesting tools or MaxTurns is reached.
//
// A run does not end at the first quiet turn. Messages submitted while
// it was working are drained and run next, and a settle handler may
// ask for one more run; agent_settled fires only when none of that is
// left, so a consumer can treat it as "the agent is idle now".
func (a *Agent) Run(ctx context.Context, input string) (*Result, error) {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	return a.run(ctx, input)
}

// run is the body of Run.
//
// The running flag is raised by Run and lowered here, because only this
// function knows when the queue is empty: the check and the clear happen
// in one critical section, so a message queued concurrently is either
// drained by this run or seen by a caller that then starts the next one.
func (a *Agent) run(ctx context.Context, input string) (*Result, error) {
	// cleared reports that the normal exit already lowered the flag. A
	// run that started in the meantime must not have it lowered again.
	cleared := false
	defer func() {
		a.mu.Lock()
		if !cleared {
			a.running = false
		}
		a.mu.Unlock()
	}()

	// input: extensions see the prompt before it becomes a message.
	// A handler may rewrite the text (add context, redact a secret,
	// expand an alias) or deal with it itself, in which case the agent
	// does not run.
	//
	// This is the one place every prompt passes through — the REPL,
	// one-shot, JSON, RPC, a skill and a prompt template all arrive
	// here — so the event cannot be skipped by an entry point that
	// forgets to fire it.
	inputEv := &ext.Event{Type: ext.EventInput, Text: input}
	a.emit(inputEv)
	if inputEv.Cancel {
		// An extension that blocked the prompt owes the user a reason;
		// the run produces no output, so this is the only place it can
		// surface.
		if inputEv.BlockReason != "" && a.Logf != nil {
			a.Logf("input blocked: %s", inputEv.BlockReason)
		}
		return &Result{Cancelled: true}, nil
	}
	input = inputEv.Text

	userMsg := llm.Message{Role: llm.RoleUser, Content: input}

	// before_agent_start: extensions may rewrite the message or the
	// system prompt, or cancel the run entirely.
	ev := &ext.Event{
		Type:         ext.EventBeforeAgentStart,
		Message:      &userMsg,
		SystemPrompt: a.Config().SystemPrompt,
	}
	a.emit(ev)
	if ev.Cancel {
		return &Result{Cancelled: true}, nil
	}
	a.mu.Lock()
	a.cfg.SystemPrompt = ev.SystemPrompt
	maxTurns := a.cfg.MaxTurns
	a.mu.Unlock()
	if ev.Message != nil {
		userMsg = *ev.Message
	}

	a.append(userMsg)

	res := &Result{}

	// A message queued just before this run began belongs to it: the
	// caller accepted it while a run was starting.
	for _, text := range a.takeQueued() {
		a.append(llm.Message{Role: llm.RoleUser, Content: text})
		res.Queued++
	}

	// One low-level run: the turns until the model stops asking for
	// tools. A queued message or a settle handler may start another.
	runTurns := func() error {
		for turn := 0; turn < maxTurns; turn++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			res.Turns++
			a.emit(&ext.Event{Type: ext.EventTurnStart})

			assistantMsg, usage, req, err := a.complete(ctx)
			if err != nil {
				return err
			}
			res.Usage.Add(usage)

			a.append(assistantMsg)
			if assistantMsg.Content != "" {
				res.Final = assistantMsg.Content
			}

			// Execute requested tool calls (if any).
			for _, toolMsg := range a.executeToolCalls(ctx, assistantMsg.ToolCalls) {
				a.append(toolMsg)
			}

			// turn_end: extensions may append entries (e.g. reminders)
			// or force an extra turn. The queue is drained here too: a
			// message typed while the tools ran belongs after their
			// results, never between a call and its result.
			turnEv := &ext.Event{
				Type:     ext.EventTurnEnd,
				Continue: len(assistantMsg.ToolCalls) > 0,
				Request:  req,
			}
			a.emit(turnEv)
			for _, injected := range turnEv.Messages {
				a.append(injected)
			}
			if queued := a.takeQueued(); len(queued) > 0 {
				for _, text := range queued {
					a.append(llm.Message{Role: llm.RoleUser, Content: text})
					res.Queued++
				}
				continue
			}
			if turnEv.Continue {
				continue
			}
			break
		}
		return nil
	}

	// The settle boundary is bounded: a handler that asks for another
	// run every time would otherwise keep the agent busy forever, and
	// the user would never get the prompt back.
	settleRounds := 0

	for {
		a.emit(&ext.Event{Type: ext.EventAgentStart})
		runErr := runTurns()
		a.emit(&ext.Event{Type: ext.EventAgentEnd, Usage: res.Usage})
		if runErr != nil {
			// An aborted run has no settle boundary to offer: it goes
			// straight to the notification.
			a.emit(&ext.Event{Type: ext.EventAgentSettled, Aborted: ctx.Err() != nil})
			return res, runErr
		}

		// Queued messages start another run, so agent_end is not the
		// last word.
		if queued := a.takeQueued(); len(queued) > 0 {
			appendQueued(a, res, queued)
			continue
		}

		// agent_before_settle: the last chance to add work. A handler
		// that appends messages or asks for a continuation gets another
		// run; otherwise the run is over.
		more := false
		if settleRounds < maxSettleRounds {
			settleEv := &ext.Event{Type: ext.EventAgentBeforeSettle}
			a.emit(settleEv)
			added := len(settleEv.Messages) > 0
			for _, injected := range settleEv.Messages {
				a.append(injected)
			}
			if settleEv.Continue || added {
				settleRounds++
				res.Continued = true
				more = true
			}
		} else if a.Logf != nil {
			a.Logf("agent_before_settle: stopped after %d continuations", settleRounds)
		}
		if more {
			continue
		}

		// The run is over when nothing is queued. Checking the queue and
		// clearing the running flag happen in one critical section, so a
		// message that arrives concurrently cannot be lost: Enqueue sees
		// either a live run (which drains it here) or a clear flag (and
		// its caller starts the next run).
		a.mu.Lock()
		if len(a.queued) == 0 {
			a.running = false
			cleared = true
			a.mu.Unlock()
			a.emit(&ext.Event{Type: ext.EventAgentSettled, Aborted: ctx.Err() != nil})
			return res, nil
		}
		queued := a.queued
		a.queued = nil
		a.mu.Unlock()
		appendQueued(a, res, queued)
	}
}

// appendQueued adds the queued messages to history as user messages.
func appendQueued(a *Agent, res *Result, queued []string) {
	for _, text := range queued {
		a.append(llm.Message{Role: llm.RoleUser, Content: text})
		res.Queued++
	}
}

// Drain runs whatever is queued, without a prompt of its own, until the
// queue is empty.
//
// It covers the moment a run ends: a message can be queued after the
// run's last drain and before it returns, and nothing else would pick it
// up. A caller that queues input from another goroutine (a REPL whose
// main loop keeps reading) calls this once its run returns.
func (a *Agent) Drain(ctx context.Context) error {
	for {
		a.mu.Lock()
		if len(a.queued) == 0 {
			a.mu.Unlock()
			return nil
		}
		first := a.queued[0]
		a.queued = a.queued[1:]
		a.mu.Unlock()

		if _, err := a.Run(ctx, first); err != nil {
			return err
		}
	}
}

// complete performs one model completion, streaming events as they
// arrive, and returns the assembled assistant message.
//
// The third result describes the request for the observers that need
// it: the provider, the model, the tool count, the stop reason, the
// token usage and the elapsed time. It carries no content.
func (a *Agent) complete(ctx context.Context) (llm.Message, llm.Usage, ext.Request, error) {
	// context hook: extensions may rewrite the outgoing context.
	rawHistory := a.History()
	ctxEv := &ext.Event{Type: ext.EventContext, Messages: rawHistory}
	a.emit(ctxEv)
	messages := ctxEv.Messages

	// The configuration is read as a snapshot: a model switch that
	// lands while the request is being built applies to the next turn,
	// not halfway through this one.
	cfg := a.Config()
	tools := a.Tools.Definitions()
	window, maxOut := a.contextLimits(cfg)

	a.mu.Lock()
	contextBuilder := a.Context
	policy := a.ContextPolicy
	a.mu.Unlock()
	// The extension policies wrap the kernel's builder. A policy may
	// replace compaction entirely, so it is applied here rather than
	// baked in when the agent is built.
	if policy != nil {
		contextBuilder = policy(contextBuilder)
	}
	if contextBuilder != nil {
		result, err := contextBuilder.Build(ctx, contextmgr.Input{
			SystemPrompt:    cfg.SystemPrompt,
			Messages:        messages,
			Tools:           tools,
			Model:           cfg.Model,
			ContextWindow:   window,
			MaxOutputTokens: maxOut,
		})
		if err != nil {
			if a.Logf != nil {
				a.Logf("context: %v", err)
			}
		} else {
			messages = result.Messages
			for _, warning := range result.Warnings {
				if a.Logf != nil {
					a.Logf("context: %s", warning)
				}
			}
			if result.Compacted && result.NewHistory != nil {
				// A context hook may have injected transient messages;
				// they must not be persisted into the session.
				if sameHistory(ctxEv.Messages, rawHistory) {
					a.SetHistory(result.NewHistory)
					a.recordContextCompaction(result)
					a.emit(&ext.Event{
						Type:     ext.EventSessionCompact,
						Messages: result.NewHistory,
						Data: map[string]any{
							"automatic":    true,
							"tokensBefore": result.TokensBefore,
							"tokensAfter":  result.TokensAfter,
							"promptBudget": result.PromptBudget,
							"summary":      result.Summary,
							"artifacts":    result.Artifacts,
							"warnings":     result.Warnings,
						},
					})
				} else if a.Logf != nil {
					a.Logf("context: compaction not persisted because a context hook rewrote the transcript")
				}
			}
		}
	}

	req := llm.ChatRequest{
		Model:       cfg.Model,
		System:      cfg.SystemPrompt,
		Messages:    messages,
		Tools:       tools,
		MaxTokens:   cfg.MaxTokens,
		Temperature: cfg.Temperature,
		Thinking:    cfg.Thinking,
	}
	provider, err := a.providerFor()
	if err != nil {
		return llm.Message{}, llm.Usage{}, ext.Request{}, err
	}

	// The facts about the request that are safe to record.
	info := ext.Request{
		Provider: provider.Name(),
		Model:    cfg.Model,
		Tools:    len(req.Tools),
	}

	// The request is retried on transient failures (see retry.go).
	//
	// Only a failure that arrives before any content has been streamed
	// is retried: once deltas are on their way to the transcript, a
	// second attempt would show the answer twice. A mid-stream failure
	// is reported instead.
	var msg llm.Message
	var usage llm.Usage
	started := time.Now()
	for attempt := 0; ; attempt++ {
		a.emit(&ext.Event{Type: ext.EventRequestStart, Request: info})
		started = time.Now()

		streamed, err := a.streamOnce(ctx, provider, req, &msg, &usage, &info)
		if err == nil {
			break
		}
		info.Duration = time.Since(started)

		cfg := a.Config()
		if streamed || attempt >= cfg.MaxRetries || !isRetryable(err) {
			return llm.Message{}, usage, info, err
		}
		delay := retryDelay(cfg.RetryBaseDelay, cfg.RetryMaxDelay, attempt+1)
		a.emit(&ext.Event{
			Type:   ext.EventRetry,
			Reason: err.Error(),
			Data: map[string]any{
				"attempt":     attempt + 1,
				"maxAttempts": cfg.MaxRetries,
				"delayMs":     delay.Milliseconds(),
			},
		})
		if !sleepCtx(ctx, delay) {
			return llm.Message{}, usage, info, ctx.Err()
		}
		// The retried request starts from a clean message: the failed
		// attempt may have collected tool calls it never returned.
		msg = llm.Message{}
		usage = llm.Usage{}
		info.StopReason = ""
		info.Usage = llm.Usage{}
		info.Duration = 0
	}
	msg.Role = llm.RoleAssistant
	info.Usage = usage
	info.Duration = time.Since(started)

	// message_end: extensions may replace the final message.
	endEv := &ext.Event{Type: ext.EventMessageEnd, Message: &msg}
	a.emit(endEv)
	if endEv.Message != nil {
		msg = *endEv.Message
	}
	return msg, usage, info, nil
}

// streamOnce performs one model request and streams it into msg.
//
// The second result reports whether any content reached the
// transcript: a failure after that point cannot be retried without
// showing the answer twice, so the caller must report it instead.
func (a *Agent) streamOnce(ctx context.Context, provider llm.Provider, req llm.ChatRequest, msg *llm.Message, usage *llm.Usage, info *ext.Request) (streamed bool, err error) {
	stream, err := provider.Chat(ctx, req)
	if err != nil {
		return false, fmt.Errorf("agent: provider chat: %w", err)
	}
	defer stream.Close()

	a.emit(&ext.Event{Type: ext.EventMessageStart})

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return streamed, nil
		}
		if err != nil {
			return streamed, fmt.Errorf("agent: stream: %w", err)
		}
		switch ev.Type {
		case llm.EventTextDelta:
			streamed = true
			msg.Content += ev.Text
			a.emit(&ext.Event{Type: ext.EventMessageUpdate, Text: ev.Text})
		case llm.EventThinkingDelta:
			// Thinking output is surfaced to extensions but not kept in
			// the transcript (models do not expect it back).
			streamed = true
			a.emit(&ext.Event{Type: ext.EventMessageUpdate, Text: ev.Text, Data: map[string]any{"thinking": true}})
		case llm.EventToolCall:
			msg.ToolCalls = append(msg.ToolCalls, *ev.ToolCall)
		case llm.EventDone:
			if ev.Message != nil {
				*msg = *ev.Message
			}
			*usage = ev.Usage
			info.StopReason = ev.StopReason
		}
	}
}

// ContextStatus reports the active context budget and the last
// automatic compaction. The second result is false when no inspector
// is installed.
func (a *Agent) ContextStatus() (contextmgr.Status, bool) {
	cfg := a.Config()
	messages := a.History()
	tools := a.Tools.Definitions()
	window, maxOut := a.contextLimits(cfg)

	a.mu.Lock()
	builder := a.Context
	policy := a.ContextPolicy
	a.mu.Unlock()
	if policy != nil {
		builder = policy(builder)
	}
	inspector, ok := builder.(contextmgr.Inspector)
	if !ok {
		return contextmgr.Status{}, false
	}
	return inspector.Status(contextmgr.Input{
		SystemPrompt:    cfg.SystemPrompt,
		Messages:        messages,
		Tools:           tools,
		Model:           cfg.Model,
		ContextWindow:   window,
		MaxOutputTokens: maxOut,
	}), true
}

// contextLimits returns the effective window and output reservation. An
// explicit completion cap wins over the model metadata, because that is
// the amount the provider is actually allowed to generate.
func (a *Agent) contextLimits(cfg Config) (window, maxOut int) {
	window = cfg.ContextWindow
	maxOut = cfg.MaxTokens
	if cfg.ContextBudget == nil {
		return window, maxOut
	}
	modelWindow, modelMaxOut := cfg.ContextBudget(cfg.Model)
	if window <= 0 && modelWindow > 0 {
		window = modelWindow
	}
	if maxOut <= 0 && modelMaxOut > 0 {
		maxOut = modelMaxOut
	}
	return window, maxOut
}

// executeTool runs one tool call and returns the tool-result message.
func (a *Agent) executeTool(ctx context.Context, call *llm.ToolCall) llm.Message {
	a.emit(&ext.Event{Type: ext.EventToolExecutionStart, ToolCall: call})

	// tool_call: extensions may patch the arguments or block the call
	// (pi's permission gate lives here).
	callEv := &ext.Event{Type: ext.EventToolCall, ToolCall: call}
	a.emit(callEv)
	if callEv.Block {
		result := tools.Result{
			Content: fmt.Sprintf("tool call blocked: %s", callEv.BlockReason),
			IsError: true,
		}
		resCopy := result
		a.emit(&ext.Event{Type: ext.EventToolResult, ToolCall: call, ToolResult: &resCopy})
		a.emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call, ToolResult: &result})
		return llm.Message{
			Role:       llm.RoleTool,
			Content:    result.Content,
			ToolCallID: call.ID,
			IsError:    true,
		}
	}
	// The arguments may have been patched by a handler.
	if callEv.ToolCall != nil {
		*call = *callEv.ToolCall
	}

	var result tools.Result
	t, ok := a.Tools.Get(call.Name)
	if !ok {
		result = tools.Result{
			Content: fmt.Sprintf("unknown tool %q", call.Name),
			IsError: true,
		}
	} else {
		onUpdate := func(u tools.Update) {
			a.emit(&ext.Event{Type: ext.EventToolExecutionUpdate, ToolCall: call, Text: u.Content})
		}
		allowed := true
		if a.Checkpointer != nil && !t.ReadOnly {
			if err := a.Checkpointer.BeforeTool(ctx, call.Name, call.Arguments); err != nil {
				result = tools.Result{
					Content: fmt.Sprintf(
						"refusing to run mutating tool %s because the workspace checkpoint failed: %v",
						call.Name, err),
					IsError: true,
				}
				allowed = false
			}
		}
		if allowed {
			execResult, err := t.Execute(ctx, call.ID, call.Arguments, onUpdate)
			if err != nil {
				result = tools.Result{Content: fmt.Sprintf("tool %s failed: %v", call.Name, err), IsError: true}
			} else {
				result = execResult
			}
		}
	}

	resCopy := result
	a.emit(&ext.Event{Type: ext.EventToolResult, ToolCall: call, ToolResult: &resCopy})
	a.emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call, ToolResult: &result})

	return llm.Message{
		Role:       llm.RoleTool,
		Content:    result.Content,
		ToolCallID: call.ID,
		IsError:    result.IsError,
	}
}

// Compact compresses the history (session_before_compact /
// session_compact). A hook handler may supply replacement messages via
// e.Messages; otherwise the built-in Compactor runs; without either it
// is an error.
//
// When a session is attached, the compaction is also recorded in the
// session tree, so a reloaded session rebuilds the same context: the
// summary stands in for the compacted messages.
func (a *Agent) Compact(ctx context.Context) error {
	ev := &ext.Event{Type: ext.EventSessionBeforeCompact, Messages: a.History()}
	a.emit(ev)
	if ev.Cancel {
		return nil
	}
	msgs := ev.Messages
	// A handler that leaves Messages untouched (same slice) is not a
	// replacement; only hook-provided changes or the Compactor apply.
	if sameHistory(msgs, a.history) {
		msgs = nil
	}
	if msgs == nil {
		if a.Compactor == nil {
			return fmt.Errorf("agent: no compaction provider (set Agent.Compactor or handle session_before_compact)")
		}
		compacted, err := a.Compactor(ctx, a.History())
		if err != nil {
			return err
		}
		msgs = compacted
	}
	a.SetHistory(msgs)
	a.recordCompaction(msgs)
	a.emit(&ext.Event{
		Type:     ext.EventSessionCompact,
		Messages: msgs,
		Data:     map[string]any{"automatic": false},
	})
	return nil
}

// recordCompaction writes a compaction entry to the session tree when
// the compacted history starts with a summary and the agent has a
// session.
//
// The retained range is the messages that follow the summary in the
// compacted history; the entry that follows them in the tree is the
// first kept one.
func (a *Agent) recordCompaction(msgs []llm.Message) {
	if a.Session == nil || len(msgs) == 0 {
		return
	}
	summary := stripCompactionPrefix(msgs[0].Content)
	firstKept := findKeptStart(a.Session.Tree().Branch(), msgs[1:])
	if firstKept == "" && len(msgs) > 1 {
		if a.Logf != nil {
			a.Logf("session compaction: retained messages could not be located on the current branch; compaction was not persisted")
		}
		return
	}
	if err := a.Session.Compaction(summary, firstKept, 0); err != nil && a.Logf != nil {
		a.Logf("session compaction: %v", err)
	}
}

// recordContextCompaction persists an automatic compaction. The
// summary is stored without its display prefix: the session tree adds
// the prefix back when it builds model context.
func (a *Agent) recordContextCompaction(result contextmgr.Result) {
	if a.Session == nil || len(result.NewHistory) == 0 {
		return
	}
	firstKept := findKeptStart(a.Session.Tree().Branch(), result.NewHistory[1:])
	if firstKept == "" && len(result.NewHistory) > 1 {
		if a.Logf != nil {
			a.Logf("session compaction: retained messages could not be located on the current branch; compaction was not persisted")
		}
		return
	}
	if err := a.Session.Compaction(result.Summary, firstKept, result.TokensBefore); err != nil && a.Logf != nil {
		a.Logf("session compaction: %v", err)
	}
}

// findKeptStart finds the session entry that begins the retained
// sequence. It matches a whole suffix, so repeated message text cannot
// make a compaction point point at the wrong entry.
func findKeptStart(entries []*session.Entry, kept []llm.Message) string {
	if len(kept) == 0 {
		return ""
	}
	var messages []*session.Entry
	for _, e := range entries {
		if e.Type == session.EntryMessage && e.Message != nil {
			messages = append(messages, e)
		}
	}
	if len(messages) < len(kept) {
		return ""
	}
	for start := len(messages) - len(kept); start >= 0; start-- {
		ok := true
		for i := range kept {
			if !sameMessage(*messages[start+i].Message, kept[i]) {
				ok = false
				break
			}
		}
		if ok {
			return messages[start].ID
		}
	}
	return ""
}

func sameMessage(a, b llm.Message) bool {
	da, _ := json.Marshal(a)
	db, _ := json.Marshal(b)
	return bytes.Equal(da, db)
}

func stripCompactionPrefix(s string) string {
	return contextmgr.StripSummaryPrefix(s)
}

// sameHistory reports whether two message slices hold identical content.
func sameHistory(a, b []llm.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		da, _ := json.Marshal(a[i])
		db, _ := json.Marshal(b[i])
		if !bytes.Equal(da, db) {
			return false
		}
	}
	return true
}

// AppendMessage adds a message to the conversation.
//
// It is how a feature outside the agent — the "!" shell shortcut, an
// extension — puts something in front of the model without going
// through a tool call.
func (a *Agent) AppendMessage(msg llm.Message) {
	a.append(msg)
}

// append adds a message to history and mirrors it to the session.
//
// The history is guarded: a run appends to it while an outside caller
// — the RPC server, an extension — may read it.
func (a *Agent) append(msgs ...llm.Message) {
	a.mu.Lock()
	a.history = append(a.history, msgs...)
	a.mu.Unlock()
	if a.Session != nil {
		if err := a.Session.Append(msgs...); err != nil && a.Logf != nil {
			a.Logf("session append: %v", err)
		}
	}
}

// emit dispatches a hook event, logging handler errors without aborting
// the run (matching pi's fire-and-collect semantics).
func (a *Agent) emit(e *ext.Event) {
	if a.Bus == nil {
		return
	}
	cfg := a.Config()
	ext.SetDelegation(e, cfg.AgentID, cfg.ParentAgentID, cfg.Workspace)
	if err := a.Bus.Emit(e); err != nil && a.Logf != nil {
		a.Logf("hook %s: %v", e.Type, err)
	}
}

// maxParallelTools bounds how many read-only calls run at once.
//
// The cap is small on purpose: the point is to stop five independent
// reads from being serialized, not to saturate the machine. A model
// that asks for fifty reads should not open fifty file handles.
const maxParallelTools = 4

// executeToolCalls runs the calls of one assistant message and returns
// their result messages, in the order the model asked for them.
//
// Consecutive read-only calls run in parallel: they cannot observe each
// other's effects, so serializing them only costs time. Everything else
// runs in order, because a mutating call and whatever follows it are
// ordered — a write followed by a read of the same file must see the
// write, and two mutations of one file must not interleave.
//
// The result order is the call order, never the completion order: the
// tool messages answer specific call IDs, and a provider expects them
// in the order its calls appeared.
func (a *Agent) executeToolCalls(ctx context.Context, calls []llm.ToolCall) []llm.Message {
	out := make([]llm.Message, len(calls))

	for i := 0; i < len(calls); {
		if !a.canRunInParallel(&calls[i]) {
			out[i] = a.executeTool(ctx, &calls[i])
			i++
			continue
		}
		// Take the longest run of read-only calls starting here.
		end := i
		for end < len(calls) && a.canRunInParallel(&calls[end]) {
			end++
		}
		batch := calls[i:end]
		if len(batch) == 1 {
			out[i] = a.executeTool(ctx, &calls[i])
			i = end
			continue
		}

		results := make([]llm.Message, len(batch))
		limit := make(chan struct{}, maxParallelTools)
		var wg sync.WaitGroup
		for j := range batch {
			j := j
			wg.Add(1)
			limit <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-limit }()
				call := batch[j]
				results[j] = a.executeTool(ctx, &call)
			}()
		}
		wg.Wait()
		for j := range results {
			out[i+j] = results[j]
		}
		i = end
	}
	return out
}

// canRunInParallel reports whether a call may run beside its neighbors.
//
// A tool is parallel-safe when it is registered and declared read-only.
// An unknown tool is not: it is cheap to answer and running it beside
// others would only make the error harder to reason about.
func (a *Agent) canRunInParallel(call *llm.ToolCall) bool {
	t, ok := a.Tools.Get(call.Name)
	return ok && t.ReadOnly
}
