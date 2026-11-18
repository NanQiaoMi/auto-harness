package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/tools"
)

type testWorker struct {
	run func(context.Context, string) (Outcome, error)

	mu      sync.Mutex
	prompts []string
	closed  bool
	closes  int
}

func (w *testWorker) Run(ctx context.Context, prompt string) (Outcome, error) {
	w.mu.Lock()
	w.prompts = append(w.prompts, prompt)
	w.mu.Unlock()
	return w.run(ctx, prompt)
}

func (w *testWorker) Close() error {
	w.mu.Lock()
	w.closed = true
	w.closes++
	w.mu.Unlock()
	return nil
}

func (w *testWorker) closeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closes
}

func (w *testWorker) promptList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.prompts...)
}

func TestSpawnWaitAndFollowUp(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	worker := &testWorker{run: func(ctx context.Context, prompt string) (Outcome, error) {
		started <- prompt
		if prompt == "first" {
			select {
			case <-release:
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
		}
		return Outcome{Summary: "finished " + prompt, Turns: 2}, nil
	}}
	manager := newTestManager(t, Config{
		Factory: func(ctx context.Context, task Task) (Prepared, error) {
			return Prepared{
				Worker:      worker,
				SessionID:   "session-" + task.ID,
				SessionPath: "/tmp/session-" + task.ID,
				Workspace:   "/tmp/workspace-" + task.ID,
				Isolation:   IsolationWorktree,
			}, nil
		},
	})
	defer manager.Close()

	task, err := manager.Spawn(context.Background(), SpawnRequest{
		Prompt: "first",
		Name:   "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateQueued || task.Depth != 0 {
		t.Fatalf("spawned task = %+v", task)
	}
	waitPrompt(t, started, "first")
	if err := manager.Send(context.Background(), task.ID, "second"); err != nil {
		t.Fatal(err)
	}
	close(release)

	final, done, err := manager.Wait(context.Background(), task.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !done || final.State != StateCompleted {
		t.Fatalf("final = %+v, done=%v", final, done)
	}
	if final.Summary != "finished second" || final.Runs != 2 || final.Turns != 4 {
		t.Fatalf("final counters = %+v", final)
	}
	if final.SessionID != "session-"+task.ID || final.Workspace == "" {
		t.Fatalf("prepared metadata = %+v", final)
	}
	if got := worker.promptList(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("prompts = %#v", got)
	}
}

func TestCancelStopsTaskAndDescendants(t *testing.T) {
	started := make(chan string, 4)
	worker := &testWorker{run: func(ctx context.Context, prompt string) (Outcome, error) {
		started <- prompt
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}}
	manager := newTestManager(t, Config{
		MaxDepth: 1,
		Factory: func(ctx context.Context, task Task) (Prepared, error) {
			return Prepared{Worker: worker, SessionID: task.ID}, nil
		},
	})
	defer manager.Close()

	root, err := manager.Spawn(context.Background(), SpawnRequest{Prompt: "root"})
	if err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, started, "root")

	childCtx := WithTaskID(context.Background(), root.ID)
	child, err := manager.Spawn(childCtx, SpawnRequest{Prompt: "child"})
	if err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, started, "child")
	if child.ParentID != root.ID || child.Depth != 1 {
		t.Fatalf("child = %+v", child)
	}
	if manager.CanAccess(child.ID, root.ID) {
		t.Fatal("a child must not be able to inspect its parent")
	}
	if !manager.CanAccess(root.ID, child.ID) {
		t.Fatal("a parent must be able to inspect its child")
	}
	if _, err := manager.Spawn(WithTaskID(context.Background(), child.ID),
		SpawnRequest{Prompt: "grandchild"}); err == nil {
		t.Fatal("delegation beyond MaxDepth succeeded")
	}

	if err := manager.Cancel(context.Background(), root.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root.ID, child.ID} {
		task, done, err := manager.Wait(context.Background(), id, time.Second)
		if err != nil || !done || task.State != StateCancelled {
			t.Fatalf("cancelled %s = %+v done=%v err=%v", id, task, done, err)
		}
	}
}

func TestFailedParentCancelsDescendants(t *testing.T) {
	rootStarted := make(chan struct{}, 1)
	childStarted := make(chan struct{}, 1)
	releaseFailure := make(chan struct{})
	rootWorker := &testWorker{run: func(ctx context.Context, _ string) (Outcome, error) {
		rootStarted <- struct{}{}
		select {
		case <-releaseFailure:
			return Outcome{}, errors.New("root provider failed")
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}}
	childWorker := &testWorker{run: func(ctx context.Context, _ string) (Outcome, error) {
		childStarted <- struct{}{}
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}}
	manager := newTestManager(t, Config{
		MaxDepth: 1,
		Factory: func(_ context.Context, task Task) (Prepared, error) {
			if task.ParentID == "" {
				return Prepared{Worker: rootWorker}, nil
			}
			return Prepared{Worker: childWorker}, nil
		},
	})
	defer manager.Close()

	root, err := manager.Spawn(context.Background(), SpawnRequest{Prompt: "root"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-rootStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("root did not start")
	}
	child, err := manager.Spawn(WithTaskID(context.Background(), root.ID),
		SpawnRequest{Prompt: "child"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-childStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	close(releaseFailure)

	failed, done, err := manager.Wait(context.Background(), root.ID, 5*time.Second)
	if err != nil || !done || failed.State != StateFailed {
		t.Fatalf("root = %+v done=%v err=%v", failed, done, err)
	}
	cancelled, done, err := manager.Wait(context.Background(), child.ID, 5*time.Second)
	if err != nil || !done || cancelled.State != StateCancelled {
		t.Fatalf("child = %+v done=%v err=%v", cancelled, done, err)
	}
}

func TestTaskPersistenceAndRestartRecovery(t *testing.T) {
	store := t.TempDir()
	manager := newTestManager(t, Config{
		StoreDir: store,
		Factory: func(ctx context.Context, task Task) (Prepared, error) {
			return Prepared{
				Worker: &testWorker{run: func(context.Context, string) (Outcome, error) {
					return Outcome{Summary: "persisted", Turns: 1}, nil
				}},
				SessionID: "session-" + task.ID,
			}, nil
		},
	})
	task, err := manager.Spawn(context.Background(), SpawnRequest{Prompt: "save"})
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := manager.Wait(context.Background(), task.ID, 5*time.Second); err != nil || !done {
		t.Fatalf("wait: done=%v err=%v", done, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(context.Background(), Config{StoreDir: store})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	got, ok := reloaded.Get(task.ID)
	if !ok || got.State != StateCompleted || got.Summary != "persisted" {
		t.Fatalf("reloaded task = %+v, ok=%v", got, ok)
	}
	if err := reloaded.Send(context.Background(), task.ID, "continue"); err == nil ||
		!strings.Contains(err.Error(), "restored from disk") {
		t.Fatalf("Send after restart = %v, want a history-only error", err)
	}

	crashed := Task{
		ID:        "sub-crashed",
		Name:      "crashed",
		State:     StateRunning,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(crashed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "tasks", crashed.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(context.Background(), Config{StoreDir: store})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	got, ok = recovered.Get(crashed.ID)
	if !ok || got.State != StateInterrupted || !strings.Contains(got.Error, "restart") {
		t.Fatalf("recovered task = %+v, ok=%v", got, ok)
	}
}

func TestResultIsBounded(t *testing.T) {
	manager := newTestManager(t, Config{
		ResultLimit: 32,
		Factory: func(ctx context.Context, task Task) (Prepared, error) {
			return Prepared{Worker: &testWorker{run: func(context.Context, string) (Outcome, error) {
				return Outcome{Summary: strings.Repeat("界", 100)}, nil
			}}}, nil
		},
	})
	defer manager.Close()
	task, err := manager.Spawn(context.Background(), SpawnRequest{Prompt: "long"})
	if err != nil {
		t.Fatal(err)
	}
	final, done, err := manager.Wait(context.Background(), task.ID, 5*time.Second)
	if err != nil || !done {
		t.Fatalf("wait: done=%v err=%v", done, err)
	}
	if !final.SummaryCut || len(final.Summary) <= 32 {
		t.Fatalf("summary was not bounded correctly: cut=%v value=%q", final.SummaryCut, final.Summary)
	}
}

func TestAgentToolActions(t *testing.T) {
	manager := newTestManager(t, Config{
		Factory: func(ctx context.Context, task Task) (Prepared, error) {
			return Prepared{Worker: &testWorker{run: func(context.Context, string) (Outcome, error) {
				return Outcome{Summary: "tool result"}, nil
			}}}, nil
		},
	})
	defer manager.Close()
	tool := NewAgentTool(manager)

	spawn := executeAgentTool(t, tool, `{"action":"spawn","prompt":"PROMPT_SECRET","name":"work"}`)
	if spawn.IsError {
		t.Fatalf("spawn = %+v", spawn)
	}
	if strings.Contains(spawn.Content, "PROMPT_SECRET") {
		t.Fatal("agent tool echoed the full prompt to the parent")
	}
	var payload struct {
		Task Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(spawn.Content), &payload); err != nil {
		t.Fatal(err)
	}
	wait := executeAgentTool(t, tool, `{"action":"wait","id":"`+payload.Task.ID+`","timeout_ms":5000}`)
	if wait.IsError || !strings.Contains(wait.Content, `"state": "completed"`) {
		t.Fatalf("wait = %+v", wait)
	}
	list := executeAgentTool(t, tool, `{"action":"list"}`)
	if list.IsError || !strings.Contains(list.Content, payload.Task.ID) {
		t.Fatalf("list = %+v", list)
	}
}

func newTestManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	manager, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func waitPrompt(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("prompt = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for prompt %q", want)
	}
}

func executeAgentTool(t *testing.T, tool *tools.Tool, args string) tools.Result {
	t.Helper()
	result, err := tool.Execute(context.Background(), "call", json.RawMessage(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWaitTimeoutIsNotFailure(t *testing.T) {
	block := make(chan struct{})
	manager := newTestManager(t, Config{
		Factory: func(ctx context.Context, task Task) (Prepared, error) {
			return Prepared{Worker: &testWorker{run: func(ctx context.Context, _ string) (Outcome, error) {
				select {
				case <-block:
					return Outcome{Summary: "late"}, nil
				case <-ctx.Done():
					return Outcome{}, ctx.Err()
				}
			}}}, nil
		},
	})
	task, err := manager.Spawn(context.Background(), SpawnRequest{Prompt: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, done, err := manager.Wait(context.Background(), task.ID, 20*time.Millisecond)
	if err != nil || done || snapshot.Terminal() {
		t.Fatalf("timeout snapshot = %+v done=%v err=%v", snapshot, done, err)
	}
	close(block)
	if _, done, err := manager.Wait(context.Background(), task.ID, 5*time.Second); err != nil || !done {
		t.Fatalf("final wait: done=%v err=%v", done, err)
	}
	if err := manager.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCloseClosesWorkerExactlyOnce(t *testing.T) {
	worker := &testWorker{run: func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}}
	manager := newTestManager(t, Config{
		Factory: func(context.Context, Task) (Prepared, error) {
			return Prepared{Worker: worker}, nil
		},
	})
	task, err := manager.Spawn(context.Background(), SpawnRequest{Prompt: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, ok := manager.Get(task.ID)
		if !ok {
			t.Fatal("task disappeared")
		}
		if current.State == StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not start: %+v", current)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if got := worker.closeCount(); got != 1 {
		t.Fatalf("worker closed %d times, want 1", got)
	}
}
