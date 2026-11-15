//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/rpc"
)

// rpcProcess is the harness running in RPC mode.
//
// It is the whole point of the mode: a program that speaks JSON can
// drive the harness without knowing anything about the REPL.
type rpcProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	mu     sync.Mutex
	// records holds every record read so far, and the two cursors
	// say how far the two kinds of lookup have looked.
	//
	// The records are kept because the order between a response and
	// the run's events is not fixed: the settled event can arrive
	// before the response that ended the run. A client that throws
	// away what it read early loses it, and then waits for a second
	// copy that never comes.
	records   []map[string]any
	responses int
	settles   int
}

// startRPC starts the harness in RPC mode.
//
// The binary is built first: the test drives the real command, not an
// in-process server, so the subprocess boundary is part of what is
// being tested.
func startRPC(t *testing.T, workdir string, extraArgs ...string) *rpcProcess {
	return startRPCWithEnv(t, workdir, nil, extraArgs...)
}

// startRPCWithEnv starts the harness in RPC mode with extra environment
// entries, used by the tests that need the project trusted: the mode is
// non-interactive, so an untrusted project's resources do not load.
func startRPCWithEnv(t *testing.T, workdir string, extraEnv []string, extraArgs ...string) *rpcProcess {
	t.Helper()

	key := apiKey(t)
	binary := buildHarness(t)

	args := append([]string{
		"-rpc",
		"-model", testModel,
		"-base-url", goBaseURL,
	}, extraArgs...)

	cmd := exec.Command(binary, args...)
	cmd.Dir = workdir
	cmd.Env = append(rpcEnv(t, key), extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	// The diagnostics go to stderr: the protocol owns stdout.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	process := &rpcProcess{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return process
}

// rpcEnv returns the environment for a harness subprocess.
//
// The home and the session directories are redirected to temporary
// ones, so a test run never reads the user's own config or writes into
// their sessions. The permissions are disabled: the harness has no user
// to ask, and a blocked call would only make the test flaky.
func rpcEnv(t *testing.T, key string) []string {
	t.Helper()
	return append(os.Environ(),
		"HOME="+t.TempDir(),
		"OPENAI_API_KEY="+key,
		"OPENCODE_API_KEY="+key,
		"HARNESS_SESSION_DIR="+filepath.Join(t.TempDir(), "sessions"),
		"HARNESS_PROVIDER=openai",
	)
}

// buildHarness builds the harness binary and returns its path.
func buildHarness(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "harness")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/harness")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the harness: %v\n%s", err, out)
	}
	return binary
}

// repoRoot returns the repository's root directory.
//
// The e2e tests run in the e2e directory, so the root is its parent.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(dir)
}

// send writes a command.
func (p *rpcProcess) send(command any) {
	p.t.Helper()
	if err := rpc.WriteCommand(p.stdin, command); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

// next reads the next record.
//
// The record is kept, so a later lookup can still find it: the order
// between a response and the run's events is not fixed.
//
// The wait has a deadline: a harness that goes quiet must fail the
// test rather than hang it. The read runs in a goroutine of its own,
// because a pipe read cannot be interrupted — the deadline is
// enforced by giving up on the result.
func (p *rpcProcess) next() map[string]any {
	p.t.Helper()

	type result struct {
		line string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		line, err := p.reader.ReadString('\n')
		results <- result{line, err}
	}()

	select {
	case r := <-results:
		if r.err != nil && r.line == "" {
			p.t.Fatalf("read: %v", r.err)
		}
		line := strings.TrimSpace(r.line)
		if line == "" {
			return p.next()
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			p.t.Fatalf("the record %q is not JSON: %v", line, err)
		}
		p.mu.Lock()
		p.records = append(p.records, record)
		p.mu.Unlock()
		return record
	case <-time.After(120 * time.Second):
		p.t.Fatal("no record arrived within 120s")
		return nil
	}
}

// take returns the first record that matches, reading more when the
// ones already read do not match.
//
// The records that do not match are kept: they were read ahead of the
// one being looked for, and a later lookup may well be the one they
// are.
//
// Each kind of lookup has its own cursor, so looking for a response
// does not consume the settled event that a later lookup wants.
func (p *rpcProcess) take(cursor *int, matches func(map[string]any) bool) map[string]any {
	p.t.Helper()
	for {
		p.mu.Lock()
		for *cursor < len(p.records) {
			record := p.records[*cursor]
			*cursor++
			if matches(record) {
				p.mu.Unlock()
				return record
			}
		}
		p.mu.Unlock()
		p.next()
	}
}

// response reads records until the response to a command arrives.
//
// The protocol says clients correlate by ID, not by order: the run's
// events share the stream with the responses, so their order is not
// fixed.
func (p *rpcProcess) response(command string) map[string]any {
	p.t.Helper()
	return p.take(&p.responses, func(r map[string]any) bool {
		return r["type"] == "response" && r["command"] == command
	})
}

// settled reads records until the run settles.
func (p *rpcProcess) settled() map[string]any {
	p.t.Helper()
	return p.take(&p.settles, func(r map[string]any) bool {
		return r["type"] == rpc.EventAgentSettled
	})
}

// answer reads records until the run settles and returns the last
// assistant message.
//
// The last one is the answer: a run may hold several assistant
// messages, and the earlier ones are the model's "let me check"
// remarks before it acts.
func (p *rpcProcess) answer() string {
	p.t.Helper()
	answer, _ := p.answerAndSettle()
	return answer
}

// answerAndSettle reads records until the run settles and returns the
// last assistant message along with the settled event.
//
// It looks for the settled event with the settled cursor, so a later
// settled() call does not find the event already taken.
func (p *rpcProcess) answerAndSettle() (string, map[string]any) {
	p.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var answer string
	for {
		record := p.next()
		switch record["type"] {
		case rpc.EventMessageEnd:
			data, _ := record["data"].(map[string]any)
			message, _ := data["message"].(map[string]any)
			if content, _ := message["content"].(string); content != "" {
				answer = content
			}
		case rpc.EventAgentSettled:
			p.mu.Lock()
			p.settles = len(p.records)
			p.mu.Unlock()
			return answer, record
		}
		if time.Now().After(deadline) {
			p.t.Fatal("the answer did not arrive")
		}
	}
}

// close ends the harness's side of the pipe.
func (p *rpcProcess) close() {
	_ = p.stdin.Close()
}

// TestLiveRPCPromptProducesTheAnswer checks the whole RPC loop against
// the real model.
//
// The harness runs as a subprocess, the client sends a prompt over a
// pipe, and the answer comes back in the event stream. This is the
// path an IDE or a script would take.
func TestLiveRPCPromptProducesTheAnswer(t *testing.T) {
	process := startRPC(t, t.TempDir())
	defer process.close()

	process.send(map[string]any{
		"id":      "r1",
		"type":    rpc.CmdPrompt,
		"message": "Reply with exactly: RPC_OK",
	})

	response := process.response(rpc.CmdPrompt)
	if response["success"] != true {
		t.Fatalf("the prompt was rejected: %v", response)
	}

	answer, settled := process.answerAndSettle()
	if !strings.Contains(answer, "RPC_OK") {
		t.Errorf("the answer is %q", answer)
	}

	// The run settles afterwards, and it was not aborted.
	data, _ := settled["data"].(map[string]any)
	if data["aborted"] != false {
		t.Errorf("the run was aborted: %v", settled)
	}
}

// TestLiveRPCStateAndMessages checks the inspection commands against
// the real model.
//
// The state reports the model and the session, and the history holds
// the conversation.
func TestLiveRPCStateAndMessages(t *testing.T) {
	process := startRPC(t, t.TempDir())
	defer process.close()

	process.send(map[string]any{"id": "r1", "type": rpc.CmdGetState})
	state := process.response(rpc.CmdGetState)
	if state["success"] != true {
		t.Fatalf("the state command failed: %v", state)
	}
	data, _ := state["data"].(map[string]any)
	if data["model"] != testModel {
		t.Errorf("the model is %v", data["model"])
	}
	if data["sessionId"] == "" {
		t.Errorf("the state has no session id: %v", data)
	}

	process.send(map[string]any{"id": "r2", "type": rpc.CmdPrompt, "message": "Reply with exactly: RPC_STATE"})
	if response := process.response(rpc.CmdPrompt); response["success"] != true {
		t.Fatalf("the prompt was rejected: %v", response)
	}
	process.settled()

	process.send(map[string]any{"id": "r3", "type": rpc.CmdGetMessages})
	messages := process.response(rpc.CmdGetMessages)
	if messages["success"] != true {
		t.Fatalf("the messages command failed: %v", messages)
	}
	data, _ = messages["data"].(map[string]any)
	list, _ := data["messages"].([]any)
	if len(list) < 2 {
		t.Fatalf("the messages are %v", list)
	}
}

// TestLiveRPCBashCommand checks the bash command.
//
// The command runs through the same tool the model uses, so the
// sandbox and the permission layer apply to it.
func TestLiveRPCBashCommand(t *testing.T) {
	process := startRPC(t, t.TempDir())
	defer process.close()

	process.send(map[string]any{
		"id":      "r1",
		"type":    rpc.CmdBash,
		"command": "echo RPC_BASH_OK",
	})
	response := process.response(rpc.CmdBash)
	if response["success"] != true {
		t.Fatalf("the bash command failed: %v", response)
	}
	data, _ := response["data"].(map[string]any)
	output, _ := data["output"].(string)
	if !strings.Contains(output, "RPC_BASH_OK") {
		t.Errorf("the output is %q", output)
	}
}

// TestLiveRPCBashReportsTheExitCode checks that a failing command
// reports its status.
//
// The exit code is what a client needs: the command ran, and here is
// how it ended.
func TestLiveRPCBashReportsTheExitCode(t *testing.T) {
	process := startRPC(t, t.TempDir())
	defer process.close()

	process.send(map[string]any{
		"id":      "r1",
		"type":    rpc.CmdBash,
		"command": "exit 3",
	})
	response := process.response(rpc.CmdBash)
	if response["success"] != true {
		t.Fatalf("the command was rejected: %v", response)
	}
	data, _ := response["data"].(map[string]any)
	if data["exitCode"].(float64) != 3 {
		t.Errorf("the exit code is %v, want 3", data["exitCode"])
	}
	if data["isError"] != true {
		t.Errorf("the result is %v, want a failure", data)
	}
}

// TestLiveRPCUnknownCommand checks that an unknown command fails
// cleanly instead of killing the session.
func TestLiveRPCUnknownCommand(t *testing.T) {
	process := startRPC(t, t.TempDir())
	defer process.close()

	process.send(map[string]any{"id": "r1", "type": "no_such_command"})
	response := process.response("no_such_command")
	if response["success"] != false {
		t.Fatalf("the unknown command succeeded: %v", response)
	}
	if !strings.Contains(response["error"].(string), "Unknown command") {
		t.Errorf("the error is %v", response["error"])
	}

	// The session survives: the next command still works.
	process.send(map[string]any{"id": "r2", "type": rpc.CmdGetState})
	state := process.response(rpc.CmdGetState)
	if state["success"] != true {
		t.Errorf("the session did not survive: %v", state)
	}
}

// TestLiveRPCSkillsReachTheModel checks that the skills reach the model
// through the RPC mode too.
//
// The skill is advertised in the system prompt, and the model reads
// the file itself.
func TestLiveRPCSkillsReachTheModel(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".harness", "skills", "fruit-tasting")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `---
name: fruit-tasting
description: Describes the taste of a named fruit. Use when asked to describe how a fruit tastes.
---

When you are asked to describe a fruit's taste, reply with exactly: HARNESS_RPC_SKILL and nothing else.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The harness runs in the skill's directory, so it finds the
	// project's skills — once the project is trusted. The RPC mode is
	// non-interactive, so the trust decision has to be a saved one: the
	// subprocess is given a home whose trust file already says yes. The
	// key is the resolved path, because that is the form the harness
	// derives from its working directory.
	home := t.TempDir()
	trustDir := filepath.Join(home, ".harness")
	if err := os.MkdirAll(trustDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve the project directory: %v", err)
	}
	trust, err := json.Marshal(map[string]any{
		"projects": map[string]string{resolved: "yes"},
	})
	if err != nil {
		t.Fatalf("marshal the trust file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(trustDir, "trust.json"), trust, 0o644); err != nil {
		t.Fatalf("write the trust file: %v", err)
	}
	process := startRPCWithEnv(t, dir, []string{"HOME=" + home})
	defer process.close()

	process.send(map[string]any{
		"id":      "r1",
		"type":    rpc.CmdPrompt,
		"message": "Describe how a banana tastes.",
	})
	if response := process.response(rpc.CmdPrompt); response["success"] != true {
		t.Fatalf("the prompt was rejected: %v", response)
	}

	answer := process.answer()
	if !strings.Contains(answer, "HARNESS_RPC_SKILL") {
		t.Errorf("the answer is %q, want it to come from the skill", answer)
	}
}

// TestLiveRPCWaitsForTheRunAfterEOF checks the shutdown behaviour.
//
// A client that closes stdin right after sending a prompt expects the
// answer to still arrive: the harness waits for the run in progress.
func TestLiveRPCWaitsForTheRunAfterEOF(t *testing.T) {
	key := apiKey(t)
	binary := buildHarness(t)

	cmd := exec.Command(binary,
		"-rpc",
		"-model", testModel,
		"-base-url", goBaseURL,
	)
	cmd.Dir = t.TempDir()
	cmd.Env = rpcEnv(t, key)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// The prompt goes out, and stdin closes right after: the answer
	// must still come.
	command := `{"id":"r1","type":"prompt","message":"Reply with exactly: RPC_AFTER_EOF"}` + "\n"
	if _, err := io.WriteString(stdin, command); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = stdin.Close()

	reader := bufio.NewReader(stdout)
	var answer string
	deadline := time.Now().Add(120 * time.Second)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the record %q is not JSON: %v", line, err)
		}
		if record["type"] != rpc.EventMessageEnd {
			continue
		}
		data, _ := record["data"].(map[string]any)
		message, _ := data["message"].(map[string]any)
		if content, _ := message["content"].(string); content != "" {
			answer = content
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the answer did not arrive")
		}
	}
	if !strings.Contains(answer, "RPC_AFTER_EOF") {
		t.Errorf("the answer is %q", answer)
	}
}
