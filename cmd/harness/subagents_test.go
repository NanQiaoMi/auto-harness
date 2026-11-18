package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/orchestrator"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

type subagentScriptStream struct {
	events []llm.StreamEvent
	next   int
}

func (s *subagentScriptStream) Recv() (llm.StreamEvent, error) {
	if s.next >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}

func (s *subagentScriptStream) Close() error { return nil }

type subagentScriptProvider struct{}

func (p *subagentScriptProvider) Name() string { return "mock" }

func (p *subagentScriptProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.Stream, error) {
	for _, message := range req.Messages {
		if message.Role == llm.RoleTool {
			message := llm.Message{Role: llm.RoleAssistant, Content: "child complete"}
			return &subagentScriptStream{events: []llm.StreamEvent{
				{Type: llm.EventTextDelta, Text: "child complete"},
				{Type: llm.EventDone, Message: &message, StopReason: "end_turn"},
			}}, nil
		}
	}
	call := llm.ToolCall{
		ID:        "write-child",
		Name:      "write",
		Arguments: json.RawMessage(`{"path":"child.txt","content":"child\n"}`),
	}
	message := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}
	return &subagentScriptStream{events: []llm.StreamEvent{
		{Type: llm.EventToolCall, ToolCall: &call},
		{Type: llm.EventDone, Message: &message, StopReason: "tool_calls"},
	}}, nil
}

func TestSubagentFactoryCreatesIsolatedWorktreeAndSession(t *testing.T) {
	repo := newGitCommandRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, repo, "add", "tracked.txt")
	runGitCommand(t, repo, "commit", "-m", "base")

	sessionDir := t.TempDir()
	parentSession, err := session.New(sessionDir, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer parentSession.Close()

	bus := ext.NewBus()
	registry := tools.NewRegistry()
	parentCheckpoint := gitkit.NewCheckpointManager(repo)
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(),
		tools.NewReadTool(),
		tools.NewWriteTool(),
		tools.NewEditTool(),
		gitkit.NewGitTool(repo),
		gitkit.NewCheckpointTool(parentCheckpoint),
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	parent := agent.New(agent.Config{
		Model:        "test-model",
		SystemPrompt: "parent prompt",
	}, bus, registry)
	parent.Session = parentSession
	parent.SetHistory(parentSession.Messages)

	storeDir := filepath.Join(sessionDir, "subagents", parentSession.Meta.ID)
	runtime := &subagentRuntime{
		parent:             parent,
		cfg:                &config.Config{Model: "test-model", SessionDir: sessionDir},
		bus:                bus,
		registry:           registry,
		llmReg:             llm.NewRegistry(),
		checkpointLimit:    10,
		checkpointsEnabled: true,
		maxDepth:           3,
		parentWorkdir:      repo,
		storeDir:           storeDir,
		sessionDir:         sessionDir,
		quiet:              true,
	}
	runtime.parentSessionPath.set(parentSession.Path)

	task := orchestrator.Task{
		ID:        "sub-worktree-test",
		Name:      "isolated",
		Isolation: orchestrator.IsolationAuto,
	}
	prepared, err := runtime.factory(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Worker == nil || prepared.SessionPath == "" || prepared.Workspace == "" {
		t.Fatalf("prepared = %+v", prepared)
	}
	if prepared.Isolation != orchestrator.IsolationWorktree {
		t.Fatalf("isolation = %q", prepared.Isolation)
	}
	if prepared.Workspace == repo || !filepath.IsAbs(prepared.Workspace) {
		t.Fatalf("workspace = %q", prepared.Workspace)
	}
	if _, err := os.Stat(prepared.Workspace); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(prepared.Workspace, "tracked.txt")); err != nil ||
		string(data) != "base\n" {
		t.Fatalf("worktree content = %q err=%v", data, err)
	}
	childID := strings.TrimSuffix(filepath.Base(prepared.SessionPath), ".jsonl")
	child, err := session.Load(sessionDir, childID)
	if err == nil {
		_ = child.Close()
	} else {
		// The path is available directly; Load verifies the same file
		// through the normal session reader.
		t.Fatalf("load child session: %v", err)
	}

	if err := prepared.Worker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gitkit.RemoveWorktree(context.Background(), repo, prepared.Workspace, true); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentWorkerRunsToolsAgainstItsOwnWorkspace(t *testing.T) {
	repo := newGitCommandRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, repo, "add", "tracked.txt")
	runGitCommand(t, repo, "commit", "-m", "base")

	sessionDir := t.TempDir()
	parentSession, err := session.New(sessionDir, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer parentSession.Close()

	bus := ext.NewBus()
	registry := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(),
		tools.NewReadTool(),
		tools.NewWriteTool(),
		tools.NewEditTool(),
		gitkit.NewGitTool(repo),
		gitkit.NewCheckpointTool(gitkit.NewCheckpointManager(repo)),
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	parent := agent.New(agent.Config{
		Model:        "test-model",
		SystemPrompt: "parent prompt",
	}, bus, registry)
	parent.Session = parentSession
	parent.SetHistory(parentSession.Messages)

	llmReg := llm.NewRegistry()
	llmReg.SetFactory(func(llm.ProviderConfig) (llm.Provider, error) {
		return &subagentScriptProvider{}, nil
	})
	if err := llmReg.RegisterProvider(llm.ProviderConfig{
		Name: "mock", BaseURL: "mock://", Protocol: "openai",
	}); err != nil {
		t.Fatal(err)
	}
	llmReg.RegisterModel(llm.Model{
		ID: "test-model", Provider: "mock", Protocol: "openai",
		ContextWindow: 100000, MaxOutput: 8192,
	})

	runtime := &subagentRuntime{
		parent:             parent,
		cfg:                &config.Config{Model: "test-model", SessionDir: sessionDir},
		bus:                bus,
		registry:           registry,
		llmReg:             llmReg,
		checkpointLimit:    10,
		checkpointsEnabled: true,
		maxDepth:           3,
		parentWorkdir:      repo,
		storeDir:           filepath.Join(sessionDir, "subagents", parentSession.Meta.ID),
		sessionDir:         sessionDir,
		quiet:              true,
	}
	runtime.parentSessionPath.set(parentSession.Path)

	prepared, err := runtime.factory(context.Background(), orchestrator.Task{
		ID:        "sub-worker-test",
		Name:      "worker",
		Isolation: orchestrator.IsolationAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = prepared.Worker.Close()
		_ = gitkit.RemoveWorktree(context.Background(), repo, prepared.Workspace, true)
	}()

	outcome, err := prepared.Worker.Run(context.Background(), "write the child file")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outcome.Summary, "child complete") {
		t.Fatalf("summary = %q", outcome.Summary)
	}
	if data, err := os.ReadFile(filepath.Join(prepared.Workspace, "child.txt")); err != nil ||
		string(data) != "child\n" {
		t.Fatalf("child workspace file = %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "child.txt")); !os.IsNotExist(err) {
		t.Fatalf("child tool escaped the worktree: %v", err)
	}
}
