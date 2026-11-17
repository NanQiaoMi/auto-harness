package gitkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testConversation struct {
	id       string
	leaf     string
	turn     string
	anchor   string
	reloads  int
	setErr   error
	setCalls int
}

func (c *testConversation) SessionID() string { return c.id }

func (c *testConversation) CurrentLeaf() string { return c.leaf }

func (c *testConversation) CurrentTurn() string { return c.turn }

func (c *testConversation) CheckpointAnchor() string { return c.anchor }

func (c *testConversation) CanSetLeaf(id string) bool { return true }

func (c *testConversation) SetLeaf(id string) error {
	c.setCalls++
	if c.setErr != nil {
		err := c.setErr
		c.setErr = nil
		return err
	}
	c.leaf = id
	return nil
}

func (c *testConversation) ReloadHistory() { c.reloads++ }

func TestCheckpointBindsSessionAndUndoTurnRestoresBoth(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "a.txt")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	convo := &testConversation{id: "session-1", leaf: "turn-2", turn: "turn-2", anchor: "before-turn"}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		anchor := convo.leaf
		turn := ""
		if kind == CheckpointAuto {
			anchor = "before-turn"
			turn = convo.turn
		}
		return SessionBinding{ID: convo.id, Anchor: anchor, Turn: turn}
	}
	checkpoint, created, err := manager.Create(context.Background(), "before write", CheckpointAuto, "write")
	if err != nil || !created {
		t.Fatalf("create = %#v, %v, %v", checkpoint, created, err)
	}
	if checkpoint.SessionID != "session-1" || checkpoint.SessionAnchor != "before-turn" {
		t.Fatalf("session binding = %#v", checkpoint)
	}

	writeTestFile(t, path, "changed\n")
	result, err := manager.UndoTurn(context.Background(), convo, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ConversationRestored || result.Noop || result.Safety.ID == "" {
		t.Fatalf("undo result = %#v", result)
	}
	if got := readTestFile(t, path); got != "base\n" {
		t.Fatalf("worktree = %q", got)
	}
	if convo.leaf != "before-turn" || convo.reloads != 1 {
		t.Fatalf("conversation = %#v", convo)
	}

	// The safety snapshot is also a valid turn checkpoint, so undoing
	// the undo restores the changed workspace and the later leaf.
	redo, err := manager.UndoTurn(context.Background(), convo, result.Safety.ID)
	if err != nil {
		t.Fatal(err)
	}
	if redo.Noop || !redo.ConversationRestored {
		t.Fatalf("redo = %#v", redo)
	}
	if got := readTestFile(t, path); got != "changed\n" {
		t.Fatalf("redo worktree = %q", got)
	}
	if convo.leaf != "turn-2" {
		t.Fatalf("redo leaf = %q", convo.leaf)
	}
}

func TestCheckpointBindingChangesAreNotDeduplicated(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	binding := SessionBinding{ID: "s1", Anchor: "a1"}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		return binding
	}
	if _, created, err := manager.Create(context.Background(), "first", CheckpointAuto, "write"); err != nil || !created {
		t.Fatalf("first = %v, %v", created, err)
	}
	if _, created, err := manager.Create(context.Background(), "same", CheckpointAuto, "write"); err != nil || created {
		t.Fatalf("same binding created = %v, %v", created, err)
	}
	binding.Anchor = "a2"
	second, created, err := manager.Create(context.Background(), "next", CheckpointAuto, "write")
	if err != nil || !created {
		t.Fatalf("next binding = %#v, %v, %v", second, created, err)
	}
	if second.SessionAnchor != "a2" {
		t.Fatalf("next anchor = %q", second.SessionAnchor)
	}
}

func TestUndoTurnConversationFailureRollsBackWorkspace(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "a.txt")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	convo := &testConversation{id: "s1", leaf: "turn", turn: "t1", anchor: "before"}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		anchor := convo.leaf
		turn := ""
		if kind == CheckpointAuto {
			anchor = "before"
			turn = convo.turn
		}
		return SessionBinding{ID: convo.id, Anchor: anchor, Turn: turn}
	}
	if _, _, err := manager.Create(context.Background(), "before", CheckpointAuto, "write"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "changed\n")
	convo.setErr = errors.New("session write failed")

	_, err := manager.UndoTurn(context.Background(), convo, "")
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("undo error = %v", err)
	}
	if got := readTestFile(t, path); got != "changed\n" {
		t.Fatalf("workspace after failed undo = %q", got)
	}
	if convo.leaf != "turn" {
		t.Fatalf("leaf after failed undo = %q", convo.leaf)
	}
}

func TestUndoTurnRejectsAnotherSession(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	owner := &testConversation{id: "owner", leaf: "owner-leaf"}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		return SessionBinding{ID: owner.id, Anchor: "anchor"}
	}
	checkpoint, _, err := manager.Create(context.Background(), "owner", CheckpointAuto, "write")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "a.txt"), "changed\n")
	stranger := &testConversation{id: "stranger", leaf: "leaf"}
	if _, err := manager.UndoTurn(context.Background(), stranger, checkpoint.ID); err == nil ||
		!strings.Contains(err.Error(), "another session") {
		t.Fatalf("cross-session error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(repo, "a.txt")); got != "changed\n" {
		t.Fatalf("workspace changed before session check = %q", got)
	}
}

func TestUndoTurnNoopWhenWorkspaceAndLeafMatch(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	convo := &testConversation{id: "s1", leaf: "anchor"}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		return SessionBinding{ID: convo.id, Anchor: "anchor"}
	}
	checkpoint, _, err := manager.Create(context.Background(), "same", CheckpointManual, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.UndoTurn(context.Background(), convo, checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Noop || result.ConversationRestored {
		t.Fatalf("noop result = %#v", result)
	}
}

func TestUndoTurnCanClearFirstTurn(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")
	convo := &testConversation{id: "s1", leaf: "", turn: "t1", anchor: ""}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		return SessionBinding{ID: convo.id, Anchor: "", Turn: convo.turn}
	}
	if _, _, err := manager.Create(context.Background(), "first turn", CheckpointAuto, "write"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	convo.leaf = "tool-call"
	if _, err := manager.UndoTurn(context.Background(), convo, ""); err != nil {
		t.Fatal(err)
	}
	if convo.leaf != "" {
		t.Fatalf("first-turn leaf = %q, want empty", convo.leaf)
	}
}

func TestUndoTurnRestoresBeforeFirstToolOfTurn(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "a.txt")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	convo := &testConversation{
		id:     "s1",
		leaf:   "turn-2",
		turn:   "turn-2",
		anchor: "before-turn",
	}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		anchor, turn := convo.leaf, ""
		if kind == CheckpointAuto {
			anchor, turn = "before-turn", convo.turn
		}
		return SessionBinding{ID: convo.id, Anchor: anchor, Turn: turn}
	}
	if _, created, err := manager.Create(context.Background(), "before tool 1",
		CheckpointAuto, "write"); err != nil || !created {
		t.Fatalf("first checkpoint: created=%v err=%v", created, err)
	}
	writeTestFile(t, path, "one\n")
	if _, created, err := manager.Create(context.Background(), "before tool 2",
		CheckpointAuto, "bash"); err != nil || !created {
		t.Fatalf("second checkpoint: created=%v err=%v", created, err)
	}
	writeTestFile(t, path, "two\n")

	if _, err := manager.UndoTurn(context.Background(), convo, ""); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, path); got != "base\n" {
		t.Fatalf("turn undo restored %q, want the pre-turn state", got)
	}
	if convo.leaf != "before-turn" {
		t.Fatalf("leaf = %q", convo.leaf)
	}
}

func TestUndoTurnUsesTurnIDNotJustAnchor(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "a.txt")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	convo := &testConversation{
		id:     "s1",
		leaf:   "after-t1",
		turn:   "t1",
		anchor: "shared-anchor",
	}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		anchor, turn := convo.leaf, ""
		if kind == CheckpointAuto {
			anchor, turn = convo.anchor, convo.turn
		}
		return SessionBinding{ID: convo.id, Anchor: anchor, Turn: turn}
	}
	if _, _, err := manager.Create(context.Background(), "turn one",
		CheckpointAuto, "write"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "after-one\n")

	// A new branch starts from the same parent entry but is a different
	// turn. Its undo must not jump back to the abandoned branch's state.
	convo.leaf = "after-t2"
	convo.turn = "t2"
	if _, created, err := manager.Create(context.Background(), "turn two",
		CheckpointAuto, "write"); err != nil || !created {
		t.Fatalf("second turn: created=%v err=%v", created, err)
	}
	writeTestFile(t, path, "after-two\n")

	if _, err := manager.UndoTurn(context.Background(), convo, ""); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, path); got != "after-one\n" {
		t.Fatalf("turn undo restored %q, want the second turn's base", got)
	}
}

func TestUndoTurnWithoutWorkspaceCheckpointMovesConversationOnly(t *testing.T) {
	repo := newTestRepo(t)
	path := filepath.Join(repo, "a.txt")
	writeTestFile(t, path, "base\n")
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-m", "base")

	convo := &testConversation{
		id:     "s1",
		leaf:   "assistant",
		turn:   "t1",
		anchor: "before-turn",
	}
	manager := NewCheckpointManager(repo)
	manager.BindSession = func(kind CheckpointKind, tool string) SessionBinding {
		return SessionBinding{ID: convo.id, Anchor: convo.leaf}
	}

	result, err := manager.UndoTurn(context.Background(), convo, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceRestored || !result.ConversationRestored {
		t.Fatalf("conversation-only result = %#v", result)
	}
	if result.Safety.ID == "" || !strings.Contains(result.Warning, "without changing files") {
		t.Fatalf("conversation-only safety/warning = %#v", result)
	}
	if convo.leaf != "before-turn" {
		t.Fatalf("leaf = %q", convo.leaf)
	}
	if got := readTestFile(t, path); got != "base\n" {
		t.Fatalf("conversation-only undo changed files: %q", got)
	}
}
