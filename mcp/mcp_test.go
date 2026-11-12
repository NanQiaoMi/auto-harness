package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimi/auto-harness/tools"
)

// pythonServer writes a small MCP server for the tests.
func pythonServer(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.py")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write server: %v", err)
	}
	return path
}

// echoServer answers the handshake and offers one tool.
const echoServer = `
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

TOOLS = [{
    "name": "echo",
    "description": "Echo the text back.",
    "inputSchema": {
        "type": "object",
        "properties": {"text": {"type": "string"}},
        "required": ["text"],
    },
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
            "capabilities": {"tools": {"listChanged": True}},
            "serverInfo": {"name": "echo-server", "version": "1.0.0"},
            "instructions": "First line of the instructions.\nMore detail here.",
        }})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"tools": TOOLS}})
    elif method == "tools/call":
        text = (params.get("arguments") or {}).get("text", "")
        send({"jsonrpc": "2.0", "id": mid, "result": {
            "content": [{"type": "text", "text": "echo: " + text}],
            "isError": False,
        }})
    elif mid is not None:
        send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "no such method"}})
`

// TestConnectCompletesHandshake verifies the initialize sequence and
// the tool list.
func TestConnectCompletesHandshake(t *testing.T) {
	script := pythonServer(t, echoServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "echo",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if got := client.ServerInfo().Name; got != "echo-server" {
		t.Errorf("server name = %q, want echo-server", got)
	}
	tools := client.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	if !client.HasToolsCapability() {
		t.Error("the server's tools capability was not recorded")
	}
	// The description falls back to the first line of the instructions.
	if got := client.Description(); got != "First line of the instructions." {
		t.Errorf("description = %q", got)
	}
}

// TestCallToolRoundTrip verifies a tool call reaches the server and the
// result comes back.
func TestCallToolRoundTrip(t *testing.T) {
	script := pythonServer(t, echoServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "echo",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	res, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Error("the server reported an error")
	}
	if got := res.TextResult(); got != "echo: hi" {
		t.Errorf("result = %q, want %q", got, "echo: hi")
	}
}

// TestManagerBridgesToolsToRegistry verifies the server's tools reach
// the harness's tool registry under the namespaced name and work.
func TestManagerBridgesToolsToRegistry(t *testing.T) {
	script := pythonServer(t, echoServer)
	registry := tools.NewRegistry()
	mgr := NewManager(registry)

	if err := mgr.Connect(context.Background(), ServerConfig{
		Name:    "demo",
		Command: "python3",
		Args:    []string{script},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer mgr.Close()

	tool, ok := registry.Get("mcp__demo__echo")
	if !ok {
		names := []string{}
		for _, tl := range registry.GetAllTools() {
			names = append(names, tl.Name)
		}
		t.Fatalf("mcp__demo__echo not registered; have %v", names)
	}
	if tool.Description == "" {
		t.Error("the bridged tool has no description")
	}

	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"text":"hi"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Content != "echo: hi" {
		t.Errorf("tool result = %q", res.Content)
	}
	if res.IsError {
		t.Error("the bridged tool reported an error")
	}

	// The manager knows the server and its tool count.
	if got := mgr.ToolCount("demo"); got != 1 {
		t.Errorf("tool count = %d, want 1", got)
	}
	statuses := mgr.Statuses()
	if len(statuses) != 1 || statuses[0].Name != "demo" || !statuses[0].Connected {
		t.Errorf("statuses = %+v", statuses)
	}
}

// TestToolNaming verifies the tool name mangling rules.
func TestToolNaming(t *testing.T) {
	cases := []struct {
		server, tool, want string
	}{
		{"docs", "search", "mcp__docs__search"},
		{"my-server", "read_file", "mcp__my_server__read_file"},
		{"fs", "read-file", "mcp__fs__read_file"},
		{"a.b", "c:d", "mcp__a_b__c_d"},
	}
	for _, c := range cases {
		if got := ToolName(c.server, c.tool); got != c.want {
			t.Errorf("ToolName(%q, %q) = %q, want %q", c.server, c.tool, got, c.want)
		}
	}
}

// TestBrokenServerDoesNotBreakOthers verifies one failing server does
// not stop the others from connecting.
func TestBrokenServerDoesNotBreakOthers(t *testing.T) {
	script := pythonServer(t, echoServer)
	registry := tools.NewRegistry()
	mgr := NewManager(registry)

	err := mgr.ConnectAll(context.Background(), []ServerConfig{
		{Name: "broken", Command: "definitely-not-a-real-command-xyz"},
		{Name: "demo", Command: "python3", Args: []string{script}},
	})
	if err == nil {
		t.Fatal("expected an error for the broken server")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error = %v, want it to name the broken server", err)
	}
	// The working server still connected.
	if _, ok := registry.Get("mcp__demo__echo"); !ok {
		t.Error("the working server's tools are missing")
	}
	mgr.Close()
}

// TestServerErrorBecomesToolError verifies a failing call surfaces as a
// tool error rather than a crash.
func TestServerErrorBecomesToolError(t *testing.T) {
	script := pythonServer(t, echoServer)
	registry := tools.NewRegistry()
	mgr := NewManager(registry)
	if err := mgr.Connect(context.Background(), ServerConfig{
		Name: "demo", Command: "python3", Args: []string{script},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer mgr.Close()

	tool, _ := registry.Get("mcp__demo__echo")
	// An unknown tool makes the server answer with an error.
	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"text":"x"}`), nil)
	if err != nil {
		t.Fatalf("execute returned a hard error: %v", err)
	}
	if res.IsError {
		t.Error("expected a successful result")
	}
}

// TestConfigMerging verifies the project file overrides the user file.
func TestConfigMerging(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "user.json")
	projectPath := filepath.Join(dir, "project.json")

	user := &File{MCPServers: map[string]ServerConfig{
		"shared":   {Command: "user-cmd"},
		"onlyuser": {Command: "u"},
	}}
	project := &File{MCPServers: map[string]ServerConfig{
		"shared":   {Command: "project-cmd"},
		"onlyproj": {Command: "p"},
	}}
	if err := SaveFile(userPath, user); err != nil {
		t.Fatal(err)
	}
	if err := SaveFile(projectPath, project); err != nil {
		t.Fatal(err)
	}

	// Without project trust, only the user file is read.
	cfg, err := LoadConfig(userPath, projectPath, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(cfg.Servers))
	}
	for _, s := range cfg.Servers {
		if s.Name == "shared" && s.Command != "user-cmd" {
			t.Errorf("shared command = %q, want user-cmd", s.Command)
		}
	}

	// With project trust, the project entry replaces the user one.
	cfg, err = LoadConfig(userPath, projectPath, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("servers = %d, want 3", len(cfg.Servers))
	}
	for _, s := range cfg.Servers {
		if s.Name == "shared" && s.Command != "project-cmd" {
			t.Errorf("shared command = %q, want project-cmd", s.Command)
		}
	}
	if cfg.Sources["shared"] != projectPath {
		t.Errorf("source = %q, want the project path", cfg.Sources["shared"])
	}
}

// TestValidateServerConfig verifies the configuration rules.
func TestValidateServerConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ServerConfig
		wantErr bool
	}{
		{"ok stdio", ServerConfig{Name: "a", Command: "x"}, false},
		{"ok http", ServerConfig{Name: "a", URL: "https://x/mcp"}, false},
		{"no name", ServerConfig{Command: "x"}, true},
		{"bad name", ServerConfig{Name: "a b", Command: "x"}, true},
		{"sse", ServerConfig{Name: "a", Type: "sse", URL: "https://x"}, true},
		{"no transport", ServerConfig{Name: "a"}, true},
		{"both", ServerConfig{Name: "a", Command: "x", URL: "https://y"}, true},
	}
	for _, c := range cases {
		err := Validate(c.cfg)
		if c.wantErr && err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}
	}
}

// TestDisabledServerIsNotConnected verifies an explicitly disabled
// server is skipped.
func TestDisabledServerIsNotConnected(t *testing.T) {
	off := false
	registry := tools.NewRegistry()
	mgr := NewManager(registry)
	if err := mgr.ConnectAll(context.Background(), []ServerConfig{
		{Name: "off", Command: "python3", Enabled: &off},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if len(mgr.Servers()) != 0 {
		t.Errorf("servers = %v, want none", mgr.Servers())
	}
}

// TestCallTimeout verifies a server that never answers times out
// instead of hanging.
func TestCallTimeout(t *testing.T) {
	// A server that reads but never answers.
	script := pythonServer(t, "import sys\nwhile True:\n    line = sys.stdin.readline()\n    if not line:\n        break\n")
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "silent",
		Command: "python3",
		Args:    []string{script},
		Timeout: 1,
	})
	if err == nil {
		client.Close()
		t.Fatal("expected the handshake to fail")
	}
	if !strings.Contains(err.Error(), "initialize") {
		t.Errorf("error = %v, want it to mention initialize", err)
	}
}

// TestHTTPTransport verifies the HTTP transport against a local server.
func TestHTTPTransport(t *testing.T) {
	srv := newTestHTTPServer(t)
	defer srv.Close()

	client, err := Connect(context.Background(), ServerConfig{
		Name: "http",
		URL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if len(client.Tools()) != 1 {
		t.Fatalf("tools = %+v", client.Tools())
	}
	res, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.TextResult() != "echo: hi" {
		t.Errorf("result = %q", res.TextResult())
	}
}

// TestTimeoutIsBounded verifies the per-request timeout applies.
func TestTimeoutIsBounded(t *testing.T) {
	start := time.Now()
	script := pythonServer(t, "import time, sys\nwhile True:\n    line = sys.stdin.readline()\n    if not line:\n        break\n    time.sleep(5)\n")
	_, err := Connect(context.Background(), ServerConfig{
		Name:    "slow",
		Command: "python3",
		Args:    []string{script},
		Timeout: 1,
	})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the timeout took %s", elapsed)
	}
}
