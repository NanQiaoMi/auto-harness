package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/NanQiaoMi/auto-harness/tools"
)

const maxWait = 10 * time.Minute

// NewAgentTool returns the model-facing delegation tool.
func NewAgentTool(manager *Manager) *tools.Tool {
	return &tools.Tool{
		Name:  "agent",
		Label: "Delegated Agent",
		// Spawning does not mutate the parent workspace. A worktree
		// task snapshots itself and enforces its own checkpoints.
		ReadOnly: true,
		Description: "Delegate work to an isolated background agent with its own conversation and context budget. " +
			"Use spawn to start a task, wait to collect its bounded result, send for a follow-up, status/list to inspect, and cancel to stop it.",
		PromptSnippet: "agent: spawn, wait, send, inspect or cancel a delegated agent",
		PromptGuidelines: []string{
			"Delegate independent, substantial work; do not delegate a one-line edit.",
			"Give each subagent a self-contained task: goal, relevant paths, constraints, acceptance checks and what to report.",
			"The parent receives a bounded summary, not the full child transcript; use wait after spawn when the result is needed.",
			"Prefer isolation=auto. It uses a separate Git worktree when available and reports when it falls back to the shared workspace.",
			"A delegated agent inherits permission and telemetry hooks, but gets a fresh context instead of the parent conversation.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "spawn, send, wait, status, list or cancel",
					"enum":        []string{"spawn", "send", "wait", "status", "list", "cancel"},
				},
				"id": map[string]any{
					"type":        "string",
					"description": "Delegated task ID for send, wait, status and cancel",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "Self-contained task or follow-up prompt",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Short display name for spawn",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override for spawn",
				},
				"isolation": map[string]any{
					"type":        "string",
					"description": "auto uses a Git worktree when available; worktree requires one; shared is deliberately unisolated",
					"enum":        []string{"auto", "worktree", "shared"},
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "For wait: maximum time to block (default 30000, max 600000)",
				},
				"task_timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "For spawn: maximum duration of one child run (optional)",
				},
				"recursive": map[string]any{
					"type":        "boolean",
					"description": "For list: include the full delegated subtree (default false)",
				},
			},
			"required": []string{"action"},
		},
		Execute: func(ctx context.Context, callID string, raw json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			if manager == nil {
				return tools.Result{Content: "delegated agents are not configured", IsError: true}, nil
			}
			var params struct {
				Action             string `json:"action"`
				ID                 string `json:"id"`
				Prompt             string `json:"prompt"`
				Name               string `json:"name"`
				Model              string `json:"model"`
				Isolation          string `json:"isolation"`
				TimeoutMS          int    `json:"timeout_ms"`
				TaskTimeoutSeconds int    `json:"task_timeout_seconds"`
				Recursive          bool   `json:"recursive"`
			}
			if err := json.Unmarshal(raw, &params); err != nil {
				return tools.Result{
					Content: fmt.Sprintf("invalid agent arguments: %v", err),
					IsError: true,
				}, nil
			}
			scope := TaskID(ctx)
			switch strings.TrimSpace(params.Action) {
			case "spawn":
				isolation := Isolation(strings.TrimSpace(params.Isolation))
				if isolation == "" {
					isolation = IsolationAuto
				}
				task, err := manager.Spawn(ctx, SpawnRequest{
					Prompt:    params.Prompt,
					Name:      params.Name,
					Model:     params.Model,
					Isolation: isolation,
					Timeout:   time.Duration(params.TaskTimeoutSeconds) * time.Second,
				})
				if err != nil {
					return toolError(err), nil
				}
				return encodeTaskResult("spawned", task, false)
			case "send":
				id, err := accessibleTask(manager, scope, params.ID)
				if err != nil {
					return toolError(err), nil
				}
				if err := manager.Send(ctx, id, params.Prompt); err != nil {
					return toolError(err), nil
				}
				task, _ := manager.Get(id)
				return encodeTaskResult("sent", task, false)
			case "wait":
				id, err := accessibleTask(manager, scope, params.ID)
				if err != nil {
					return toolError(err), nil
				}
				timeout := 30 * time.Second
				if params.TimeoutMS > 0 {
					timeout = time.Duration(params.TimeoutMS) * time.Millisecond
				}
				if timeout > maxWait {
					timeout = maxWait
				}
				task, done, err := manager.Wait(ctx, id, timeout)
				if err != nil {
					return toolError(err), nil
				}
				return encodeTaskResult("waited", task, !done)
			case "status":
				id, err := accessibleTask(manager, scope, params.ID)
				if err != nil {
					return toolError(err), nil
				}
				task, _ := manager.Get(id)
				return encodeTaskResult("status", task, false)
			case "list":
				tasks := manager.List(scope, params.Recursive)
				return encodeListResult(tasks), nil
			case "cancel":
				id, err := accessibleTask(manager, scope, params.ID)
				if err != nil {
					return toolError(err), nil
				}
				if err := manager.Cancel(ctx, id); err != nil {
					return toolError(err), nil
				}
				task, _ := manager.Get(id)
				return encodeTaskResult("cancelled", task, false)
			default:
				return tools.Result{
					Content:  "agent action must be spawn, send, wait, status, list or cancel",
					IsError:  true,
					ExitCode: 1,
				}, nil
			}
		},
	}
}

func accessibleTask(manager *Manager, scope, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("agent action requires id")
	}
	if !manager.CanAccess(scope, id) {
		return "", fmt.Errorf("delegated task %s is not visible from this agent", id)
	}
	return id, nil
}

func toolError(err error) tools.Result {
	return tools.Result{Content: err.Error(), IsError: true, ExitCode: 1}
}

func encodeTaskResult(action string, task Task, timedOut bool) (tools.Result, error) {
	payload := struct {
		Action   string `json:"action"`
		TimedOut bool   `json:"timedOut,omitempty"`
		Task     Task   `json:"task"`
	}{Action: action, TimedOut: timedOut, Task: modelTask(task)}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return toolError(err), nil
	}
	return tools.Result{Content: string(data)}, nil
}

func encodeListResult(tasks []Task) tools.Result {
	if tasks == nil {
		tasks = []Task{}
	}
	for i := range tasks {
		tasks[i] = modelTask(tasks[i])
	}
	payload := struct {
		Action string `json:"action"`
		Tasks  []Task `json:"tasks"`
	}{Action: "list", Tasks: tasks}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return toolError(err)
	}
	return tools.Result{Content: string(data)}
}

// modelTask omits the original prompt from model-facing task snapshots.
// The prompt is already in the child session and may be large; echoing
// it would bypass the parent's bounded-result contract.
func modelTask(task Task) Task {
	task.Prompt = ""
	return task
}
