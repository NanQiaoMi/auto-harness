//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonRun is one run of the harness in JSON output mode.
type jsonRun struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout *bufio.Reader
	stderr *strings.Builder
}

// startJSONMode runs the harness in JSON output mode.
//
// The mode is one-shot: the harness runs the prompt and exits, so the
// test reads the stream to the end and then checks the exit code.
//
// The harness runs under a temporary home with a config that allows
// the shell commands: a one-shot run has nobody to approve a call, so
// the permission layer would block them, and the test is about the
// stream rather than about the gate.
func startJSONMode(t *testing.T, workdir string, args ...string) *jsonRun {
	t.Helper()

	key := apiKey(t)
	binary := buildHarness(t)

	full := append([]string{
		"--mode", "json",
		"-model", testModel,
		"-base-url", goBaseURL,
	}, args...)

	cmd := exec.Command(binary, full...)
	cmd.Dir = workdir
	cmd.Env = jsonEnv(t, key)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	run := &jsonRun{t: t, cmd: cmd, stdout: bufio.NewReader(stdout), stderr: stderr}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return run
}

// jsonEnv returns the environment for a JSON-mode subprocess.
//
// The home directory is a temporary one, so the run never touches the
// user's real config or sessions, and the config it finds there
// allows the shell commands the test's prompts ask for.
func jsonEnv(t *testing.T, key string) []string {
	t.Helper()

	home := t.TempDir()
	configDir := filepath.Join(home, ".harness")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The permission rules live in the config file: the user's own
	// rules come before the built-in defaults, so an allow rule here
	// wins over the default "ask" for shell commands.
	//
	// The default alone would not do it: the built-in rules match
	// first, and the shell rule asks rather than falling through to
	// the default.
	config := `{
  "permissions": {
    "rules": [
      {
        "name": "the test's own commands",
        "tools": ["bash"],
        "action": "allow",
        "reason": "the test asks for these commands by name"
      }
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return append(os.Environ(),
		"HOME="+home,
		"OPENAI_API_KEY="+key,
		"OPENCODE_API_KEY="+key,
		"HARNESS_SESSION_DIR="+filepath.Join(t.TempDir(), "sessions"),
		"HARNESS_PROVIDER=openai",
	)
}

// records reads the whole stream.
func (r *jsonRun) records() []map[string]any {
	r.t.Helper()
	var parsed []map[string]any
	for {
		line, err := r.stdout.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			var record map[string]any
			if jsonErr := json.Unmarshal([]byte(line), &record); jsonErr != nil {
				r.t.Fatalf("the record %q is not JSON: %v", line, jsonErr)
			}
			parsed = append(parsed, record)
		}
		if err != nil {
			break
		}
	}
	return parsed
}

// wait runs the harness and waits for it to exit.
func (r *jsonRun) wait() {
	r.t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			r.t.Fatalf("the harness failed: %v\n%s", err, r.stderr.String())
		}
	case <-time.After(180 * time.Second):
		r.t.Fatalf("the harness did not exit\n%s", r.stderr.String())
	}
}

// find returns the records of one type.
func find(records []map[string]any, eventType string) []map[string]any {
	var out []map[string]any
	for _, record := range records {
		if record["type"] == eventType {
			out = append(out, record)
		}
	}
	return out
}

// TestLiveJSONModeStreamsTheRun checks the whole stream against the
// real model.
//
// The stream is the machine-readable half of the print mode: the
// session header first, then every event, then the settled record.
func TestLiveJSONModeStreamsTheRun(t *testing.T) {
	run := startJSONMode(t, t.TempDir(), "Reply with exactly: JSON_MODE_OK")
	records := run.records()
	run.wait()

	if len(records) == 0 {
		t.Fatalf("the stream is empty\n%s", run.stderr.String())
	}

	// The header comes first.
	header := records[0]
	if header["type"] != "session" {
		t.Errorf("the first record is %v", header)
	}
	if header["id"] == "" {
		t.Errorf("the header has no session id: %v", header)
	}

	// The run's events are all there.
	for _, eventType := range []string{"agent_start", "turn_start", "request_start", "message_start", "message_end", "agent_end", "agent_settled"} {
		if len(find(records, eventType)) == 0 {
			t.Errorf("the stream has no %s event: %v", eventType, records)
		}
	}

	// The answer is in the message_end event.
	messages := find(records, "message_end")
	last := messages[len(messages)-1]
	message, _ := last["message"].(map[string]any)
	content, _ := message["content"].(string)
	if !strings.Contains(content, "JSON_MODE_OK") {
		t.Errorf("the answer is %q", content)
	}

	// The usage is reported.
	ends := find(records, "agent_end")
	usage, _ := ends[len(ends)-1]["usage"].(map[string]any)
	if usage["inputTokens"].(float64) == 0 {
		t.Errorf("the usage is %v", usage)
	}
}

// TestLiveJSONModeDeltasBuildTheAnswer checks the streaming.
//
// The deltas are the answer as it arrives: a consumer that shows the
// run live concatenates them.
func TestLiveJSONModeDeltasBuildTheAnswer(t *testing.T) {
	run := startJSONMode(t, t.TempDir(), "Reply with exactly: JSON_DELTA_OK")
	records := run.records()
	run.wait()

	var streamed string
	for _, record := range find(records, "message_update") {
		if record["thinking"] == true {
			continue
		}
		text, _ := record["text"].(string)
		streamed += text
	}
	if !strings.Contains(streamed, "JSON_DELTA_OK") {
		t.Errorf("the streamed text is %q", streamed)
	}
}

// TestLiveJSONModeReportsToolCalls checks that the tool calls are on
// the stream.
//
// The run's tools are part of the story: a consumer that renders the
// run needs to know what ran.
func TestLiveJSONModeReportsToolCalls(t *testing.T) {
	run := startJSONMode(t, t.TempDir(),
		"Run this command with the bash tool: echo JSON_TOOL_OK")
	records := run.records()
	run.wait()

	starts := find(records, "tool_execution_start")
	if len(starts) == 0 {
		t.Fatalf("the stream has no tool events: %v", records)
	}
	call, _ := starts[0]["toolCall"].(map[string]any)
	if call["name"] == "" {
		t.Errorf("the tool call has no name: %v", call)
	}

	ends := find(records, "tool_execution_end")
	result, _ := ends[len(ends)-1]["result"].(map[string]any)
	if !strings.Contains(result["content"].(string), "JSON_TOOL_OK") {
		t.Errorf("the tool result is %v", result)
	}
}

// TestLiveJSONModeNeedsAPrompt checks that the mode refuses to run
// without a prompt.
func TestLiveJSONModeNeedsAPrompt(t *testing.T) {
	run := startJSONMode(t, t.TempDir())
	run.records()
	err := run.cmd.Wait()
	if err == nil {
		t.Fatal("the mode ran without a prompt")
	}
}

// TestLiveJSONModeRejectsABadMode checks the flag's validation.
//
// A typo in the mode must fail loudly rather than fall back to the
// text output: a script that asked for JSON and got prose would
// break in a confusing way.
func TestLiveJSONModeRejectsABadMode(t *testing.T) {
	key := apiKey(t)
	binary := buildHarness(t)

	cmd := exec.Command(binary, "--mode", "nonsense", "hello")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"OPENAI_API_KEY="+key,
		"OPENCODE_API_KEY="+key,
		"HARNESS_SESSION_DIR="+filepath.Join(t.TempDir(), "sessions"),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("the mode accepted an invalid value")
	}
	if !strings.Contains(stderr.String(), "invalid mode") {
		t.Errorf("the error is %q", stderr.String())
	}
}
