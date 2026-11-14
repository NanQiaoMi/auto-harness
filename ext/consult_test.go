package ext

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestProcessExtensionCanBlockAToolCall verifies the consult channel.
//
// The harness asks the extension about a tool call before it runs, and
// the extension's answer can block the call. That is the piece a
// process extension could not do before: it could provide tools, but it
// had no way to veto what the harness does.
//
// The extension here is a Python script, so the test also covers the
// wire protocol rather than a Go-to-Go shortcut.
func TestProcessExtensionCanBlockAToolCall(t *testing.T) {
	script := filepath.Join(t.TempDir(), "veto.py")
	writeFile(t, script, pythonVetoExtensionScript)

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())

	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	// The extension sees the call, and lets it through.
	ev := &Event{
		Type:     EventToolCall,
		ToolCall: &llm.ToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo ok"}`)},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ev.Block {
		t.Fatalf("the call was blocked: %s", ev.BlockReason)
	}

	// And it can block one.
	ev = &Event{
		Type:     EventToolCall,
		ToolCall: &llm.ToolCall{ID: "c2", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf /"}`)},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("the destructive call was not blocked")
	}
	if ev.BlockReason == "" {
		t.Error("the block has no reason")
	}
}

// TestProcessExtensionThatDoesNotAnswerBlocksTheCall verifies the
// failure mode.
//
// The policy is fail-closed, the same one pi documents for its gate
// handlers: an extension that cannot make a safety decision has not
// made one, so the operation does not happen. A broken policy
// extension must not become an open door.
func TestProcessExtensionThatDoesNotAnswerBlocksTheCall(t *testing.T) {
	script := filepath.Join(t.TempDir(), "mute.py")
	writeFile(t, script, pythonMuteExtensionScript)

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())

	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	ev := &Event{
		Type:     EventToolCall,
		ToolCall: &llm.ToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo ok"}`)},
	}
	start := time.Now()
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Error("the call was not blocked by an extension that never answered")
	}
	if ev.BlockReason == "" {
		t.Error("the block has no reason")
	}
	// The wait is bounded, so the harness is not stuck.
	if elapsed := time.Since(start); elapsed > consultTimeout+3*time.Second {
		t.Errorf("the consult took %s, want it bounded by %s", elapsed, consultTimeout)
	}
}

// TestProcessExtensionThatDoesNotOptInIsNotAsked verifies the opt-in.
//
// The harness fails closed, so an extension that never asked to be
// consulted about an event must not be treated as failing to answer:
// its silence is not a refusal. Without this, every extension written
// before the consult channel existed would break every tool call.
func TestProcessExtensionThatDoesNotOptInIsNotAsked(t *testing.T) {
	script := filepath.Join(t.TempDir(), "plain.py")
	writeFile(t, script, pythonPlainExtensionScript)

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())

	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	// The extension does not implement the consult method at all, so
	// asking it would be an error. It was never asked, so the call
	// goes through untouched.
	ev := &Event{
		Type:     EventToolCall,
		ToolCall: &llm.ToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo ok"}`)},
	}
	start := time.Now()
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ev.Block {
		t.Errorf("the call was blocked by an extension that never opted in: %s", ev.BlockReason)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the consult took %s, want it skipped entirely", elapsed)
	}
}

// TestProcessExtensionCanBlockAUserCommand verifies that the consult
// channel covers the "!" shortcut too, not just the model's tool calls.
//
// The command reaches the extension as the bashCommand field, so the
// extension can decide on it without parsing the tool arguments.
func TestProcessExtensionCanBlockAUserCommand(t *testing.T) {
	script := filepath.Join(t.TempDir(), "veto.py")
	writeFile(t, script, pythonVetoExtensionScript)

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())

	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	ev := &Event{Type: EventUserBash, BashCommand: "rm -rf /tmp/whatever"}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("the destructive command was not blocked")
	}
	if ev.BlockReason == "" {
		t.Error("the block has no reason")
	}

	// A harmless command still runs.
	ev = &Event{Type: EventUserBash, BashCommand: "echo ok"}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ev.Block {
		t.Errorf("the harmless command was blocked: %s", ev.BlockReason)
	}
}

// pythonVetoExtensionScript is an extension that blocks the calls whose
// command mentions "rm -rf".
const pythonVetoExtensionScript = `
import json, sys

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    line = line.strip()
    if not line:
        return read()
    return json.loads(line)

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

MANIFEST = {
    "name": "veto",
    "description": "blocks destructive calls",
    "consults": ["tool_call", "user_bash"],
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
    elif method == "event.consult":
        call = params.get("toolCall") or {}
        args = call.get("arguments") or {}
        command = args.get("command", "")
        if not command:
            command = params.get("bashCommand", "")
        if "rm -rf" in command:
            send({"id": mid, "result": {"block": True, "reason": "the veto extension blocks rm -rf"}})
        else:
            send({"id": mid, "result": {}})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`

// pythonMuteExtensionScript never answers a consult: it reads the
// request and says nothing.
const pythonMuteExtensionScript = `
import json, sys

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    line = line.strip()
    if not line:
        return read()
    return json.loads(line)

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

MANIFEST = {
    "name": "mute",
    "description": "never answers",
    "consults": ["tool_call"],
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
    if method == "manifest":
        send({"id": mid, "result": MANIFEST})
    elif method == "event.consult":
        # Deliberately silent: the harness must not hang on this, and
        # the call must not run either.
        continue
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`

// pythonPlainExtensionScript is an extension that knows nothing about
// the consult channel — the shape every extension written before the
// channel existed has.
const pythonPlainExtensionScript = `
import json, sys

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    line = line.strip()
    if not line:
        return read()
    return json.loads(line)

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

MANIFEST = {"name": "plain", "description": "knows nothing about consult"}

while True:
    try:
        msg = read()
    except Exception:
        break
    if msg is None:
        break
    method = msg.get("method", "")
    mid = msg.get("id")
    if method == "manifest":
        send({"id": mid, "result": MANIFEST})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
    elif mid:
        # An unknown method is an error, exactly as it would be in a
        # real extension that predates the consult channel.
        send({"id": mid, "error": {"code": -32601, "message": "unknown method: " + method}})
`
