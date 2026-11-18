package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/orchestrator"
)

// registerAgentCommands exposes delegated tasks to the user. The model
// uses the agent tool; this is the human inspection and control path.
func registerAgentCommands(commands *ext.CommandRegistry, manager *orchestrator.Manager, out io.Writer) {
	if manager == nil {
		return
	}
	commands.Register("agents", &ext.Command{
		Description: "list, inspect, wait for or cancel delegated agents",
		Handler: func(ctx context.Context, args string) error {
			return handleAgentsCommand(ctx, manager, args, out)
		},
	})
}

func handleAgentsCommand(ctx context.Context, manager *orchestrator.Manager, args string, out io.Writer) error {
	args = strings.TrimSpace(args)
	if args == "" || args == "list" {
		tasks := manager.List("", true)
		if len(tasks) == 0 {
			fmt.Fprintln(out, "no delegated agents")
			return nil
		}
		fmt.Fprintf(out, "%-34s %-12s %5s  %s\n", "ID", "STATE", "TURNS", "NAME")
		for _, task := range tasks {
			fmt.Fprintf(out, "%-34s %-12s %5d  %s\n",
				task.ID, task.State, task.Turns, task.Name)
		}
		return nil
	}
	action, rest := splitCommandArgs(args)
	switch action {
	case "show":
		task, err := lookupAgent(manager, rest)
		if err != nil {
			return err
		}
		return writeAgentJSON(out, task)
	case "wait":
		id, timeoutArg := splitCommandArgs(rest)
		task, err := lookupAgent(manager, id)
		if err != nil {
			return err
		}
		timeout := 30 * time.Second
		if strings.TrimSpace(timeoutArg) != "" {
			seconds, parseErr := strconv.Atoi(strings.TrimSpace(timeoutArg))
			if parseErr != nil || seconds < 1 {
				return fmt.Errorf("usage: /agents wait <id> [timeout-seconds]")
			}
			timeout = time.Duration(seconds) * time.Second
			if timeout > 10*time.Minute {
				timeout = 10 * time.Minute
			}
		}
		result, done, err := manager.Wait(ctx, task.ID, timeout)
		if err != nil {
			return err
		}
		if err := writeAgentJSON(out, result); err != nil {
			return err
		}
		if !done {
			fmt.Fprintf(out, "\nstill running after %s; use /agents wait %s again\n", timeout, task.ID)
		}
		return nil
	case "cancel":
		task, err := lookupAgent(manager, rest)
		if err != nil {
			return err
		}
		if err := manager.Cancel(ctx, task.ID); err != nil {
			return err
		}
		current, _ := manager.Get(task.ID)
		fmt.Fprintf(out, "cancelled %s (%s)\n", current.ID, current.State)
		return nil
	default:
		return fmt.Errorf("usage: /agents [list|show <id>|wait <id> [seconds]|cancel <id>]")
	}
}

func lookupAgent(manager *orchestrator.Manager, selector string) (orchestrator.Task, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return orchestrator.Task{}, fmt.Errorf("an agent ID is required")
	}
	task, ok := manager.Get(selector)
	if ok {
		return task, nil
	}
	matches := make([]orchestrator.Task, 0)
	for _, candidate := range manager.List("", true) {
		if strings.HasPrefix(candidate.ID, selector) {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return orchestrator.Task{}, fmt.Errorf("no delegated agent matches %q", selector)
	case 1:
		return matches[0], nil
	default:
		return orchestrator.Task{}, fmt.Errorf("delegated agent prefix %q is ambiguous", selector)
	}
}

func writeAgentJSON(out io.Writer, task orchestrator.Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}
