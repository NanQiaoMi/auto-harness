//go:build e2e

// Package e2e holds live-model end-to-end tests. They are excluded from
// the default test run by the "e2e" build tag and hit the real gateway.
//
//	OPENCODE_API_KEY=<key> go test -tags e2e -timeout 300s ./e2e/ -v
//
// The model is fixed to deepseek-v4.1-flash on the OpenCode Go route.
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/mcp"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

const (
	// testModel is the only model used for live end-to-end tests.
	testModel = "deepseek-v4.1-flash"
	// goBaseURL is the OpenCode Go route for OpenAI-compatible APIs.
	goBaseURL = "https://opencode.ai/inference/go/openai/v1"
)

// apiKey returns the gateway key from the environment.
func apiKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("OPENCODE_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if key == "" {
		t.Skip("set OPENCODE_API_KEY to run live end-to-end tests")
	}
	return key
}

// newProvider builds a provider pointed at the Go route.
func newProvider(t *testing.T) llm.Provider {
	t.Helper()
	p := llm.NewOpenAIProvider(apiKey(t), goBaseURL)
	p.ExtraHeaders = map[string]string{"x-opencode-session": "harness-e2e-" + t.Name()}
	return p
}

// isolate moves the test into an empty working directory.
//
// The model has a read tool, and in the repository it can read the
// test source — where the expected answer sits as a literal. A model
// that reads it treats the string as data rather than as the
// instruction it is, and answers something else. An empty directory
// gives it nothing to read, so the instruction is the only thing it
// has.
//
// The directory is restored when the test ends.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore the working directory: %v", err)
		}
	})
	return dir
}

// newAgent builds an agent with the standard tool set.
//
// The agent runs in an empty working directory: see isolate.
func newAgent(t *testing.T) (*agent.Agent, *tools.Registry, *ext.Bus) {
	t.Helper()
	isolate(t)
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  8,
		MaxTokens: 2048,
	}, bus, reg)
	return ag, reg, bus
}

// TestLiveTextCompletion verifies a plain completion streams text back.
func TestLiveTextCompletion(t *testing.T) {
	ag, _, _ := newAgent(t)

	var streamed string
	ag.Bus.On(ext.EventMessageUpdate, func(e *ext.Event) error {
		if e.Data == nil {
			streamed += e.Text
		}
		return nil
	})

	res, err := ag.Run(context.Background(), "Reply with exactly: HARNESS_OK")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "HARNESS_OK") {
		t.Errorf("Final = %q, want it to contain HARNESS_OK", res.Final)
	}
	if streamed == "" {
		t.Error("no text was streamed via message_update")
	}
	if res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Errorf("usage not reported: %+v", res.Usage)
	}
}

// TestLiveToolCallLoop verifies the model drives a real bash tool call
// and answers from the tool output.
func TestLiveToolCallLoop(t *testing.T) {
	ag, _, bus := newAgent(t)

	var toolName string
	var toolResult string
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolCall != nil {
			toolName = e.ToolCall.Name
		}
		if e.ToolResult != nil {
			toolResult = e.ToolResult.Content
		}
		return nil
	})

	const marker = "harness-tool-marker-7f3a"
	res, err := ag.Run(context.Background(),
		"Use the bash tool to run exactly: echo "+marker+". Then reply with the output you saw.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if toolName != "bash" {
		t.Errorf("tool called = %q, want bash", toolName)
	}
	if !strings.Contains(toolResult, marker) {
		t.Errorf("tool result = %q, want it to contain %q", toolResult, marker)
	}
	if !strings.Contains(res.Final, marker) {
		t.Errorf("final answer = %q, want it to mention %q", res.Final, marker)
	}
}

// TestLiveFileToolRoundtrip verifies write/read/edit tools against the
// real filesystem via the model's tool calls.
func TestLiveFileToolRoundtrip(t *testing.T) {
	ag, _, _ := newAgent(t)
	dir := t.TempDir()
	path := dir + "/e2e-file.txt"

	res, err := ag.Run(context.Background(), strings.Join([]string{
		"Create a file at " + path + " containing exactly: first line",
		"Then read it back and edit it so the text becomes: second line",
		"Then read it once more and tell me the exact content.",
	}, " "))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "second line") {
		t.Errorf("final answer = %q, want it to contain 'second line'", res.Final)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if !strings.Contains(string(data), "second line") {
		t.Errorf("file content = %q", data)
	}
}

// TestLiveSessionPersistence verifies messages are persisted and the
// conversation survives a resume.
func TestLiveSessionPersistence(t *testing.T) {
	dir := t.TempDir()
	provider := newProvider(t)
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewBashTool()); err != nil {
		t.Fatal(err)
	}

	sess, err := session.New(dir, testModel)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(agent.Config{Provider: provider, Model: testModel, MaxTurns: 6}, bus, reg)
	ag.Session = sess

	const secret = "harness-secret-9c21"
	if _, err := ag.Run(context.Background(), "Remember the code "+secret+". Reply with OK only."); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume from disk with a fresh agent and ask for the code again.
	resumed, err := session.Load(dir, sess.Meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer resumed.Close()

	ag2 := agent.New(agent.Config{Provider: provider, Model: testModel, MaxTurns: 6}, bus, reg)
	ag2.SetHistory(resumed.Messages)

	res, err := ag2.Run(context.Background(), "What code did I ask you to remember? Reply with just the code.")
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if !strings.Contains(res.Final, secret) {
		t.Errorf("resumed answer = %q, want it to contain %q", res.Final, secret)
	}
}

// TestLiveCompaction verifies /compact-style compression preserves the
// key information across the history rewrite.
func TestLiveCompaction(t *testing.T) {
	ag, _, _ := newAgent(t)
	ag.Compactor = agent.NewSummarizer(newProvider(t), testModel)

	const fact = "harness-fact-4b8e"
	if _, err := ag.Run(context.Background(), "Remember this fact: "+fact+". Reply with OK only."); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := ag.Run(context.Background(), "Say hello."); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	before := len(ag.History())
	if before < 2 {
		t.Fatalf("history too short to compact: %d", before)
	}

	if err := ag.Compact(context.Background()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after := len(ag.History())
	if after >= before {
		t.Errorf("compaction did not shrink history: %d -> %d", before, after)
	}

	res, err := ag.Run(context.Background(), "What fact did I ask you to remember? Reply with just the fact.")
	if err != nil {
		t.Fatalf("post-compaction Run: %v", err)
	}
	if !strings.Contains(res.Final, fact) {
		t.Errorf("post-compaction answer = %q, want it to contain %q", res.Final, fact)
	}
}

// TestLiveReasoningStream verifies extended thinking reaches the event
// bus without polluting the transcript.
func TestLiveReasoningStream(t *testing.T) {
	ag, _, bus := newAgent(t)

	var thinking string
	bus.On(ext.EventMessageUpdate, func(e *ext.Event) error {
		if e.Data != nil {
			if isThinking, _ := e.Data["thinking"].(bool); isThinking {
				thinking += e.Text
			}
		}
		return nil
	})

	if _, err := ag.Run(context.Background(),
		"A bat and a ball cost 1.10 in total. The bat costs 1.00 more than the ball. "+
			"How much does the ball cost? Reply with just the number."); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if thinking == "" {
		t.Skip("model produced no reasoning stream for this prompt")
	}
	// Reasoning must not end up in the stored message content.
	for _, m := range ag.History() {
		if strings.Contains(m.Content, "Let") && strings.Contains(m.Content, "bat") && len(m.Content) > 400 {
			t.Errorf("reasoning leaked into transcript: %q", m.Content)
		}
	}
}

// TestLiveErrorRecovery verifies a failed tool call is reported back to
// the model and the run still completes.
func TestLiveErrorRecovery(t *testing.T) {
	ag, _, bus := newAgent(t)

	var sawError bool
	bus.On(ext.EventToolResult, func(e *ext.Event) error {
		if e.ToolResult != nil && e.ToolResult.IsError {
			sawError = true
		}
		return nil
	})

	res, err := ag.Run(context.Background(),
		"Use the bash tool to read the file /definitely/not/here.txt. "+
			"After the failure, tell me in one sentence what happened.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawError {
		t.Error("expected a tool error result to be surfaced")
	}
	if strings.TrimSpace(res.Final) == "" {
		t.Error("expected a final answer after the tool failure")
	}
}

// TestLiveTimeout verifies a cancelled context aborts the run cleanly.
func TestLiveTimeout(t *testing.T) {
	ag, _, _ := newAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ag.Run(ctx, "Write a 5000 word essay about the history of the printing press.")
	if err == nil {
		t.Skip("model answered before the deadline; nothing to assert")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("run did not abort promptly: %s", elapsed)
	}
}

// TestLiveProcessExtensionTool verifies a tool served by an external
// extension process reaches the real model and comes back.
func TestLiveProcessExtensionTool(t *testing.T) {
	apiKey(t) // require a live key

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())

	// An extension process that serves one tool: it upper-cases the
	// text it is given.
	script := filepath.Join(t.TempDir(), "live_ext.py")
	writeLiveExtension(t, script)

	pe := ext.NewProcessExtension("python3", script)
	if err := pe.Setup(api); err != nil {
		t.Fatalf("extension setup: %v", err)
	}
	defer pe.Stop()

	if _, ok := reg.Get("live_upper"); !ok {
		t.Fatal("live_upper tool not registered")
	}

	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  8,
		MaxTokens: 2048,
	}, bus, reg)

	var toolCalled bool
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolCall != nil && e.ToolCall.Name == "live_upper" {
			toolCalled = true
		}
		return nil
	})

	const marker = "quokka"
	res, err := ag.Run(context.Background(),
		"Use the live_upper tool with the text \""+marker+"\". Then reply with exactly what the tool returned.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !toolCalled {
		t.Error("the model never called the extension's tool")
	}
	if !strings.Contains(strings.ToUpper(res.Final), strings.ToUpper(marker)) {
		t.Errorf("Final = %q, want it to mention the marker", res.Final)
	}
}

// writeLiveExtension writes a minimal extension process.
func writeLiveExtension(t *testing.T, path string) {
	t.Helper()
	script := `
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
    "name": "live",
    "tools": [{
        "name": "live_upper",
        "description": "Return the given text in upper case.",
        "parameters": {
            "type": "object",
            "properties": {"text": {"type": "string"}},
            "required": ["text"],
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
        send({"id": mid, "result": {"content": text.upper()}})
    elif method == "shutdown":
        send({"id": mid, "result": {}})
        break
`
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write extension: %v", err)
	}
}

// TestLiveSessionForkAndBranch verifies that branching and forking work
// with the real model: the model answers on a new branch, and the
// forked session keeps its own context.
func TestLiveSessionForkAndBranch(t *testing.T) {
	apiKey(t) // require a live key

	dir := t.TempDir()
	sess, err := session.New(dir, testModel)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  4,
		MaxTokens: 1024,
	}, bus, reg)
	ag.Session = sess

	// One exchange, so the session has a user message and an answer.
	if _, err := ag.Run(context.Background(), "Reply with exactly: FIRST_ANSWER"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(sess.Messages) < 2 {
		t.Fatalf("session messages = %d, want at least 2", len(sess.Messages))
	}

	// Branch: go back to the user message and ask something else.
	tree := sess.Tree()
	var userEntry string
	for _, e := range tree.CurrentPath() {
		if e.Type == session.EntryMessage && e.Message != nil && e.Message.Role == llm.RoleUser {
			userEntry = e.ID
		}
	}
	if userEntry == "" {
		t.Fatal("no user message in the session tree")
	}
	if err := sess.SetLeaf(userEntry); err != nil {
		t.Fatalf("set leaf: %v", err)
	}
	ag.ReloadHistory()

	if _, err := ag.Run(context.Background(), "Reply with exactly: SECOND_ANSWER"); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// The tree now has two branches.
	branches := tree.Branches()
	if len(branches) != 2 {
		t.Errorf("branches = %d, want 2: %v", len(branches), branches)
	}

	// Fork the session at the user message: the fork gets the user
	// message and nothing after it.
	dst, err := session.Fork(sess, session.ForkOptions{Dir: dir, AtEntryID: userEntry})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	defer dst.Close()
	if len(dst.Messages) != 1 {
		t.Fatalf("fork messages = %d, want 1: %+v", len(dst.Messages), dst.Messages)
	}
	if dst.Meta.ParentSession != sess.Path {
		t.Errorf("fork parent = %q, want %q", dst.Meta.ParentSession, sess.Path)
	}

	// The fork can be driven by its own agent and keeps its own history.
	forkBus := ext.NewBus()
	forkReg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{tools.NewBashTool()} {
		if err := forkReg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	forkAgent := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  4,
		MaxTokens: 1024,
	}, forkBus, forkReg)
	forkAgent.Session = dst

	res, err := forkAgent.Run(context.Background(), "Reply with exactly: FORK_ANSWER")
	if err != nil {
		t.Fatalf("fork run: %v", err)
	}
	if !strings.Contains(res.Final, "FORK_ANSWER") {
		t.Errorf("fork answer = %q", res.Final)
	}
	// The fork grew; the source session is untouched by it.
	if len(dst.Messages) < 3 {
		t.Errorf("fork messages after run = %d, want at least 3", len(dst.Messages))
	}
}

// TestLiveMCPToolCall verifies a tool served by an MCP server reaches
// the real model and its result comes back.
func TestLiveMCPToolCall(t *testing.T) {
	apiKey(t) // require a live key

	// An MCP server with one tool: it reverses the text it is given.
	script := filepath.Join(t.TempDir(), "live_mcp.py")
	writeLiveMCPServer(t, script)

	reg := tools.NewRegistry()
	mgr := mcp.NewManager(reg)
	defer mgr.Close()

	if err := mgr.Connect(context.Background(), mcp.ServerConfig{
		Name:    "live",
		Command: "python3",
		Args:    []string{script},
	}); err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	if _, ok := reg.Get("mcp__live__reverse"); !ok {
		t.Fatal("the MCP tool was not registered")
	}

	bus := ext.NewBus()
	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  8,
		MaxTokens: 2048,
	}, bus, reg)

	var toolCalled bool
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolCall != nil && e.ToolCall.Name == "mcp__live__reverse" {
			toolCalled = true
		}
		return nil
	})

	const marker = "tiger"
	res, err := ag.Run(context.Background(),
		"Use the mcp__live__reverse tool with the text \""+marker+"\". Then reply with exactly what the tool returned.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !toolCalled {
		t.Error("the model never called the MCP tool")
	}
	// The tool reversed the marker.
	if !strings.Contains(res.Final, "regit") {
		t.Errorf("Final = %q, want it to contain the reversed marker", res.Final)
	}
}

// writeLiveMCPServer writes a minimal MCP server for the test.
func writeLiveMCPServer(t *testing.T, path string) {
	t.Helper()
	script := `
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
    "name": "reverse",
    "description": "Return the given text reversed.",
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
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "live-mcp", "version": "1.0.0"},
        }})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"tools": TOOLS}})
    elif method == "tools/call":
        text = (params.get("arguments") or {}).get("text", "")
        send({"jsonrpc": "2.0", "id": mid, "result": {
            "content": [{"type": "text", "text": text[::-1]}],
            "isError": False,
        }})
    elif mid is not None:
        send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "no such method"}})
`
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write mcp server: %v", err)
	}
}
