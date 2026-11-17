package gitkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NanQiaoMi/auto-harness/tools"
)

const (
	defaultGitTimeout = 120 * time.Second
	maxGitOutput      = 30000
	maxGitStdin       = 10 << 20
)

// NewGitTool returns a Git porcelain tool. Arguments are passed as an
// array to the Git executable, never through a shell.
func NewGitTool(start string) *tools.Tool {
	return &tools.Tool{
		Name:  "git",
		Label: "Git",
		Description: "Run a non-interactive Git command with arguments passed directly, without a shell. " +
			"Use it for status, diff, log, add, commit, branch, switch, merge, rebase, cherry-pick, revert, reset, stash, remote and tags. " +
			"Mutating calls are snapshotted first when workspace checkpoints are enabled.",
		PromptSnippet: "git: run Git with an argument array; mutating calls get an automatic checkpoint",
		PromptGuidelines: []string{
			"Pass Git arguments as an array, for example [\"status\", \"--short\"].",
			"Do not wrap the command in `git`; the tool already runs Git.",
			"The tool is non-interactive: pass commit messages with -m or -F -, and patches through stdin.",
			"Use the checkpoint or git tool to inspect/restore workspace state instead of destructive shell pipelines.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"args": map[string]any{
					"type":        "array",
					"description": "Git arguments, without the leading git executable",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Optional repository-relative directory to run Git from",
				},
				"stdin": map[string]any{
					"type":        "string",
					"description": "Optional standard input (for example a patch for git apply, or a message for commit -F -); maximum 10 MiB",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Timeout in milliseconds (default 120000)",
				},
			},
			"required": []string{"args"},
		},
		Execute: func(ctx context.Context, callID string, raw json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			var params struct {
				Args      []string `json:"args"`
				Workdir   string   `json:"workdir"`
				Stdin     string   `json:"stdin"`
				TimeoutMS int      `json:"timeout_ms"`
			}
			if err := json.Unmarshal(raw, &params); err != nil {
				return tools.Result{Content: fmt.Sprintf("invalid git arguments: %v", err), IsError: true}, nil
			}
			if len(params.Args) == 0 {
				return tools.Result{Content: "git args must not be empty", IsError: true}, nil
			}
			if len(params.Stdin) > maxGitStdin {
				return tools.Result{
					Content:  fmt.Sprintf("git stdin exceeds the %d-byte limit", maxGitStdin),
					IsError:  true,
					ExitCode: 1,
				}, nil
			}
			for _, arg := range params.Args {
				if strings.ContainsRune(arg, 0) {
					return tools.Result{Content: "git arguments must not contain NUL", IsError: true}, nil
				}
			}
			root, err := Repository(ctx, start)
			if err != nil {
				return tools.Result{Content: err.Error(), IsError: true}, nil
			}
			dir := root
			if strings.TrimSpace(params.Workdir) != "" {
				dir, err = gitWorkdir(root, params.Workdir)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
			}
			timeout := defaultGitTimeout
			if params.TimeoutMS > 0 {
				timeout = time.Duration(params.TimeoutMS) * time.Millisecond
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			var stdin []byte
			if params.Stdin != "" {
				stdin = []byte(params.Stdin)
			}
			out, code, err := runGit(runCtx, dir, stdin, nil, params.Args...)
			if runCtx.Err() == context.DeadlineExceeded {
				return tools.Result{
					Content:  fmt.Sprintf("git command timed out after %s\n%s", timeout, truncateGit(string(out))),
					IsError:  true,
					ExitCode: 124,
				}, nil
			}
			if err != nil {
				return tools.Result{Content: fmt.Sprintf("git failed: %v", err), IsError: true, ExitCode: 1}, nil
			}
			content := truncateGit(string(out))
			if strings.TrimSpace(content) == "" {
				content = "(no output)"
			}
			if code != 0 {
				content = fmt.Sprintf("%s\n[exit %d]", content, code)
			}
			return tools.Result{
				Content:  content,
				IsError:  code != 0,
				ExitCode: code,
			}, nil
		},
	}
}

func gitWorkdir(root, value string) (string, error) {
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("git workdir must be relative to the repository")
	}
	dir := filepath.Join(root, filepath.FromSlash(value))
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("git workdir escapes the repository")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("git workdir %s is not a directory", value)
	}
	return dir, nil
}

func truncateGit(value string) string {
	if len(value) <= maxGitOutput {
		return value
	}
	return value[:maxGitOutput] +
		fmt.Sprintf("\n... [truncated, %d bytes total]", len(value))
}
