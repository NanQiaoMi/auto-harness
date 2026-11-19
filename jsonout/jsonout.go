// Package jsonout implements the harness's JSON output mode: a
// one-shot run whose whole story is a JSON event stream on stdout.
//
// The mode is the machine-readable half of the print mode:
//
//	harness -print "..."            # the answer, as text
//	harness --mode json "..."       # the whole run, as JSON lines
//
// The stream is JSONL: one JSON object per line, in the order the
// events happened. The first record is the session header, so a
// consumer knows which session the events belong to before it sees
// any of them.
//
// The mode borrows pi's shape: the event names are the harness's own,
// and the message_update event carries the usage alongside the delta
// rather than repeating the whole message.
package jsonout

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
)

// The event types the stream carries.
//
// They are the harness's own event names, so a consumer that knows
// the REPL's events knows these ones. The mode adds the ones that
// only make sense in a headless run.
const (
	// EventSessionHeader is the first record: it names the session
	// the events belong to.
	EventSessionHeader = "session"
	// EventAgentStart fires when a run starts.
	EventAgentStart = "agent_start"
	// EventAgentEnd fires when a run ends.
	EventAgentEnd = "agent_end"
	// EventAgentBeforeSettle is the last actionable boundary of a run:
	// an extension may still ask for one more turn here.
	EventAgentBeforeSettle = "agent_before_settle"
	// EventAgentSettled fires when the harness has no more automatic
	// work. It is the last record of a successful run.
	EventAgentSettled = "agent_settled"
	// EventRetry fires when a failed model request or tool call is
	// about to be tried again.
	EventRetry = "retry"
	// EventTurnStart fires when one turn starts.
	EventTurnStart = "turn_start"
	// EventTurnEnd fires when one turn ends.
	EventTurnEnd = "turn_end"
	// EventRequestStart fires when a model request begins.
	EventRequestStart = "request_start"
	// EventMessageStart fires when a message starts.
	EventMessageStart = "message_start"
	// EventMessageUpdate fires for each streamed delta.
	EventMessageUpdate = "message_update"
	// EventMessageEnd fires when a message completes.
	EventMessageEnd = "message_end"
	// EventToolExecutionStart fires when a tool call starts.
	EventToolExecutionStart = "tool_execution_start"
	// EventToolExecutionEnd fires when a tool call ends.
	EventToolExecutionEnd = "tool_execution_end"
	// EventSessionBeforeCompact fires before the history is compacted.
	EventSessionBeforeCompact = "session_before_compact"
	// EventSessionCompact fires after the history is compacted.
	EventSessionCompact = "session_compact"
)

// Header is the first record of the stream.
//
// It is the session's identity: a consumer that writes the events to
// a file can find the session again from the header alone.
type Header struct {
	// Type is always "session".
	Type string `json:"type"`
	// Version is the session file's format version.
	Version int `json:"version,omitempty"`
	// ID identifies the session.
	ID string `json:"id"`
	// Timestamp is when the session started.
	Timestamp string `json:"timestamp"`
	// CWD is the directory the session was created in.
	CWD string `json:"cwd,omitempty"`
	// ParentSession is the session this one was forked from.
	ParentSession string `json:"parentSession,omitempty"`
}

// Record is one event on the stream.
//
// The type names the event; the rest of the fields are its payload.
// The fields are flat rather than nested: a consumer reads the event
// it knows and ignores the rest.
type Record struct {
	// Type names the event.
	Type string `json:"type"`
	// Text is the delta of a message_update.
	Text string `json:"text,omitempty"`
	// Message is the message of a message_start or message_end.
	Message *Message `json:"message,omitempty"`
	// Thinking marks a message_update that carries the model's
	// reasoning rather than its answer.
	Thinking bool `json:"thinking,omitempty"`
	// Usage is the token accounting of the run so far.
	Usage *Usage `json:"usage,omitempty"`
	// Request describes the model request a turn made.
	Request *Request `json:"request,omitempty"`
	// ToolCall names the tool a tool execution is about.
	ToolCall *ToolCall `json:"toolCall,omitempty"`
	// ToolResult is the outcome of a tool execution.
	ToolResult *ToolResult `json:"result,omitempty"`
	// Aborted says the run was stopped rather than finished.
	Aborted bool `json:"aborted,omitempty"`
	// WillRetry says the run will be retried, so agent_end is not
	// the last word.
	WillRetry bool `json:"willRetry,omitempty"`
	// Reason is the error a retry record is about.
	Reason string `json:"reason,omitempty"`
	// Attempt is the retry number (1-based) and MaxAttempts the budget.
	Attempt     int `json:"attempt,omitempty"`
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// Tool names the tool a tool retry is about; empty on a retry of
	// the model request itself.
	Tool string `json:"tool,omitempty"`
	// DelayMs is the backoff before the attempt.
	DelayMs int64 `json:"delayMs,omitempty"`
}

// Message is a message on the wire.
type Message struct {
	// Role is the message's role.
	Role string `json:"role"`
	// Content is the message's text.
	Content string `json:"content,omitempty"`
	// ToolCalls are the calls an assistant message asks for.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ToolCallID links a tool message to the call it answers.
	ToolCallID string `json:"toolCallId,omitempty"`
	// IsError marks a tool message that reports a failure.
	IsError bool `json:"isError,omitempty"`
}

// ToolCall is one call the model asks for.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolResult is the outcome of one tool call.
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

// Usage is the token accounting.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Request describes one model request.
type Request struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Tools    int    `json:"tools"`
}

// Writer writes the event stream.
//
// The stream is written from the agent's goroutine while the run
// proceeds, so the writer is guarded: two records must never
// interleave.
type Writer struct {
	out io.Writer
	mu  sync.Mutex
}

// NewWriter creates a writer that writes to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{out: w}
}

// WriteHeader writes the session header.
//
// It goes first, before any event: a consumer that sees an event
// without a header has missed something.
func (w *Writer) WriteHeader(sess *session.Session, cwd string) error {
	if sess == nil {
		return fmt.Errorf("jsonout: there is no session")
	}
	header := Header{
		Type:    EventSessionHeader,
		ID:      sess.Meta.ID,
		Version: session.VersionTree,
		CWD:     cwd,
	}
	if !sess.Meta.CreatedAt.IsZero() {
		header.Timestamp = sess.Meta.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return w.write(header)
}

// Write writes one record.
func (w *Writer) Write(record Record) error {
	return w.write(record)
}

// write sends one JSON line to the output.
func (w *Writer) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("jsonout: marshal: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.out.Write(data); err != nil {
		return fmt.Errorf("jsonout: write: %w", err)
	}
	if _, err := w.out.Write([]byte("\n")); err != nil {
		return fmt.Errorf("jsonout: write: %w", err)
	}
	return nil
}

// Install subscribes the writer to the harness's event bus.
//
// Every event the harness emits becomes a record on the stream. The
// subscription is returned so the caller can undo it.
func (w *Writer) Install(bus *ext.Bus) func() {
	if bus == nil {
		return func() {}
	}
	var unsubscribes []func()
	for _, eventType := range w.eventTypes() {
		t := eventType
		unsubscribes = append(unsubscribes, bus.On(t, func(e *ext.Event) error {
			// A delegated agent has an isolated transcript. Its report
			// reaches the primary stream through the agent tool result,
			// not as duplicate top-level lifecycle records.
			if ext.AgentID(e) != "" {
				return nil
			}
			w.emit(e)
			return nil
		}))
	}
	return func() {
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
	}
}

// eventTypes returns the bus events the stream carries.
func (w *Writer) eventTypes() []ext.EventType {
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

// emit writes one harness event to the stream.
func (w *Writer) emit(e *ext.Event) {
	record := Record{Type: string(e.Type)}

	switch e.Type {
	case ext.EventMessageUpdate:
		record.Text = e.Text
		// The thinking flag rides along with the delta: a consumer
		// that shows the answer must not show the reasoning in the
		// same stream.
		if e.Data != nil {
			thinking, _ := e.Data["thinking"].(bool)
			record.Thinking = thinking
		}
	case ext.EventMessageStart, ext.EventMessageEnd:
		if e.Message != nil {
			record.Message = convertMessage(*e.Message)
		}
	case ext.EventToolExecutionStart:
		if e.ToolCall != nil {
			record.ToolCall = &ToolCall{
				ID:        e.ToolCall.ID,
				Name:      e.ToolCall.Name,
				Arguments: string(e.ToolCall.Arguments),
			}
		}
	case ext.EventToolExecutionEnd:
		if e.ToolCall != nil {
			record.ToolCall = &ToolCall{
				ID:        e.ToolCall.ID,
				Name:      e.ToolCall.Name,
				Arguments: string(e.ToolCall.Arguments),
			}
		}
		if e.ToolResult != nil {
			record.ToolResult = &ToolResult{
				Content: e.ToolResult.Content,
				IsError: e.ToolResult.IsError,
			}
		}
	case ext.EventRequestStart:
		if e.Request.Model != "" {
			record.Request = &Request{
				Provider: e.Request.Provider,
				Model:    e.Request.Model,
				Tools:    e.Request.Tools,
			}
		}
	case ext.EventAgentEnd:
		record.Usage = &Usage{
			InputTokens:  e.Usage.InputTokens,
			OutputTokens: e.Usage.OutputTokens,
		}
	case ext.EventAgentSettled:
		record.Aborted = e.Aborted
	case ext.EventRetry:
		record.Reason = e.Reason
		record.Attempt, _ = e.Data["attempt"].(int)
		record.MaxAttempts, _ = e.Data["maxAttempts"].(int)
		record.DelayMs, _ = e.Data["delayMs"].(int64)
		record.Tool, _ = e.Data["tool"].(string)
	}

	_ = w.write(record)
}

// WriteSettled writes the record that ends a run.
//
// The run is over: nothing automatic is left to do. A consumer that
// wants to know when to stop reading waits for this one.
func (w *Writer) WriteSettled(aborted bool) error {
	return w.write(Record{Type: EventAgentSettled, Aborted: aborted})
}

// convertMessage turns a harness message into the wire shape.
func convertMessage(msg llm.Message) *Message {
	out := &Message{
		Role:       string(msg.Role),
		Content:    msg.Content,
		ToolCallID: msg.ToolCallID,
		IsError:    msg.IsError,
	}
	for _, call := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: string(call.Arguments),
		})
	}
	return out
}

// FinalText returns the answer of a finished run.
//
// The answer is the last assistant message: a run may hold several,
// and the earlier ones are the model's "let me check" remarks.
func FinalText(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	return ""
}
