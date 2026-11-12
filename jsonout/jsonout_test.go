package jsonout

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/llm"
	"github.com/mimi/auto-harness/session"
	"github.com/mimi/auto-harness/tools"
)

// records reads the records the writer wrote.
func records(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var parsed []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the record %q is not JSON: %v", line, err)
		}
		parsed = append(parsed, record)
	}
	return parsed
}

// newSession creates a session for the header tests.
func newSession(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.New(t.TempDir(), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestWriteHeaderNamesTheSession checks the first record.
//
// The header comes first: a consumer that sees an event must already
// know which session it belongs to.
func TestWriteHeaderNamesTheSession(t *testing.T) {
	sess := newSession(t)

	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteHeader(sess, "/tmp/work"); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	parsed := records(t, &out)
	if len(parsed) != 1 {
		t.Fatalf("the records are %v", parsed)
	}
	header := parsed[0]
	if header["type"] != EventSessionHeader {
		t.Errorf("the type is %v", header["type"])
	}
	if header["id"] != sess.Meta.ID {
		t.Errorf("the id is %v, want %s", header["id"], sess.Meta.ID)
	}
	if header["cwd"] != "/tmp/work" {
		t.Errorf("the cwd is %v", header["cwd"])
	}
	if header["timestamp"] == "" {
		t.Errorf("the header has no timestamp: %v", header)
	}
}

// TestWriteHeaderNeedsASession checks that a missing session is an
// error rather than an empty header.
func TestWriteHeaderNeedsASession(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteHeader(nil, "/tmp"); err == nil {
		t.Error("the header was written without a session")
	}
	if out.Len() != 0 {
		t.Errorf("the output is %q", out.String())
	}
}

// TestWriteIsOneLine checks the framing.
//
// The stream is JSONL: one record per line, so a consumer can read it
// line by line without buffering the whole run.
func TestWriteIsOneLine(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.Write(Record{Type: "message_update", Text: "hello"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if out.String() != "{\"type\":\"message_update\",\"text\":\"hello\"}\n" {
		t.Errorf("the output is %q", out.String())
	}
}

// TestInstallForwardsTheEvents checks the event forwarding.
//
// Every event the harness emits becomes a record, so the stream tells
// the whole story of the run.
func TestInstallForwardsTheEvents(t *testing.T) {
	bus := ext.NewBus()
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.Install(bus)

	mustEmit(t, bus, &ext.Event{Type: ext.EventAgentStart})
	mustEmit(t, bus, &ext.Event{Type: ext.EventTurnStart})
	mustEmit(t, bus, &ext.Event{Type: ext.EventMessageStart})
	mustEmit(t, bus, &ext.Event{
		Type:    ext.EventMessageUpdate,
		Text:    "hi",
		Message: &llm.Message{Role: llm.RoleAssistant},
	})
	mustEmit(t, bus, &ext.Event{
		Type:    ext.EventMessageEnd,
		Message: &llm.Message{Role: llm.RoleAssistant, Content: "hi there"},
	})
	mustEmit(t, bus, &ext.Event{
		Type:  ext.EventAgentEnd,
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 3},
	})

	parsed := records(t, &out)
	if len(parsed) != 6 {
		t.Fatalf("the records are %v", parsed)
	}
	if parsed[0]["type"] != EventAgentStart {
		t.Errorf("the first record is %v", parsed[0])
	}
	if parsed[3]["text"] != "hi" {
		t.Errorf("the delta record is %v", parsed[3])
	}
	end := parsed[4]
	message, _ := end["message"].(map[string]any)
	if message["content"] != "hi there" {
		t.Errorf("the message is %v", message)
	}
	usage, _ := parsed[5]["usage"].(map[string]any)
	if usage["inputTokens"].(float64) != 10 {
		t.Errorf("the usage is %v", usage)
	}
}

// TestThinkingDeltasAreMarked checks that the reasoning is marked.
//
// A consumer that shows the answer must not show the reasoning in the
// same stream, so the flag rides along with the delta.
func TestThinkingDeltasAreMarked(t *testing.T) {
	bus := ext.NewBus()
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.Install(bus)

	mustEmit(t, bus, &ext.Event{Type: ext.EventMessageUpdate, Text: "thinking..."})
	mustEmit(t, bus, &ext.Event{Type: ext.EventMessageUpdate, Text: "answer"})
	mustEmit(t, bus, &ext.Event{
		Type: ext.EventMessageUpdate,
		Text: "more thinking",
		Data: map[string]any{"thinking": true},
	})

	parsed := records(t, &out)
	if len(parsed) != 3 {
		t.Fatalf("the records are %v", parsed)
	}
	if parsed[0]["thinking"] != nil {
		t.Errorf("the answer delta is marked as thinking: %v", parsed[0])
	}
	if parsed[2]["thinking"] != true {
		t.Errorf("the reasoning delta is not marked: %v", parsed[2])
	}
}

// TestToolEventsCarryTheCall checks the tool events.
//
// A consumer that renders the run needs to know which tool ran and
// what it produced.
func TestToolEventsCarryTheCall(t *testing.T) {
	bus := ext.NewBus()
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.Install(bus)

	call := &llm.ToolCall{ID: "call-1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)}
	mustEmit(t, bus, &ext.Event{Type: ext.EventToolExecutionStart, ToolCall: call})
	mustEmit(t, bus, &ext.Event{
		Type:       ext.EventToolExecutionEnd,
		ToolCall:   call,
		ToolResult: &tools.Result{Content: "file.txt", IsError: true},
	})

	parsed := records(t, &out)
	if len(parsed) != 2 {
		t.Fatalf("the records are %v", parsed)
	}
	start, _ := parsed[0]["toolCall"].(map[string]any)
	if start["name"] != "bash" {
		t.Errorf("the call is %v", start)
	}
	if start["arguments"] != `{"command":"ls"}` {
		t.Errorf("the arguments are %v", start["arguments"])
	}
	result, _ := parsed[1]["result"].(map[string]any)
	if result["content"] != "file.txt" {
		t.Errorf("the result is %v", result)
	}
	if result["isError"] != true {
		t.Errorf("the result is %v", result)
	}
}

// TestRequestEventCarriesTheRequest checks the request event.
func TestRequestEventCarriesTheRequest(t *testing.T) {
	bus := ext.NewBus()
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.Install(bus)

	mustEmit(t, bus, &ext.Event{
		Type: ext.EventRequestStart,
		Request: ext.Request{
			Provider: "openai",
			Model:    "some-model",
			Tools:    5,
		},
	})

	parsed := records(t, &out)
	request, _ := parsed[0]["request"].(map[string]any)
	if request["model"] != "some-model" {
		t.Errorf("the request is %v", request)
	}
	if request["tools"].(float64) != 5 {
		t.Errorf("the request is %v", request)
	}
}

// TestWriteSettledClosesTheStream checks the last record.
//
// The settled record is what a consumer reads until: it says the run
// has no more automatic work.
func TestWriteSettledClosesTheStream(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteSettled(false); err != nil {
		t.Fatalf("WriteSettled: %v", err)
	}
	parsed := records(t, &out)
	if parsed[0]["type"] != EventAgentSettled {
		t.Errorf("the record is %v", parsed[0])
	}
	if parsed[0]["aborted"] != nil {
		t.Errorf("a finished run is marked as aborted: %v", parsed[0])
	}

	out.Reset()
	writer = NewWriter(&out)
	if err := writer.WriteSettled(true); err != nil {
		t.Fatalf("WriteSettled: %v", err)
	}
	parsed = records(t, &out)
	if parsed[0]["aborted"] != true {
		t.Errorf("an aborted run is not marked: %v", parsed[0])
	}
}

// TestFinalTextPicksTheLastAnswer checks how the answer is found.
//
// A run may hold several assistant messages: the earlier ones are the
// model's "let me check" remarks, and the last one is the answer.
func TestFinalTextPicksTheLastAnswer(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "the question"},
		{Role: llm.RoleAssistant, Content: "let me check"},
		{Role: llm.RoleTool, Content: "a tool result"},
		{Role: llm.RoleAssistant, Content: "the answer"},
	}
	if got := FinalText(messages); got != "the answer" {
		t.Errorf("the text is %q", got)
	}
	if got := FinalText(nil); got != "" {
		t.Errorf("the text is %q", got)
	}
}

// TestMessagesCarryTheirToolCalls checks the message shape.
func TestMessagesCarryTheirToolCalls(t *testing.T) {
	bus := ext.NewBus()
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.Install(bus)

	mustEmit(t, bus, &ext.Event{
		Type: ext.EventMessageEnd,
		Message: &llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read", Arguments: []byte(`{"path":"a.txt"}`)},
			},
		},
	})

	parsed := records(t, &out)
	message, _ := parsed[0]["message"].(map[string]any)
	calls, _ := message["toolCalls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("the tool calls are %v", calls)
	}
	call, _ := calls[0].(map[string]any)
	if call["name"] != "read" {
		t.Errorf("the call is %v", call)
	}
}

// mustEmit emits an event and fails the test if it cannot.
func mustEmit(t *testing.T, bus *ext.Bus, e *ext.Event) {
	t.Helper()
	if err := bus.Emit(e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

// TestTheTimestampIsUTC checks the header's timestamp format.
//
// The header is read by machines, so the timestamp is RFC 3339 in UTC
// rather than the local zone.
func TestTheTimestampIsUTC(t *testing.T) {
	sess := newSession(t)
	sess.Meta.CreatedAt = time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC)

	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteHeader(sess, "/tmp"); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	parsed := records(t, &out)
	if parsed[0]["timestamp"] != "2026-11-12T08:30:00.000Z" {
		t.Errorf("the timestamp is %v", parsed[0]["timestamp"])
	}
}

// TestConcurrentWritesDoNotInterleave checks the framing under
// concurrency.
//
// The agent writes the events from its own goroutine while the run
// proceeds, and a consumer reads the stream line by line. A record
// split across two writes would break that.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := writer.Write(Record{Type: "message_update", Text: strings.Repeat("x", 100)}); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 160 {
		t.Fatalf("the stream has %d lines, want 160", len(lines))
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the record %q is not JSON: %v", line, err)
		}
	}
}
