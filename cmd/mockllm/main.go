// Command mockllm is a tiny offline OpenAI-compatible gateway for testing
// the harness without network access. It streams scripted SSE responses:
// the first completion asks for a bash tool call, the next one quotes the
// tool result. Point the harness at it with:
//
//	mockllm -addr 127.0.0.1:8099
//	harness -model mock-model -base-url http://127.0.0.1:8099/v1 -print "hi"
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
)

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Function map[string]any `json:"function"`
	} `json:"tools"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "mock-model", "object": "model"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", handleChat)
	log.Println("mockllm listening on", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Decide the scripted response: with a tool result in history, finish;
	// otherwise (if the client advertises tools) request a bash call.
	hasToolResult := false
	lastUser := ""
	toolOutput := ""
	userCount := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			hasToolResult = true
			toolOutput = m.Content
		}
		if m.Role == "user" {
			lastUser = m.Content
			userCount++
		}
	}
	hasBash := false
	for _, t := range req.Tools {
		if name, _ := t.Function["name"].(string); name == "bash" {
			hasBash = true
		}
	}

	// A tool advertised by a process extension: call it, so the harness
	// exercises the extension bridge end to end.
	extTool := ""
	for _, t := range req.Tools {
		if name, _ := t.Function["name"].(string); name == "demo_echo" {
			extTool = name
		}
	}

	// A tool served by an MCP server: call it, so the harness exercises
	// the MCP bridge end to end.
	mcpTool := ""
	for _, t := range req.Tools {
		if name, _ := t.Function["name"].(string); name == "mcp__demo__add" {
			mcpTool = name
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	flush := w.(http.Flusher)
	send := func(payload string) {
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flush.Flush()
	}

	switch {
	case hasToolResult:
		// Echo the tool's output so the caller can see it arrived.
		text := "mockllm done; tool output confirmed: " + toolOutput
		send(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text))
		send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	case strings.Contains(lastUser, "BRANCH"):
		// A scripted answer for branch-tree testing: the reply names
		// how many user messages came before it, so two branches from
		// the same point get different text.
		text := fmt.Sprintf("mockllm branch answer (user messages: %d)", userCount)
		send(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text))
		send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	case extTool != "":
		send(`{"choices":[{"delta":{"content":"calling extension tool"}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ext","function":{"name":"demo_echo","arguments":""}}]}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":\"hello-from-extension\"}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	case mcpTool != "":
		send(`{"choices":[{"delta":{"content":"calling mcp tool"}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_mcp","function":{"name":"mcp__demo__add","arguments":""}}]}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":17,\"b\":25}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	case strings.Contains(lastUser, "ESCAPE"):
		// A scripted write outside the working directory, so the
		// sandbox can be exercised end to end.
		send(`{"choices":[{"delta":{"content":"escaping the sandbox"}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_escape","function":{"name":"bash","arguments":""}}]}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"echo escaped > /tmp/harness-sandbox-escape.txt\"}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	case strings.Contains(lastUser, "DANGEROUS"):
		// A scripted dangerous command, so the permission layer can be
		// exercised end to end.
		send(`{"choices":[{"delta":{"content":"running dangerous command"}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_danger","function":{"name":"bash","arguments":""}}]}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"rm -rf /tmp/should-not-happen\"}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	case hasBash:
		send(`{"choices":[{"delta":{"content":"running tool"}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_mock","function":{"name":"bash","arguments":""}}]}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"echo mock-e2e-ok\"}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	default:
		text := "mockllm says: " + lastUser
		send(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text))
		send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	}
	send(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	send(`[DONE]`)
}
