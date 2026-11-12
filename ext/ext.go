// Package ext is the extension system, modeled after pi's ExtensionAPI:
// extensions subscribe to lifecycle/agent hooks via an event bus and
// register tools and slash commands. Everything the harness does emits
// events here, so extensions can observe and shape behavior without
// patching the core.
package ext

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mimi/auto-harness/llm"
	"github.com/mimi/auto-harness/tools"
)

// EventType identifies a hook. Names mirror pi's event names where the
// concepts match.
type EventType string

const (
	// Session lifecycle
	EventSessionStart    EventType = "session_start"
	EventSessionShutdown EventType = "session_shutdown"

	// Agent lifecycle
	EventBeforeAgentStart EventType = "before_agent_start" // may modify Message / SystemPrompt
	EventAgentStart       EventType = "agent_start"
	EventAgentEnd         EventType = "agent_end"

	// Turn lifecycle (a turn is one model completion + its tool executions)
	EventTurnStart EventType = "turn_start"
	EventTurnEnd   EventType = "turn_end" // may set Continue / append Messages

	// EventRequestStart fires when a model request begins. It carries
	// the request's facts — provider, model, tool count — so an
	// observer can measure one request without patching the provider.
	EventRequestStart EventType = "request_start"

	// Message streaming
	EventMessageStart  EventType = "message_start"
	EventMessageUpdate EventType = "message_update"
	EventMessageEnd    EventType = "message_end" // may replace Message

	// Tool execution
	EventToolExecutionStart  EventType = "tool_execution_start"
	EventToolExecutionUpdate EventType = "tool_execution_update"
	EventToolResult          EventType = "tool_result"
	EventToolExecutionEnd    EventType = "tool_execution_end"
	// EventToolCall fires before a tool runs. It may modify the tool
	// arguments in place and block the call (pi's tool_call hook).
	EventToolCall EventType = "tool_call"

	// Context assembly (may replace Messages before a provider call)
	EventContext EventType = "context"

	// Compaction lifecycle
	EventSessionBeforeCompact EventType = "session_before_compact"
	EventSessionCompact       EventType = "session_compact"

	// User input (may transform or fully handle the line)
	EventInput EventType = "input"

	// EventUserBash fires when the user runs a shell command through
	// the "!" shortcut. A handler may block the command.
	EventUserBash EventType = "user_bash"
)

// Event is a hook payload. Handlers mutate fields in place; which fields
// are meaningful for each event type is documented on the EventType.
type Event struct {
	Type   EventType
	Reason string // session_start/shutdown reason: startup/new/resume/reload...

	// Message is the message being produced/consumed (mutable on
	// before_agent_start, message_end, input).
	Message *llm.Message

	// SystemPrompt is mutable on before_agent_start.
	SystemPrompt string

	// Text carries delta content for message_update / tool_execution_update.
	Text string

	// ToolCall / ToolResult are set on tool execution events.
	ToolCall   *llm.ToolCall
	ToolResult *tools.Result

	// Messages is the context slice for EventContext (mutable) and the
	// entries appended for EventTurnEnd.
	Messages []llm.Message

	// Cancel asks the caller to abort the guarded operation.
	Cancel bool

	// Block stops the guarded operation from running (tool_call). The
	// BlockReason explains why, and is reported back to the model.
	Block bool
	// BlockReason is the message shown when a call is blocked.
	BlockReason string

	// Continue asks the agent loop to run another turn (turn_end).
	Continue bool

	// Usage accumulates token accounting on agent_end.
	Usage llm.Usage

	// Request describes the model request a turn made. It is set on
	// request_start and turn_end, so an observer can measure one request
	// without patching the provider.
	Request Request

	// Data is an escape hatch for extension-specific payloads.
	Data map[string]any

	// BashCommand is the command the user ran through the "!"
	// shortcut (user_bash).
	BashCommand string
	// ExcludeFromContext reports that the command's output must not
	// reach the model (the "!!" shortcut).
	ExcludeFromContext bool
}

// Request describes one model request.
//
// The fields are the facts about a request that are safe to record:
// names and counts, never the prompt, the completion or the tool
// arguments.
type Request struct {
	// Provider is the provider that served the request.
	Provider string
	// Model is the requested model.
	Model string
	// Tools is the number of tools offered to the model.
	Tools int
	// StopReason is the provider's normalized stop reason.
	StopReason string
	// Usage is the request's token accounting.
	Usage llm.Usage
	// Duration is how long the request took.
	Duration time.Duration
}

// Handler processes an event. Returning an error does not stop dispatch;
// errors are collected and returned by Emit.
type Handler func(e *Event) error

type subscription struct {
	id      int
	handler Handler
}

// Bus dispatches events to handlers in subscription order (pi runs
// handlers in load order then registration order).
type Bus struct {
	mu       sync.RWMutex
	handlers map[EventType][]subscription
	nextID   int
}

func NewBus() *Bus {
	return &Bus{handlers: map[EventType][]subscription{}}
}

// On subscribes a handler and returns an unsubscribe function.
func (b *Bus) On(t EventType, h Handler) (unsubscribe func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID
	b.handlers[t] = append(b.handlers[t], subscription{id: id, handler: h})
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.handlers[t]
		for i, s := range subs {
			if s.id == id {
				b.handlers[t] = append(subs[:i], subs[i+1:]...)
				return
			}
		}
	}
}

// Emit dispatches an event to all handlers of its type in order. Dispatch
// continues past handler errors so one bad extension cannot starve the
// others; the joined error is returned to the caller.
func (b *Bus) Emit(e *Event) error {
	b.mu.RLock()
	subs := append([]subscription(nil), b.handlers[e.Type]...)
	b.mu.RUnlock()
	var errs []error
	for _, s := range subs {
		if err := s.handler(e); err != nil {
			errs = append(errs, fmt.Errorf("ext: handler for %s: %w", e.Type, err))
		}
	}
	return errors.Join(errs...)
}

// HandlerCount returns the number of handlers for an event type.
func (b *Bus) HandlerCount(t EventType) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.handlers[t])
}

// Command is a REPL slash command (pi.registerCommand).
type Command struct {
	Description string
	Handler     func(ctx context.Context, args string) error
}

// CommandRegistry holds slash commands.
type CommandRegistry struct {
	mu   sync.RWMutex
	cmds map[string]*Command
}

func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{cmds: map[string]*Command{}}
}

// Register adds a command. Duplicate names get a numeric suffix
// ("review" -> "review:1"), matching pi's behavior. Returns the final name.
func (r *CommandRegistry) Register(name string, c *Command) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	final := name
	for i := 1; ; i++ {
		if _, exists := r.cmds[final]; !exists {
			break
		}
		final = fmt.Sprintf("%s:%d", name, i)
	}
	r.cmds[final] = c
	return final
}

// Get returns a command by name.
func (r *CommandRegistry) Get(name string) (*Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.cmds[name]
	return c, ok
}

// Unregister removes a command by name. It is used when an extension is
// unloaded (hot reload). Returns an error if the command is unknown.
func (r *CommandRegistry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cmds[name]; !ok {
		return fmt.Errorf("ext: no such command %q", name)
	}
	delete(r.cmds, name)
	return nil
}

// List returns command names sorted alphabetically.
func (r *CommandRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.cmds))
	for n := range r.cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// API is the object extensions receive (pi.ExtensionAPI equivalent).
// It exposes exactly the surfaces extensions should build on.
type API struct {
	Bus      *Bus
	Tools    *tools.Registry
	Commands *CommandRegistry

	// scopes tracks what each extension registered, so the manager can
	// undo it on reload.
	scopes scopeStack

	mu       sync.Mutex
	model    string
	thinking bool
}

// NewAPI creates an extension API wired to the given registries.
func NewAPI(bus *Bus, reg *tools.Registry, cmds *CommandRegistry) *API {
	return &API{Bus: bus, Tools: reg, Commands: cmds}
}

// On subscribes to a hook event. The subscription is recorded in the
// current extension scope, so it is removed automatically on reload.
func (a *API) On(t EventType, h Handler) (unsubscribe func()) {
	unsub := a.Bus.On(t, h)
	if sc := a.currentScope(); sc != nil {
		sc.recordUnsubscribe(unsub)
	}
	return unsub
}

// RegisterTool adds a tool (pi.registerTool). The tool is recorded in
// the current extension scope, so it is removed automatically on reload.
func (a *API) RegisterTool(t *tools.Tool) error {
	if err := a.Tools.Register(t); err != nil {
		return err
	}
	if sc := a.currentScope(); sc != nil {
		sc.recordTool(a.toolName(t))
	}
	return nil
}

// RegisterToolRenderer adds a renderer resolver (pi's
// registerToolRenderer).
//
// The resolver decides how a tool's calls and results are shown. It
// may return its own renderers, or call next to get the ones the
// remaining resolvers — and finally the registered tool — would use.
// The resolvers run in registration order.
//
// The resolver is recorded in the current extension scope, so it is
// removed automatically on reload: a reloaded extension must not end
// up with its renderer registered twice.
func (a *API) RegisterToolRenderer(resolver tools.RendererResolver) {
	remove := a.Tools.AddRenderer(resolver)
	if sc := a.currentScope(); sc != nil {
		sc.recordUnsubscribe(remove)
	}
}

// RegisterCommand adds a slash command (pi.registerCommand). Returns the
// final name (possibly suffixed on collision). The command is recorded in
// the current extension scope, so it is removed automatically on reload.
func (a *API) RegisterCommand(name string, c *Command) string {
	final := a.Commands.Register(name, c)
	if sc := a.currentScope(); sc != nil {
		sc.recordCommand(final)
	}
	return final
}

// SetModel switches the active model (pi.setModel).
func (a *API) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
}

// Model returns the active model set via SetModel, if any.
func (a *API) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// SetThinking toggles extended thinking (pi.setThinkingLevel minimal).
func (a *API) SetThinking(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinking = on
}

// Thinking reports whether extended thinking is requested.
func (a *API) Thinking() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.thinking
}
