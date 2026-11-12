package ext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mimi/auto-harness/tools"
)

func TestBusHandlerOrderAndUnsubscribe(t *testing.T) {
	bus := NewBus()
	var got []string
	un1 := bus.On(EventTurnStart, func(e *Event) error {
		got = append(got, "a")
		return nil
	})
	bus.On(EventTurnStart, func(e *Event) error {
		got = append(got, "b")
		return nil
	})
	bus.On(EventAgentStart, func(e *Event) error {
		got = append(got, "other")
		return nil
	})

	if err := bus.Emit(&Event{Type: EventTurnStart}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("order = %v, want [a b]", got)
	}

	un1()
	got = nil
	if err := bus.Emit(&Event{Type: EventTurnStart}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("after unsubscribe = %v, want [b]", got)
	}
	if n := bus.HandlerCount(EventTurnStart); n != 1 {
		t.Errorf("HandlerCount = %d, want 1", n)
	}
}

func TestBusCollectsHandlerErrors(t *testing.T) {
	bus := NewBus()
	called := false
	bus.On(EventTurnStart, func(e *Event) error {
		return errors.New("boom")
	})
	bus.On(EventTurnStart, func(e *Event) error {
		called = true
		return nil
	})
	err := bus.Emit(&Event{Type: EventTurnStart})
	if err == nil {
		t.Fatal("want joined error")
	}
	if !called {
		t.Error("second handler must still run after first errors")
	}
}

func TestBusMutationSemantics(t *testing.T) {
	bus := NewBus()
	bus.On(EventBeforeAgentStart, func(e *Event) error {
		e.SystemPrompt = "changed"
		e.Cancel = true
		return nil
	})
	ev := &Event{Type: EventBeforeAgentStart, SystemPrompt: "orig"}
	if err := bus.Emit(ev); err != nil {
		t.Fatal(err)
	}
	if ev.SystemPrompt != "changed" || !ev.Cancel {
		t.Errorf("event not mutated: %+v", ev)
	}
}

func TestCommandRegistryDuplicateSuffix(t *testing.T) {
	r := NewCommandRegistry()
	c := &Command{Description: "d", Handler: func(ctx context.Context, args string) error { return nil }}
	if name := r.Register("review", c); name != "review" {
		t.Errorf("first name = %q", name)
	}
	if name := r.Register("review", c); name != "review:1" {
		t.Errorf("second name = %q", name)
	}
	if name := r.Register("review", c); name != "review:2" {
		t.Errorf("third name = %q", name)
	}
	if names := r.List(); len(names) != 3 {
		t.Errorf("list = %v", names)
	}
}

func TestToolRegistryActiveTools(t *testing.T) {
	r := tools.NewRegistry()
	mk := func(name string) *tools.Tool {
		return &tools.Tool{
			Name:       name,
			Parameters: map[string]any{"type": "object"},
			Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
				return tools.Result{}, nil
			},
		}
	}
	for _, n := range []string{"a", "b", "c"} {
		if err := r.Register(mk(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(mk("a")); err == nil {
		t.Error("duplicate registration must fail")
	}

	if defs := r.Definitions(); len(defs) != 3 {
		t.Errorf("all active by default, got %d", len(defs))
	}
	if err := r.SetActiveTools([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if defs := r.Definitions(); len(defs) != 2 {
		t.Errorf("want 2 active, got %d", len(defs))
	}
	if err := r.SetActiveTools([]string{"nope"}); err == nil {
		t.Error("unknown tool activation must fail")
	}
	if err := r.SetActiveTools(nil); err != nil {
		t.Fatal(err)
	}
	if defs := r.Definitions(); len(defs) != 3 {
		t.Errorf("reset to all, got %d", len(defs))
	}
}

func TestAPIRegisterSurfaces(t *testing.T) {
	bus := NewBus()
	reg := tools.NewRegistry()
	cmds := NewCommandRegistry()
	api := NewAPI(bus, reg, cmds)

	un := api.On(EventAgentEnd, func(e *Event) error { return nil })
	if bus.HandlerCount(EventAgentEnd) != 1 {
		t.Error("On did not subscribe")
	}
	un()

	err := api.RegisterTool(&tools.Tool{
		Name: "t",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("t"); !ok {
		t.Error("tool not registered")
	}
	if name := api.RegisterCommand("x", &Command{}); name != "x" {
		t.Errorf("command name = %q", name)
	}

	api.SetModel("m1")
	api.SetThinking(true)
	if api.Model() != "m1" || !api.Thinking() {
		t.Error("SetModel/SetThinking broken")
	}
}

func TestManagerLoadAndShutdown(t *testing.T) {
	bus := NewBus()
	api := NewAPI(bus, tools.NewRegistry(), NewCommandRegistry())
	m := NewManager(api)

	started := false
	api.On(EventSessionShutdown, func(e *Event) error {
		started = true
		return nil
	})

	calls := 0
	ext := Named("counting", func(api *API) error {
		calls++
		return nil
	})
	if err := m.LoadAll(ext, ext); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("Setup calls = %d", calls)
	}
	if names := m.LoadedNames(); len(names) != 2 {
		t.Errorf("loaded = %v", names)
	}

	m.Shutdown()
	m.Shutdown() // idempotent
	if !started {
		t.Error("session_shutdown not emitted")
	}
	if err := m.Load(ext); err == nil {
		t.Error("load after shutdown must fail")
	}
}
