package ext

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestRegisterToolRendererLetsAnExtensionTakeOver verifies the
// extension API can take over the presentation of a tool it does not
// own.
func TestRegisterToolRendererLetsAnExtensionTakeOver(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	api := NewAPI(bus, reg, NewCommandRegistry())

	// A tool the extension does not own.
	tool := &tools.Tool{
		Name: "mcp__docs__search",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "ok"}, nil
		},
	}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	api.RegisterToolRenderer(func(name string, next func() *tools.Renderers) *tools.Renderers {
		if name != "mcp__docs__search" {
			return next()
		}
		return &tools.Renderers{
			RenderCall: func(args map[string]any, ctx tools.RenderContext) ([]string, error) {
				return []string{"searching the docs"}, nil
			},
		}
	})

	renderers := reg.ResolveRenderers("mcp__docs__search")
	if renderers == nil || renderers.RenderCall == nil {
		t.Fatal("the extension's renderer was not found")
	}
	lines, err := renderers.RenderCall(nil, tools.RenderContext{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || lines[0] != "searching the docs" {
		t.Errorf("lines = %v, want the extension's rendering", lines)
	}
}

// TestRegisterToolRendererCanDeferToTheNext verifies a resolver may
// pass the question on, so the tool's own renderers still win.
func TestRegisterToolRendererCanDeferToTheNext(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	api := NewAPI(bus, reg, NewCommandRegistry())

	tool := &tools.Tool{
		Name: "custom",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "ok"}, nil
		},
		Renderers: &tools.Renderers{
			RenderCall: func(args map[string]any, ctx tools.RenderContext) ([]string, error) {
				return []string{"the tool's own"}, nil
			},
		},
	}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	api.RegisterToolRenderer(func(name string, next func() *tools.Renderers) *tools.Renderers {
		return next()
	})

	renderers := reg.ResolveRenderers("custom")
	lines, _ := renderers.RenderCall(nil, tools.RenderContext{})
	if len(lines) != 1 || lines[0] != "the tool's own" {
		t.Errorf("lines = %v, want the tool's own rendering", lines)
	}
}

// TestUserBashEventCarriesTheCommand verifies the user_bash event
// reaches the handlers with the command the user typed.
func TestUserBashEventCarriesTheCommand(t *testing.T) {
	bus := NewBus()
	var got string
	var excluded bool
	bus.On(EventUserBash, func(e *Event) error {
		got = e.BashCommand
		excluded = e.ExcludeFromContext
		return nil
	})
	if err := bus.Emit(&Event{
		Type:               EventUserBash,
		BashCommand:        "ls -la",
		ExcludeFromContext: true,
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got != "ls -la" {
		t.Errorf("command = %q, want %q", got, "ls -la")
	}
	if !excluded {
		t.Error("the excluded flag was not carried")
	}
}

// TestUserBashCanBeBlocked verifies an extension can stop the command.
func TestUserBashCanBeBlocked(t *testing.T) {
	bus := NewBus()
	bus.On(EventUserBash, func(e *Event) error {
		e.Block = true
		e.BlockReason = "not here"
		return nil
	})
	ev := &Event{Type: EventUserBash, BashCommand: "rm -rf /"}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Error("the command should have been blocked")
	}
	if !strings.Contains(ev.BlockReason, "not here") {
		t.Errorf("reason = %q", ev.BlockReason)
	}
}

// TestReloadDoesNotDoubleRegisterTheRenderer verifies a reloaded
// extension's renderer is registered once.
//
// The renderer is not a tool or a command, so the manager cannot
// unregister it by name: the scope has to record the removal.
func TestReloadDoesNotDoubleRegisterTheRenderer(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	api := NewAPI(bus, reg, NewCommandRegistry())
	mgr := NewManager(api)

	// The extension counts how often its renderer was asked.
	var asked int
	ext := Named("renderer", func(api *API) error {
		api.RegisterToolRenderer(func(name string, next func() *tools.Renderers) *tools.Renderers {
			asked++
			return next()
		})
		return nil
	})

	if err := mgr.Load(ext); err != nil {
		t.Fatalf("load: %v", err)
	}
	reg.ResolveRenderers("anything")
	if asked != 1 {
		t.Fatalf("asked = %d, want 1", asked)
	}

	// Reload: the old resolver must go, and the new one must take its
	// place — not stack on top of it.
	if err := mgr.Reload(ext); err != nil {
		t.Fatalf("reload: %v", err)
	}
	asked = 0
	reg.ResolveRenderers("anything")
	if asked != 1 {
		t.Errorf("asked = %d, want 1 after the reload", asked)
	}
}
