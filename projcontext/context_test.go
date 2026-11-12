package projcontext

import (
	"os"
	"path/filepath"
	"testing"
)

// write writes a file into a directory.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestLoadFindsTheInstructions checks that the instructions file is
// read from the directory.
func TestLoadFindsTheInstructions(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, InstructionsFile, "Follow the house style.")

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if files.Body != "Follow the house style." {
		t.Errorf("the body is %q", files.Body)
	}
	if filepath.Base(files.Path) != InstructionsFile {
		t.Errorf("the path is %q", files.Path)
	}
}

// TestLoadWalksUpToTheParent checks that the instructions are searched
// from the directory upwards.
func TestLoadWalksUpToTheParent(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, InstructionsFile, "The parent's instructions.")
	child := filepath.Join(parent, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}

	files, err := Load(child)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if files.Body != "The parent's instructions." {
		t.Errorf("the body is %q, want the parent's instructions", files.Body)
	}
}

// TestLoadPrefersTheNearestInstructions checks that a directory's own
// instructions shadow the one above it.
func TestLoadPrefersTheNearestInstructions(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, InstructionsFile, "The parent's instructions.")
	child := filepath.Join(parent, "a")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	write(t, child, InstructionsFile, "The child's instructions.")

	files, err := Load(child)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if files.Body != "The child's instructions." {
		t.Errorf("the body is %q, want the child's instructions", files.Body)
	}
}

// TestLoadWithoutAnyFile checks that a directory without the context
// files is not an error.
func TestLoadWithoutAnyFile(t *testing.T) {
	files, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if files.Body != "" || files.System != "" {
		t.Errorf("the files are %+v, want them empty", files)
	}
}

// TestLoadReadsTheSystemFile checks that the system file is read.
func TestLoadReadsTheSystemFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, SystemFile, "You are a careful agent.")

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if files.System != "You are a careful agent." {
		t.Errorf("the system prompt is %q", files.System)
	}
}

// TestLoadDoesNotInheritTheSystemFile checks that the system file is
// only read from the directory itself.
//
// A system prompt inherited from a parent directory would be
// surprising: the parent's prompt is not this project's prompt.
func TestLoadDoesNotInheritTheSystemFile(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, SystemFile, "The parent's prompt.")
	child := filepath.Join(parent, "a")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}

	files, err := Load(child)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if files.System != "" {
		t.Errorf("the system prompt is %q, want it empty", files.System)
	}
}

// TestApplyAppendsTheInstructions checks that the instructions are
// added to the system prompt.
func TestApplyAppendsTheInstructions(t *testing.T) {
	files := &Instructions{Body: "Follow the house style."}
	got := Apply("You are a coding agent.", files)
	want := "You are a coding agent.\n\nFollow the house style."
	if got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
}

// TestApplyReplacesWithTheSystemFile checks that the system file
// replaces the prompt outright.
func TestApplyReplacesWithTheSystemFile(t *testing.T) {
	files := &Instructions{
		Body:   "Follow the house style.",
		System: "You are a careful agent.",
	}
	got := Apply("You are a coding agent.", files)
	if got != "You are a careful agent." {
		t.Errorf("Apply = %q, want %q", got, "You are a careful agent.")
	}
}

// TestApplyWithoutAnyFile checks that an empty context changes
// nothing.
func TestApplyWithoutAnyFile(t *testing.T) {
	got := Apply("You are a coding agent.", &Instructions{})
	if got != "You are a coding agent." {
		t.Errorf("Apply = %q, want the prompt unchanged", got)
	}
	got = Apply("You are a coding agent.", nil)
	if got != "You are a coding agent." {
		t.Errorf("Apply = %q, want the prompt unchanged", got)
	}
}
