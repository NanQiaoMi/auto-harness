package ext

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

func TestSupervisedProcessRestartsOnFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "supervised.py")
	writeFile(t, script, pythonExtensionScript)

	bus := NewBus()
	registry := tools.NewRegistry()
	api := NewAPI(bus, registry, NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.RestartPolicy = RestartPolicy{
		Mode:        RestartOnFailure,
		MaxRestarts: 2,
		Backoff:     10 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
	}
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	first := pe.currentCommand()
	if first == nil || first.Process == nil {
		t.Fatal("the first child process was not started")
	}
	firstPID := first.Process.Pid
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill first child: %v", err)
	}

	status := waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return status.Restarts == 1 && status.Healthy
	})
	if status.LastExit == "" {
		t.Error("the supervised extension did not record its last exit")
	}
	second := pe.currentCommand()
	if second == nil || second.Process == nil {
		t.Fatal("the replacement child process was not started")
	}
	if second.Process.Pid == firstPID {
		t.Fatalf("child PID did not change: %d", firstPID)
	}
	if first.ProcessState == nil {
		t.Error("the failed child was not reaped")
	}

	tool, ok := registry.Get("probe_tool")
	if !ok {
		t.Fatal("probe_tool disappeared after the restart")
	}
	result, err := tool.Execute(
		context.Background(), "after-restart", json.RawMessage(`{"text":"ok"}`), nil)
	if err != nil {
		t.Fatalf("execute after restart: %v", err)
	}
	if result.Content != "echo:ok" || result.IsError {
		t.Fatalf("result after restart = %#v", result)
	}
}

func TestSupervisedProcessNeverRestartsByDefault(t *testing.T) {
	script := filepath.Join(t.TempDir(), "unsupervised.py")
	writeFile(t, script, pythonExtensionScript)

	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	cmd := pe.currentCommand()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no child process")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	status := waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return !status.Healthy && !status.Restarting && status.LastExit != ""
	})
	if status.Mode != RestartNever || status.Restarts != 0 || status.MaxRestarts != 0 {
		t.Fatalf("unexpected supervision status: %#v", status)
	}
	time.Sleep(100 * time.Millisecond)
	if cmd := pe.currentCommand(); cmd != nil {
		t.Fatal("an unsupervised process was restarted")
	}
	if status := pe.SupervisionStatus(); status.Restarts != 0 {
		t.Fatalf("restart count = %d, want 0", status.Restarts)
	}
}

func TestSupervisedProcessRestartLimitIsLifetimeBounded(t *testing.T) {
	script := filepath.Join(t.TempDir(), "limited.py")
	writeFile(t, script, pythonExtensionScript)

	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.RestartPolicy = RestartPolicy{
		Mode:        RestartOnFailure,
		MaxRestarts: 1,
		Backoff:     10 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
	}
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	first := pe.currentCommand()
	if first == nil || first.Process == nil {
		t.Fatal("no initial child process")
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill initial child: %v", err)
	}
	waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return status.Restarts == 1 && status.Healthy
	})

	second := pe.currentCommand()
	if second == nil || second.Process == nil {
		t.Fatal("no replacement child process")
	}
	if err := second.Process.Kill(); err != nil {
		t.Fatalf("kill replacement child: %v", err)
	}
	status := waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return !status.Healthy && !status.Restarting &&
			strings.Contains(status.LastError, "restart limit 1")
	})
	if status.Restarts != 1 {
		t.Fatalf("restart count = %d, want the lifetime limit of 1", status.Restarts)
	}
	time.Sleep(100 * time.Millisecond)
	if cmd := pe.currentCommand(); cmd != nil {
		t.Fatal("the supervisor started another process after reaching its limit")
	}
}

func TestSupervisedProcessRejectsManifestChange(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "changing.py")
	writeFile(t, script, pythonChangingManifestScript)

	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.RestartPolicy = RestartPolicy{
		Mode:        RestartOnFailure,
		MaxRestarts: 3,
		Backoff:     10 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
	}
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	writeFile(t, filepath.Join(dir, "changed"), "change the manifest")
	cmd := pe.currentCommand()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no child process")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	status := waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return !status.Healthy && !status.Restarting &&
			strings.Contains(status.LastError, "different manifest")
	})
	if status.Restarts != 0 {
		t.Fatalf("a changed manifest counted as a valid restart: %#v", status)
	}
	if cmd := pe.currentCommand(); cmd != nil {
		t.Fatal("the changed process was retained")
	}
}

func TestSupervisedProcessFailsClosedDuringBackoffAndStopsPromptly(t *testing.T) {
	script := filepath.Join(t.TempDir(), "policy.py")
	writeFile(t, script, pythonVetoExtensionScript)

	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.RestartPolicy = RestartPolicy{
		Mode:        RestartOnFailure,
		MaxRestarts: 2,
		Backoff:     5 * time.Second,
		MaxBackoff:  5 * time.Second,
	}
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	cmd := pe.currentCommand()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no child process")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return status.Restarting && !status.Healthy
	})

	ev := &Event{
		Type: EventToolCall,
		ToolCall: &llm.ToolCall{
			ID:        "during-backoff",
			Name:      "bash",
			Arguments: json.RawMessage(`{"command":"echo ok"}`),
		},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("a gate was left open while the policy process was restarting")
	}
	if !strings.Contains(ev.BlockReason, "not running") {
		t.Fatalf("block reason = %q", ev.BlockReason)
	}

	start := time.Now()
	if err := pe.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop waited for the %s backoff instead of cancelling it", elapsed)
	}
	status := pe.SupervisionStatus()
	if status.Restarting || status.Healthy {
		t.Fatalf("stopped status is still live: %#v", status)
	}
}

func TestSupervisedProcessAlwaysRestartsAfterCleanExit(t *testing.T) {
	script := filepath.Join(t.TempDir(), "always.py")
	writeFile(t, script, pythonExtensionScript)

	api := NewAPI(NewBus(), tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	pe.RestartPolicy = RestartPolicy{
		Mode:        RestartAlways,
		MaxRestarts: 1,
		Backoff:     10 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
	}
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	first := pe.currentCommand()
	if first == nil || first.Process == nil {
		t.Fatal("no child process")
	}
	firstPID := first.Process.Pid
	notifyExtension(t, pe, MethodShutdown, nil)

	waitForSupervision(t, pe, 5*time.Second, func(status SupervisionStatus) bool {
		return status.Restarts == 1 && status.Healthy
	})
	second := pe.currentCommand()
	if second == nil || second.Process == nil || second.Process.Pid == firstPID {
		t.Fatal("a clean exit was not replaced under restart=always")
	}
}

func TestProcessExtensionDropsEventsBeforeManifestApproval(t *testing.T) {
	script := filepath.Join(t.TempDir(), "eager.py")
	writeFile(t, script, pythonEagerEventScript)

	bus := NewBus()
	got := make(chan string, 2)
	bus.On(EventInput, func(ev *Event) error {
		got <- ev.Text
		return nil
	})
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	pe := NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = pe.Stop() })

	select {
	case text := <-got:
		t.Fatalf("event emitted before manifest approval was forwarded: %q", text)
	case <-time.After(100 * time.Millisecond):
	}

	notifyExtension(t, pe, "emit_test", nil)
	select {
	case text := <-got:
		if text != "after-manifest" {
			t.Fatalf("event text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an event emitted after approval was not forwarded")
	}
}

func TestRestartDelayBacksOffAndClamps(t *testing.T) {
	policy := RestartPolicy{
		Mode:        RestartOnFailure,
		MaxRestarts: 4,
		Backoff:     100 * time.Millisecond,
		MaxBackoff:  250 * time.Millisecond,
	}
	got := []time.Duration{
		restartDelay(policy, 1),
		restartDelay(policy, 2),
		restartDelay(policy, 3),
		restartDelay(policy, 4),
	}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		250 * time.Millisecond,
		250 * time.Millisecond,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delay %d = %s, want %s", i+1, got[i], want[i])
		}
	}
}

func TestRestartPolicyDefaults(t *testing.T) {
	policy := RestartPolicy{Mode: RestartOnFailure}.normalized()
	if policy.MaxRestarts != 0 {
		t.Fatalf("MaxRestarts = %d; the process API requires an explicit budget", policy.MaxRestarts)
	}
	if policy.Backoff != 250*time.Millisecond {
		t.Fatalf("Backoff = %s, want 250ms", policy.Backoff)
	}
	if policy.MaxBackoff != 30*time.Second {
		t.Fatalf("MaxBackoff = %s, want 30s", policy.MaxBackoff)
	}

	policy = RestartPolicy{
		Mode:        RestartAlways,
		MaxRestarts: 101,
		Backoff:     time.Millisecond,
		MaxBackoff:  2 * time.Minute,
	}.normalized()
	if policy.MaxRestarts != 100 ||
		policy.Backoff != 10*time.Millisecond ||
		policy.MaxBackoff != time.Minute {
		t.Fatalf("normalized policy = %#v", policy)
	}
}

func waitForSupervision(t *testing.T, pe *ProcessExtension, timeout time.Duration, ready func(SupervisionStatus) bool) SupervisionStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status SupervisionStatus
	for time.Now().Before(deadline) {
		status = pe.SupervisionStatus()
		if ready(status) {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for supervision state; last status: %#v", status)
	return status
}

const pythonChangingManifestScript = `
import json, os, sys

def read():
    line = sys.stdin.readline()
    if not line:
        return None
    return json.loads(line)

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

TOOL = {
    "name": "original_tool",
    "description": "a tool whose name must not change on restart",
    "parameters": {"type": "object", "properties": {}},
}
if os.path.exists(os.path.join(os.path.dirname(os.path.abspath(__file__)), "changed")):
    TOOL["name"] = "changed_tool"

MANIFEST = {"name": "changing", "tools": [TOOL]}

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
        send({"id": mid, "result": {}})
`

const pythonEagerEventScript = `
import json, sys

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

send({"method": "event", "params": {"event": "input", "text": "before-manifest"}})
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method", "")
    mid = msg.get("id")
    if method == "manifest":
        send({"id": mid, "result": {"name": "eager"}})
    elif method == "emit_test":
        send({"method": "event", "params": {"event": "input", "text": "after-manifest"}})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`
