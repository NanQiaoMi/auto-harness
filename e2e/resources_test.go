//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimi/auto-harness/agent"
	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/mcp"
	"github.com/mimi/auto-harness/tools"
)

// writeLiveResourceServer writes an MCP server that offers one
// resource, for the resource tests.
func writeLiveResourceServer(t *testing.T, path string) {
	t.Helper()
	script := `
import json, sys

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    line = line.strip()
    return json.loads(line) if line else read()

RESOURCES = [{
    "uri": "file:///live-note.md",
    "name": "live-note",
    "description": "A note the model can read.",
    "mimeType": "text/markdown",
}]

while True:
    try:
        msg = read()
    except Exception:
        break
    if msg is None:
        break
    method = msg.get("method", "")
    mid = msg.get("id")
    params = msg.get("params") or {}
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": mid, "result": {
            "protocolVersion": "2025-06-18",
            "capabilities": {"tools": {"listChanged": True}, "resources": {}},
            "serverInfo": {"name": "live-resource-server", "version": "1.0.0"},
        }})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"tools": []}})
    elif method == "resources/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"resources": RESOURCES}})
    elif method == "resources/templates/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"resourceTemplates": []}})
    elif method == "resources/read":
        send({"jsonrpc": "2.0", "id": mid, "result": {"contents": [
            {"uri": params.get("uri", ""), "mimeType": "text/markdown",
             "text": "The pelican remembers the live resource marker."},
        ]}})
    elif mid is not None:
        send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "no such method"}})
`
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write server: %v", err)
	}
}

// TestLiveMCPResourceReachesTheModel verifies the resource tools
// reach the real model: it lists the resources of the connected
// server, reads one and answers from its content.
func TestLiveMCPResourceReachesTheModel(t *testing.T) {
	apiKey(t) // require a live key
	isolate(t)

	script := filepath.Join(t.TempDir(), "live_resource.py")
	writeLiveResourceServer(t, script)

	reg := tools.NewRegistry()
	mgr := mcp.NewManager(reg)
	defer mgr.Close()

	if err := mgr.Connect(context.Background(), mcp.ServerConfig{
		Name:    "live",
		Command: "python3",
		Args:    []string{script},
	}); err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	if err := mgr.RegisterResourceTools(); err != nil {
		t.Fatalf("register resource tools: %v", err)
	}
	for _, name := range []string{mcp.ListResourcesTool, mcp.ReadResourceTool} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("%s was not registered", name)
		}
	}

	bus := ext.NewBus()
	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  8,
		MaxTokens: 2048,
	}, bus, reg)

	var listed, read bool
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolCall == nil {
			return nil
		}
		switch e.ToolCall.Name {
		case mcp.ListResourcesTool:
			listed = true
		case mcp.ReadResourceTool:
			read = true
		}
		return nil
	})

	res, err := ag.Run(context.Background(),
		"Use the list_mcp_resources tool to find the resource of the server named \"live\", "+
			"then use the read_mcp_resource tool to read it, and reply with the sentence it contains.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !listed {
		t.Error("the model never listed the resources")
	}
	if !read {
		t.Error("the model never read a resource")
	}
	if !strings.Contains(res.Final, "pelican") {
		t.Errorf("Final = %q, want it to contain the resource's content", res.Final)
	}
}
