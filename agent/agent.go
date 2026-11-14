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
	// MaxTokens bounds one completion (provider default when 0).
	MaxTokens int
	// MaxTurns caps loop iterations (default 25). It is a safety net,
	// not a substitute for the model deciding to stop.
	MaxTurns    int
	Temperature *float64
	Thinking    bool
	// Resolver supplies a provider for a model ID. When set, the agent
	// asks it for a provider on every turn, so switching models can
	// switch providers too (see llm.Registry.ProviderFor).
	Resolver func(modelID string) (llm.Provider, error)
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

	history []llm.Message

	// mu guards the history and the configuration.
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
		var b strings.Builder
		for _, m := range msgs {
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
		}
		stream, err := provider.Chat(ctx, llm.ChatRequest{
			Model: model,
			System: "You compress conversation transcripts for a coding agent. " +
				"Produce a faithful, dense summary preserving: the user's goals, decisions made, " +
				"files changed (with paths), commands run and their outcomes, and any unfinished work. " +
				"Output only the summary text.",
			Messages:  []llm.Message{{Role: llm.RoleUser, Content: b.String()}},
			MaxTokens: 2048,
		})
		if err != nil {
			return nil, fmt.Errorf("agent: summarizer: %w", err)
		}
		msg, _, err := llm.Collect(stream, nil)
		if err != nil {
			return nil, fmt.Errorf("agent: summarizer: %w", err)
		}
		if msg == nil || msg.Content == "" {
			return nil, fmt.Errorf("agent: summarizer produced no content")
		}
		return []llm.Message{{
			Role:    llm.RoleUser,
			Content: "【会话压缩摘要 — 此前对话已压缩，请据此继续协助】\n" + msg.Content,
		}}, nil
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
	return &Agent{cfg: cfg, Bus: bus, Tools: registry}
}

// Config returns the agent's effective configuration.
func (a *Agent) Config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SetModel switches the model for subsequent runs.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Model = model
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

// Run executes one user turn: the input is appended to history, then the
// model is called repeatedly (with tool executions in between) until it
// stops requesting tools or MaxTurns is reached.
func (a *Agent) Run(ctx context.Context, input string) (*Result, error) {
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
	a.emit(&ext.Event{Type: ext.EventAgentStart})

	res := &Result{}
	defer func() {
		endEv := &ext.Event{Type: ext.EventAgentEnd, Usage: res.Usage}
		a.emit(endEv)
	}()

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Turns++
		a.emit(&ext.Event{Type: ext.EventTurnStart})

		assistantMsg, usage, req, err := a.complete(ctx)
		if err != nil {
			return res, err
		}
		res.Usage.Add(usage)

		a.append(assistantMsg)
		if assistantMsg.Content != "" {
			res.Final = assistantMsg.Content
		}

		// Execute requested tool calls (if any).
		for i := range assistantMsg.ToolCalls {
			call := assistantMsg.ToolCalls[i]
			toolMsg := a.executeTool(ctx, &call)
			a.append(toolMsg)
		}

		// turn_end: extensions may append entries (e.g. reminders) or
		// force an extra turn.
		turnEv := &ext.Event{
			Type:     ext.EventTurnEnd,
			Continue: len(assistantMsg.ToolCalls) > 0,
			Request:  req,
		}
		a.emit(turnEv)
		for _, injected := range turnEv.Messages {
			a.append(injected)
		}
		if turnEv.Continue {
			continue
		}
		break
	}
	return res, nil
}

// complete performs one model completion, streaming events as they
// arrive, and returns the assembled assistant message.
//
// The third result describes the request for the observers that need
// it: the provider, the model, the tool count, the stop reason, the
// token usage and the elapsed time. It carries no content.
func (a *Agent) complete(ctx context.Context) (llm.Message, llm.Usage, ext.Request, error) {
	// context hook: extensions may rewrite the outgoing context.
	ctxEv := &ext.Event{Type: ext.EventContext, Messages: a.History()}
	a.emit(ctxEv)
	messages := ctxEv.Messages

	// The configuration is read as a snapshot: a model switch that
	// lands while the request is being built applies to the next turn,
	// not halfway through this one.
	cfg := a.Config()

	req := llm.ChatRequest{
		Model:       cfg.Model,
		System:      cfg.SystemPrompt,
		Messages:    messages,
		Tools:       a.Tools.Definitions(),
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
	a.emit(&ext.Event{Type: ext.EventRequestStart, Request: info})

	started := time.Now()
	stream, err := provider.Chat(ctx, req)
	if err != nil {
		return llm.Message{}, llm.Usage{}, info, fmt.Errorf("agent: provider chat: %w", err)
	}
	defer stream.Close()

	a.emit(&ext.Event{Type: ext.EventMessageStart})

	var msg llm.Message
	var usage llm.Usage
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			info.Duration = time.Since(started)
			return llm.Message{}, usage, info, fmt.Errorf("agent: stream: %w", err)
		}
		switch ev.Type {
		case llm.EventTextDelta:
			msg.Content += ev.Text
			a.emit(&ext.Event{Type: ext.EventMessageUpdate, Text: ev.Text})
		case llm.EventThinkingDelta:
			// Thinking output is surfaced to extensions but not kept in
			// the transcript (models do not expect it back).
			a.emit(&ext.Event{Type: ext.EventMessageUpdate, Text: ev.Text, Data: map[string]any{"thinking": true}})
		case llm.EventToolCall:
			msg.ToolCalls = append(msg.ToolCalls, *ev.ToolCall)
		case llm.EventDone:
			if ev.Message != nil {
				msg = *ev.Message
			}
			usage = ev.Usage
			info.StopReason = ev.StopReason
		}
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
		execResult, err := t.Execute(ctx, call.ID, call.Arguments, onUpdate)
		if err != nil {
			result = tools.Result{Content: fmt.Sprintf("tool %s failed: %v", call.Name, err), IsError: true}
		} else {
			result = execResult
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
	a.emit(&ext.Event{Type: ext.EventSessionCompact, Messages: msgs})
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
	summary := msgs[0].Content
	kept := len(msgs) - 1

	// Find the entry the retained messages start at, by matching the
	// last retained message against the tree's entries.
	firstKept := ""
	if kept > 0 {
		want := msgs[kept]
		for _, e := range a.Session.Tree().Entries() {
			if e.Type != session.EntryMessage || e.Message == nil {
				continue
			}
			if e.Message.Role == want.Role && e.Message.Content == want.Content {
				firstKept = e.ID
				break
			}
		}
	}
	if err := a.Session.Compaction(summary, firstKept, 0); err != nil && a.Logf != nil {
		a.Logf("session compaction: %v", err)
	}
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
	if err := a.Bus.Emit(e); err != nil && a.Logf != nil {
		a.Logf("hook %s: %v", e.Type, err)
	}
}
