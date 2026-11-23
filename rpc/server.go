package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/orchestrator"
	"github.com/NanQiaoMi/auto-harness/session"
)

// Server runs the harness's RPC mode.
//
// The server owns the agent, the session and the command registry, and
// answers the commands a client sends. It writes the responses and the
// events to one stream.
//
// The server is safe for concurrent use: the client's commands are
// handled one at a time, but a run streams its events while the client
// may still be sending commands.
type Server struct {
	// Agent is the agent the commands act on.
	Agent *agent.Agent
	// Session is the session the commands act on.
	Session *session.Session
	// Commands is the registry the slash commands live in.
	Commands *ext.CommandRegistry
	// SessionDir is where new sessions are written.
	SessionDir string
	// Models lists the known models, for the get_available_models
	// command.
	Models func() []ModelInfo
	// Bash runs a shell command for the bash command. It is nil when
	// the harness has no bash tool.
	Bash func(ctx context.Context, command string) (string, int, error)
	// Gate lets a caller veto a command before it runs. It is nil when
	// nothing is watching the commands.
	Gate func(command string) error
	// Profile returns the active agent environment description.
	Profile func() any
	// Plugins returns the discovered plugin catalog.
	Plugins func() any
	// Checkpoints lists workspace rollback points.
	Checkpoints func(ctx context.Context) ([]gitkit.Checkpoint, error)
	// CreateCheckpoint creates a manual workspace snapshot.
	CreateCheckpoint func(ctx context.Context, label string) (gitkit.Checkpoint, bool, error)
	// RewindCheckpoint restores a workspace snapshot.
	RewindCheckpoint func(ctx context.Context, selector string, resetHead bool) (gitkit.RewindResult, error)
	// UndoTurn restores a workspace snapshot and the conversation turn
	// that produced it.
	UndoTurn func(ctx context.Context, selector string) (gitkit.TurnRewindResult, error)
	// Agents lists delegated background tasks.
	Agents func() []orchestrator.Task
	// AgentTask reports one delegated task.
	AgentTask func(id string) (orchestrator.Task, error)
	// CancelAgent stops one delegated task and its descendants.
	CancelAgent func(ctx context.Context, id string) error
	// OnSessionChange observes a session switch so runtime bindings
	// (for example delegated-session parent links) can follow it.
	OnSessionChange func(*session.Session)

	// out is the stream the records go to.
	out io.Writer
	// mu guards the output stream and the run state.
	mu sync.Mutex
	// streaming reports whether a run is in progress.
	streaming bool
	// cancel stops the current run.
	cancel context.CancelFunc
	// stats accumulates the session's token accounting.
	stats StatsPayload
	// turns counts the runs.
	turns int
	// runs tracks the runs in progress, so the server can wait for
	// them before it exits.
	runs sync.WaitGroup
}

// NewServer creates a server that writes to w.
func NewServer(ag *agent.Agent, sess *session.Session, commands *ext.CommandRegistry, w io.Writer) *Server {
	return &Server{
		Agent:    ag,
		Session:  sess,
		Commands: commands,
		out:      w,
	}
}

// Install subscribes the server to the harness's event bus.
//
// Every event the harness emits becomes an RPC event, so a client sees
// the same activity the REPL shows. The subscription is returned so the
// caller can undo it.
func (s *Server) Install(bus *ext.Bus) func() {
	if bus == nil {
		return func() {}
	}
	var unsubscribes []func()
	for _, eventType := range s.eventTypes() {
		t := eventType
		unsubscribes = append(unsubscribes, bus.On(t, func(e *ext.Event) error {
			// Delegated agents have their own transcript. Their
			// lifecycle events must not be mistaken for the primary
			// run's events; the agent tool reports their result.
			if ext.AgentID(e) != "" {
				return nil
			}
			s.emitEvent(e)
			return nil
		}))
	}
	return func() {
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
	}
}

// eventTypes returns the bus events the RPC mode forwards.
func (s *Server) eventTypes() []ext.EventType {
	return []ext.EventType{
		ext.EventAgentStart,
		ext.EventAgentEnd,
		ext.EventAgentBeforeSettle,
		ext.EventAgentSettled,
		ext.EventRetry,
		ext.EventTurnStart,
		ext.EventTurnEnd,
		ext.EventMessageStart,
		ext.EventMessageUpdate,
		ext.EventMessageEnd,
		ext.EventToolExecutionStart,
		ext.EventToolExecutionEnd,
		ext.EventRequestStart,
		ext.EventSessionBeforeCompact,
		ext.EventSessionCompact,
	}
}

// Serve reads commands until the input ends.
//
// The loop handles one command at a time. A prompt's response is
// written as soon as the prompt is accepted, and the run's events keep
// streaming afterwards — a client that waits for the response alone
// would miss the work.
//
// When the input ends, the server waits for the runs that are still
// going before it returns. A client that closes stdin right after
// sending a prompt expects the answer to still arrive.
func (s *Server) Serve(ctx context.Context, in io.Reader) error {
	reader := NewReader(in)
	for {
		raw, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			// A malformed record is reported and the stream continues:
			// one bad line should not kill a session.
			s.write(Failure("", "parse", err.Error()))
			continue
		}
		command, err := parseCommand(raw)
		if err != nil {
			s.write(Failure("", "parse", err.Error()))
			continue
		}
		response := s.dispatch(ctx, command)
		if response != nil {
			s.write(response)
		}
	}

	s.waitForRuns()
	return nil
}

// waitForRuns blocks until the runs in progress are done.
//
// The wait is what makes "close stdin after sending a prompt" work:
// the answer still arrives, it just arrives after the input ended.
func (s *Server) waitForRuns() {
	s.runs.Wait()
}

// dispatch routes a command to its handler.
//
// The handler returns nil for a command that answers itself — a prompt
// answers when it is accepted, and the run's events carry the rest.
func (s *Server) dispatch(ctx context.Context, command *Command) *Response {
	switch command.Type {
	case CmdPrompt:
		return s.prompt(ctx, command)
	case CmdAbort:
		return s.abort(command)
	case CmdNewSession:
		return s.newSession(command)
	case CmdGetState:
		return s.getState(command)
	case CmdGetMessages:
		return s.getMessages(command)
	case CmdSetModel:
		return s.setModel(command)
	case CmdGetAvailableModels:
		return s.getAvailableModels(command)
	case CmdSetThinking:
		return s.setThinking(command)
	case CmdCompact:
		return s.compact(ctx, command)
	case CmdBash:
		return s.bash(ctx, command)
	case CmdGetSessionStats:
		return s.getSessionStats(command)
	case CmdGetContext:
		return s.getContext(command)
	case CmdGetEntries:
		return s.getEntries(command)
	case CmdGetTree:
		return s.getTree(command)
	case CmdGetLastAssistantText:
		return s.getLastAssistantText(command)
	case CmdSetSessionName:
		return s.setSessionName(command)
	case CmdGetCommands:
		return s.getCommands(command)
	case CmdGetProfile:
		return s.getProfile(command)
	case CmdGetPlugins:
		return s.getPlugins(command)
	case CmdFork:
		return s.fork(command, "fork")
	case CmdClone:
		return s.fork(&Command{ID: command.ID, Type: CmdFork, raw: command.raw}, "clone")
	case CmdGetCheckpoints:
		return s.getCheckpoints(ctx, command)
	case CmdCreateCheckpoint:
		return s.createCheckpoint(ctx, command)
	case CmdRewindCheckpoint:
		return s.rewindCheckpoint(ctx, command)
	case CmdGetAgents:
		return s.getAgents(command)
	case CmdGetAgent:
		return s.getAgent(command)
	case CmdCancelAgent:
		return s.cancelAgent(ctx, command)
	default:
		return Failure(command.ID, command.Type, fmt.Sprintf("Unknown command: %s", command.Type))
	}
}

func (s *Server) getAgents(command *Command) *Response {
	if s.Agents == nil {
		return Success(command.ID, CmdGetAgents, AgentsPayload{Tasks: []orchestrator.Task{}})
	}
	tasks := s.Agents()
	if tasks == nil {
		tasks = []orchestrator.Task{}
	}
	return Success(command.ID, CmdGetAgents, AgentsPayload{Tasks: tasks})
}

func (s *Server) getAgent(command *Command) *Response {
	var args AgentCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdGetAgent, err.Error())
	}
	if strings.TrimSpace(args.AgentID) == "" {
		return Failure(command.ID, CmdGetAgent, "agentId is required")
	}
	if s.AgentTask == nil {
		return Failure(command.ID, CmdGetAgent, "delegated agents are not configured")
	}
	task, err := s.AgentTask(args.AgentID)
	if err != nil {
		return Failure(command.ID, CmdGetAgent, err.Error())
	}
	return Success(command.ID, CmdGetAgent, AgentPayload{Task: task})
}

func (s *Server) cancelAgent(ctx context.Context, command *Command) *Response {
	var args AgentCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdCancelAgent, err.Error())
	}
	if strings.TrimSpace(args.AgentID) == "" {
		return Failure(command.ID, CmdCancelAgent, "agentId is required")
	}
	if s.CancelAgent == nil {
		return Failure(command.ID, CmdCancelAgent, "delegated agents are not configured")
	}
	if err := s.CancelAgent(ctx, args.AgentID); err != nil {
		return Failure(command.ID, CmdCancelAgent, err.Error())
	}
	state := string(orchestrator.StateCancelled)
	if s.AgentTask != nil {
		if task, err := s.AgentTask(args.AgentID); err == nil {
			state = string(task.State)
		}
	}
	return Success(command.ID, CmdCancelAgent, map[string]any{
		"agentId": args.AgentID,
		"state":   state,
	})
}

func (s *Server) getCheckpoints(ctx context.Context, command *Command) *Response {
	if s.Checkpoints == nil {
		return Failure(command.ID, CmdGetCheckpoints, "workspace checkpoints are not configured")
	}
	items, err := s.Checkpoints(ctx)
	if err != nil {
		return Failure(command.ID, CmdGetCheckpoints, err.Error())
	}
	if items == nil {
		items = []gitkit.Checkpoint{}
	}
	return Success(command.ID, CmdGetCheckpoints, CheckpointsPayload{Checkpoints: items})
}

func (s *Server) createCheckpoint(ctx context.Context, command *Command) *Response {
	if s.CreateCheckpoint == nil {
		return Failure(command.ID, CmdCreateCheckpoint, "workspace checkpoints are not configured")
	}
	var args CreateCheckpointCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdCreateCheckpoint, err.Error())
	}
	item, created, err := s.CreateCheckpoint(ctx, args.Label)
	if err != nil {
		return Failure(command.ID, CmdCreateCheckpoint, err.Error())
	}
	return Success(command.ID, CmdCreateCheckpoint, CheckpointPayload{
		Checkpoint: item,
		Created:    created,
	})
}

func (s *Server) rewindCheckpoint(ctx context.Context, command *Command) *Response {
	if s.RewindCheckpoint == nil {
		return Failure(command.ID, CmdRewindCheckpoint, "workspace checkpoints are not configured")
	}
	if s.isStreaming() {
		return Failure(command.ID, CmdRewindCheckpoint, "cannot restore a checkpoint while a run is in progress")
	}
	var args RewindCheckpointCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdRewindCheckpoint, err.Error())
	}
	selector := strings.TrimSpace(args.Checkpoint)
	if selector != "" {
		if s.Checkpoints == nil {
			return Failure(command.ID, CmdRewindCheckpoint, "workspace checkpoints are not configured")
		}
		items, err := s.Checkpoints(ctx)
		if err != nil {
			return Failure(command.ID, CmdRewindCheckpoint, err.Error())
		}
		selector, err = gitkit.ParseCheckpointOrdinal(items, selector)
		if err != nil {
			return Failure(command.ID, CmdRewindCheckpoint, err.Error())
		}
	}
	resetHead := true
	if args.ResetHead != nil {
		resetHead = *args.ResetHead
	}
	if args.RestoreContext {
		if !resetHead {
			return Failure(command.ID, CmdRewindCheckpoint, "restoreContext requires resetHead=true")
		}
		if s.UndoTurn == nil {
			return Failure(command.ID, CmdRewindCheckpoint, "turn undo is not configured")
		}
		result, err := s.UndoTurn(ctx, selector)
		if err != nil {
			return Failure(command.ID, CmdRewindCheckpoint, err.Error())
		}
		return Success(command.ID, CmdRewindCheckpoint, result)
	}
	result, err := s.RewindCheckpoint(ctx, selector, resetHead)
	if err != nil {
		return Failure(command.ID, CmdRewindCheckpoint, err.Error())
	}
	return Success(command.ID, CmdRewindCheckpoint, result)
}

// getProfile reports the active environment without exposing profile
// secrets or child-process environment values.
func (s *Server) getProfile(command *Command) *Response {
	if s.Profile == nil {
		return Success(command.ID, CmdGetProfile, map[string]any{"enabled": false})
	}
	return Success(command.ID, CmdGetProfile, s.Profile())
}

// getPlugins reports the plugin catalog and load state.
func (s *Server) getPlugins(command *Command) *Response {
	if s.Plugins == nil {
		return Success(command.ID, CmdGetPlugins, PluginsPayload{Plugins: []PluginInfo{}})
	}
	return Success(command.ID, CmdGetPlugins, s.Plugins())
}

// prompt sends a message to the agent.
//
// The response goes out as soon as the run starts, not when it ends:
// the run's events carry the rest, and a client that waits for the
// whole run before reading anything would deadlock on a full pipe.
func (s *Server) prompt(ctx context.Context, command *Command) *Response {
	var args PromptCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdPrompt, err.Error())
	}
	images, err := decodePromptImages(args.Images)
	if err != nil {
		return Failure(command.ID, CmdPrompt, err.Error())
	}
	if strings.TrimSpace(args.Message) == "" && len(images) == 0 {
		return Failure(command.ID, CmdPrompt, "the prompt has no message")
	}

	// A prompt that arrives while a run is in progress is queued, not
	// rejected: the agent drains it between turns, so a client can
	// steer the work it started. The server's own flag is checked
	// because it is set synchronously, while the agent's is set by the
	// goroutine that may not have entered Run yet.
	s.mu.Lock()
	running := s.streaming
	s.mu.Unlock()
	if running {
		if err := s.Agent.EnqueueWithImages(args.Message, images); err != nil {
			return Failure(command.ID, CmdPrompt, err.Error())
		}
		return Success(command.ID, CmdPrompt, map[string]any{"disposition": "queued"})
	}

	// The run gets its own context, so the abort command can stop it
	// without stopping the server.
	runCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.streaming = true
	s.cancel = cancel
	s.mu.Unlock()

	s.write(Success(command.ID, CmdPrompt, map[string]any{"disposition": "started"}))

	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		finish := func() {
			s.mu.Lock()
			s.streaming = false
			s.cancel = nil
			s.mu.Unlock()
		}
		defer finish()
		_, err := s.Agent.RunWithImages(runCtx, args.Message, images)
		if err != nil {
			s.emit(map[string]any{"type": "error", "message": err.Error()})
		}
		// A client may react to agent_settled immediately. Clear the
		// run state first so checkpoint restore is not rejected by a
		// stale streaming flag.
		//
		// The settled event itself comes from the bus: the agent emits
		// it once the queue and the settle handlers are done, which is
		// what "no automatic work is left" means. Writing one here too
		// would report idle twice.
		finish()
	}()

	return nil
}

func decodePromptImages(inputs []ImageInput) ([]llm.Image, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	images := make([]llm.Image, 0, len(inputs))
	for i, in := range inputs {
		switch {
		case in.Path != "" && in.Data != "":
			return nil, fmt.Errorf("images[%d]: specify either path or data, not both", i)
		case in.Path != "":
			img, err := llm.LoadImageFile(in.Path)
			if err != nil {
				return nil, fmt.Errorf("images[%d]: %w", i, err)
			}
			if in.MediaType != "" && !strings.EqualFold(in.MediaType, img.MediaType) && !(strings.EqualFold(in.MediaType, "image/jpg") && img.MediaType == "image/jpeg") {
				return nil, fmt.Errorf("images[%d]: content is %s, declared %s", i, img.MediaType, in.MediaType)
			}
			if in.Label != "" {
				img.Label = in.Label
			}
			images = append(images, img)
		case in.Data != "":
			raw, err := base64.StdEncoding.DecodeString(in.Data)
			if err != nil {
				return nil, fmt.Errorf("images[%d]: invalid base64: %w", i, err)
			}
			img, err := llm.ImageFromBytes(raw, in.MediaType, in.Label)
			if err != nil {
				return nil, fmt.Errorf("images[%d]: %w", i, err)
			}
			images = append(images, img)
		default:
			return nil, fmt.Errorf("images[%d]: path or data is required", i)
		}
	}
	return images, nil
}

// abort stops the current run.
func (s *Server) abort(command *Command) *Response {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return Success(command.ID, CmdAbort, nil)
}

// gate fires a session approval event and reports whether the
// operation may proceed.
//
// A handler error is reported on the event stream but does not veto:
// only an explicit Cancel stops the operation.
func (s *Server) gate(ev *ext.Event) bool {
	if s.Agent == nil || s.Agent.Bus == nil {
		return true
	}
	ok, err := s.Agent.Bus.EmitGate(ev)
	if err != nil {
		s.emit(map[string]any{"type": "error", "message": err.Error()})
	}
	return ok
}

// newSession starts a fresh session.
func (s *Server) newSession(command *Command) *Response {
	if s.isStreaming() {
		return Failure(command.ID, CmdNewSession, "cannot start a new session while a run is in progress")
	}
	ev := &ext.Event{Type: ext.EventSessionBeforeSwitch, Reason: "new"}
	if !s.gate(ev) {
		reason := ev.BlockReason
		if reason == "" {
			reason = "the new session was cancelled by an extension"
		}
		return Failure(command.ID, CmdNewSession, reason)
	}
	sess, err := session.New(s.SessionDir, s.Agent.Config().Model)
	if err != nil {
		return Failure(command.ID, CmdNewSession, err.Error())
	}
	s.mu.Lock()
	old := s.Session
	s.Session = sess
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	s.Agent.Session = sess
	s.Agent.SetHistory(nil)
	if s.OnSessionChange != nil {
		s.OnSessionChange(sess)
	}
	return Success(command.ID, CmdNewSession, NewSessionPayload{
		SessionID:   sess.Meta.ID,
		SessionFile: sess.Path,
	})
}

// CurrentSession returns the active conversation session. Checkpoint
// binding and turn undo use it so a session switch cannot leave them
// pointing at a closed session.
func (s *Server) CurrentSession() *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Session
}

// getState reports the session's state.
func (s *Server) getState(command *Command) *Response {
	state := State{
		SchemaVersion: SchemaVersion,
		Model:         s.Agent.Config().Model,
		Thinking:      s.Agent.Config().Thinking,
		IsStreaming:   s.isStreaming(),
		MessageCount:  len(s.Agent.History()),
	}
	if s.Agent.Provider() != nil {
		state.Provider = s.Agent.Provider().Name()
	}
	if s.Session != nil {
		state.SessionID = s.Session.Meta.ID
		state.SessionFile = s.Session.Path
		state.SessionName = s.Session.Meta.Name
	}
	return Success(command.ID, CmdGetState, state)
}

// getMessages returns the conversation history.
func (s *Server) getMessages(command *Command) *Response {
	history := s.Agent.History()
	messages := make([]Message, 0, len(history))
	for _, msg := range history {
		messages = append(messages, convertMessage(msg))
	}
	return Success(command.ID, CmdGetMessages, MessagesPayload{Messages: messages})
}

// setModel switches the model.
func (s *Server) setModel(command *Command) *Response {
	var args SetModelCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdSetModel, err.Error())
	}
	if args.Model == "" {
		return Failure(command.ID, CmdSetModel, "the model is empty")
	}
	s.Agent.SetModel(args.Model)
	if s.Session != nil {
		s.Session.SetModel(args.Model)
	}
	provider := ""
	if s.Agent.Provider() != nil {
		provider = s.Agent.Provider().Name()
	}
	return Success(command.ID, CmdSetModel, ModelInfo{
		ID:       args.Model,
		Name:     args.Model,
		Provider: provider,
	})
}

// getAvailableModels lists the known models.
func (s *Server) getAvailableModels(command *Command) *Response {
	if s.Models == nil {
		return Success(command.ID, CmdGetAvailableModels, ModelsPayload{Models: []ModelInfo{}})
	}
	return Success(command.ID, CmdGetAvailableModels, ModelsPayload{Models: s.Models()})
}

// setThinking turns the thinking mode on or off.
func (s *Server) setThinking(command *Command) *Response {
	var args SetThinkingCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdSetThinking, err.Error())
	}
	s.Agent.SetThinking(args.Enabled)
	return Success(command.ID, CmdSetThinking, nil)
}

// compact compresses the conversation history.
func (s *Server) compact(ctx context.Context, command *Command) *Response {
	if err := s.Agent.Compact(ctx); err != nil {
		return Failure(command.ID, CmdCompact, err.Error())
	}
	return Success(command.ID, CmdCompact, map[string]any{
		"messages": len(s.Agent.History()),
	})
}

// bash runs a shell command.
//
// The command is the client's own, but it is still a command on the
// user's machine, so it goes through the same gate the model's
// commands do: an extension may veto it.
func (s *Server) bash(ctx context.Context, command *Command) *Response {
	var args BashCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdBash, err.Error())
	}
	if s.Bash == nil {
		return Failure(command.ID, CmdBash, "the harness has no bash tool")
	}
	if s.Gate != nil {
		if err := s.Gate(args.Command); err != nil {
			return Failure(command.ID, CmdBash, err.Error())
		}
	}
	output, exitCode, err := s.Bash(ctx, args.Command)
	payload := BashPayload{Output: output, ExitCode: exitCode}
	if err != nil {
		payload.IsError = true
		if payload.Output == "" {
			payload.Output = err.Error()
		}
	}
	return Success(command.ID, CmdBash, payload)
}

// getSessionStats reports the session's token accounting.
func (s *Server) getSessionStats(command *Command) *Response {
	s.mu.Lock()
	stats := s.stats
	stats.Messages = len(s.Agent.History())
	s.mu.Unlock()
	return Success(command.ID, CmdGetSessionStats, stats)
}

// getContext reports the context budget without exposing transcript or
// summary content.
func (s *Server) getContext(command *Command) *Response {
	status, ok := s.Agent.ContextStatus()
	if !ok {
		return Success(command.ID, CmdGetContext, ContextPayload{Enabled: false})
	}
	return Success(command.ID, CmdGetContext, ContextPayload{
		Enabled: true,
		Status:  &status,
	})
}

// getEntries returns the session tree's entries.
func (s *Server) getEntries(command *Command) *Response {
	entries, leafID := s.treeEntries()
	return Success(command.ID, CmdGetEntries, EntriesPayload{Entries: entries, LeafID: leafID})
}

// getTree returns the session tree.
func (s *Server) getTree(command *Command) *Response {
	entries, leafID := s.treeEntries()
	return Success(command.ID, CmdGetTree, TreePayload{Entries: entries, LeafID: leafID})
}

// getLastAssistantText returns the last assistant message.
func (s *Server) getLastAssistantText(command *Command) *Response {
	history := s.Agent.History()
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleAssistant && history[i].Content != "" {
			return Success(command.ID, CmdGetLastAssistantText, TextPayload{Text: history[i].Content})
		}
	}
	return Success(command.ID, CmdGetLastAssistantText, TextPayload{})
}

// setSessionName names the session.
func (s *Server) setSessionName(command *Command) *Response {
	var args SetSessionNameCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdSetSessionName, err.Error())
	}
	if s.Session == nil {
		return Failure(command.ID, CmdSetSessionName, "there is no session")
	}
	if err := s.Session.SetName(args.Name); err != nil {
		return Failure(command.ID, CmdSetSessionName, err.Error())
	}
	return Success(command.ID, CmdSetSessionName, nil)
}

// getCommands lists the slash commands the harness knows.
func (s *Server) getCommands(command *Command) *Response {
	payload := CommandsPayload{Commands: []CommandInfo{}}
	if s.Commands != nil {
		for _, name := range s.Commands.List() {
			cmd, ok := s.Commands.Get(name)
			if !ok {
				continue
			}
			payload.Commands = append(payload.Commands, CommandInfo{
				Name:        name,
				Description: cmd.Description,
				Source:      "builtin",
			})
		}
	}
	return Success(command.ID, CmdGetCommands, payload)
}

// fork branches the session into a new file. The reason says which
// command asked for it: "fork" or "clone".
func (s *Server) fork(command *Command, reason string) *Response {
	if s.Session == nil {
		return Failure(command.ID, CmdFork, "there is no session")
	}
	var args ForkCommand
	if err := decodeArgs(command.raw, &args); err != nil {
		return Failure(command.ID, CmdFork, err.Error())
	}
	// The event fires before the file is written, so a veto leaves no
	// half-forked session behind.
	entry := args.EntryID
	if entry == "" {
		if tree := s.Session.Tree(); tree != nil {
			entry = tree.LeafID()
		}
	}
	ev := &ext.Event{Type: ext.EventSessionBeforeFork, Reason: reason, EntryID: entry}
	if !s.gate(ev) {
		msg := ev.BlockReason
		if msg == "" {
			msg = "the " + reason + " was cancelled by an extension"
		}
		return Failure(command.ID, CmdFork, msg)
	}
	forked, err := session.Fork(s.Session, session.ForkOptions{
		Dir:       s.SessionDir,
		AtEntryID: args.EntryID,
	})
	if err != nil {
		return Failure(command.ID, CmdFork, err.Error())
	}
	return Success(command.ID, CmdFork, ForkPayload{
		SessionID:   forked.Meta.ID,
		SessionFile: forked.Path,
	})
}

// treeEntries returns the session tree's entries and its leaf.
func (s *Server) treeEntries() ([]Entry, string) {
	if s.Session == nil {
		return []Entry{}, ""
	}
	tree := s.Session.Tree()
	if tree == nil {
		return []Entry{}, ""
	}
	entries := tree.Entries()
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, Entry{
			ID:        entry.ID,
			ParentID:  entry.ParentID,
			Type:      string(entry.Type),
			Timestamp: entry.Timestamp.UnixMilli(),
		})
	}
	return out, tree.LeafID()
}

// write sends one record to the output stream.
//
// The stream is guarded, because a run's events and a command's
// response can be written at the same time and a half-written record
// would break the framing.
func (s *Server) write(record any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = WriteRecord(s.out, record)
}

// emit sends an event to the output stream.
func (s *Server) emit(data map[string]any) {
	s.write(Event{SchemaVersion: SchemaVersion, Type: eventType(data), Data: data})
}

// emitEvent forwards a harness event to the output stream.
//
// The event's type is the harness's own name, so a client that knows
// the REPL's events knows the RPC ones. The payload is the event's
// fields, without the ones the harness keeps for itself.
func (s *Server) emitEvent(e *ext.Event) {
	data := map[string]any{"type": string(e.Type)}
	if e.Reason != "" {
		// retry carries the failure; the session events carry what
		// asked for the operation ("new", "fork", "reload").
		data["reason"] = e.Reason
	}
	if e.Text != "" {
		data["text"] = e.Text
	}
	if e.Message != nil {
		data["message"] = convertMessage(*e.Message)
	}
	if e.ToolCall != nil {
		data["toolCall"] = map[string]any{
			"id":        e.ToolCall.ID,
			"name":      e.ToolCall.Name,
			"arguments": string(e.ToolCall.Arguments),
		}
	}
	if e.ToolResult != nil {
		data["result"] = map[string]any{
			"content": e.ToolResult.Content,
			"isError": e.ToolResult.IsError,
		}
	}
	if e.Usage.InputTokens != 0 || e.Usage.OutputTokens != 0 {
		data["usage"] = map[string]any{
			"inputTokens":  e.Usage.InputTokens,
			"outputTokens": e.Usage.OutputTokens,
		}
	}
	if e.Request.Model != "" {
		data["request"] = map[string]any{
			"provider": e.Request.Provider,
			"model":    e.Request.Model,
			"tools":    e.Request.Tools,
		}
	}
	// The settled record always says whether the run was aborted: a
	// client that reads "aborted" and gets nothing cannot tell a
	// finished run from an old server that never wrote the field.
	if e.Type == ext.EventAgentSettled {
		data["aborted"] = e.Aborted
	}
	if e.Data != nil {
		for key, value := range e.Data {
			data[key] = value
		}
	}

	// The usage event is where the session's token accounting is
	// accumulated: the RPC server keeps the totals it reports.
	if e.Type == ext.EventAgentEnd {
		s.mu.Lock()
		s.turns++
		s.stats.InputTokens += e.Usage.InputTokens
		s.stats.OutputToken += e.Usage.OutputTokens
		s.stats.Turns = s.turns
		s.mu.Unlock()
	}

	s.write(Event{SchemaVersion: SchemaVersion, Type: string(e.Type), Data: data})
}

// eventType reads an event's type out of its payload.
func eventType(data map[string]any) string {
	if t, ok := data["type"].(string); ok {
		return t
	}
	return ""
}

// setStreaming records whether a run is in progress.
func (s *Server) setStreaming(streaming bool) {
	s.mu.Lock()
	s.streaming = streaming
	s.mu.Unlock()
}

// isStreaming reports whether a run is in progress.
func (s *Server) isStreaming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streaming
}

// convertMessage turns a harness message into the RPC shape.
func convertMessage(msg llm.Message) Message {
	out := Message{
		Role:       string(msg.Role),
		Content:    msg.Content,
		ToolCallID: msg.ToolCallID,
		IsError:    msg.IsError,
	}
	for _, call := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}
	return out
}

// parseCommand decodes one command record.
//
// The raw bytes are kept on the command: each handler decodes the
// arguments it needs, so the dispatcher does not have to know every
// command's fields.
func parseCommand(raw []byte) (*Command, error) {
	var command Command
	if err := json.Unmarshal(raw, &command); err != nil {
		return nil, fmt.Errorf("rpc: parse command: %w", err)
	}
	if err := ValidateSchemaVersion(command.SchemaVersion); err != nil {
		return nil, fmt.Errorf("rpc: %w", err)
	}
	if command.Type == "" {
		return nil, fmt.Errorf("rpc: the command has no type")
	}
	command.raw = raw
	return &command, nil
}
