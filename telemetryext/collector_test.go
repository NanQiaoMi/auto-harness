package telemetryext

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/llm"
	"github.com/mimi/auto-harness/telemetry"
	"github.com/mimi/auto-harness/tools"
)

// The distinctive strings the test feeds through the event bus. Nothing
// that contains them may end up in a telemetry attribute.
const (
	secretPrompt = "the eagle lands at dawn and the password is hunter2"
	secretArgs   = "secret-argument-value-9f3c"
	secretOutput = "secret-tool-output-value-7a1b"
)

// session drives a collector through one session and returns the
// recorder and the collector.
func session(t *testing.T) (*telemetry.MemoryContext, *Collector) {
	t.Helper()
	recorder := telemetry.NewMemoryContext()
	collector := New(recorder, "opencode-go", "deepseek-v4.1-flash")
	collector.Logf = func(format string, args ...any) {}

	api := ext.NewAPI(ext.NewBus(), tools.NewRegistry(), ext.NewCommandRegistry())
	if err := collector.Setup(api); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return recorder, collector
}

// spanByName returns the recorded span with the name.
func spanByName(t *testing.T, recorder *telemetry.MemoryContext, name string) telemetry.RecordedSpan {
	t.Helper()
	for _, s := range recorder.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span named %q; got %v", name, spanNames(recorder))
	return telemetry.RecordedSpan{}
}

// spanNames lists the recorded span names.
func spanNames(recorder *telemetry.MemoryContext) []string {
	var out []string
	for _, s := range recorder.GetSpans() {
		out = append(out, s.Name)
	}
	return out
}

// TestCollectorRecordsTheSession verifies the root span opens at the
// session start and closes at the shutdown.
func TestCollectorRecordsTheSession(t *testing.T) {
	recorder, collector := session(t)

	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	if err := collector.Setup(api); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown})

	spans := recorder.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %v, want one", spanNames(recorder))
	}
	s := spans[0]
	if s.Name != "pi.harness.session" {
		t.Errorf("name = %q", s.Name)
	}
	if !s.Settled {
		t.Error("the session span is not settled")
	}
	if s.Attributes["pi.harness.provider"] != "opencode-go" {
		t.Errorf("provider = %v", s.Attributes["pi.harness.provider"])
	}
	if s.Attributes["pi.harness.model"] != "deepseek-v4.1-flash" {
		t.Errorf("model = %v", s.Attributes["pi.harness.model"])
	}
	if s.Attributes["pi.harness.turns"] != 0 {
		t.Errorf("turns = %v, want 0", s.Attributes["pi.harness.turns"])
	}
}

// TestCollectorRecordsTheRequest verifies the request span and its
// measurements.
func TestCollectorRecordsTheRequest(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventRequestStart, Request: ext.Request{
		Provider: "opencode-go",
		Model:    "deepseek-v4.1-flash",
		Tools:    7,
	}})
	_ = bus.Emit(&ext.Event{Type: ext.EventTurnEnd, Request: ext.Request{
		Provider:   "opencode-go",
		Model:      "deepseek-v4.1-flash",
		StopReason: "tool_calls",
		Usage:      llm.Usage{InputTokens: 120, OutputTokens: 45},
		Duration:   1500 * time.Millisecond,
	}})

	s := spanByName(t, recorder, "pi.harness.request")
	if !s.Settled {
		t.Error("the request span is not settled")
	}
	if s.Attributes["pi.harness.tools"] != 7 {
		t.Errorf("tools = %v, want 7", s.Attributes["pi.harness.tools"])
	}
	if s.Attributes["pi.harness.stop"] != "tool_calls" {
		t.Errorf("stop = %v", s.Attributes["pi.harness.stop"])
	}
	if s.Attributes["pi.harness.usage.input"] != 120 {
		t.Errorf("input tokens = %v, want 120", s.Attributes["pi.harness.usage.input"])
	}
	if s.Attributes["pi.harness.usage.output"] != 45 {
		t.Errorf("output tokens = %v, want 45", s.Attributes["pi.harness.usage.output"])
	}
	if len(s.Events) != 1 || s.Events[0].Name != "pi.harness.first_chunk" {
		t.Errorf("events = %v", s.Events)
	}

	// The measurements.
	meas := recorder.GetMeasurements()
	var sawDuration, sawTokens bool
	for _, m := range meas {
		switch m.Name {
		case "pi.harness.request.duration":
			sawDuration = true
			if m.Value != 1500 {
				t.Errorf("duration = %v, want 1500", m.Value)
			}
		case "pi.harness.tokens":
			sawTokens = true
			if m.Attributes["pi.harness.kind"] != "input" && m.Attributes["pi.harness.kind"] != "output" {
				t.Errorf("the token kind = %v", m.Attributes["pi.harness.kind"])
			}
		}
	}
	if !sawDuration {
		t.Error("the request duration was not recorded")
	}
	if !sawTokens {
		t.Error("the token usage was not recorded")
	}
}

// TestCollectorRecordsTheToolCall verifies the tool span and its
// counter.
func TestCollectorRecordsTheToolCall(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	call := &llm.ToolCall{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"` + secretArgs + `"}`)}
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolCall, ToolCall: call})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call,
		ToolResult: &tools.Result{Content: secretOutput}})

	s := spanByName(t, recorder, "pi.harness.tool")
	if !s.Settled {
		t.Error("the tool span is not settled")
	}
	if s.Attributes["pi.harness.tool"] != "read" {
		t.Errorf("tool = %v", s.Attributes["pi.harness.tool"])
	}
	if s.Attributes["pi.harness.tool_error"] != false {
		t.Errorf("tool_error = %v, want false", s.Attributes["pi.harness.tool_error"])
	}

	var sawCall bool
	for _, m := range recorder.GetMeasurements() {
		if m.Name == "pi.harness.tool.calls" {
			sawCall = true
			if m.Value != 1 {
				t.Errorf("the tool call count = %v, want 1", m.Value)
			}
		}
	}
	if !sawCall {
		t.Error("the tool call was not counted")
	}
}

// TestCollectorRecordsAFailedTool verifies a failing tool settles its
// span with an error status.
func TestCollectorRecordsAFailedTool(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	call := &llm.ToolCall{ID: "c1", Name: "bash"}
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolCall, ToolCall: call})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call,
		ToolResult: &tools.Result{Content: "boom", IsError: true}})

	s := spanByName(t, recorder, "pi.harness.tool")
	if s.Status.Code != telemetry.StatusError {
		t.Errorf("status = %v, want an error", s.Status)
	}
	if s.Attributes["pi.harness.tool_error"] != true {
		t.Errorf("tool_error = %v, want true", s.Attributes["pi.harness.tool_error"])
	}
}

// TestCollectorRecordsTheSessionEvents verifies the events the schema
// declares on the session span: one per turn, one per tool call.
func TestCollectorRecordsTheSessionEvents(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventRequestStart, Request: ext.Request{
		Provider: "opencode-go", Model: "deepseek-v4.1-flash", Tools: 2,
	}})
	_ = bus.Emit(&ext.Event{Type: ext.EventTurnEnd, Request: ext.Request{
		Provider: "opencode-go", Model: "deepseek-v4.1-flash", StopReason: "tool_calls",
	}})

	call := &llm.ToolCall{ID: "c1", Name: "bash"}
	_ = bus.Emit(&ext.Event{Type: ext.EventToolCall, ToolCall: call})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call})
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown})

	session := spanByName(t, recorder, "pi.harness.session")
	var turns, calls int
	for _, e := range session.Events {
		switch e.Name {
		case "pi.harness.turn":
			turns++
			if e.Attributes["pi.harness.turn_index"] != 1 {
				t.Errorf("the turn index = %v, want 1", e.Attributes["pi.harness.turn_index"])
			}
		case "pi.harness.tool_call":
			calls++
			if e.Attributes["pi.harness.tool"] != "bash" {
				t.Errorf("the tool = %v, want bash", e.Attributes["pi.harness.tool"])
			}
			if e.Attributes["pi.harness.tool_blocked"] != false {
				t.Errorf("tool_blocked = %v, want false", e.Attributes["pi.harness.tool_blocked"])
			}
		default:
			t.Errorf("unexpected event %q", e.Name)
		}
	}
	if turns != 1 {
		t.Errorf("turns = %d, want 1", turns)
	}
	if calls != 1 {
		t.Errorf("tool calls = %d, want 1", calls)
	}
}

// TestCollectorRecordsABlockedTool verifies a call the permission layer
// blocked is recorded as blocked.
func TestCollectorRecordsABlockedTool(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	call := &llm.ToolCall{ID: "c1", Name: "bash"}
	// The permission guard sets Block on the tool_call event.
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolCall, ToolCall: call, Block: true,
		BlockReason: "blocked by permission rules"})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call,
		ToolResult: &tools.Result{Content: "tool call blocked: blocked by permission rules", IsError: true}})

	s := spanByName(t, recorder, "pi.harness.tool")
	if s.Attributes["pi.harness.tool_blocked"] != true {
		t.Errorf("tool_blocked = %v, want true", s.Attributes["pi.harness.tool_blocked"])
	}
	if s.Attributes["pi.harness.tool_error"] != true {
		t.Errorf("tool_error = %v, want true", s.Attributes["pi.harness.tool_error"])
	}

	var sawBlocked bool
	for _, m := range recorder.GetMeasurements() {
		if m.Name == "pi.harness.tool.calls" && m.Attributes["pi.harness.tool_blocked"] == true {
			sawBlocked = true
		}
	}
	if !sawBlocked {
		t.Error("the blocked call was not counted as blocked")
	}
}

// TestCollectorRecordsTheCompaction verifies the compaction span.
func TestCollectorRecordsTheCompaction(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionBeforeCompact, Messages: make([]llm.Message, 12)})
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionCompact, Messages: make([]llm.Message, 2)})

	s := spanByName(t, recorder, "pi.harness.compaction")
	if s.Attributes["pi.harness.messages_before"] != 12 {
		t.Errorf("before = %v, want 12", s.Attributes["pi.harness.messages_before"])
	}
	if s.Attributes["pi.harness.messages_after"] != 2 {
		t.Errorf("after = %v, want 2", s.Attributes["pi.harness.messages_after"])
	}
}

// TestCollectorRecordsNoContent is the privacy test: a session whose
// prompt, tool arguments and tool output are distinctive must leave
// none of them in any recorded attribute.
func TestCollectorRecordsNoContent(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	// The prompt arrives as a message; the collector must not record it.
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventBeforeAgentStart, Message: &llm.Message{
		Role:    llm.RoleUser,
		Content: secretPrompt,
	}})
	_ = bus.Emit(&ext.Event{Type: ext.EventRequestStart, Request: ext.Request{
		Provider: "opencode-go",
		Model:    "deepseek-v4.1-flash",
		Tools:    3,
	}})
	// The model's text arrives as deltas.
	_ = bus.Emit(&ext.Event{Type: ext.EventMessageUpdate, Text: secretPrompt})
	_ = bus.Emit(&ext.Event{Type: ext.EventTurnEnd, Request: ext.Request{
		Provider:   "opencode-go",
		Model:      "deepseek-v4.1-flash",
		StopReason: "tool_calls",
		Usage:      llm.Usage{InputTokens: 10},
	}})

	// The tool call carries arguments, and its result carries output.
	call := &llm.ToolCall{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"` + secretArgs + `"}`)}
	_ = bus.Emit(&ext.Event{Type: ext.EventToolCall, ToolCall: call})
	_ = bus.Emit(&ext.Event{Type: ext.EventToolExecutionEnd, ToolCall: call,
		ToolResult: &tools.Result{Content: secretOutput}})
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown})

	// Nothing that reaches the telemetry may contain the secrets.
	for _, s := range recorder.GetSpans() {
		for name, value := range s.Attributes {
			assertNoSecret(t, s.Name+"."+name, value)
		}
		for _, e := range s.Events {
			for name, value := range e.Attributes {
				assertNoSecret(t, s.Name+"."+e.Name+"."+name, value)
			}
		}
	}
	for _, m := range recorder.GetMeasurements() {
		for name, value := range m.Attributes {
			assertNoSecret(t, m.Name+"."+name, value)
		}
	}
}

// assertNoSecret fails the test when the value carries a secret.
func assertNoSecret(t *testing.T, where string, value any) {
	t.Helper()
	text := strings.ToLower(toText(value))
	for _, secret := range []string{secretPrompt, secretArgs, secretOutput, "hunter2"} {
		if strings.Contains(text, strings.ToLower(secret)) {
			t.Errorf("%s leaks content: %q", where, toText(value))
		}
	}
}

// toText renders an attribute value as text.
func toText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []string:
		return strings.Join(v, " ")
	default:
		return fmt.Sprintf("%v", value)
	}
}

// TestCollectorTeardownSettlesTheOpenSpans verifies the teardown path.
func TestCollectorTeardownSettlesTheOpenSpans(t *testing.T) {
	recorder, collector := session(t)
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventRequestStart, Request: ext.Request{
		Provider: "opencode-go", Model: "deepseek-v4.1-flash",
	}})

	// The session was never shut down; the teardown settles it.
	if err := collector.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, s := range recorder.GetSpans() {
		if !s.Settled {
			t.Errorf("the span %q is not settled", s.Name)
		}
	}
}

// TestCollectorWithoutAContextIsInert verifies the collector does
// nothing when no telemetry context is wired in.
func TestCollectorWithoutAContextIsInert(t *testing.T) {
	collector := &Collector{Provider: "opencode-go", Model: "deepseek-v4.1-flash"}
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())
	_ = collector.Setup(api)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart})
	_ = bus.Emit(&ext.Event{Type: ext.EventRequestStart, Request: ext.Request{
		Provider: "opencode-go", Model: "deepseek-v4.1-flash",
	}})
	_ = bus.Emit(&ext.Event{Type: ext.EventTurnEnd, Request: ext.Request{}})
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown})

	if err := collector.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}
