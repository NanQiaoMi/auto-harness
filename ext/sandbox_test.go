package ext

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/sandbox"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// pythonSandboxProbeScript is an extension whose tool reports what the
// process can see: whether a path outside its directory is readable,
// and whether it can write next to it.
const pythonSandboxProbeScript = `
import json, sys, os

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    return json.loads(line)

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

MANIFEST = {
    "name": "sandbox-probe",
    "description": "reports what the process can reach",
    "tools": [{"name": "probe_paths", "description": "read and write two paths"}],
}

def can_read(path):
    try:
        with open(path, "rb") as f:
            f.read()
        return True
    except Exception:
        return False

def can_write(path):
    try:
        with open(path, "w") as f:
            f.write("x")
        return True
    except Exception:
        return False

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
        secret = args.get("secret", "")
        outside = args.get("outside", "")
        result = "read=%s write=%s" % (can_read(secret), can_write(outside))
        send({"id": mid, "result": {"content": result}})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`

// sandboxProbe runs the probe tool and returns its answer.
func sandboxProbe(t *testing.T, pe *ProcessExtension, secret, outside string) string {
	t.Helper()
	reg := tools.NewRegistry()
	api := NewAPI(NewBus(), reg, NewCommandRegistry())
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	tool, ok := reg.Get("probe_paths")
	if !ok {
		t.Fatal("probe_paths not registered")
	}
	args, err := json.Marshal(map[string]string{"secret": secret, "outside": outside})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), "call-1", args, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res.Content
}

// TestSandboxedExtensionCannotReadDeniedPaths checks that the sandbox
// confines the extension process itself, not just the commands it runs.
//
// The capability list limits which protocol surfaces an extension may
// use; it says nothing about the process. A plugin that declares
// `tools` and nothing else can still read the user's SSH keys unless the
// kernel stops it, and this is that stop.
func TestSandboxedExtensionCannotReadDeniedPaths(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("no sandbox available on this platform")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.py")
	writeFile(t, script, pythonSandboxProbeScript)

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "token")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "escaped.txt")

	pe := NewProcessExtension("python3", script)
	pe.Dir = dir
	pe.Sandbox = &sandbox.Profile{
		DenyReadPaths: []string{secretDir},
	}
	answer := sandboxProbe(t, pe, secret, outside)

	if !strings.Contains(answer, "read=False") {
		t.Errorf("the extension read a denied path: %s", answer)
	}
	if !strings.Contains(answer, "write=False") {
		t.Errorf("the extension wrote outside its directory: %s", answer)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("the file was created outside the sandbox")
	}
}

// TestSandboxedExtensionKeepsItsOwnDirectory checks the other half: the
// confinement is not so tight that a plugin cannot work. Its own
// directory stays writable, which is what a plugin needs to load code
// and write state.
func TestSandboxedExtensionKeepsItsOwnDirectory(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("no sandbox available on this platform")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.py")
	writeFile(t, script, pythonSandboxProbeScript)

	own := filepath.Join(dir, "state.txt")
	pe := NewProcessExtension("python3", script)
	pe.Dir = dir
	pe.Sandbox = &sandbox.Profile{}
	answer := sandboxProbe(t, pe, filepath.Join(dir, "probe.py"), own)

	if !strings.Contains(answer, "read=True") {
		t.Errorf("the extension could not read its own script: %s", answer)
	}
	if !strings.Contains(answer, "write=True") {
		t.Errorf("the extension could not write in its own directory: %s", answer)
	}
}

// TestSandboxProfileFillsTheDefaults checks the profile the extension
// actually runs with: an empty one gets the plugin directory and the
// credential denials rather than "deny every write".
func TestSandboxProfileFillsTheDefaults(t *testing.T) {
	pe := NewProcessExtension("python3")
	pe.Dir = "/plugins/demo"
	pe.Sandbox = &sandbox.Profile{}

	profile := pe.SandboxProfile()
	if profile == nil {
		t.Fatal("no profile")
	}
	if len(profile.WritePaths) != 1 || profile.WritePaths[0] != "/plugins/demo" {
		t.Errorf("write paths = %v, want the plugin directory", profile.WritePaths)
	}
	if len(profile.DenyReadPaths) == 0 {
		t.Error("the credential denials were dropped")
	}

	// An extension that did not ask for a sandbox stays unconfined.
	if NewProcessExtension("python3").SandboxProfile() != nil {
		t.Error("an unconfigured extension got a sandbox")
	}
}
