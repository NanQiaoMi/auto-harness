package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/NanQiaoMi/auto-harness/shellext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// checkpointBefore is the subset of gitkit.Manager used by the user's
// shell shortcut. Keeping the interface small makes the fail-closed
// behavior testable without a repository.
type checkpointBefore interface {
	BeforeTool(ctx context.Context, toolName string, args json.RawMessage) error
}

// workspaceCheckpointRunner snapshots the workspace before a user shell
// command, so `!cmd` and RPC `bash` participate in the same rollback
// chain as model tool calls.
type workspaceCheckpointRunner struct {
	checkpointer checkpointBefore
	runner       shellext.Runner
}

func (r *workspaceCheckpointRunner) Run(ctx context.Context, command string) (tools.Result, error) {
	if r.checkpointer != nil {
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			return tools.Result{Content: err.Error(), IsError: true, ExitCode: 1}, nil
		}
		if err := r.checkpointer.BeforeTool(ctx, "user_bash", args); err != nil {
			return tools.Result{
				Content: fmt.Sprintf(
					"workspace checkpoint failed; refusing to run the shell command: %v", err),
				IsError:  true,
				ExitCode: 1,
			}, nil
		}
	}
	return r.runner.Run(ctx, command)
}
