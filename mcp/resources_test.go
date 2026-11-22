package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/tools"
)

// resourceServer answers the handshake and offers resources.
const resourceServer = `
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

RESOURCES = [
    {"uri": "file:///notes.md", "name": "notes", "description": "The notes.",
     "mimeType": "text/markdown"},
    {"uri": "file:///data.bin", "name": "data", "mimeType": "application/octet-stream",
     "size": 12},
]

TEMPLATES = [
    {"uriTemplate": "file:///{path}", "name": "files", "mimeType": "text/plain"},
]

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
            "serverInfo": {"name": "resource-server", "version": "1.0.0"},
        }})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"tools": []}})
    elif method == "resources/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"resources": RESOURCES}})
    elif method == "resources/templates/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"resourceTemplates": TEMPLATES}})
    elif method == "resources/read":
        uri = params.get("uri", "")
        if uri == "file:///notes.md":
            send({"jsonrpc": "2.0", "id": mid, "result": {"contents": [
                {"uri": uri, "mimeType": "text/markdown", "text": "# Notes\nSome notes."},
            ]}})
        elif uri == "file:///data.bin":
            send({"jsonrpc": "2.0", "id": mid, "result": {"contents": [
                {"uri": uri, "mimeType": "application/octet-stream", "blob": "AAECAwQFBgc="},
            ]}})
        else:
            send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32002, "message": "no such resource"}})
    elif mid is not None:
        send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "no such method"}})
`

// TestListResources verifies resources/list reaches the server.
func TestListResources(t *testing.T) {
	script := pythonServer(t, resourceServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if !client.HasResourcesCapability() {
		t.Fatal("the server's resources capability was not recorded")
	}
	resources, err := client.ListResources(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("resources = %+v, want two", resources)
	}
	if resources[0].URI != "file:///notes.md" || resources[0].Name != "notes" {
		t.Errorf("resources[0] = %+v", resources[0])
	}
	if resources[0].MimeType != "text/markdown" {
		t.Errorf("mime type = %q", resources[0].MimeType)
	}
	if resources[1].Size == nil || *resources[1].Size != 12 {
		t.Errorf("size = %v, want 12", resources[1].Size)
	}
}

// TestListResourceTemplates verifies resources/templates/list.
func TestListResourceTemplates(t *testing.T) {
	script := pythonServer(t, resourceServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	templates, err := client.ListResourceTemplates(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(templates) != 1 || templates[0].URITemplate != "file:///{path}" {
		t.Errorf("templates = %+v", templates)
	}
}

// TestReadResource verifies resources/read returns the content.
func TestReadResource(t *testing.T) {
	script := pythonServer(t, resourceServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	result, err := client.ReadResource(context.Background(), "file:///notes.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("contents = %+v", result.Contents)
	}
	if !strings.Contains(result.TextResult(), "Some notes") {
		t.Errorf("text = %q, want the content", result.TextResult())
	}
}

// TestReadResourceBinary verifies a binary resource is described
// rather than dumped: the harness's messages carry text only.
func TestReadResourceBinary(t *testing.T) {
	script := pythonServer(t, resourceServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	result, err := client.ReadResource(context.Background(), "file:///data.bin")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := result.TextResult()
	if strings.Contains(text, "AAECAwQFBgc=") {
		t.Errorf("text = %q, want the blob omitted", text)
	}
	if !strings.Contains(text, "binary resource") {
		t.Errorf("text = %q, want the resource described", text)
	}
}

// TestReadResourceUnknown verifies a server error surfaces.
func TestReadResourceUnknown(t *testing.T) {
	script := pythonServer(t, resourceServer)
	client, err := Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if _, err := client.ReadResource(context.Background(), "file:///missing"); err == nil {
		t.Fatal("expected the error to surface")
	}
}

// TestResourceToolsReachTheRegistry verifies the three resource tools
// are registered and work.
func TestResourceToolsReachTheRegistry(t *testing.T) {
	script := pythonServer(t, resourceServer)
	registry := tools.NewRegistry()
	mgr := NewManager(registry)

	if err := mgr.Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer mgr.Close()

	if err := mgr.RegisterResourceTools(); err != nil {
		t.Fatalf("register resource tools: %v", err)
	}

	for _, name := range []string{ListResourcesTool, ListResourceTemplatesTool, ReadResourceTool} {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("%s not registered", name)
		}
	}

	// The listing covers every server with resources.
	tool, ok := registry.Get(ListResourcesTool)
	if !ok {
		t.Fatalf("%s not registered", ListResourcesTool)
	}
	res, err := tool.Execute(context.Background(), "1", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("result = %v, want a successful listing", res)
	}
	if !strings.Contains(res.Content, "file:///notes.md") {
		t.Errorf("content = %q, want the resource", res.Content)
	}
	if !strings.Contains(res.Content, `"server": "res"`) {
		t.Errorf("content = %q, want the server tag", res.Content)
	}

	// The reading tool returns the content.
	tool, ok = registry.Get(ReadResourceTool)
	if !ok {
		t.Fatalf("%s not registered", ReadResourceTool)
	}
	res, err = tool.Execute(context.Background(), "2", json.RawMessage(`{"server":"res","uri":"file:///notes.md"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("result = %v, want a successful read", res)
	}
	if !strings.Contains(res.Content, "Some notes") {
		t.Errorf("content = %q, want the resource's content", res.Content)
	}
}

// TestResourceToolsAreNotRegisteredWithoutResources verifies a server
// without the resources capability does not add the tools.
func TestResourceToolsAreNotRegisteredWithoutResources(t *testing.T) {
	script := pythonServer(t, echoServer)
	registry := tools.NewRegistry()
	mgr := NewManager(registry)

	if err := mgr.Connect(context.Background(), ServerConfig{
		Name:    "echo",
		Command: "python3",
		Args:    []string{script},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer mgr.Close()

	if err := mgr.RegisterResourceTools(); err != nil {
		t.Fatalf("register resource tools: %v", err)
	}
	if _, ok := registry.Get(ListResourcesTool); ok {
		t.Errorf("%s should not be registered without a server with resources", ListResourcesTool)
	}
	if len(mgr.ResourceServers()) != 0 {
		t.Errorf("resource servers = %v, want none", mgr.ResourceServers())
	}
}

// TestReadResourceToolNeedsItsArguments verifies the required
// arguments are checked.
func TestReadResourceToolNeedsItsArguments(t *testing.T) {
	script := pythonServer(t, resourceServer)
	registry := tools.NewRegistry()
	mgr := NewManager(registry)
	if err := mgr.Connect(context.Background(), ServerConfig{
		Name:    "res",
		Command: "python3",
		Args:    []string{script},
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer mgr.Close()
	if err := mgr.RegisterResourceTools(); err != nil {
		t.Fatalf("register resource tools: %v", err)
	}

	tool, _ := registry.Get(ReadResourceTool)
	res, err := tool.Execute(context.Background(), "1", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.IsError {
		t.Error("a missing server should be an error")
	}
}

func TestReadResourceResultExtractImages(t *testing.T) {
	b64PNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	res := ReadResourceResult{
		Contents: []ResourceContents{
			{URI: "file:///shot.png", MimeType: "image/png", Blob: b64PNG},
			{URI: "file:///raw.bin", MimeType: "application/octet-stream", Blob: "AAEC"},
		},
	}
	text, imgs := res.ExtractImages()
	if len(imgs) != 1 {
		t.Fatalf("imgs = %d, want 1", len(imgs))
	}
	if !strings.Contains(text, "image resource file:///shot.png") {
		t.Errorf("text = %q", text)
	}
	if !strings.Contains(text, "binary resource file:///raw.bin omitted") {
		t.Errorf("text = %q", text)
	}
}
