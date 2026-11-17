package gitkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NanQiaoMi/auto-harness/tools"
)

// NewCheckpointTool returns the agent-facing workspace checkpoint tool.
func NewCheckpointTool(manager *Manager) *tools.Tool {
	return &tools.Tool{
		Name:  "checkpoint",
		Label: "Workspace Checkpoint",
		Description: "Create, list or restore Git-backed workspace checkpoints. " +
			"A restore rolls back files, the exact index (including merge conflicts), HEAD, branches, tags, remote refs and the stash stack.",
		PromptSnippet: "checkpoint: list/create/restore rollback points for the Git worktree",
		PromptGuidelines: []string{
			"Use action=list before restoring when you are unsure which step to undo.",
			"reset_head=false restores only files and the index; the default also restores Git history and refs.",
			"Restores refuse to change a branch that is checked out in another worktree.",
			"Automatic checkpoints are taken before mutating tools when the harness runs inside a Git repository.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "create, list or restore",
					"enum":        []string{"create", "list", "restore"},
				},
				"label": map[string]any{
					"type":        "string",
					"description": "Label for action=create",
				},
				"id": map[string]any{
					"type":        "string",
					"description": "Checkpoint ID, ID prefix, or 1-based list number for action=restore",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum checkpoints to list (default 20)",
				},
				"reset_head": map[string]any{
					"type":        "boolean",
					"description": "Restore HEAD, commits, branches, tags, remote refs and the stash stack too (default true)",
				},
			},
			"required": []string{"action"},
		},
		Execute: func(ctx context.Context, callID string, raw json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			var params struct {
				Action    string `json:"action"`
				Label     string `json:"label"`
				ID        string `json:"id"`
				Limit     int    `json:"limit"`
				ResetHead *bool  `json:"reset_head"`
			}
			if err := json.Unmarshal(raw, &params); err != nil {
				return tools.Result{Content: fmt.Sprintf("invalid checkpoint arguments: %v", err), IsError: true}, nil
			}
			if manager == nil {
				return tools.Result{Content: "workspace checkpoints are not configured", IsError: true}, nil
			}
			switch strings.TrimSpace(params.Action) {
			case "create":
				item, created, err := manager.Create(ctx, params.Label, CheckpointManual, "checkpoint")
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				state := "already current"
				if created {
					state = "created"
				}
				return tools.Result{Content: fmt.Sprintf("%s checkpoint %s (%s), HEAD %s",
					state, item.ID, item.Label, checkpointHead(item))}, nil
			case "list":
				items, err := manager.List(ctx)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				limit := params.Limit
				if limit <= 0 {
					limit = 20
				}
				if limit < len(items) {
					items = items[:limit]
				}
				return tools.Result{Content: FormatCheckpoints(items)}, nil
			case "restore":
				selector := strings.TrimSpace(params.ID)
				if selector == "" {
					return tools.Result{Content: "checkpoint restore requires id", IsError: true}, nil
				}
				items, err := manager.List(ctx)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				selector, err = ParseCheckpointOrdinal(items, selector)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				resetHead := true
				if params.ResetHead != nil {
					resetHead = *params.ResetHead
				}
				result, err := manager.Rewind(ctx, selector, resetHead)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				return tools.Result{Content: FormatRewindResult(result)}, nil
			default:
				return tools.Result{
					Content: "checkpoint action must be create, list or restore",
					IsError: true,
				}, nil
			}
		},
	}
}

func checkpointHead(item Checkpoint) string {
	if item.Head == "" {
		return "unborn"
	}
	return shortOID(item.Head)
}

// FormatRewindResult formats one restore for the REPL and tools.
func FormatRewindResult(result RewindResult) string {
	if result.Noop {
		return fmt.Sprintf("checkpoint %s already matches the workspace", result.Restored.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "restored checkpoint %s (%s)\n", result.Restored.ID, result.Restored.Label)
	if result.HeadReset {
		fmt.Fprintf(&b, "HEAD reset to %s\n", checkpointHead(result.Restored))
	}
	if result.RefsReset {
		b.WriteString("Git refs and stash restored\n")
	}
	if result.Safety.ID != "" {
		fmt.Fprintf(&b, "safety checkpoint: %s\n", result.Safety.ID)
	}
	if result.Warning != "" {
		fmt.Fprintf(&b, "warning: %s\n", result.Warning)
	}
	return strings.TrimRight(b.String(), "\n")
}
