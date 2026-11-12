package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestHTTPServer starts an HTTP server that speaks the MCP
// streamable HTTP transport.
func newTestHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case MethodInitialize:
			result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "http-server", "version": "1.0.0"},
			}
		case MethodToolsList:
			result = map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echo the text back.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
					},
				}},
			}
		case MethodToolsCall:
			var params CallToolParams
			json.Unmarshal(req.Params, &params)
			text, _ := params.Arguments["text"].(string)
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo: " + text}},
			}
		default:
			result = map[string]any{}
		}
		data, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}
