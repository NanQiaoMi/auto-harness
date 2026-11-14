package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestIntegrationToolLoopOverSSE runs the real stack end-to-end against a
// scripted SSE gateway: real OpenAIProvider, real agent loop, real bash
// tool. Turn 1 returns a tool call, turn 2 returns the final answer that
// quotes the tool output.
func TestIntegrationToolLoopOverSSE(t *testing.T) {
	var mu sync.Mutex
	var requests []openAIWireRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req openAIWireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		requests = append(requests, req)
		turn := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		send := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flush.Flush()
		}

		if turn == 1 {
			send(`{"choices":[{"delta":{"content":"running the command"}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":""}}]}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"echo e2e-ok\"}"}}]}}]}`)
			send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			send(`{"choices":[{"delta":{"content":"tool said: e2e-ok"}}]}`)
			send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		}
		send(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
		send(`[DONE]`)
	}))
	defer srv.Close()

	provider := llm.NewOpenAIProvider("test-key", srv.URL+"/v1")
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewBashTool()); err != nil {
		t.Fatal(err)
	}
	ag := New(Config{Provider: provider, Model: "mock-model", MaxTurns: 5}, bus, reg)

	var toolEvents []string
	bus.On(ext.EventToolExecutionStart, func(e *ext.Event) error {
		toolEvents = append(toolEvents, "start:"+e.ToolCall.Name)
		return nil
	})
	bus.On(ext.EventToolResult, func(e *ext.Event) error {
		toolEvents = append(toolEvents, "result")
		return nil
	})

	res, err := ag.Run(context.Background(), "run echo e2e-ok and tell me the output")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "tool said: e2e-ok" {
		t.Errorf("Final = %q", res.Final)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	if res.Usage.InputTokens != 22 || res.Usage.OutputTokens != 14 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	if strings.Join(toolEvents, ",") != "start:bash,result" {
		t.Errorf("tool events = %v", toolEvents)
	}

	// The second request must carry the assistant tool call and the tool
	// result in OpenAI wire format.
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	msgs := requests[1].Messages
	var sawAssistantCall, sawToolResult bool
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call_1" {
			sawAssistantCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "call_1" && strings.Contains(m.Content, "e2e-ok") {
			sawToolResult = true
		}
	}
	if !sawAssistantCall || !sawToolResult {
		t.Errorf("wire round-trip broken: assistantCall=%v toolResult=%v msgs=%+v", sawAssistantCall, sawToolResult, msgs)
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Function["name"] != "bash" {
		t.Errorf("tools not advertised: %+v", requests[0].Tools)
	}
}

// openAIWireRequest mirrors the OpenAI wire shape for assertions.
type openAIWireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Type     string         `json:"type"`
		Function map[string]any `json:"function"`
	} `json:"tools"`
	Stream bool `json:"stream"`
}
