// Package llm defines the neutral LLM abstraction used by the harness,
// modeled after pi's pi-ai package: a single message model and streaming
// interface that every provider adapts to.
package llm

import (
	"context"
	"encoding/json"
	"io"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries the result of a tool call back to the model.
	RoleTool Role = "tool"
)

// ToolCall is a model request to invoke a tool. Arguments holds the raw
// JSON object produced by the model; decoding is the tool's business.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message is the internal neutral message format. Provider adapters map
// this to and from their wire formats.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// Images carries image attachments on user or tool messages.
	Images []Image `json:"images,omitempty"`
	// ToolCalls is set on assistant messages requesting tool invocations.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a RoleTool message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// IsError marks a tool result as a failure (surfaced to the model as such).
	IsError bool `json:"is_error,omitempty"`
}

// ToolDefinition describes a tool exposed to the model. Parameters is a
// JSON Schema object describing the tool's arguments.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Usage reports token accounting for a completion.
type Usage struct {
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Add accumulates other into u.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

// Total is the number of tokens billed for the requests, cached or not.
//
// The cache counts are included because they are tokens the provider
// processed: a budget that ignored them would let a cached run look
// free. They are not added to the input count, which keeps the input
// number what the provider reported.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// ChatRequest is a single completion request.
type ChatRequest struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolDefinition
	MaxTokens   int
	Temperature *float64
	// Thinking requests extended reasoning where the provider supports it.
	Thinking bool
}

// EventType enumerates streaming event kinds.
type EventType string

const (
	// EventTextDelta carries a Text chunk of assistant output.
	EventTextDelta EventType = "text_delta"
	// EventThinkingDelta carries a Text chunk of reasoning output.
	EventThinkingDelta EventType = "thinking_delta"
	// EventToolCall carries one complete tool call as soon as it is fully
	// assembled from the stream.
	EventToolCall EventType = "tool_call"
	// EventDone carries the final Message, StopReason and Usage.
	EventDone EventType = "done"
)

// StreamEvent is one event from a streaming completion.
type StreamEvent struct {
	Type EventType
	// Text holds delta content for text/thinking events.
	Text string
	// ToolCall is set for EventToolCall.
	ToolCall *ToolCall
	// Message is the fully assembled assistant message on EventDone.
	Message *Message
	// StopReason is set on EventDone (e.g. "end_turn", "tool_calls", "max_tokens").
	StopReason string
	// Usage is set on EventDone.
	Usage Usage
}

// Stream is a streaming completion. Recv returns events in order and ends
// with io.EOF after the EventDone event. Provider errors are returned from
// Recv. Close aborts the stream.
type Stream interface {
	Recv() (StreamEvent, error)
	Close() error
}

// Provider is a unified chat-completion backend (pi-ai equivalent).
type Provider interface {
	// Name returns the provider identifier ("openai", "anthropic", ...).
	Name() string
	// Chat starts a streaming completion.
	Chat(ctx context.Context, req ChatRequest) (Stream, error)
}

// Hooks lets extensions observe and adjust provider traffic (pi's
// before_provider_headers / before_provider_request /
// after_provider_response). All methods are optional; a nil Hooks means
// no interception.
type Hooks struct {
	// BeforeHeaders mutates the outgoing HTTP headers in place — the
	// place to add auth variants, routing or trace headers.
	BeforeHeaders func(provider string, headers map[string]string)
	// BeforeRequest mutates the outgoing request in place (e.g. to swap
	// the model or trim messages).
	BeforeRequest func(provider string, req *ChatRequest)
	// AfterResponse is called once a response status is known, with the
	// HTTP status code and the response body (headers, error bodies).
	AfterResponse func(provider string, status int, body []byte)
}

// UserAgent identifies this harness to gateways that ask clients to
// self-identify (e.g. OpenCode Go). Override per provider via ExtraHeaders
// when embedding.
const UserAgent = "auto-harness/0.1.6"

// Collect drains a stream and returns the final assembled message and
// usage. Deltas are passed to onDelta when non-nil. It is a convenience
// for providers' tests and simple callers; the agent loop consumes events
// directly.
func Collect(stream Stream, onDelta func(StreamEvent)) (*Message, Usage, error) {
	defer stream.Close()
	var msg *Message
	var usage Usage
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return msg, usage, nil
		}
		if err != nil {
			return msg, usage, err
		}
		switch ev.Type {
		case EventTextDelta, EventThinkingDelta:
			if onDelta != nil {
				onDelta(ev)
			}
		case EventDone:
			msg = ev.Message
			usage = ev.Usage
		}
	}
}
