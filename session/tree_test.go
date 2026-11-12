package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimi/auto-harness/llm"
)

// TestTreeBranching verifies that moving the leaf and appending creates
// a new branch while the abandoned one stays in the tree.
func TestTreeBranching(t *testing.T) {
	tree := NewTree(Header{Type: "session", Version: VersionTree, ID: "s1"})

	first := tree.AppendMessage(llm.Message{Role: llm.RoleUser, Content: "one"})
	second := tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "two"})

	// Go back to the first entry and append a different answer.
	if err := tree.SetLeaf(first); err != nil {
		t.Fatalf("set leaf: %v", err)
	}
	alt := tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "two-alternate"})

	if alt == second {
		t.Fatal("the alternate branch reused an entry ID")
	}

	// The current branch is the new one.
	msgs := tree.Messages()
	if len(msgs) != 2 {
		t.Fatalf("current branch messages = %d, want 2", len(msgs))
	}
	if msgs[1].Content != "two-alternate" {
		t.Errorf("current branch = %q", msgs[1].Content)
	}

	// The abandoned branch is still there.
	old, ok := tree.Entry(second)
	if !ok {
		t.Fatal("the abandoned entry was dropped")
	}
	if old.ParentID != first {
		t.Errorf("abandoned entry parent = %q, want %q", old.ParentID, first)
	}

	// There are two branch tips.
	branches := tree.Branches()
	if len(branches) != 2 {
		t.Fatalf("branches = %v, want 2", branches)
	}
}

// TestTreeSiblingNavigation verifies the alternatives available from a
// given point.
func TestTreeSiblingNavigation(t *testing.T) {
	tree := NewTree(Header{Type: "session", Version: VersionTree, ID: "s1"})
	root := tree.AppendMessage(llm.Message{Role: llm.RoleUser, Content: "q"})
	a := tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "a"})
	tree.SetLeaf(root)
	b := tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "b"})

	sibs := tree.Siblings(a)
	if len(sibs) != 2 {
		t.Fatalf("siblings = %v, want 2", sibs)
	}
	found := map[string]bool{}
	for _, s := range sibs {
		found[s] = true
	}
	if !found[a] || !found[b] {
		t.Errorf("siblings = %v, want both %s and %s", sibs, a, b)
	}
}

// TestTreeCompactionHonorsKeptRange verifies a compaction entry
// replaces the older messages with its summary and keeps the retained
// range.
func TestTreeCompactionHonorsKeptRange(t *testing.T) {
	tree := NewTree(Header{Type: "session", Version: VersionTree, ID: "s1"})

	old1 := tree.AppendMessage(llm.Message{Role: llm.RoleUser, Content: "old-1"})
	old2 := tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "old-2"})
	kept := tree.AppendMessage(llm.Message{Role: llm.RoleUser, Content: "kept"})

	tree.Append(&Entry{
		Type:             EntryCompaction,
		Summary:          "summary of old-1 and old-2",
		FirstKeptEntryID: kept,
	})
	tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "after"})

	branch := tree.Branch()
	// The compaction entry, the kept entry, and the entry after it.
	if len(branch) != 3 {
		t.Fatalf("branch entries = %d, want 3: %v", len(branch), branch)
	}
	if branch[0].Type != EntryCompaction {
		t.Errorf("first branch entry = %s, want compaction", branch[0].Type)
	}
	if branch[1].Message == nil || branch[1].Message.Content != "kept" {
		t.Errorf("second branch entry = %+v, want the kept message", branch[1])
	}

	msgs := tree.Messages()
	// The summary stands in for the compacted messages.
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0].Content, "summary of old-1 and old-2") {
		t.Errorf("first message = %q, want the summary", msgs[0].Content)
	}
	if msgs[1].Content != "kept" {
		t.Errorf("second message = %q, want kept", msgs[1].Content)
	}
	if msgs[2].Content != "after" {
		t.Errorf("third message = %q, want after", msgs[2].Content)
	}
	_ = old1
	_ = old2
}

// TestForkCreatesIndependentSession verifies a fork copies the branch
// up to the fork point into a new file and leaves the original alone.
func TestForkCreatesIndependentSession(t *testing.T) {
	dir := t.TempDir()
	src, err := New(dir, "test-model")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer src.Close()

	src.Append(
		llm.Message{Role: llm.RoleUser, Content: "first question"},
		llm.Message{Role: llm.RoleAssistant, Content: "first answer"},
	)

	// Fork from the first user message.
	tree := src.Tree()
	path := tree.CurrentPath()
	if len(path) < 1 {
		t.Fatal("no entries")
	}
	forkPoint := path[0].ID

	dst, err := Fork(src, ForkOptions{Dir: dir, AtEntryID: forkPoint})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	defer dst.Close()

	// The fork has the messages up to the fork point.
	if len(dst.Messages) != 1 {
		t.Fatalf("fork messages = %d, want 1: %+v", len(dst.Messages), dst.Messages)
	}
	if dst.Messages[0].Content != "first question" {
		t.Errorf("fork message = %q", dst.Messages[0].Content)
	}
	if dst.Meta.ParentSession != src.Path {
		t.Errorf("parent session = %q, want %q", dst.Meta.ParentSession, src.Path)
	}

	// The fork is a separate file that records the parent.
	if dst.Path == src.Path {
		t.Fatal("the fork wrote to the source file")
	}
	data, err := os.ReadFile(dst.Path)
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	firstLine := strings.SplitN(string(data), "\n", 2)[0]
	var header map[string]any
	if err := json.Unmarshal([]byte(firstLine), &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if header["type"] != "session" {
		t.Errorf("header type = %v, want session", header["type"])
	}
	if header["parentSession"] != src.Path {
		t.Errorf("header parentSession = %v, want %q", header["parentSession"], src.Path)
	}

	// Appending to the fork does not change the source.
	dst.Append(llm.Message{Role: llm.RoleUser, Content: "fork only"})
	if len(src.Messages) != 2 {
		t.Errorf("source messages = %d, want 2 (unchanged)", len(src.Messages))
	}
}

// TestCloneCopiesCurrentBranch verifies /clone copies the whole current
// branch.
func TestCloneCopiesCurrentBranch(t *testing.T) {
	dir := t.TempDir()
	src, err := New(dir, "test-model")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer src.Close()

	src.Append(
		llm.Message{Role: llm.RoleUser, Content: "a"},
		llm.Message{Role: llm.RoleAssistant, Content: "b"},
		llm.Message{Role: llm.RoleUser, Content: "c"},
	)

	dst, err := Clone(src, ForkOptions{Dir: dir})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer dst.Close()

	if len(dst.Messages) != 3 {
		t.Fatalf("clone messages = %d, want 3", len(dst.Messages))
	}
	if dst.Messages[2].Content != "c" {
		t.Errorf("clone last message = %q", dst.Messages[2].Content)
	}
}

// TestBranchMessagesExcludesCommonAncestor verifies the input to a
// branch summarizer holds only what the abandoned branch added.
func TestBranchMessagesExcludesCommonAncestor(t *testing.T) {
	tree := NewTree(Header{Type: "session", Version: VersionTree, ID: "s1"})
	root := tree.AppendMessage(llm.Message{Role: llm.RoleUser, Content: "shared"})
	abandoned := tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "abandoned work"})
	tree.SetLeaf(root)
	tree.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: "current work"})

	msgs := tree.BranchMessages(abandoned)
	if len(msgs) != 1 {
		t.Fatalf("branch messages = %d, want 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Content != "abandoned work" {
		t.Errorf("branch message = %q", msgs[0].Content)
	}
}

// TestSessionPersistsBranches verifies a branched session survives a
// reload with both branches intact.
func TestSessionPersistsBranches(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "test-model")
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	s.Append(llm.Message{Role: llm.RoleUser, Content: "question"})
	tree := s.Tree()
	root := tree.CurrentPath()[0].ID
	s.Append(llm.Message{Role: llm.RoleAssistant, Content: "answer A"})

	if err := s.SetLeaf(root); err != nil {
		t.Fatalf("set leaf: %v", err)
	}
	s.Append(llm.Message{Role: llm.RoleAssistant, Content: "answer B"})
	s.Close()

	// Reload.
	reloaded, err := Load(dir, s.Meta.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer reloaded.Close()

	if len(reloaded.Messages) != 2 {
		t.Fatalf("reloaded messages = %d, want 2: %+v", len(reloaded.Messages), reloaded.Messages)
	}
	if reloaded.Messages[1].Content != "answer B" {
		t.Errorf("reloaded message = %q, want answer B", reloaded.Messages[1].Content)
	}

	// The abandoned branch is still in the file.
	found := false
	for _, e := range reloaded.Tree().Entries() {
		if e.Message != nil && e.Message.Content == "answer A" {
			found = true
		}
	}
	if !found {
		t.Error("the abandoned branch was lost on reload")
	}
}

// TestLegacySessionMigrates verifies a v1 linear session loads and is
// readable as a tree.
func TestLegacySessionMigrates(t *testing.T) {
	dir := t.TempDir()
	id := "20260101-120000.000000000"
	path := filepath.Join(dir, id+".jsonl")

	var b strings.Builder
	meta, _ := json.Marshal(map[string]any{
		"kind": "meta",
		"at":   "2026-01-01T12:00:00Z",
		"meta": map[string]any{
			"id":         id,
			"created_at": "2026-01-01T12:00:00Z",
			"model":      "legacy-model",
		},
	})
	b.Write(meta)
	b.WriteByte('\n')
	for _, content := range []string{"hello", "world"} {
		rec, _ := json.Marshal(map[string]any{
			"kind": "message",
			"at":   "2026-01-01T12:00:01Z",
			"message": map[string]any{
				"role":    "user",
				"content": content,
			},
		})
		b.Write(rec)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir, id)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	defer s.Close()

	if len(s.Messages) != 2 {
		t.Fatalf("legacy messages = %d, want 2: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[1].Content != "world" {
		t.Errorf("legacy message = %q", s.Messages[1].Content)
	}
	if s.Meta.Model != "legacy-model" {
		t.Errorf("legacy model = %q", s.Meta.Model)
	}
	// The migrated tree has the messages as a chain.
	if len(s.Tree().CurrentPath()) != 2 {
		t.Errorf("migrated path = %d entries, want 2", len(s.Tree().CurrentPath()))
	}
}
