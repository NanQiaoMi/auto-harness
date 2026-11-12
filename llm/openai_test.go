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

func TestOpenAIStreamTextAndToolCall(t *testing.T) {
	var gotBody openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("auth = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			if strings.HasPrefix(c, "data:") {
				io.WriteString(w, c+"\n\n")
			} else {
				io.WriteString(w, "data: "+c+"\n\n")
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAIProvider("test-key", srv.URL)
	req := ChatRequest{
		Model:    "test-model",
		System:   "be nice",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"}}},
	}
	stream, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	msg, usage, err := Collect(stream, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if msg.Content != "Hello" {
		t.Errorf("content = %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "bash" {
		t.Errorf("tool call = %+v", tc)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("tool args not valid JSON: %v (%s)", err, tc.Arguments)
	}
	if args.Command != "ls" {
		t.Errorf("command = %q", args.Command)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}

	// Wire-format sanity: request carried system, tools and streaming flag.
	if gotBody.Model != "test-model" || !gotBody.Stream {
		t.Errorf("request = %+v", gotBody)
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Function["name"] != "bash" {
		t.Errorf("request tools = %+v", gotBody.Tools)
	}
	if len(gotBody.Messages) != 2 || gotBody.Messages[0].Role != "system" {
		t.Errorf("request messages = %+v", gotBody.Messages)
	}
}

func TestOpenAIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := NewOpenAIProvider("bad", srv.URL)
	_, err := p.Chat(context.Background(), ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestOpenAIReasoningPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
			`{"choices":[{"delta":{"reasoning_content":" more thought"}}]}`,
			`{"choices":[{"delta":{"content":"answer"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			if strings.HasPrefix(c, "data:") {
				io.WriteString(w, c+"\n\n")
			} else {
				io.WriteString(w, "data: "+c+"\n\n")
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAIProvider("k", srv.URL)
	stream, err := p.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var thinking, text []string
	msg, _, err := Collect(stream, func(ev StreamEvent) {
		if ev.Type == EventThinkingDelta {
			thinking = append(thinking, ev.Text)
		} else if ev.Type == EventTextDelta {
			text = append(text, ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(thinking, "") != "thinking... more thought" {
		t.Errorf("thinking = %v", thinking)
	}
	if strings.Join(text, "") != "answer" {
		t.Errorf("text = %v", text)
	}
	// Reasoning must not leak into the transcript content.
	if msg.Content != "answer" {
		t.Errorf("msg.Content = %q", msg.Content)
	}
}

// TestOpenAIChunkWithBothReasoningAndText checks a chunk that carries
// the reasoning and the text together.
//
// Some gateways put both in one chunk. The text is part of the answer,
// so it must not be dropped when the reasoning comes first.
func TestOpenAIChunkWithBothReasoningAndText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"reasoning_content":"hmm","content":"ans"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			if strings.HasPrefix(c, "data:") {
				io.WriteString(w, c+"\n\n")
			} else {
				io.WriteString(w, "data: "+c+"\n\n")
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAIProvider("k", srv.URL)
	stream, err := p.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var thinking, text []string
	msg, _, err := Collect(stream, func(ev StreamEvent) {
		if ev.Type == EventThinkingDelta {
			thinking = append(thinking, ev.Text)
		} else if ev.Type == EventTextDelta {
			text = append(text, ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(thinking, "") != "hmm" {
		t.Errorf("thinking = %v", thinking)
	}
	if strings.Join(text, "") != "ans" {
		t.Errorf("text = %v, want the text of the chunk", text)
	}
	if msg.Content != "ans" {
		t.Errorf("msg.Content = %q, want the text of the chunk", msg.Content)
	}
}
