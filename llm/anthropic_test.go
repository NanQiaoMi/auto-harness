package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicStreamTextAndToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Error("missing x-api-key")
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing Bearer auth, got %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["system"] != "be nice" {
			t.Errorf("system = %v", body["system"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		events := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":0}}}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi!"}}`,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":1}`,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
		}
		for _, e := range events {
			io.WriteString(w, e+"\n")
			if strings.HasPrefix(e, "data:") {
				io.WriteString(w, "\n")
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	p := NewAnthropicProvider("test-key", srv.URL)
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model:    "claude-sonnet-5-5",
		System:   "be nice",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "read", Description: "r", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var events []EventType
	msg, usage, err := Collect(stream, func(ev StreamEvent) {
		events = append(events, ev.Type)
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if msg.Content != "Hi!" {
		t.Errorf("content = %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "read" {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(msg.ToolCalls[0].Arguments, &args); err != nil || args.Path != "a.txt" {
		t.Errorf("args = %s (%v)", msg.ToolCalls[0].Arguments, err)
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 4 {
		t.Errorf("usage = %+v", usage)
	}
	if len(events) != 1 || events[0] != EventTextDelta {
		t.Errorf("delta events = %v", events)
	}
}

func TestGuessProvider(t *testing.T) {
	if got := GuessProvider("claude-sonnet-5-5"); got != "anthropic" {
		t.Errorf("claude -> %q", got)
	}
	if got := GuessProvider("deepseek-chat"); got != "openai" {
		t.Errorf("deepseek -> %q", got)
	}
	if got := GuessProvider("gpt-5.2"); got != "openai" {
		t.Errorf("gpt -> %q", got)
	}
}

func TestAnthropicMultimodalWirePayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	img, err := ImageFromBytes(makePNG(t, 4, 4), "image/png", "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	p := NewAnthropicProvider("k", srv.URL)
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []Message{
			{Role: RoleUser, Content: "inspect", Images: []Image{img}},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t1", Name: "read", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, ToolCallID: "t1", Content: "ok", Images: []Image{img}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()

	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	userBlocks := msgs[0].(map[string]any)["content"].([]any)
	if len(userBlocks) != 2 {
		t.Fatalf("user blocks = %d, want 2", len(userBlocks))
	}
	imgBlock := userBlocks[1].(map[string]any)
	if imgBlock["type"] != "image" {
		t.Errorf("imgBlock type = %v", imgBlock["type"])
	}
	src := imgBlock["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] == "" {
		t.Errorf("source = %#v", src)
	}
	toolBlocks := msgs[2].(map[string]any)["content"].([]any)
	toolRes := toolBlocks[0].(map[string]any)
	toolResContent := toolRes["content"].([]any)
	if len(toolResContent) != 2 || toolResContent[1].(map[string]any)["type"] != "image" {
		t.Errorf("tool_result content = %#v", toolResContent)
	}
}
