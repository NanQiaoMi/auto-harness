package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// fakeStream replays a scripted assistant message as a fixed event
// sequence, then io.EOF.
type fakeStream struct {
	events []llm.StreamEvent
	pos    int
}

func newFakeStream(msg llm.Message) *fakeStream {
	var events []llm.StreamEvent
	if msg.Content != "" {
		events = append(events, llm.StreamEvent{Type: llm.EventTextDelta, Text: msg.Content})
	}
	for i := range msg.ToolCalls {
		tc := msg.ToolCalls[i]
		events = append(events, llm.StreamEvent{Type: llm.EventToolCall, ToolCall: &tc})
	}
	m := msg
	events = append(events, llm.StreamEvent{
		Type:       llm.EventDone,
		Message:    &m,
		StopReason: "end_turn",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	})
	return &fakeStream{events: events}
}

func (s *fakeStream) Recv() (llm.StreamEvent, error) {
	if s.pos >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *fakeStream) Close() error { return nil }

// fakeProvider returns scripted responses in order and records requests.
type fakeProvider struct {
	responses []llm.Message
	requests  []llm.ChatRequest
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return nil, errors.New("fake: no scripted responses left")
	}
	msg := p.responses[0]
	p.responses = p.responses[1:]
	return newFakeStream(msg), nil
}

func newTestAgent(t *testing.T, provider llm.Provider) (*Agent, *tools.Registry, *ext.Bus) {
	t.Helper()
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 5}, bus, reg)
	return ag, reg, bus
}

func TestRunToolCallLoop(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{Content: "let me check", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)},
		}},
		{Content: "done"},
	}}
	ag, reg, bus := newTestAgent(t, provider)

	var events []string
	for _, et := range []ext.EventType{
		ext.EventBeforeAgentStart, ext.EventAgentStart, ext.EventTurnStart,
		ext.EventMessageStart, ext.EventMessageUpdate, ext.EventMessageEnd,
		ext.EventToolExecutionStart, ext.EventToolResult, ext.EventToolExecutionEnd,
		ext.EventTurnEnd, ext.EventAgentEnd,
	} {
		et := et
		bus.On(et, func(e *ext.Event) error {
			events = append(events, string(et))
			return nil
		})
	}

	var gotArgs string
	err := reg.Register(&tools.Tool{
		Name:       "echo",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return tools.Result{}, err
			}
			gotArgs = p.Text
			return tools.Result{Content: "echo:" + p.Text}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "done" {
		t.Errorf("Final = %q, want %q", res.Final, "done")
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	if gotArgs != "hi" {
		t.Errorf("tool args = %q, want %q", gotArgs, "hi")
	}
	if res.Usage.InputTokens != 20 || res.Usage.OutputTokens != 10 {
		t.Errorf("Usage = %+v, want 20 in / 10 out", res.Usage)
	}

	// History: user, assistant+call, tool result, assistant final.
	hist := ag.History()
	if len(hist) != 4 {
		t.Fatalf("history len = %d, want 4", len(hist))
	}
	if hist[2].Role != llm.RoleTool || hist[2].ToolCallID != "c1" || hist[2].Content != "echo:hi" {
		t.Errorf("tool message = %+v", hist[2])
	}

	want := strings.Join([]string{
		"before_agent_start", "agent_start",
		"turn_start", "message_start", "message_update", "message_end",
		"tool_execution_start", "tool_result", "tool_execution_end",
		"turn_end",
		"turn_start", "message_start", "message_update", "message_end",
		"turn_end",
		"agent_end",
	}, ",")
	// message_update fires once per delta; fake stream emits one per message.
	got := strings.Join(events, ",")
	if got != want {
		t.Errorf("event order:\n got %s\nwant %s", got, want)
	}
}

func TestBeforeAgentStartCanRewriteAndCancel(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{{Content: "ok"}}}
	ag, _, bus := newTestAgent(t, provider)

	bus.On(ext.EventBeforeAgentStart, func(e *ext.Event) error {
		e.Message.Content = "rewritten: " + e.Message.Content
		e.SystemPrompt = "custom prompt"
		return nil
	})
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if provider.requests[0].System != "custom prompt" {
		t.Errorf("system prompt = %q", provider.requests[0].System)
	}
	if provider.requests[0].Messages[0].Content != "rewritten: hi" {
		t.Errorf("user message = %q", provider.requests[0].Messages[0].Content)
	}

	// Cancel path.
	bus2 := ext.NewBus()
	ag2 := New(Config{Provider: provider, Model: "m", MaxTurns: 1}, bus2, tools.NewRegistry())
	bus2.On(ext.EventBeforeAgentStart, func(e *ext.Event) error {
		e.Cancel = true
		return nil
	})
	res, err := ag2.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled {
		t.Error("expected run to be cancelled")
	}
}

func TestInputCanRewriteAndHandle(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{{Content: "ok"}}}
	ag, _, bus := newTestAgent(t, provider)

	// The input event fires before the prompt becomes a message, so a
	// handler that rewrites the text is what the model sees.
	var order []ext.EventType
	bus.On(ext.EventInput, func(e *ext.Event) error {
		order = append(order, e.Type)
		e.Text = "rewritten: " + e.Text
		return nil
	})
	bus.On(ext.EventBeforeAgentStart, func(e *ext.Event) error {
		order = append(order, e.Type)
		return nil
	})
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].Messages[0].Content; got != "rewritten: hi" {
		t.Errorf("user message = %q", got)
	}
	if len(order) != 2 || order[0] != ext.EventInput || order[1] != ext.EventBeforeAgentStart {
		t.Errorf("event order = %v, want input then before_agent_start", order)
	}

	// A handler that deals with the prompt itself stops the run before
	// the model is called.
	provider2 := &fakeProvider{responses: []llm.Message{{Content: "should not run"}}}
	ag2, _, bus2 := newTestAgent(t, provider2)
	bus2.On(ext.EventInput, func(e *ext.Event) error {
		e.Cancel = true
		e.BlockReason = "handled by a test"
		return nil
	})
	res, err := ag2.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled {
		t.Error("expected the run to be cancelled")
	}
	if len(provider2.requests) != 0 {
		t.Errorf("the model was called %d times, want 0", len(provider2.requests))
	}
}

func TestTurnEndCanInjectMessages(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{{Content: "first"}, {Content: "second"}}}
	ag, _, bus := newTestAgent(t, provider)

	injected := false
	bus.On(ext.EventTurnEnd, func(e *ext.Event) error {
		if !injected {
			injected = true
			e.Messages = []llm.Message{{Role: llm.RoleUser, Content: "reminder: keep going"}}
			e.Continue = true
		}
		return nil
	})
	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (turn_end forced a second turn)", res.Turns)
	}
	// Second request must carry the injected message.
	found := false
	for _, m := range provider.requests[1].Messages {
		if m.Content == "reminder: keep going" {
			found = true
		}
	}
	if !found {
		t.Error("injected message missing from second request")
	}
}

func TestUnknownToolReturnsErrorResult(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "nope", Arguments: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	ag, _, _ := newTestAgent(t, provider)
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	hist := ag.History()
	if !hist[2].IsError || !strings.Contains(hist[2].Content, "unknown tool") {
		t.Errorf("want unknown-tool error result, got %+v", hist[2])
	}
}

func TestCompactWithCompactor(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{Content: "first"}, {Content: "second"},
		{Content: "SUMMARY"},
	}}
	ag, _, _ := newTestAgent(t, provider)
	if _, err := ag.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if n := len(ag.History()); n != 4 {
		t.Fatalf("history = %d, want 4", n)
	}

	ag.Compactor = NewSummarizer(provider, "test-model")
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	hist := ag.History()
	if len(hist) != 1 || !strings.Contains(hist[0].Content, "SUMMARY") {
		t.Errorf("compacted history = %+v", hist)
	}

	// Hook replacement takes precedence over the Compactor.
	ag2, _, bus2 := newTestAgent(t, provider)
	bus2.On(ext.EventSessionBeforeCompact, func(e *ext.Event) error {
		e.Messages = []llm.Message{{Role: llm.RoleUser, Content: "hook summary"}}
		return nil
	})
	ag2.SetHistory([]llm.Message{{Role: llm.RoleUser, Content: "a"}, {Role: llm.RoleUser, Content: "b"}})
	if err := ag2.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := ag2.History(); len(h) != 1 || h[0].Content != "hook summary" {
		t.Errorf("hook compaction = %+v", h)
	}

	// Cancel is a no-op.
	bus3 := ext.NewBus()
	ag3 := New(Config{Provider: provider, Model: "m", MaxTurns: 1}, bus3, tools.NewRegistry())
	bus3.On(ext.EventSessionBeforeCompact, func(e *ext.Event) error {
		e.Cancel = true
		return nil
	})
	ag3.SetHistory([]llm.Message{{Role: llm.RoleUser, Content: "keep"}})
	if err := ag3.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := ag3.History(); len(h) != 1 {
		t.Errorf("cancel should keep history, got %+v", h)
	}
}

func TestCompactWithoutProvider(t *testing.T) {
	ag, _, _ := newTestAgent(t, &fakeProvider{})
	if err := ag.Compact(context.Background()); err == nil {
		t.Error("want error when no compaction provider is configured")
	}
}

func TestResolverSwitchesProviderWithModel(t *testing.T) {
	first := &fakeProvider{responses: []llm.Message{{Content: "from first"}}}
	second := &fakeProvider{responses: []llm.Message{{Content: "from second"}}}

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	ag := New(Config{Model: "model-a", MaxTurns: 3}, bus, reg)
	ag.SetResolver(func(modelID string) (llm.Provider, error) {
		if modelID == "model-a" {
			return first, nil
		}
		return second, nil
	})
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(first.requests) != 1 || len(second.requests) != 0 {
		t.Fatalf("first turn should use provider A: a=%d b=%d", len(first.requests), len(second.requests))
	}

	// Switching the model must route to the other provider.
	ag.SetModel("model-b")
	if got := ag.Provider(); got != second {
		t.Fatalf("Provider() = %p after model switch, want second provider %p", got, second)
	}
	if _, err := ag.Run(context.Background(), "hi again"); err != nil {
		t.Fatal(err)
	}
	if len(second.requests) != 1 {
		t.Fatalf("second turn should use provider B: b=%d", len(second.requests))
	}
	if second.requests[0].Model != "model-b" {
		t.Errorf("provider B saw model %q", second.requests[0].Model)
	}
}

func TestResolverErrorIsReported(t *testing.T) {
	bus := ext.NewBus()
	ag := New(Config{Model: "m", MaxTurns: 1}, bus, tools.NewRegistry())
	ag.SetResolver(func(modelID string) (llm.Provider, error) {
		return nil, errors.New("no provider for model")
	})
	ag.SetModel("unroutable")
	if ag.Provider() != nil {
		t.Fatal("a failed model switch kept a stale provider")
	}
	if _, err := ag.Run(context.Background(), "hi"); err == nil {
		t.Fatal("want error from resolver")
	}
}

// ensure fakes satisfy the interfaces.
var (
	_ llm.Stream   = (*fakeStream)(nil)
	_ llm.Provider = (*fakeProvider)(nil)
)
