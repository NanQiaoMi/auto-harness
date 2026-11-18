package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const defaultBashTimeout = 120 * time.Second

// SandboxRunner runs a shell command inside an operating-system
// sandbox. It is set on a bash tool to confine what the command can
// touch; leaving it nil runs the command unchanged.
type SandboxRunner func(ctx context.Context, command, workdir string) (output string, sandboxed bool, err error)

// NewBashTool returns the shell execution tool. Commands run via
// /bin/bash -c with a timeout; stdout and stderr are merged and truncated.
func NewBashTool() *Tool {
	return newBashToolAt("", nil)
}

// NewSandboxedBashTool returns a bash tool whose commands run inside
// the given operating-system sandbox.
func NewSandboxedBashTool(runner SandboxRunner) *Tool {
	return newBashToolAt("", runner)
}

// NewBashToolAt returns a bash tool whose default directory is root and
// whose relative workdir argument is confined to root.
func NewBashToolAt(root string) *Tool {
	return newBashToolAt(root, nil)
}

// NewSandboxedBashToolAt is NewBashToolAt with an OS sandbox runner.
func NewSandboxedBashToolAt(root string, runner SandboxRunner) *Tool {
	return newBashToolAt(root, runner)
}

// newBashToolAt builds the bash tool with an optional workspace root
// and sandbox runner.
func newBashToolAt(root string, runner SandboxRunner) *Tool {
	return &Tool{
		Name:          "bash",
		Label:         "Bash",
		Description:   "Run a shell command and return its output. The command runs non-interactively with stdout/stderr merged.",
		PromptSnippet: "bash: run a shell command (state changes persist across calls)",
		PromptGuidelines: []string{
			"Prefer dedicated tools (read/write/edit) for file operations.",
			"Long-running commands: set timeout_ms and run in background yourself if needed.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Timeout in milliseconds (default 120000)",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory for the command (optional)",
				},
			},
			"required": []string{"command"},
		},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error) {
			var params struct {
				Command   string `json:"command"`
				TimeoutMS int    `json:"timeout_ms"`
				Workdir   string `json:"workdir"`
			}
			if err := decodeArgs(args, &params); err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			workdir, err := resolveCommandWorkdir(root, params.Workdir)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			timeout := defaultBashTimeout
			if params.TimeoutMS > 0 {
				timeout = time.Duration(params.TimeoutMS) * time.Millisecond
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			var out []byte
			if runner != nil {
				// The sandbox runner reports whether it actually
				// confined the command.
				text, sandboxed, runErr := runner(runCtx, params.Command, workdir)
				out = []byte(text)
				err = runErr
				if !sandboxed {
					out = append(out, "\n[no sandbox available: the command ran unrestricted]"...)
				}
			} else {
				cmd := exec.CommandContext(runCtx, "/bin/bash", "-c", params.Command)
				if workdir != "" {
					cmd.Dir = workdir
				}
				out, err = cmd.CombinedOutput()
			}
			result := Result{Content: truncate(string(out))}
			if runCtx.Err() == context.DeadlineExceeded {
				result.Content = truncate(string(out)) + fmt.Sprintf("\n[command timed out after %s]", timeout)
				result.IsError = true
				return result, nil
			}
			if err != nil {
				if result.Content == "" {
					result.Content = fmt.Sprintf("command failed: %v", err)
				}
				result.IsError = true
				// The exit status is worth keeping: a caller that runs
				// a command on the user's behalf needs to know whether
				// it worked, not just that it did not.
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					result.ExitCode = exitErr.ExitCode()
				} else {
					result.ExitCode = 1
				}
			}
			if result.Content == "" {
				result.Content = "(no output)"
			}
			return result, nil
		},
	}
}

// resolveCommandWorkdir resolves the bash tool's optional working
// directory. With a workspace root, absolute paths and relative paths
// must stay inside it.
func resolveCommandWorkdir(root, value string) (string, error) {
	if root == "" {
		return value, nil
	}
	if value == "" {
		return root, nil
	}
	dir, err := resolveWorkspacePath(root, value)
	if err != nil {
		return "", fmt.Errorf("bash workdir: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("bash workdir %s is not a directory", value)
	}
	return dir, nil
}
