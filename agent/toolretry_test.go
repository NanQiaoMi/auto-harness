package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// toolRetryProvider asks for one tool call, then answers.
type toolRetryProvider struct {
	calls []llm.ToolCall
	n     int32
}

func (p *toolRetryProvider) Name() string { return "tool-retry" }

func (p *toolRetryProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	if atomic.AddInt32(&p.n, 1) == 1 {
		return newFakeStream(llm.Message{Role: llm.RoleAssistant, ToolCalls: p.calls}), nil
	}
	return newFakeStream(llm.Message{Role: llm.RoleAssistant, Content: "done"}), nil
}

// retryTestAgent builds an agent with a fast tool backoff.
func retryTestAgent(t *testing.T, provider llm.Provider, maxToolRetries int) (*Agent, *tools.Registry, *ext.Bus) {
	t.Helper()
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	ag := New(Config{
		Provider:       provider,
		Model:          "test-model",
		MaxTurns:       4,
		ToolMaxRetries: maxToolRetries,
		ToolRetryDelay: time.Millisecond,
	}, bus, reg)
	return ag, reg, bus
}

// TestRetryableToolIsRetried checks that a tool that fails with an
// error and declares itself retryable runs again.
func TestRetryableToolIsRetried(t *testing.T) {
	var runs int32
	provider := &toolRetryProvider{calls: []llm.ToolCall{{ID: "t1", Name: "flaky"}}}
	ag, reg, bus := retryTestAgent(t, provider, 1)
	if err := reg.Register(&tools.Tool{
		Name: "flaky", Retryable: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			if atomic.AddInt32(&runs, 1) == 1 {
				return tools.Result{}, errors.New("connection reset by peer")
			}
			return tools.Result{Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	var retries int
	bus.On(ext.EventRetry, func(e *ext.Event) error {
		retries++
		if e.Data["tool"] != "flaky" {
			t.Errorf("retry record is about %v, want the tool", e.Data["tool"])
		}
		return nil
	})

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs != 2 {
		t.Errorf("tool ran %d times, want 2", runs)
	}
	if retries != 1 {
		t.Errorf("retry events = %d, want 1", retries)
	}
	// The retried call's result is what the model sees.
	for _, m := range ag.History() {
		if m.Role == llm.RoleTool && m.Content != "ok" {
			t.Errorf("tool result = %q, want ok", m.Content)
		}
	}
}

// TestToolThatDoesNotDeclareRetryableRunsOnce checks the opt-in: a tool
// whose failure is not declared transient is not run twice.
func TestToolThatDoesNotDeclareRetryableRunsOnce(t *testing.T) {
	var runs int32
	provider := &toolRetryProvider{calls: []llm.ToolCall{{ID: "t1", Name: "strict"}}}
	ag, reg, _ := retryTestAgent(t, provider, 3)
	if err := reg.Register(&tools.Tool{
		Name: "strict", Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			atomic.AddInt32(&runs, 1)
			return tools.Result{}, errors.New("boom")
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs != 1 {
		t.Errorf("tool ran %d times, want 1", runs)
	}
}

// TestToolErrorResultIsNotRetried checks the line between the two kinds
// of failure: a Result with IsError is the tool's answer, not a
// transient error, so the agent passes it to the model instead of
// running the call again.
func TestToolErrorResultIsNotRetried(t *testing.T) {
	var runs int32
	provider := &toolRetryProvider{calls: []llm.ToolCall{{ID: "t1", Name: "answers-with-error"}}}
	ag, reg, _ := retryTestAgent(t, provider, 3)
	if err := reg.Register(&tools.Tool{
		Name: "answers-with-error", Retryable: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			atomic.AddInt32(&runs, 1)
			return tools.Result{Content: "no such file", IsError: true}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs != 1 {
		t.Errorf("tool ran %d times, want 1", runs)
	}
}

// TestToolRetriesAreBounded checks that a tool that never succeeds
// stops after its budget and reports the failure.
func TestToolRetriesAreBounded(t *testing.T) {
	var runs int32
	provider := &toolRetryProvider{calls: []llm.ToolCall{{ID: "t1", Name: "broken"}}}
	ag, reg, _ := retryTestAgent(t, provider, 2)
	if err := reg.Register(&tools.Tool{
		Name: "broken", Retryable: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			atomic.AddInt32(&runs, 1)
			return tools.Result{}, errors.New("still down")
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One initial attempt plus two retries.
	if runs != 3 {
		t.Errorf("tool ran %d times, want 3", runs)
	}
	var reported bool
	for _, m := range ag.History() {
		if m.Role == llm.RoleTool && m.IsError {
			reported = true
		}
	}
	if !reported {
		t.Error("the failure never reached the model")
	}
}

// TestToolRetriesCanBeDisabled checks the negative value.
func TestToolRetriesCanBeDisabled(t *testing.T) {
	var runs int32
	provider := &toolRetryProvider{calls: []llm.ToolCall{{ID: "t1", Name: "flaky"}}}
	ag, reg, _ := retryTestAgent(t, provider, -1)
	if err := reg.Register(&tools.Tool{
		Name: "flaky", Retryable: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			atomic.AddInt32(&runs, 1)
			return tools.Result{}, errors.New("down")
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs != 1 {
		t.Errorf("tool ran %d times, want 1", runs)
	}
}

// TestToolRetryStopsWhenTheRunIsCancelled checks that a retrying tool
// does not keep the user waiting for an abort.
func TestToolRetryStopsWhenTheRunIsCancelled(t *testing.T) {
	var runs int32
	ctx, cancel := context.WithCancel(context.Background())
	provider := &toolRetryProvider{calls: []llm.ToolCall{{ID: "t1", Name: "slow-down"}}}
	ag, reg, _ := retryTestAgent(t, provider, 5)
	// A long backoff: the cancellation has to cut through the sleep.
	ag.mu.Lock()
	ag.cfg.ToolRetryDelay = time.Hour
	ag.mu.Unlock()
	if err := reg.Register(&tools.Tool{
		Name: "slow-down", Retryable: true, Parameters: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			atomic.AddInt32(&runs, 1)
			cancel()
			return tools.Result{}, errors.New("down")
		},
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ag.Run(ctx, "go")
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not stop when it was cancelled")
	}
	if runs != 1 {
		t.Errorf("tool ran %d times, want 1", runs)
	}
}
