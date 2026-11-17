package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
)

func TestCheckpointCommandsUseConfiguredOutput(t *testing.T) {
	repo := newGitCommandRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, repo, "add", "a.txt")
	runGitCommand(t, repo, "commit", "-m", "base")

	manager := gitkit.NewCheckpointManager(repo)
	var out bytes.Buffer
	if err := handleCheckpointCommand(context.Background(), manager, "create before edit", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "created checkpoint") {
		t.Fatalf("create output = %q", out.String())
	}

	if err := os.WriteFile(tracked, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := handleCheckpointCommand(context.Background(), manager, "list", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "before edit") {
		t.Fatalf("list output = %q", out.String())
	}

	out.Reset()
	if err := handleRewindCommand(context.Background(), manager, "", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "restored checkpoint") {
		t.Fatalf("rewind output = %q", out.String())
	}
	data, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base\n" {
		t.Fatalf("restored content = %q", data)
	}
}

func TestUndoTurnRestoresWorkspaceAndSession(t *testing.T) {
	repo := newGitCommandRepo(t)
	tracked := filepath.Join(repo, "a.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, repo, "add", "a.txt")
	runGitCommand(t, repo, "commit", "-m", "base")

	sess, err := session.New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.Append(
		llm.Message{Role: llm.RoleUser, Content: "first turn"},
		llm.Message{Role: llm.RoleAssistant, Content: "first answer"},
	); err != nil {
		t.Fatal(err)
	}
	firstLeaf := sess.CurrentLeaf()

	manager := gitkit.NewCheckpointManager(repo)
	manager.BindSession = func(kind gitkit.CheckpointKind, tool string) gitkit.SessionBinding {
		anchor := sess.CurrentLeaf()
		turn := ""
		if kind == gitkit.CheckpointAuto {
			anchor, turn = sess.TurnBinding()
		}
		return gitkit.SessionBinding{ID: sess.Meta.ID, Anchor: anchor, Turn: turn}
	}

	if err := sess.Append(llm.Message{Role: llm.RoleUser, Content: "second turn"}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := manager.Create(context.Background(), "before bash",
		gitkit.CheckpointAuto, "bash"); err != nil || !created {
		t.Fatalf("create auto checkpoint: created=%v err=%v", created, err)
	}
	if err := sess.Append(llm.Message{Role: llm.RoleAssistant, Content: "second answer"}); err != nil {
		t.Fatal(err)
	}
	secondLeaf := sess.CurrentLeaf()
	if err := os.WriteFile(tracked, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := undoTurnWithConversation(context.Background(), manager, nil, sess, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ConversationRestored || result.SessionAnchor != firstLeaf {
		t.Fatalf("undo result = %+v", result)
	}
	data, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base\n" {
		t.Fatalf("restored content = %q", data)
	}
	if leaf := sess.CurrentLeaf(); leaf != firstLeaf {
		t.Fatalf("leaf after undo = %q, want %q", leaf, firstLeaf)
	}
	if len(sess.Messages) != 2 || sess.Messages[0].Content != "first turn" {
		t.Fatalf("messages after undo = %+v", sess.Messages)
	}

	// The safety checkpoint captures the undone turn, so restoring it
	// both redoes the workspace change and returns the conversation to
	// the leaf it had before the undo.
	redo, err := undoTurnWithConversation(context.Background(), manager, nil, sess, result.Safety.ID)
	if err != nil {
		t.Fatal(err)
	}
	if redo.SessionAnchor != secondLeaf {
		t.Fatalf("redo anchor = %q, want %q", redo.SessionAnchor, secondLeaf)
	}
	data, err = os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "changed\n" {
		t.Fatalf("redone content = %q", data)
	}
	if leaf := sess.CurrentLeaf(); leaf != secondLeaf {
		t.Fatalf("leaf after redo = %q, want %q", leaf, secondLeaf)
	}
	if len(sess.Messages) != 4 || sess.Messages[3].Content != "second answer" {
		t.Fatalf("messages after redo = %+v", sess.Messages)
	}
}

func newGitCommandRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	runGitCommand(t, repo, "init")
	runGitCommand(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCommand(t, repo, "config", "user.name", "Harness Test")
	runGitCommand(t, repo, "config", "user.email", "harness@example.test")
	return repo
}

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
