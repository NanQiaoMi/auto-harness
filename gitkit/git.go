// Package gitkit provides the harness's built-in Git tooling and
// workspace checkpoints.
package gitkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	checkpointRootRefPrefix = "refs/harness/"
	checkpointRefPrefix     = checkpointRootRefPrefix + "checkpoints/"
	checkpointAuthor        = "auto-harness"
	checkpointEmail         = "auto-harness@localhost"
)

// ErrNotRepository reports that the selected directory is not inside a
// Git worktree.
var ErrNotRepository = errors.New("gitkit: not inside a Git repository")

// gitError is one failed Git command with its captured output.
type gitError struct {
	Args     []string
	ExitCode int
	Output   string
}

func (e *gitError) Error() string {
	output := strings.TrimSpace(e.Output)
	if output == "" {
		return fmt.Sprintf("git %s: exit %d", strings.Join(e.Args, " "), e.ExitCode)
	}
	return fmt.Sprintf("git %s: exit %d: %s",
		strings.Join(e.Args, " "), e.ExitCode, output)
}

// Repository returns the absolute worktree root containing start.
func Repository(ctx context.Context, start string) (string, error) {
	start = strings.TrimSpace(start)
	if start == "" {
		start = "."
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	out, code, err := runGit(ctx, abs, nil, nil,
		"rev-parse", "--show-toplevel", "--is-inside-work-tree")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", ErrNotRepository
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("gitkit: unexpected rev-parse output %q", strings.TrimSpace(string(out)))
	}
	root := strings.TrimSpace(lines[0])
	inside := strings.TrimSpace(lines[1])
	if root == "" {
		return "", ErrNotRepository
	}
	if inside != "true" {
		return "", fmt.Errorf("gitkit: %s is not a worktree", root)
	}
	return filepath.Clean(root), nil
}

// runGit executes one non-interactive Git command. A non-zero Git exit
// is returned as a code, not as an execution error.
func runGit(ctx context.Context, dir string, stdin []byte, extraEnv []string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(extraEnv)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return out, 0, nil
	}
	if ctx.Err() != nil {
		return out, -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	return out, -1, err
}

// gitEnv makes Git non-interactive and removes ambient settings that
// would otherwise steal stdin or invoke a pager.
func gitEnv(extra []string) []string {
	overrides := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "cat",
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"LC_ALL":              "C",
		"LANG":                "C",
		"PAGER":               "cat",
	}
	keys := make(map[string]bool, len(overrides))
	for key := range overrides {
		keys[key] = true
	}
	env := make([]string, 0, len(os.Environ())+len(overrides)+len(extra))
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && keys[key] {
			continue
		}
		env = append(env, item)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	env = append(env, extra...)
	return env
}

func gitCommandError(dir string, args []string, output []byte, code int) error {
	return &gitError{
		Args:     append([]string{"-C", dir}, args...),
		ExitCode: code,
		Output:   string(output),
	}
}

func checkpointEnv(now time.Time) []string {
	date := now.UTC().Format(time.RFC3339)
	return []string{
		"GIT_AUTHOR_NAME=" + checkpointAuthor,
		"GIT_AUTHOR_EMAIL=" + checkpointEmail,
		"GIT_COMMITTER_NAME=" + checkpointAuthor,
		"GIT_COMMITTER_EMAIL=" + checkpointEmail,
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_DATE=" + date,
	}
}
