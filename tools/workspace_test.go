package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryCloneAndReplaceAreIndependent(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(NewReadTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewWriteTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetActiveToolsExact([]string{"read"}); err != nil {
		t.Fatal(err)
	}

	clone := registry.Clone()
	replacement := NewReadToolAt(t.TempDir())
	if err := clone.Replace(replacement); err != nil {
		t.Fatal(err)
	}
	original, _ := registry.Get("read")
	replaced, _ := clone.Get("read")
	if original == replaced {
		t.Fatal("clone replacement changed the parent registry")
	}
	if active := clone.GetActiveTools(); len(active) != 1 || active[0].Name != "read" {
		t.Fatalf("clone active tools = %+v", active)
	}
	missing := NewReadTool()
	missing.Name = "missing"
	if err := clone.Replace(missing); err == nil {
		t.Fatal("replacing a different name succeeded")
	}
}

func TestWorkspaceToolsConfineRelativePaths(t *testing.T) {
	root := t.TempDir()
	write := NewWriteToolAt(root)
	read := NewReadToolAt(root)

	result := execTool(t, write, `{"path":"sub/a.txt","content":"hello\n"}`)
	if result.IsError || !strings.Contains(result.Content, filepath.Join(root, "sub", "a.txt")) {
		t.Fatalf("write = %+v", result)
	}
	result = execTool(t, read, `{"path":"sub/a.txt"}`)
	if result.IsError || !strings.Contains(result.Content, "hello") {
		t.Fatalf("read = %+v", result)
	}
	result = execTool(t, write, `{"path":"../escape.txt","content":"no"}`)
	if !result.IsError || !strings.Contains(result.Content, "outside") {
		t.Fatalf("escape write = %+v", result)
	}
}

func TestBashToolAtUsesWorkspace(t *testing.T) {
	root := t.TempDir()
	tool := NewBashToolAt(root)
	result := execTool(t, tool, `{"command":"pwd"}`)
	if result.IsError || strings.TrimSpace(result.Content) != root {
		t.Fatalf("pwd = %+v", result)
	}
	result = execTool(t, tool, `{"command":"pwd","workdir":".."}`)
	if !result.IsError || !strings.Contains(result.Content, "outside") {
		t.Fatalf("escaping workdir = %+v", result)
	}
}

func TestWorkspaceToolsRejectSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	write := NewWriteToolAt(root)
	result := execTool(t, write, `{"path":"outside/escape.txt","content":"no"}`)
	if !result.IsError || !strings.Contains(result.Content, "outside") {
		t.Fatalf("symlink escape write = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("write escaped through symlink: %v", err)
	}
}

func TestSandboxedBashToolAtPreservesWorkspaceRunner(t *testing.T) {
	root := t.TempDir()
	var gotWorkdir string
	tool := NewSandboxedBashToolAt(root, func(ctx context.Context, command, workdir string) (string, bool, error) {
		gotWorkdir = workdir
		return "ok", true, nil
	})
	result := execTool(t, tool, `{"command":"true"}`)
	if result.IsError || result.Content != "ok" {
		t.Fatalf("sandboxed result = %+v", result)
	}
	if gotWorkdir != root {
		t.Fatalf("sandbox workdir = %q, want %q", gotWorkdir, root)
	}
}
