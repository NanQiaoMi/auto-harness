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

func TestGeminiStreamTextAndToolCall(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("api key header = %q", r.Header.Get("x-goog-api-key"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc1","name":"read","args":{"path":"a.txt"}}}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":" done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`,
		}
		for _, c := range chunks {
			io.WriteString(w, "data: "+c+"\n\n")
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewGeminiProvider("test-key", srv.URL)
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model:    "gemini-4-argon",
		System:   "be nice",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "read", Description: "r", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	msg, usage, err := Collect(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "/models/gemini-4-argon:streamGenerateContent") {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotPath, "alt=sse") {
		t.Errorf("missing alt=sse: %q", gotPath)
	}
	if msg.Content != "Hi done" {
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
	if usage.InputTokens != 5 || usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}

	// The request must carry the system instruction and the tool schema.
	if _, ok := gotBody["systemInstruction"]; !ok {
		t.Errorf("systemInstruction missing: %+v", gotBody)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools = %+v", gotBody["tools"])
	}
}
