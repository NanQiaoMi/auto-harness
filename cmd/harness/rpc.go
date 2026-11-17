package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/rpc"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/shellext"
)

// runRPCMode runs the harness as a long-lived subprocess driven by
// JSON records on stdin.
//
// The mode is headless: stdout carries the protocol and nothing else,
// so the diagnostics that the REPL would print on stdout go to stderr
// instead. A client that reads stdout must never find anything but
// records there.
//
// The command registry is filled with the builtins, the prompt
// templates and the skills, so a client can ask what the harness
// knows and invoke any of it.
func runRPCMode(ctx context.Context, ag *agent.Agent, sess *session.Session, bus *ext.Bus, commands *ext.CommandRegistry, cfg *config.Config, llmReg *llm.Registry, extMgr *ext.Manager, loader *extensionLoader, mcpSetup *mcpSetup, permSetup *permSetup, telemetrySetup *telemetrySetup, shellShortcut *shellext.Shortcut, promptSetup *promptSetup, skillSetup *skillSetup, trustSetup *trustSetup, profileSetup *profileSetup, checkpointMgr *gitkit.Manager, quiet bool) error {
	// The builtins go on the registry so get_commands reports them and
	// a client can invoke them.
	commandOut := newCommandOutput(os.Stderr)
	registerBuiltinCommands(commands, ag, sess, bus, cfg, llmReg, extMgr, loader,
		mcpSetup, permSetup, telemetrySetup, trustSetup, profileSetup, skillSetup,
		promptSetup, checkpointMgr, commandOut, &quiet)
	if promptSetup != nil {
		promptSetup.install(commands, ag)
	}
	if skillSetup != nil {
		skillSetup.install(commands, ag)
	}

	server := rpc.NewServer(ag, sess, commands, os.Stdout)
	// The RPC server can replace the session (new_session), so resolve
	// the binding at checkpoint time instead of capturing the startup
	// session. User-shell snapshots belong to the current position, not
	// to a model turn.
	checkpointMgr.BindSession = func(kind gitkit.CheckpointKind, tool string) gitkit.SessionBinding {
		return checkpointSessionBinding(server.CurrentSession(), kind, tool)
	}
	server.SessionDir = cfg.SessionDir
	server.Profile = func() any {
		provider := ""
		if ag.Provider() != nil {
			provider = ag.Provider().Name()
		}
		payload := profileSetup.rpcPayload(cfg, ag.Tools, loader, ag.Config().Model, provider)
		payload.MaxTurns = ag.Config().MaxTurns
		payload.MaxTokens = ag.Config().MaxTokens
		return payload
	}
	server.Plugins = func() any {
		return pluginsPayload(loader, extMgr)
	}
	if checkpointMgr != nil {
		server.Checkpoints = checkpointMgr.List
		server.CreateCheckpoint = func(ctx context.Context, label string) (gitkit.Checkpoint, bool, error) {
			if strings.TrimSpace(label) == "" {
				label = "manual checkpoint"
			}
			return checkpointMgr.Create(ctx, label, gitkit.CheckpointManual, "rpc")
		}
		server.RewindCheckpoint = func(ctx context.Context, selector string, resetHead bool) (gitkit.RewindResult, error) {
			return checkpointMgr.Rewind(ctx, selector, resetHead)
		}
		server.UndoTurn = func(ctx context.Context, selector string) (gitkit.TurnRewindResult, error) {
			current := server.CurrentSession()
			if current == nil {
				return gitkit.TurnRewindResult{}, fmt.Errorf("turn undo requires a session")
			}
			return undoTurnWithConversation(ctx, checkpointMgr, ag, current, selector)
		}
	}
	server.Models = func() []rpc.ModelInfo {
		models := llmReg.Models()
		out := make([]rpc.ModelInfo, 0, len(models))
		for _, m := range models {
			out = append(out, rpc.ModelInfo{
				ID:       m.ID,
				Name:     m.Name,
				Provider: m.Provider,
				Protocol: m.Protocol,
			})
		}
		return out
	}
	// The bash command runs through the same tool the model uses, so
	// the sandbox and the permission layer apply to it.
	if shellShortcut != nil {
		runner := shellShortcut.Runner
		server.Bash = func(ctx context.Context, command string) (string, int, error) {
			result, err := runner.Run(ctx, command)
			if err != nil {
				return result.Content, result.ExitCode, err
			}
			// A non-zero exit is a failure even though the tool
			// reported no error: the tool says so in the result, and
			// the client needs to see it.
			if result.IsError {
				return result.Content, result.ExitCode, fmt.Errorf("command failed")
			}
			return result.Content, result.ExitCode, nil
		}
		// The command is the client's own, so it is not the model's
		// permission gate's business — but an extension that vetoes
		// a user's shell command must veto it here too. The event is
		// the same one the "!" shortcut emits.
		server.Gate = func(command string) error {
			ev := &ext.Event{Type: ext.EventUserBash, BashCommand: command}
			if err := bus.Emit(ev); err != nil {
				return err
			}
			if ev.Block {
				reason := ev.BlockReason
				if reason == "" {
					reason = "blocked by an extension"
				}
				return fmt.Errorf("shellext: %s", reason)
			}
			return nil
		}
	}

	// The events go to the client: the harness's own bus becomes the
	// event stream.
	unsubscribe := server.Install(bus)
	defer unsubscribe()

	if err := server.Serve(ctx, os.Stdin); err != nil {
		return fmt.Errorf("rpc: %w", err)
	}
	return nil
}
