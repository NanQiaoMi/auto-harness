package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/contextmgr"
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

type recordingCheckpointer struct {
	names []string
	fail  bool
}

func (c *recordingCheckpointer) BeforeTool(ctx context.Context, toolName string, args json.RawMessage) error {
	c.names = append(c.names, toolName)
	if c.fail {
		return errors.New("snapshot failed")
	}
	return nil
}

func TestCheckpointerRunsOnlyBeforeMutatingTools(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{ToolCalls: []llm.ToolCall{
			{ID: "r1", Name: "read-only", Arguments: json.RawMessage(`{}`)},
			{ID: "w1", Name: "writer", Arguments: json.RawMessage(`{"value":"x"}`)},
		}},
		{Content: "done"},
	}}
	ag, reg, _ := newTestAgent(t, provider)
	checkpoints := &recordingCheckpointer{}
	ag.Checkpointer = checkpoints

	var writerCalls int
	if err := reg.Register(&tools.Tool{
		Name:       "read-only",
		ReadOnly:   true,
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "read"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.Tool{
		Name:       "writer",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			writerCalls++
			return tools.Result{Content: "wrote"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(checkpoints.names) != 1 || checkpoints.names[0] != "writer" {
		t.Fatalf("checkpoint calls = %v", checkpoints.names)
	}
	if writerCalls != 1 {
		t.Fatalf("writer calls = %d", writerCalls)
	}
}

func TestEventsCarryDelegatedAgentIdentity(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{{Content: "done"}}}
	bus := ext.NewBus()
	registry := tools.NewRegistry()
	ag := New(Config{
		Provider:      provider,
		Model:         "test-model",
		AgentID:       "sub-123",
		ParentAgentID: "sub-100",
	}, bus, registry)
	var gotID, gotParent string
	bus.On(ext.EventAgentStart, func(e *ext.Event) error {
		gotID = ext.AgentID(e)
		gotParent = ext.ParentAgentID(e)
		return nil
	})
	if _, err := ag.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if gotID != "sub-123" || gotParent != "sub-100" {
		t.Fatalf("event identity = %q / %q", gotID, gotParent)
	}
}

func TestCheckpointerFailureBlocksMutatingTool(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{ToolCalls: []llm.ToolCall{
			{ID: "w1", Name: "writer", Arguments: json.RawMessage(`{}`)},
		}},
		{Content: "done"},
	}}
	ag, reg, _ := newTestAgent(t, provider)
	ag.Checkpointer = &recordingCheckpointer{fail: true}
	called := false
	if err := reg.Register(&tools.Tool{
		Name:       "writer",
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			called = true
			return tools.Result{Content: "wrote"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("mutating tool ran after checkpoint failure")
	}
	history := ag.History()
	if len(history) < 3 || history[2].Role != llm.RoleTool ||
		!history[2].IsError || !strings.Contains(history[2].Content, "checkpoint failed") {
		t.Fatalf("tool result = %#v", history)
	}
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

// TestContextPolicyWrapsTheRequest checks that the extension hook is
// applied to every request, and that dropping it restores the kernel's
// behavior.
func TestContextPolicyWrapsTheRequest(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{{Content: "ok"}}}
	ag, _, _ := newTestAgent(t, provider)

	var sawSystem string
	ag.ContextPolicy = func(next contextmgr.Builder) contextmgr.Builder {
		return policyFunc(func(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
			sawSystem = in.SystemPrompt
			in.Messages = append(in.Messages, llm.Message{Role: llm.RoleUser, Content: "policy-note"})
			if next == nil {
				return contextmgr.Result{Messages: in.Messages}, nil
			}
			return next.Build(ctx, in)
		})
	}

	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawSystem == "" {
		t.Error("the policy did not see the system prompt")
	}
	// The injected message reached the provider.
	req := provider.requests[0]
	found := false
	for _, m := range req.Messages {
		if m.Content == "policy-note" {
			found = true
		}
	}
	if !found {
		t.Errorf("the policy's message did not reach the request: %#v", req.Messages)
	}
}

// policyFunc adapts a function to contextmgr.Builder.
type policyFunc func(context.Context, contextmgr.Input) (contextmgr.Result, error)

func (f policyFunc) Build(ctx context.Context, in contextmgr.Input) (contextmgr.Result, error) {
	return f(ctx, in)
}

// parallelProvider asks for a batch of tool calls in one message, then
// answers.
type parallelProvider struct {
	calls []llm.ToolCall
	mu    sync.Mutex
	reqs  []llm.ChatRequest
}

func (p *parallelProvider) Name() string { return "parallel" }

func (p *parallelProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	n := len(p.reqs)
	p.mu.Unlock()
	msg := llm.Message{Role: llm.RoleAssistant, Content: "done"}
	if n == 1 {
		msg = llm.Message{Role: llm.RoleAssistant, ToolCalls: p.calls}
	}
	return newFakeStream(msg), nil
}

// TestReadOnlyCallsRunInParallel checks that independent reads overlap.
//
// Each read blocks until every other read has started. If the calls
// were serialized the first one would wait forever, so a passing test
// proves the overlap.
func TestReadOnlyCallsRunInParallel(t *testing.T) {
	const n = 3
	var started sync.WaitGroup
	started.Add(n)
	allStarted := make(chan struct{})
	go func() {
		started.Wait()
		close(allStarted)
	}()

	calls := make([]llm.ToolCall, n)
	for i := range calls {
		calls[i] = llm.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "peek"}
	}
	provider := &parallelProvider{calls: calls}
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.Tool{
		Name:       "peek",
		ReadOnly:   true,
		Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			started.Done()
			select {
			case <-allStarted:
			case <-time.After(5 * time.Second):
				return tools.Result{}, fmt.Errorf("tool %s ran alone", callID)
			}
			return tools.Result{Content: "peeked:" + callID}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 4}, bus, reg)

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The results are in call order even though they finished together.
	hist := ag.History()
	var results []string
	for _, m := range hist {
		if m.Role == llm.RoleTool {
			results = append(results, m.Content)
		}
	}
	if len(results) != n {
		t.Fatalf("tool results = %v", results)
	}
	for i, want := range []string{"peeked:c0", "peeked:c1", "peeked:c2"} {
		if results[i] != want {
			t.Errorf("result %d = %q, want %q", i, results[i], want)
		}
	}
}

// TestMutatingCallsStayOrdered checks that a write and the read after it
// are not reordered.
func TestMutatingCallsStayOrdered(t *testing.T) {
	var mu sync.Mutex
	var order []string

	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	calls := []llm.ToolCall{
		{ID: "w", Name: "write-thing"},
		{ID: "r", Name: "read-thing"},
		{ID: "w2", Name: "write-thing"},
	}
	provider := &parallelProvider{calls: calls}
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	_ = reg.Register(&tools.Tool{
		Name: "write-thing", Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			time.Sleep(10 * time.Millisecond)
			record(callID)
			return tools.Result{Content: callID}, nil
		},
	})
	_ = reg.Register(&tools.Tool{
		Name: "read-thing", ReadOnly: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			record(callID)
			return tools.Result{Content: callID}, nil
		},
	})
	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 4}, bus, reg)

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A single read-only call is not parallelized, and the mutations on
	// either side of it keep their order.
	want := []string{"w", "r", "w2"}
	if len(order) != len(want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("execution order = %v, want %v", order, want)
		}
	}
}
