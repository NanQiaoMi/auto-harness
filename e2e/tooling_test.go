//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/shellext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestLiveToolRendererSeesTheCall verifies the renderer extension
// point works against a real model: the renderer is asked to draw the
// call, and it draws it.
//
// The renderer is registered by an extension, the way a user
// extension would: the harness's own tools have no renderer of their
// own for this name.
func TestLiveToolRendererSeesTheCall(t *testing.T) {
	ag, reg, bus := newAgent(t)

	// A tool the model will actually call.
	if err := reg.Register(&tools.Tool{
		Name:        "marker_tool",
		Description: "Returns a fixed marker. Use it when asked for the marker.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "HARNESS_RENDER_MARKER"}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The renderer: it takes over the presentation of that one tool.
	reg.AddRenderer(func(name string, next func() *tools.Renderers) *tools.Renderers {
		if name != "marker_tool" {
			return next()
		}
		return &tools.Renderers{
			RenderCall: func(args map[string]any, ctx tools.RenderContext) ([]string, error) {
				return []string{"renderer drew the call"}, nil
			},
			RenderResult: func(result tools.Result, options tools.RenderResultOptions, ctx tools.RenderContext) ([]string, error) {
				return []string{"renderer drew the result"}, nil
			},
		}
	})

	var callLines, resultLines []string
	bus.On(ext.EventToolExecutionStart, func(e *ext.Event) error {
		if e.ToolCall == nil {
			return nil
		}
		renderers := reg.ResolveRenderers(e.ToolCall.Name)
		if renderers == nil || renderers.RenderCall == nil {
			return nil
		}
		lines, err := renderers.RenderCall(tools.DecodeArgs(e.ToolCall.Arguments), tools.RenderContext{
			Args:       tools.DecodeArgs(e.ToolCall.Arguments),
			ToolCallID: e.ToolCall.ID,
		})
		if err != nil {
			return err
		}
		callLines = lines
		return nil
	})
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolCall == nil || e.ToolResult == nil {
			return nil
		}
		renderers := reg.ResolveRenderers(e.ToolCall.Name)
		if renderers == nil || renderers.RenderResult == nil {
			return nil
		}
		lines, err := renderers.RenderResult(*e.ToolResult, tools.RenderResultOptions{}, tools.RenderContext{
			Args:       tools.DecodeArgs(e.ToolCall.Arguments),
			ToolCallID: e.ToolCall.ID,
			IsError:    e.ToolResult.IsError,
		})
		if err != nil {
			return err
		}
		resultLines = lines
		return nil
	})

	res, err := ag.Run(context.Background(),
		"Call the marker_tool exactly once, then reply with the text it returned.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(callLines) == 0 {
		t.Fatalf("the renderer was never asked to draw the call (final: %q)", res.Final)
	}
	if callLines[0] != "renderer drew the call" {
		t.Errorf("call lines = %v, want the renderer's line", callLines)
	}
	if len(resultLines) == 0 {
		t.Fatal("the renderer was never asked to draw the result")
	}
	if resultLines[0] != "renderer drew the result" {
		t.Errorf("result lines = %v, want the renderer's line", resultLines)
	}
}

// TestLiveShellShortcutFeedsTheModel verifies the "!" shortcut runs a
// command and puts its output in front of the model, which then uses
// it.
//
// The shortcut is the user's own command: it runs without the model's
// permission gate, but the model still learns what it printed.
func TestLiveShellShortcutFeedsTheModel(t *testing.T) {
	ag, _, bus := newAgent(t)

	const marker = "harness-shell-marker-4b9c"

	// The shortcut talks to the agent's own conversation, the way the
	// REPL wires it.
	shortcut := &shellext.Shortcut{
		Runner:       &shellext.ToolRunner{Tool: tools.NewBashTool()},
		Conversation: ag,
		Bus:          bus,
		Out:          os.Stderr,
	}

	handled, err := shortcut.Handle(context.Background(), "!echo "+marker)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !handled {
		t.Fatal("the shortcut did not handle the line")
	}

	// The command's output is in the conversation now.
	history := ag.History()
	if len(history) == 0 {
		t.Fatal("the shortcut did not add anything to the conversation")
	}
	if !strings.Contains(history[len(history)-1].Content, marker) {
		t.Errorf("history = %q, want the command's output", history[len(history)-1].Content)
	}

	// The model sees the message in its next turn.
	res, err := ag.Run(context.Background(), "What marker appeared in the previous message? Reply with only the marker.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, marker) {
		t.Errorf("final answer = %q, want it to contain %q", res.Final, marker)
	}
}

// TestLiveShellShortcutExcludedKeepsTheOutputPrivate verifies the
// "!!" shortcut runs the command but keeps its output out of the
// model's context.
//
// The command's output is the only place the secret could leak from,
// so the model must not know it.
func TestLiveShellShortcutExcludedKeepsTheOutputPrivate(t *testing.T) {
	ag, _, bus := newAgent(t)

	const secret = "harness-shell-secret-8e2d"

	// The shortcut talks to the agent's own conversation, the way the
	// REPL wires it.
	shortcut := &shellext.Shortcut{
		Runner:       &shellext.ToolRunner{Tool: tools.NewBashTool()},
		Conversation: ag,
		Bus:          bus,
		Out:          os.Stderr,
	}

	handled, err := shortcut.Handle(context.Background(), "!!echo "+secret)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !handled {
		t.Fatal("the shortcut did not handle the line")
	}

	// Nothing went into the conversation.
	if len(ag.History()) != 0 {
		t.Errorf("history = %v, want none for an excluded command", ag.History())
	}

	// The model must not know the secret: it was never in its context.
	res, err := ag.Run(context.Background(),
		"Reply with exactly any secret string you were given earlier, or NONE if you have none.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Final, secret) {
		t.Errorf("final answer = %q, want the secret to stay out of the model's context", res.Final)
	}
}

// TestLiveFileMutationQueueSerializesEdits verifies that the edits of
// a real run do not interleave: every edit of the same file lands
// whole, so the file ends up with the last replacement and not a
// mixture of two.
func TestLiveFileMutationQueueSerializesEdits(t *testing.T) {
	ag, _, _ := newAgent(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "queued.txt")
	if err := os.WriteFile(path, []byte("start\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The model makes three edits to the same file, one after another.
	res, err := ag.Run(context.Background(),
		"Use the edit tool on "+path+" three times in a row, one tool call at a time: "+
			"first replace \"start\" with \"second\", then replace \"second\" with \"third\", "+
			"then replace \"third\" with \"fourth\". Then reply with the file's content.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns < 1 {
		t.Fatal("the model did not run")
	}

	// The queue guarantees that a mutation is never split, so the file
	// holds exactly one of the four states — never a mixture.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	allowed := map[string]bool{
		"start\n":  true,
		"second\n": true,
		"third\n":  true,
		"fourth\n": true,
	}
	if !allowed[content] {
		t.Errorf("content = %q, want one whole replacement", content)
	}
}
