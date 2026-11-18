package gitkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddAndRemoveDetachedWorktree(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "base\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")

	path := filepath.Join(t.TempDir(), "delegated")
	got, err := AddDetachedWorktree(context.Background(), repo, path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("worktree path = %q, want %q", got, path)
	}
	data, err := os.ReadFile(filepath.Join(path, "tracked.txt"))
	if err != nil || string(data) != "base\n" {
		t.Fatalf("worktree file = %q err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("parent file changed: %q err=%v", data, err)
	}

	dirs, err := GitDirs(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 || !strings.Contains(strings.Join(dirs, "\n"), ".git") {
		t.Fatalf("GitDirs = %#v", dirs)
	}

	if err := RemoveWorktree(context.Background(), repo, path, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
}

func TestAddDetachedWorktreeRejectsPathInsideRepository(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "base\n")
	testGit(t, repo, "add", "tracked.txt")
	testGit(t, repo, "commit", "-m", "base")

	_, err := AddDetachedWorktree(context.Background(), repo, filepath.Join(repo, "nested"))
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("nested worktree error = %v", err)
	}
}
