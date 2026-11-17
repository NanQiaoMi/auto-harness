package gitkit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/tools"
)

func TestGitToolRunsWithoutShell(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "note.txt"), "hello\n")
	tool := NewGitTool(repo)

	result := executeTestTool(t, tool, map[string]any{"args": []string{"status", "--short"}})
	if result.IsError || !strings.Contains(result.Content, "?? note.txt") {
		t.Fatalf("status = %+v", result)
	}
	result = executeTestTool(t, tool, map[string]any{"args": []string{"add", "note.txt"}})
	if result.IsError {
		t.Fatalf("add = %+v", result)
	}
	result = executeTestTool(t, tool, map[string]any{
		"args": []string{"commit", "-m", "initial"},
	})
	if result.IsError || !strings.Contains(result.Content, "initial") {
		t.Fatalf("commit = %+v", result)
	}
	result = executeTestTool(t, tool, map[string]any{
		"args": []string{"log", "--oneline", "-1"},
	})
	if result.IsError || !strings.Contains(result.Content, "initial") {
		t.Fatalf("log = %+v", result)
	}

	result = executeTestTool(t, tool, map[string]any{
		"args":    []string{"status"},
		"workdir": "../outside",
	})
	if !result.IsError || !strings.Contains(result.Content, "escapes") {
		t.Fatalf("workdir escape = %+v", result)
	}
}

func TestCheckpointRestoresWorktreeAndIndex(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")

	writeTestFile(t, tracked, "staged\n")
	testGit(t, repo, "add", "tracked.txt")
	writeTestFile(t, tracked, "working\n")
	writeTestFile(t, filepath.Join(repo, "untracked.txt"), "keep me\n")

	manager := NewCheckpointManager(repo)
	checkpoint, created, err := manager.Create(context.Background(), "mixed state", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first checkpoint was deduplicated")
	}

	writeTestFile(t, tracked, "later\n")
	testGit(t, repo, "add", "tracked.txt")
	writeTestFile(t, filepath.Join(repo, "later.txt"), "remove me\n")
	testGit(t, repo, "add", "later.txt")
	writeTestFile(t, filepath.Join(repo, "untracked.txt"), "changed\n")

	result, err := manager.Rewind(context.Background(), checkpoint.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Noop || result.Safety.ID == "" {
		t.Fatalf("rewind result = %#v", result)
	}
	if got := readTestFile(t, tracked); got != "working\n" {
		t.Fatalf("worktree = %q", got)
	}
	if got := testGit(t, repo, "show", ":tracked.txt"); got != "staged" {
		t.Fatalf("index = %q", got)
	}
	if got := readTestFile(t, filepath.Join(repo, "untracked.txt")); got != "keep me\n" {
		t.Fatalf("restored untracked = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "later.txt")); !os.IsNotExist(err) {
		t.Fatalf("later.txt still exists: %v", err)
	}
}

func TestCheckpointRewindUndoesCommit(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")
	baseHead := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "before commit", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, tracked, "committed\n")
	writeTestFile(t, filepath.Join(repo, "committed.txt"), "new\n")
	testGit(t, repo, "add", "-A")
	testGit(t, repo, "commit", "-m", "second")
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head == baseHead {
		t.Fatal("test commit did not advance HEAD")
	}

	result, err := manager.Rewind(context.Background(), checkpoint.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.HeadReset {
		t.Fatalf("HEAD was not reset: %#v", result)
	}
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head != baseHead {
		t.Fatalf("HEAD = %s, want %s", head, baseHead)
	}
	if got := readTestFile(t, tracked); got != "base\n" {
		t.Fatalf("tracked content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "committed.txt")); !os.IsNotExist(err) {
		t.Fatalf("committed.txt still exists: %v", err)
	}
	if status := strings.TrimSpace(testGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("status after rewind = %q", status)
	}
}

func TestCheckpointPreservesIgnoredFiles(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "base\n")
	testGit(t, repo, "add", ".gitignore", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")
	ignored := filepath.Join(repo, "ignored.txt")
	writeTestFile(t, ignored, "keep\n")

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "ignored file", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "changed\n")
	result, err := manager.Rewind(context.Background(), checkpoint.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Noop {
		t.Fatal("rewind unexpectedly was a no-op")
	}
	if got := readTestFile(t, ignored); got != "keep\n" {
		t.Fatalf("ignored file = %q", got)
	}
}

func TestCheckpointDeduplicatesAndBeforeTool(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")

	manager := NewCheckpointManager(repo)
	first, created, err := manager.Create(context.Background(), "first", CheckpointManual, "test")
	if err != nil || !created {
		t.Fatalf("first create = %#v, %v, %v", first, created, err)
	}
	second, created, err := manager.Create(context.Background(), "same", CheckpointManual, "test")
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("duplicate create = %#v, %v, %v", second, created, err)
	}
	writeTestFile(t, tracked, "changed\n")
	if err := manager.BeforeTool(context.Background(), "write", json.RawMessage(`{"path":"tracked.txt"}`)); err != nil {
		t.Fatal(err)
	}
	items, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != CheckpointAuto || items[0].Tool != "write" {
		t.Fatalf("checkpoints = %#v", items)
	}
	if items[0].Label != "before write" || !items[0].Current || !items[1].Differs {
		t.Fatalf("decoration = %#v", items)
	}
}

func TestCheckpointSkipsReadOnlyGitCalls(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)

	if err := manager.BeforeTool(context.Background(), "git",
		json.RawMessage(`{"args":["status","--short"]}`)); err != nil {
		t.Fatal(err)
	}
	items, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("read-only status created checkpoints: %#v", items)
	}
	if err := manager.BeforeTool(context.Background(), "git",
		json.RawMessage(`{"args":["add","a.txt"]}`)); err != nil {
		t.Fatal(err)
	}
	items, err = manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("mutating git call checkpoints = %d", len(items))
	}
}

func TestCheckpointRestoresAcrossBranchesAndUndoesItself(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")
	baseHead := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "on main", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "switch", "-c", "feature")
	writeTestFile(t, tracked, "feature\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "feature")
	featureHead := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	result, err := manager.Rewind(context.Background(), checkpoint.ID, true)
	if err != nil {
		t.Fatalf("cross-branch rewind: %v", err)
	}
	if !result.HeadReset || !result.RefsReset {
		t.Fatalf("rewind result = %#v", result)
	}
	if branch := strings.TrimSpace(testGit(t, repo, "branch", "--show-current")); branch != "main" {
		t.Fatalf("branch = %q", branch)
	}
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head != baseHead {
		t.Fatalf("HEAD = %s, want %s", head, baseHead)
	}
	if got := readTestFile(t, tracked); got != "base\n" {
		t.Fatalf("content = %q", got)
	}
	if branches := testGit(t, repo, "branch", "--list", "feature"); branches != "" {
		t.Fatalf("feature branch survived rewind: %q", branches)
	}

	// The safety checkpoint is the exact state before the rewind, so
	// restoring it undoes the rewind itself.
	if result.Safety.ID == checkpoint.ID || result.Safety.Kind != CheckpointSafety {
		t.Fatalf("safety checkpoint = %#v", result.Safety)
	}
	if _, err := manager.Rewind(context.Background(), result.Safety.ID, true); err != nil {
		t.Fatalf("undo rewind: %v", err)
	}
	if branch := strings.TrimSpace(testGit(t, repo, "branch", "--show-current")); branch != "feature" {
		t.Fatalf("branch after undo = %q", branch)
	}
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head != featureHead {
		t.Fatalf("HEAD after undo = %s, want %s", head, featureHead)
	}
	if got := readTestFile(t, tracked); got != "feature\n" {
		t.Fatalf("content after undo = %q", got)
	}
}

func TestCheckpointRestoresDetachedHead(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	writeTestFile(t, tracked, "one\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "one")
	one := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	writeTestFile(t, tracked, "two\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "two")
	two := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	testGit(t, repo, "switch", "--detach", one)
	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "detached", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "switch", "main")
	result, err := manager.Rewind(context.Background(), checkpoint.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.HeadReset {
		t.Fatalf("detached rewind result = %#v", result)
	}
	if out, err := testGitOutput(t, repo, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Fatalf("HEAD is still attached: %q", out)
	}
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head != one {
		t.Fatalf("detached HEAD = %s, want %s", head, one)
	}
	if got := readTestFile(t, tracked); got != "one\n" {
		t.Fatalf("detached content = %q", got)
	}

	if _, err := manager.Rewind(context.Background(), result.Safety.ID, true); err != nil {
		t.Fatal(err)
	}
	if branch := strings.TrimSpace(testGit(t, repo, "branch", "--show-current")); branch != "main" {
		t.Fatalf("branch after undo = %q", branch)
	}
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head != two {
		t.Fatalf("HEAD after undo = %s, want %s", head, two)
	}
}

func TestCheckpointCanUndoInitialCommit(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "tracked.txt")
	writeTestFile(t, tracked, "draft\n")

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "unborn", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Head != "" {
		t.Fatalf("unborn checkpoint HEAD = %q", checkpoint.Head)
	}
	testGit(t, repo, "add", "-A")
	testGit(t, repo, "commit", "-m", "initial")
	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err != nil {
		t.Fatal(err)
	}
	if out, code, err := runGit(context.Background(), repo, nil, nil,
		"rev-parse", "--verify", "HEAD"); err != nil || code == 0 {
		t.Fatalf("HEAD exists after undo: %q, %d, %v", out, code, err)
	}
	if got := readTestFile(t, tracked); got != "draft\n" {
		t.Fatalf("draft content = %q", got)
	}
}

func TestCheckpointNonRepositoryIsNoopForBeforeTool(t *testing.T) {
	dir := t.TempDir()
	manager := NewCheckpointManager(dir)
	if err := manager.BeforeTool(context.Background(), "write", nil); err != nil {
		t.Fatalf("BeforeTool outside repo = %v", err)
	}
	if _, _, err := manager.Create(context.Background(), "x", CheckpointManual, "test"); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Create error = %v", err)
	}
	if _, err := manager.List(context.Background()); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("List error = %v", err)
	}
}

func TestCheckpointRetention(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)
	manager.Limit = 2
	for i := 0; i < 4; i++ {
		writeTestFile(t, filepath.Join(repo, "a.txt"), strings.Repeat("x", i+1)+"\n")
		if _, _, err := manager.Create(context.Background(), "step", CheckpointManual, "test"); err != nil {
			t.Fatal(err)
		}
	}
	items, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("retained checkpoints = %d", len(items))
	}
}

func TestCheckpointToolActions(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)
	tool := NewCheckpointTool(manager)

	result := executeTestTool(t, tool, map[string]any{
		"action": "create",
		"label":  "manual",
	})
	if result.IsError || !strings.Contains(result.Content, "created checkpoint") {
		t.Fatalf("create = %+v", result)
	}
	result = executeTestTool(t, tool, map[string]any{"action": "list"})
	if result.IsError || !strings.Contains(result.Content, "manual") {
		t.Fatalf("list = %+v", result)
	}
	writeTestFile(t, filepath.Join(repo, "a.txt"), "changed\n")
	result = executeTestTool(t, tool, map[string]any{
		"action": "restore",
		"id":     "1",
	})
	if result.IsError || !strings.Contains(result.Content, "restored checkpoint") {
		t.Fatalf("restore = %+v", result)
	}
	if got := readTestFile(t, filepath.Join(repo, "a.txt")); got != "base\n" {
		t.Fatalf("restored content = %q", got)
	}
}

func TestGitReadOnlyClassification(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"status", []string{"status", "--short"}, true},
		{"log through global options", []string{"-c", "core.pager=cat", "log", "-1"}, true},
		{"status through -C", []string{"-C", ".", "status"}, true},
		{"branch list", []string{"branch"}, true},
		{"branch explicit list", []string{"branch", "--list", "feature"}, true},
		{"branch verbose", []string{"branch", "-vv"}, true},
		{"branch create", []string{"branch", "topic"}, false},
		{"branch upstream equals", []string{"branch", "--set-upstream-to=origin/main"}, false},
		{"branch upstream separate", []string{"branch", "-u", "origin/main"}, false},
		{"branch delete", []string{"branch", "-d", "old"}, false},
		{"stash list", []string{"stash", "list"}, true},
		{"stash show", []string{"stash", "show"}, true},
		{"bare stash pushes", []string{"stash"}, false},
		{"stash push", []string{"stash", "push"}, false},
		{"stash pop", []string{"stash", "pop"}, false},
		{"tag list", []string{"tag"}, true},
		{"tag contains", []string{"tag", "--contains", "HEAD"}, true},
		{"tag create", []string{"tag", "v1"}, false},
		{"tag delete", []string{"tag", "-d", "v1"}, false},
		{"remote verbose", []string{"remote", "-v"}, true},
		{"remote show", []string{"remote", "show", "origin"}, true},
		{"remote add", []string{"remote", "add", "origin", "url"}, false},
		{"reflog", []string{"reflog"}, true},
		{"reflog expire", []string{"reflog", "expire"}, false},
		{"worktree list", []string{"worktree", "list"}, true},
		{"worktree add", []string{"worktree", "add", "x"}, false},
		{"diff to stdout", []string{"diff", "--stat"}, true},
		{"diff to file", []string{"diff", "--output=review.patch"}, false},
		{"checkout", []string{"checkout", "main"}, false},
		{"reset", []string{"reset", "--hard"}, false},
		{"clean", []string{"clean", "-fdx"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"args": tt.args})
			if err != nil {
				t.Fatal(err)
			}
			if got := isReadOnlyGitCall(raw); got != tt.want {
				t.Fatalf("isReadOnlyGitCall(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestCheckpointToolIsNotSnapshottedRecursively(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)
	if err := manager.BeforeTool(context.Background(), "checkpoint",
		json.RawMessage(`{"action":"list"}`)); err != nil {
		t.Fatal(err)
	}
	items, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("checkpoint tool created checkpoints: %#v", items)
	}
}

func TestCheckpointRestoresRefs(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	baseHead := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	testGit(t, repo, "tag", "base-tag")
	testGit(t, repo, "branch", "keep")
	testGit(t, repo, "update-ref", "refs/remotes/origin/main", baseHead)
	testGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "refs", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}

	testGit(t, repo, "tag", "-d", "base-tag")
	testGit(t, repo, "branch", "-D", "keep")
	testGit(t, repo, "update-ref", "-d", "refs/remotes/origin/main")
	testGit(t, repo, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	testGit(t, repo, "tag", "extra-tag")
	testGit(t, repo, "branch", "extra-branch")
	testGit(t, repo, "update-ref", "refs/remotes/origin/extra", baseHead)

	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := testGit(t, repo, "tag", "--list", "base-tag"); got != "base-tag" {
		t.Fatalf("base tag = %q", got)
	}
	if got := testGit(t, repo, "tag", "--list", "extra-tag"); got != "" {
		t.Fatalf("extra tag survived = %q", got)
	}
	if got := testGit(t, repo, "branch", "--list", "keep"); got == "" {
		t.Fatal("kept branch was not restored")
	}
	if got := testGit(t, repo, "branch", "--list", "extra-branch"); got != "" {
		t.Fatalf("extra branch survived = %q", got)
	}
	if got := strings.TrimSpace(testGit(t, repo, "rev-parse", "refs/remotes/origin/main")); got != baseHead {
		t.Fatalf("remote ref = %s, want %s", got, baseHead)
	}
	if got := strings.TrimSpace(testGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD")); got != "refs/remotes/origin/main" {
		t.Fatalf("remote HEAD = %q", got)
	}
	if out, err := testGitOutput(t, repo, "show-ref", "refs/remotes/origin/extra"); err == nil {
		t.Fatalf("extra remote ref survived = %q", out)
	}
}

func TestCheckpointRestoresStashStack(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	writeTestFile(t, tracked, "one\n")
	testGit(t, repo, "stash", "push", "-m", "one")
	writeTestFile(t, tracked, "two\n")
	testGit(t, repo, "stash", "push", "-m", "two")

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "two stashes", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := countStashEntries(t, repo); got != 2 {
		t.Fatalf("stash entries before pop = %d", got)
	}
	testGit(t, repo, "stash", "pop")
	if got := countStashEntries(t, repo); got != 1 {
		t.Fatalf("stash entries after pop = %d", got)
	}

	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := countStashEntries(t, repo); got != 2 {
		t.Fatalf("restored stash entries = %d", got)
	}
	if got := readTestFile(t, tracked); got != "base\n" {
		t.Fatalf("worktree after stash restore = %q", got)
	}
	if status := strings.TrimSpace(testGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("status after stash restore = %q", status)
	}
}

func TestCheckpointRestoresConflictedIndex(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	testGit(t, repo, "switch", "-c", "feature")
	writeTestFile(t, tracked, "feature\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "feature")
	testGit(t, repo, "switch", "main")
	writeTestFile(t, tracked, "main\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "main")
	preMerge := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	if out, err := testGitOutput(t, repo, "merge", "feature"); err == nil {
		t.Fatalf("merge unexpectedly succeeded:\n%s", out)
	}

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "conflict", CheckpointManual, "test")
	if err != nil {
		t.Fatalf("create checkpoint with conflict: %v", err)
	}
	if checkpoint.IndexBlob == "" || checkpoint.IndexTree != "" {
		t.Fatalf("conflict checkpoint index = %#v", checkpoint)
	}

	writeTestFile(t, tracked, "resolved\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "resolved")

	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err != nil {
		t.Fatalf("rewind conflict: %v", err)
	}
	if head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); head != preMerge {
		t.Fatalf("HEAD = %s, want %s", head, preMerge)
	}
	if got := strings.TrimSpace(testGit(t, repo, "ls-files", "-u")); got == "" {
		t.Fatal("conflicted index was not restored")
	}
	if got := readTestFile(t, tracked); !strings.Contains(got, "<<<<<<<") {
		t.Fatalf("conflict markers missing: %q", got)
	}
}

func TestCheckpointPreservesUnrelatedEmptyDirectories(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	empty := filepath.Join(repo, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "empty dir", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, tracked, "changed\n")
	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(empty); err != nil || !info.IsDir() {
		t.Fatalf("empty directory was removed: %v", err)
	}
}

func TestCheckpointRestoresFileOverUntrackedDirectory(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "foo")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "foo")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "file", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(path, "bar.txt"), "untracked\n")
	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, path); got != "base\n" {
		t.Fatalf("restored file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(path, "bar.txt")); err == nil {
		t.Fatalf("untracked child survived: %v", err)
	}
}

func TestCheckpointRefusesToOverwriteIgnoredFileAtTargetPath(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "foo")
	writeTestFile(t, path, "tracked\n")
	testGit(t, repo, "add", "foo")
	testGit(t, repo, "commit", "-m", "base")
	head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "tracked file", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}

	testGit(t, repo, "rm", "--cached", "foo")
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "foo\n")
	testGit(t, repo, "add", ".gitignore")
	testGit(t, repo, "commit", "-m", "ignore foo")
	testHead := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	writeTestFile(t, path, "ignored\n")

	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err == nil ||
		!strings.Contains(err.Error(), "ignored file") {
		t.Fatalf("rewind error = %v", err)
	}
	if got := readTestFile(t, path); got != "ignored\n" {
		t.Fatalf("ignored file = %q", got)
	}
	if got := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); got != testHead {
		t.Fatalf("HEAD after failed rewind = %s, want %s (base %s)", got, testHead, head)
	}
	if got := testGit(t, repo, "ls-files", "--", "foo"); got != "" {
		t.Fatalf("foo unexpectedly returned to the index: %q", got)
	}
	if got := readTestFile(t, filepath.Join(repo, ".gitignore")); got != "foo\n" {
		t.Fatalf("gitignore after failed rewind = %q", got)
	}
}

func TestCheckpointRefusesIgnoredFileInsideDirectoryReplacement(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "foo")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "foo")
	testGit(t, repo, "commit", "-m", "base")
	head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "directory", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "rm", "--cached", "foo")
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "foo/ignored.txt\n")
	testGit(t, repo, "add", ".gitignore")
	testGit(t, repo, "commit", "-m", "ignore child")
	testHead := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "foo", "ignored.txt"), "keep\n")

	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err == nil ||
		!strings.Contains(err.Error(), "ignored file") {
		t.Fatalf("rewind error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(repo, "foo", "ignored.txt")); got != "keep\n" {
		t.Fatalf("ignored child = %q", got)
	}
	if got := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")); got != testHead {
		t.Fatalf("HEAD after failed rewind = %s, want %s (base %s)", got, testHead, head)
	}
}

func TestCheckpointRefusesRefCheckedOutInAnotherWorktree(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)
	checkpoint, _, err := manager.Create(context.Background(), "main", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "branch", "feature")
	other := filepath.Join(t.TempDir(), "other")
	testGit(t, repo, "worktree", "add", other, "feature")

	if _, err := manager.Rewind(context.Background(), checkpoint.ID, true); err == nil ||
		!strings.Contains(err.Error(), "checked out in") {
		t.Fatalf("protected ref error = %v", err)
	}
	if got := testGit(t, repo, "branch", "--list", "feature"); got == "" {
		t.Fatal("protected feature branch was deleted")
	}
}

func TestCheckpointEmptySelectorNeverJumpsForward(t *testing.T) {
	repo := newTestRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	writeTestFile(t, tracked, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	manager := NewCheckpointManager(repo)
	if _, _, err := manager.Create(context.Background(), "base", CheckpointManual, "test"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, tracked, "one\n")
	if _, _, err := manager.Create(context.Background(), "one", CheckpointManual, "test"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, tracked, "two\n")
	two, _, err := manager.Create(context.Background(), "two", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Rewind(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, tracked); got != "one\n" {
		t.Fatalf("first rewind = %q", got)
	}
	if _, err := manager.Rewind(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, tracked); got != "base\n" {
		t.Fatalf("second rewind = %q", got)
	}
	if _, err := manager.Rewind(context.Background(), "", true); err == nil {
		t.Fatal("third rewind should have no earlier checkpoint")
	}

	if _, err := manager.Rewind(context.Background(), two.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rewind(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, tracked); got != "one\n" {
		t.Fatalf("rewind after explicit restore = %q", got)
	}
}

func TestGitToolAcceptsStdin(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	tool := NewGitTool(repo)
	if result := executeTestTool(t, tool, map[string]any{"args": []string{"add", "a.txt"}}); result.IsError {
		t.Fatalf("add = %+v", result)
	}
	result := executeTestTool(t, tool, map[string]any{
		"args":  []string{"commit", "-F", "-"},
		"stdin": "message from stdin\n",
	})
	if result.IsError || !strings.Contains(result.Content, "message from stdin") {
		t.Fatalf("commit -F - = %+v", result)
	}
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	testGit(t, repo, "init")
	testGit(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	testGit(t, repo, "config", "user.name", "Harness Test")
	testGit(t, repo, "config", "user.email", "harness@example.test")
	return repo
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testGitOutput(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimRight(out, "\n")
}

func testGitOutput(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func countStashEntries(t *testing.T, dir string) int {
	t.Helper()
	out := strings.TrimSpace(testGit(t, dir, "stash", "list"))
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func executeTestTool(t *testing.T, tool *tools.Tool, args map[string]any) tools.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), "test", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
