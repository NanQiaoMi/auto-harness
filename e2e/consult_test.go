//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestLiveProcessExtensionVetoesAToolCall verifies the consult channel
// against a real model.
//
// The extension is a separate process speaking the JSON-RPC protocol,
// and it vetoes one tool call before the call runs. The model must
// see the veto as a failed tool call, not as a successful one.
func TestLiveProcessExtensionVetoesAToolCall(t *testing.T) {
	ag, reg, bus := newAgent(t)

	// A tool the model will actually call, so the veto has something
	// to land on.
	if err := reg.Register(&tools.Tool{
		Name:        "guarded_tool",
		Description: "Writes a file. Use it when asked to write the guard file.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"},
		},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "GUARDED_TOOL_RAN"}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The extension process: it vetoes the guarded tool by name.
	script := filepath.Join(t.TempDir(), "veto.py")
	if err := os.WriteFile(script, []byte(vetoExtensionScript), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	pe := ext.NewProcessExtension("python3", script)
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	// The result of the guarded call, seen from the event stream.
	var result string
	var wasError bool
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolCall == nil || e.ToolCall.Name != "guarded_tool" || e.ToolResult == nil {
			return nil
		}
		result = e.ToolResult.Content
		wasError = e.ToolResult.IsError
		return nil
	})

	// The model calls the guarded tool. The extension vetoes it, so
	// the tool never runs.
	res, err := ag.Run(context.Background(),
		"Call the guarded_tool exactly once with path \"guard.txt\". Then reply with the tool's output.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result == "" {
		t.Fatalf("the tool was never called (final: %q)", res.Final)
	}
	if !wasError {
		t.Errorf("the vetoed call reported success: %q", result)
	}
	if strings.Contains(result, "GUARDED_TOOL_RAN") {
		t.Errorf("the vetoed tool ran anyway: %q", result)
	}
	// The reason the extension gave must reach the model, so it can
	// react to the veto instead of retrying blindly.
	if !strings.Contains(result, "the veto extension blocks guarded_tool") {
		t.Errorf("the veto reason did not reach the model: %q", result)
	}
}

// vetoExtensionScript is a process extension that vetoes every call to
// the guarded_tool, and lets everything else through.
const vetoExtensionScript = `
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

MANIFEST = {"name": "veto", "description": "vetoes the guarded tool"}

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
        if call.get("name") == "guarded_tool":
            send({"id": mid, "result": {"block": True, "reason": "the veto extension blocks guarded_tool"}})
        else:
            send({"id": mid, "result": {}})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`
