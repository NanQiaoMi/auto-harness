package session

import (
	"path/filepath"
	"testing"
)

func TestNewChildStartsFreshAndRecordsParent(t *testing.T) {
	dir := t.TempDir()
	parent, err := New(dir, "parent-model")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	child, err := NewChild(dir, "child-model", parent.Path, "delegated review")
	if err != nil {
		t.Fatal(err)
	}
	path := child.Path
	childID := child.Meta.ID
	defer child.Close()

	if len(child.Messages) != 0 {
		t.Fatalf("child started with parent history: %+v", child.Messages)
	}
	if child.Meta.ParentSession != parent.Path || child.Tree().Header().ParentSession != parent.Path {
		t.Fatalf("parent link = %q / %q",
			child.Meta.ParentSession, child.Tree().Header().ParentSession)
	}
	if child.Meta.Name != "delegated review" {
		t.Fatalf("name = %q", child.Meta.Name)
	}

	loaded, err := Load(dir, childID)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if loaded.Path != filepath.Clean(path) || loaded.Meta.ParentSession != parent.Path {
		t.Fatalf("reloaded child = %+v", loaded.Meta)
	}
}
