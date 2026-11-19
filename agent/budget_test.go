package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// usageProvider answers with a scripted usage per request, so a test
// can spend a budget deterministically. The last scripted answer
// repeats.
type usageProvider struct {
	responses []llm.Message
	usage     llm.Usage
	n         int
}

func (p *usageProvider) Name() string { return "usage" }

func (p *usageProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	msg := p.responses[len(p.responses)-1]
	if p.n < len(p.responses) {
		msg = p.responses[p.n]
	}
	p.n++
	m := msg
	events := []llm.StreamEvent{{
		Type:       llm.EventDone,
		Message:    &m,
		StopReason: "end_turn",
		Usage:      p.usage,
	}}
	if msg.Content != "" {
		events = append([]llm.StreamEvent{{Type: llm.EventTextDelta, Text: msg.Content}}, events...)
	}
	for i := range msg.ToolCalls {
		tc := msg.ToolCalls[i]
		events = append([]llm.StreamEvent{{Type: llm.EventToolCall, ToolCall: &tc}}, events...)
	}
	return &fakeStream{events: events}, nil
}

// TestRunTokenBudgetStopsTheRun checks that a run that spent its
// allowance does not start another request.
func TestRunTokenBudgetStopsTheRun(t *testing.T) {
	provider := &usageProvider{
		// Each turn asks for a tool, so the loop would keep going.
		responses: []llm.Message{{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "peek"}}}},
		usage:     llm.Usage{InputTokens: 100, OutputTokens: 50},
	}
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	_ = reg.Register(&tools.Tool{
		Name: "peek", ReadOnly: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "ok"}, nil
		},
	})
	ag := New(Config{
		Provider:     provider,
		Model:        "test-model",
		MaxTurns:     50,
		MaxRunTokens: 200,
	}, bus, reg)

	res, err := ag.Run(context.Background(), "go")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrBudgetExceeded", err)
	}
	if !res.BudgetExceeded {
		t.Error("the result does not report the budget stop")
	}
	// 150 tokens per turn: the second turn is allowed (150 < 200), the
	// third is not (300 >= 200).
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2", res.Turns)
	}
	if provider.n != 2 {
		t.Errorf("requests = %d, want 2", provider.n)
	}
	// The work done before the stop is kept.
	if len(ag.History()) == 0 {
		t.Error("the history was thrown away")
	}
}

// TestRunTokenBudgetIsNotAnAbort checks that a budget stop is not
// reported as an abort: the user did not cancel the run.
func TestRunTokenBudgetIsNotAnAbort(t *testing.T) {
	provider := &usageProvider{
		responses: []llm.Message{{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "peek"}}}},
		usage:     llm.Usage{InputTokens: 100},
	}
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	_ = reg.Register(&tools.Tool{
		Name: "peek", ReadOnly: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "ok"}, nil
		},
	})
	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 50, MaxRunTokens: 100}, bus, reg)

	var settled, aborted *ext.Event
	bus.On(ext.EventAgentSettled, func(e *ext.Event) error {
		settled = e
		if e.Aborted {
			aborted = e
		}
		return nil
	})
	if _, err := ag.Run(context.Background(), "go"); err == nil {
		t.Fatal("want the budget error")
	}
	if settled == nil {
		t.Fatal("no agent_settled")
	}
	if aborted != nil {
		t.Error("a budget stop was reported as an abort")
	}
}

// TestNoBudgetMeansNoLimit checks the default: a run without a budget
// is bounded only by MaxTurns.
func TestNoBudgetMeansNoLimit(t *testing.T) {
	provider := &usageProvider{
		responses: []llm.Message{{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "peek"}}}},
		usage:     llm.Usage{InputTokens: 100000},
	}
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	_ = reg.Register(&tools.Tool{
		Name: "peek", ReadOnly: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			return tools.Result{Content: "ok"}, nil
		},
	})
	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 3}, bus, reg)
	res, err := ag.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BudgetExceeded {
		t.Error("a run without a budget reported one")
	}
	if res.Turns != 3 {
		t.Errorf("turns = %d, want 3 (MaxTurns)", res.Turns)
	}
}

// TestUsageTotalCountsCacheTokens checks the budget's unit: cached
// tokens are tokens the provider processed.
func TestUsageTotalCountsCacheTokens(t *testing.T) {
	u := llm.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100, CacheWriteTokens: 20}
	if got := u.Total(); got != 135 {
		t.Errorf("Total() = %d, want 135", got)
	}
}
