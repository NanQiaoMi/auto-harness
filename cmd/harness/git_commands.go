package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/session"
)

// registerGitCommands wires the user-facing rollback surface. The model
// gets the same operations through the git and checkpoint tools.
func registerGitCommands(commands *ext.CommandRegistry, manager *gitkit.Manager, ag *agent.Agent, sess *session.Session, out io.Writer) {
	checkpoints := &ext.Command{
		Description: "list, create or restore Git-backed workspace checkpoints",
		Handler: func(ctx context.Context, args string) error {
			return handleCheckpointCommand(ctx, manager, args, out)
		},
	}
	commands.Register("checkpoints", checkpoints)

	rewind := &ext.Command{
		Description: "restore the previous workspace checkpoint: /rewind [id|number]",
		Handler: func(ctx context.Context, args string) error {
			return handleRewindCommand(ctx, manager, args, out)
		},
	}
	commands.Register("rewind", rewind)
	commands.Register("undo", &ext.Command{
		Description: "restore the workspace and conversation before the current turn",
		Handler: func(ctx context.Context, args string) error {
			return handleUndoTurnCommand(ctx, manager, ag, sess, args, out)
		},
	})
}

func handleCheckpointCommand(ctx context.Context, manager *gitkit.Manager, args string, out io.Writer) error {
	if manager == nil {
		return fmt.Errorf("workspace checkpoints are not configured")
	}
	args = strings.TrimSpace(args)
	if args == "" || args == "list" {
		items, err := manager.List(ctx)
		if err != nil {
			return checkpointCommandError(err)
		}
		fmt.Fprintln(out, gitkit.FormatCheckpoints(items))
		return nil
	}
	action, rest := splitCommandArgs(args)
	switch action {
	case "create":
		label := strings.TrimSpace(rest)
		if label == "" {
			label = "manual checkpoint"
		}
		item, created, err := manager.Create(ctx, label,
			gitkit.CheckpointManual, "repl")
		if err != nil {
			return checkpointCommandError(err)
		}
		if created {
			fmt.Fprintf(out, "created checkpoint %s (%s)\n", item.ID, item.Label)
		} else {
			fmt.Fprintf(out, "checkpoint %s already matches the current workspace\n", item.ID)
		}
		return nil
	case "restore":
		return restoreCheckpoint(ctx, manager, strings.TrimSpace(rest), true, out)
	default:
		return fmt.Errorf("usage: /checkpoints [list|create <label>|restore <id>]")
	}
}

func handleRewindCommand(ctx context.Context, manager *gitkit.Manager, args string, out io.Writer) error {
	if manager == nil {
		return fmt.Errorf("workspace checkpoints are not configured")
	}
	return restoreCheckpoint(ctx, manager, strings.TrimSpace(args), true, out)
}

func handleUndoTurnCommand(ctx context.Context, manager *gitkit.Manager, ag *agent.Agent, sess *session.Session, args string, out io.Writer) error {
	if manager == nil {
		return fmt.Errorf("workspace checkpoints are not configured")
	}
	if sess == nil {
		return fmt.Errorf("turn undo requires a session")
	}
	result, err := undoTurnWithConversation(ctx, manager, ag, sess, strings.TrimSpace(args))
	if err != nil {
		return checkpointCommandError(err)
	}
	fmt.Fprintln(out, gitkit.FormatTurnRewindResult(result))
	return nil
}

func undoTurnWithConversation(ctx context.Context, manager *gitkit.Manager, ag *agent.Agent, sess *session.Session, selector string) (gitkit.TurnRewindResult, error) {
	if manager == nil {
		return gitkit.TurnRewindResult{}, fmt.Errorf("workspace checkpoints are not configured")
	}
	if sess == nil {
		return gitkit.TurnRewindResult{}, fmt.Errorf("turn undo requires a session")
	}
	selector = strings.TrimSpace(selector)
	if selector != "" {
		items, err := manager.List(ctx)
		if err != nil {
			return gitkit.TurnRewindResult{}, checkpointCommandError(err)
		}
		selector, err = gitkit.ParseCheckpointOrdinal(items, selector)
		if err != nil {
			return gitkit.TurnRewindResult{}, err
		}
	}
	return manager.UndoTurn(ctx, &sessionConversation{agent: ag, session: sess}, selector)
}

func restoreCheckpoint(ctx context.Context, manager *gitkit.Manager, selector string, resetHead bool, out io.Writer) error {
	if selector != "" {
		items, err := manager.List(ctx)
		if err != nil {
			return checkpointCommandError(err)
		}
		selector, err = gitkit.ParseCheckpointOrdinal(items, selector)
		if err != nil {
			return err
		}
	}
	result, err := manager.Rewind(ctx, selector, resetHead)
	if err != nil {
		return checkpointCommandError(err)
	}
	fmt.Fprintln(out, gitkit.FormatRewindResult(result))
	return nil
}

type sessionConversation struct {
	agent   *agent.Agent
	session *session.Session
}

func (c *sessionConversation) SessionID() string {
	if c == nil || c.session == nil {
		return ""
	}
	return c.session.Meta.ID
}

func (c *sessionConversation) CurrentLeaf() string {
	if c == nil || c.session == nil {
		return ""
	}
	return c.session.CurrentLeaf()
}

func (c *sessionConversation) CurrentTurn() string {
	if c == nil || c.session == nil {
		return ""
	}
	return c.session.CurrentTurn()
}

func (c *sessionConversation) CheckpointAnchor() string {
	if c == nil || c.session == nil {
		return ""
	}
	return c.session.CheckpointAnchor()
}

func (c *sessionConversation) CanSetLeaf(id string) bool {
	if c == nil || c.session == nil {
		return false
	}
	return c.session.CanSetLeaf(id)
}

func (c *sessionConversation) SetLeaf(id string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("session is not configured")
	}
	return c.session.SetLeaf(id)
}

func (c *sessionConversation) ReloadHistory() {
	if c != nil && c.agent != nil {
		c.agent.ReloadHistory()
	}
}

func checkpointCommandError(err error) error {
	if errors.Is(err, gitkit.ErrNotRepository) {
		return fmt.Errorf("workspace checkpoints require a Git repository")
	}
	return err
}

// splitCommandArgs returns the first whitespace-delimited token and the
// untouched remainder.
func splitCommandArgs(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if i := strings.IndexAny(value, " \t"); i >= 0 {
		return value[:i], strings.TrimSpace(value[i+1:])
	}
	return value, ""
}
