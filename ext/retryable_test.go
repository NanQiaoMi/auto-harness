package ext

import (
	"path/filepath"
	"testing"

	"github.com/NanQiaoMi/auto-harness/tools"
)

// pythonRetryableExtensionScript offers two tools: one that declares
// itself retryable, and one that does not.
const pythonRetryableExtensionScript = `
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
    "name": "retryable",
    "description": "declares which tools may be retried",
    "tools": [
        {
            "name": "fetch_remote",
            "description": "talks to a remote service",
            "retryable": True,
        },
        {
            "name": "write_file",
            "description": "has an effect",
        },
    ],
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
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`

// TestManifestRetryableReachesTheTool checks that a process extension's
// declaration survives the bridge: the agent decides whether to run a
// failed call again from tools.Tool.Retryable, so the flag has to make
// it onto the registered tool.
//
// The default is the safe one. A tool that does not say it is retryable
// is not, because the agent re-sends the same arguments and a
// half-applied effect would be applied twice.
func TestManifestRetryableReachesTheTool(t *testing.T) {
	script := filepath.Join(t.TempDir(), "retryable.py")
	writeFile(t, script, pythonRetryableExtensionScript)

	reg := tools.NewRegistry()
	api := NewAPI(NewBus(), reg, NewCommandRegistry())

	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer pe.Stop()

	remote, ok := reg.Get("fetch_remote")
	if !ok {
		t.Fatal("fetch_remote not registered")
	}
	if !remote.Retryable {
		t.Error("the manifest's retryable flag did not reach the tool")
	}

	local, ok := reg.Get("write_file")
	if !ok {
		t.Fatal("write_file not registered")
	}
	if local.Retryable {
		t.Error("a tool that did not declare itself retryable became retryable")
	}
}
