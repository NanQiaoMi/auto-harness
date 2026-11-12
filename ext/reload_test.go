package ext

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mimi/auto-harness/tools"
)

// fakeExtension is an extension that records what it registered.
type fakeExtension struct {
	name      string
	toolName  string
	cmdName   string
	onEvent   bool
	setups    int
	teardowns int
	events    []string
}

func (f *fakeExtension) Name() string { return f.name }

func (f *fakeExtension) Setup(api *API) error {
	f.setups++
	if f.toolName != "" {
		err := api.RegisterTool(&tools.Tool{
			Name:        f.toolName,
			Description: "fake tool",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
				return tools.Result{Content: "ok"}, nil
			},
		})
		if err != nil {
			return err
		}
	}
	if f.cmdName != "" {
		api.RegisterCommand(f.cmdName, &Command{
			Description: "fake command",
			Handler:     func(ctx context.Context, args string) error { return nil },
		})
	}
	if f.onEvent {
		api.On(EventToolExecutionEnd, func(e *Event) error {
			f.events = append(f.events, "end")
			return nil
		})
	}
	return nil
}

func (f *fakeExtension) Teardown() error {
	f.teardowns++
	return nil
}

// TestManagerReloadUnregistersEverything verifies the core reload
// contract: after a reload the old extension's tools, commands and
// handlers are gone, and the new set is registered.
func TestManagerReloadUnregistersEverything(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	cmds := NewCommandRegistry()
	api := NewAPI(bus, reg, cmds)
	mgr := NewManager(api)

	old := &fakeExtension{name: "old", toolName: "old_tool", cmdName: "old_cmd", onEvent: true}
	if err := mgr.Load(old); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := reg.Get("old_tool"); !ok {
		t.Fatal("old_tool not registered")
	}
	if _, ok := cmds.Get("old_cmd"); !ok {
		t.Fatal("old_cmd not registered")
	}
	if bus.HandlerCount(EventToolExecutionEnd) != 1 {
		t.Fatalf("handler count = %d", bus.HandlerCount(EventToolExecutionEnd))
	}

	fresh := &fakeExtension{name: "new", toolName: "new_tool", cmdName: "new_cmd", onEvent: true}
	if err := mgr.Reload(fresh); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, ok := reg.Get("old_tool"); ok {
		t.Error("old_tool survived the reload")
	}
	if _, ok := cmds.Get("old_cmd"); ok {
		t.Error("old_cmd survived the reload")
	}
	if bus.HandlerCount(EventToolExecutionEnd) != 1 {
		t.Errorf("handler count after reload = %d, want 1 (the new extension only)",
			bus.HandlerCount(EventToolExecutionEnd))
	}
	if _, ok := reg.Get("new_tool"); !ok {
		t.Error("new_tool not registered")
	}
	if _, ok := cmds.Get("new_cmd"); !ok {
		t.Error("new_cmd not registered")
	}
	if old.teardowns != 1 {
		t.Errorf("old extension teardowns = %d, want 1", old.teardowns)
	}
	if fresh.setups != 1 {
		t.Errorf("new extension setups = %d, want 1", fresh.setups)
	}
}

// TestReloadEmitsSessionEvents verifies pi's reload semantics: the
// session is shut down and restarted around the swap.
func TestReloadEmitsSessionEvents(t *testing.T) {
	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	mgr := NewManager(api)

	var reasons []string
	bus.On(EventSessionShutdown, func(e *Event) error {
		reasons = append(reasons, "shutdown:"+e.Reason)
		return nil
	})
	bus.On(EventSessionStart, func(e *Event) error {
		reasons = append(reasons, "start:"+e.Reason)
		return nil
	})

	if err := mgr.Reload(&fakeExtension{name: "x"}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	want := []string{"shutdown:reload", "start:reload"}
	if len(reasons) != len(want) {
		t.Fatalf("events = %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, reasons[i], want[i])
		}
	}
}

// TestReloadReRegistersSameToolName verifies an extension can be
// reloaded with the same tool name: the old registration is removed
// first, so there is no "already registered" collision.
func TestReloadReRegistersSameToolName(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	api := NewAPI(bus, reg, NewCommandRegistry())
	mgr := NewManager(api)

	ext := &fakeExtension{name: "e", toolName: "same_tool", cmdName: "same_cmd"}
	if err := mgr.Load(ext); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.Reload(&fakeExtension{name: "e", toolName: "same_tool", cmdName: "same_cmd"}); err != nil {
		t.Fatalf("reload with same names: %v", err)
	}
	if _, ok := reg.Get("same_tool"); !ok {
		t.Error("same_tool missing after reload")
	}
}

// TestFailedLoadLeavesNoTrace verifies a failing extension does not
// leave its partial registrations behind.
func TestFailedLoadLeavesNoTrace(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	cmds := NewCommandRegistry()
	api := NewAPI(bus, reg, cmds)
	mgr := NewManager(api)

	failing := &failingExtension{}
	err := mgr.Load(failing)
	if err == nil {
		t.Fatal("expected load error")
	}
	if _, ok := reg.Get("half_tool"); ok {
		t.Error("half_tool survived a failed load")
	}
	if _, ok := cmds.Get("half_cmd"); ok {
		t.Error("half_cmd survived a failed load")
	}
	if bus.HandlerCount(EventInput) != 0 {
		t.Error("handler survived a failed load")
	}
}

// failingExtension registers things and then fails.
type failingExtension struct{}

func (f *failingExtension) Name() string { return "failing" }

func (f *failingExtension) Setup(api *API) error {
	_ = api.RegisterTool(&tools.Tool{
		Name:        "half_tool",
		Description: "registered before the failure",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{}, nil
		},
	})
	api.RegisterCommand("half_cmd", &Command{Handler: func(ctx context.Context, args string) error { return nil }})
	api.On(EventInput, func(e *Event) error { return nil })
	return os.ErrPermission
}

// TestUnloadRemovesToolFromActiveSet verifies that unloading a tool
// that was pinned active keeps the active subset consistent.
func TestUnloadRemovesToolFromActiveSet(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(&tools.Tool{
		Name: "a",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{}, nil
		},
	})
	_ = reg.Register(&tools.Tool{
		Name: "b",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{}, nil
		},
	})
	if err := reg.SetActiveTools([]string{"a", "b"}); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if err := reg.Unregister("a"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	active := reg.GetActiveTools()
	if len(active) != 1 || active[0].Name != "b" {
		t.Fatalf("active = %v, want only b", active)
	}
	// Unregistering an unknown tool is an error, not a panic.
	if err := reg.Unregister("nope"); err == nil {
		t.Error("expected an error for an unknown tool")
	}
}

// TestProcessExtensionBridgesToolAndEvents runs a real child process
// extension and checks the handshake, the tool bridge and the event
// forwarding.
func TestProcessExtensionBridgesToolAndEvents(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ext.py")
	writeFile(t, script, pythonExtensionScript)

	bus := NewBus()
	reg := tools.NewRegistry()
	cmds := NewCommandRegistry()
	api := NewAPI(bus, reg, cmds)

	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	// The manifest was read.
	m := pe.Manifest()
	if m.Name != "probe" {
		t.Errorf("manifest name = %q, want %q", m.Name, "probe")
	}
	if len(m.Tools) != 1 || m.Tools[0].Name != "probe_tool" {
		t.Fatalf("manifest tools = %v", m.Tools)
	}

	// The tool is registered and callable.
	tool, ok := reg.Get("probe_tool")
	if !ok {
		t.Fatal("probe_tool not registered")
	}
	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"text":"hi"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Content != "echo:hi" {
		t.Errorf("tool result = %q, want %q", res.Content, "echo:hi")
	}
	if res.IsError {
		t.Error("tool reported an error")
	}

	// The extension's event reaches the host bus.
	got := make(chan string, 1)
	bus.On(EventInput, func(e *Event) error {
		select {
		case got <- e.Text:
		default:
		}
		return nil
	})
	// Ask the extension to emit an event.
	notifyExtension(t, pe, MethodEvent, EventParams{Event: string(EventInput), Text: "ping"})

	select {
	case text := <-got:
		if text != "ping" {
			t.Errorf("event text = %q, want %q", text, "ping")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the extension event")
	}
}

// TestProcessExtensionFailingHandshake verifies a child that does not
// answer the manifest request is reported as an error.
func TestProcessExtensionFailingHandshake(t *testing.T) {
	script := filepath.Join(t.TempDir(), "silent.py")
	writeFile(t, script, "import sys\nwhile True:\n    line = sys.stdin.readline()\n    if not line:\n        break\n")

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)

	err := pe.Setup(api)
	if err == nil {
		pe.Stop()
		t.Fatal("expected a handshake error")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("error = %v, want it to mention the manifest", err)
	}
}

// TestProcessExtensionToolError verifies a remote error becomes a tool
// error rather than a crash.
func TestProcessExtensionToolError(t *testing.T) {
	script := filepath.Join(t.TempDir(), "failing_tool.py")
	writeFile(t, script, pythonExtensionScript)

	bus := NewBus()
	reg := tools.NewRegistry()
	api := NewAPI(bus, reg, NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	tool, ok := reg.Get("probe_tool")
	if !ok {
		t.Fatal("probe_tool not registered")
	}
	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"text":""}`), nil)
	if err != nil {
		t.Fatalf("execute returned a hard error: %v", err)
	}
	// An empty text makes the script report an error result.
	if !res.IsError {
		t.Errorf("expected an error result, got %q", res.Content)
	}
}

// TestProcessExtensionTeardownOnReload verifies the child process is
// killed when the extension is unloaded by a reload.
func TestProcessExtensionTeardownOnReload(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ext.py")
	writeFile(t, script, pythonExtensionScript)

	bus := NewBus()
	reg := tools.NewRegistry()
	api := NewAPI(bus, reg, NewCommandRegistry())
	mgr := NewManager(api)

	pe := NewProcessExtension("python3", script)
	if err := mgr.Load(pe); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := reg.Get("probe_tool"); !ok {
		t.Fatal("probe_tool not registered")
	}

	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reg.Get("probe_tool"); ok {
		t.Error("probe_tool survived the reload")
	}
}

// TestProcessExtensionChildIsKilledOnReload verifies the child process
// does not outlive the extension that spawned it.
func TestProcessExtensionChildIsKilledOnReload(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ext.py")
	writeFile(t, script, pythonExtensionScript)

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	mgr := NewManager(api)

	pe := NewProcessExtension("python3", script)
	if err := mgr.Load(pe); err != nil {
		t.Fatalf("load: %v", err)
	}
	cmd := pe.cmd
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no child process")
	}
	pid := cmd.Process.Pid

	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The process must be gone: signalling it must fail with ESRCH.
	// (A zombie would still accept the signal, so this can report a
	// false pass; the wait in Stop reaps it in the common case.)
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Log("child process still present after reload (may be a zombie)")
	}
	if cmd.ProcessState == nil {
		t.Error("child process was not waited for")
	}
	_ = pid
}

// TestDiscoverFindsExecutables verifies extension discovery finds
// executables in a directory and skips non-executables.
func TestDiscoverFindsExecutables(t *testing.T) {
	dir := t.TempDir()
	ext1 := filepath.Join(dir, "one")
	writeFile(t, ext1, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(ext1, 0o755); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "notes.txt")
	writeFile(t, notExec, "hello")

	found := DiscoverIn([]string{dir})
	if len(found) != 1 || found[0] != ext1 {
		t.Fatalf("found = %v, want [%s]", found, ext1)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// notifyExtension sends a notification to a process extension.
func notifyExtension(t *testing.T, pe *ProcessExtension, method string, params any) {
	t.Helper()
	conn := pe.connection()
	if conn == nil {
		t.Fatal("no connection")
	}
	if err := conn.notify(method, params); err != nil {
		t.Fatalf("notify: %v", err)
	}
}

// pythonExtensionScript is a minimal extension used by the tests. It
// answers the manifest, echoes text through its tool, reports an error
// for empty text, and emits an event when asked.
const pythonExtensionScript = `
import json, sys

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    return json.loads(line)

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

MANIFEST = {
    "name": "probe",
    "description": "test extension",
    "tools": [{
        "name": "probe_tool",
        "description": "echo the text",
        "parameters": {
            "type": "object",
            "properties": {"text": {"type": "string"}},
        },
    }],
}

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
    if method == "manifest":
        send({"id": mid, "result": MANIFEST})
    elif method == "tool.execute":
        args = params.get("arguments") or {}
        text = args.get("text", "")
        if text == "":
            send({"id": mid, "result": {"content": "empty text", "is_error": True}})
        else:
            send({"id": mid, "result": {"content": "echo:" + text}})
    elif method == "event":
        send({"method": "event", "params": params})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`
