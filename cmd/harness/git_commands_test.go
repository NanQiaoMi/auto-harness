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
