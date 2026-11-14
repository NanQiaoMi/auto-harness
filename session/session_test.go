package session

import (
	"testing"

	"github.com/NanQiaoMi/auto-harness/llm"
)

func TestSessionAppendLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()

	s, err := New(dir, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	id := s.Meta.ID
	if err := s.Append(
		llm.Message{Role: llm.RoleUser, Content: "hello"},
		llm.Message{Role: llm.RoleAssistant, Content: "hi", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)}}},
		llm.Message{Role: llm.RoleTool, Content: "file.txt", ToolCallID: "c1"},
	); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()

	if loaded.Meta.Model != "test-model" {
		t.Errorf("model = %q", loaded.Meta.Model)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(loaded.Messages))
	}
	if loaded.Messages[1].ToolCalls[0].Name != "bash" {
		t.Errorf("tool call lost: %+v", loaded.Messages[1])
	}
	if loaded.Messages[2].ToolCallID != "c1" {
		t.Errorf("tool result lost: %+v", loaded.Messages[2])
	}

	// appending to a resumed session persists too
	if err := loaded.Append(llm.Message{Role: llm.RoleUser, Content: "again"}); err != nil {
		t.Fatal(err)
	}
	loaded.Close()

	reloaded, err := Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if len(reloaded.Messages) != 4 {
		t.Errorf("messages after append = %d, want 4", len(reloaded.Messages))
	}
}

func TestListNewestFirst(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(dir, "m1")
	if err != nil {
		t.Fatal(err)
	}
	s1.Append(llm.Message{Role: llm.RoleUser, Content: "one"})
	s1.Close()

	s2, err := New(dir, "m2")
	if err != nil {
		t.Fatal(err)
	}
	s2.Append(llm.Message{Role: llm.RoleUser, Content: "two"})
	s2.Close()

	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
	if list[0].ID != s2.Meta.ID {
		t.Errorf("newest first violated: %s before %s", list[0].ID, s2.Meta.ID)
	}
	if list[0].Messages != 1 || list[0].Model != "m2" {
		t.Errorf("info = %+v", list[0])
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(t.TempDir(), "nope"); err == nil {
		t.Error("want error for missing session")
	}
}
