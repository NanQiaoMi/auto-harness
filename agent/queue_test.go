package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// gatedProvider blocks the first request until the test releases it, so
// the test can act while a run is in progress.
type gatedProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func newGatedProvider() *gatedProvider {
	return &gatedProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *gatedProvider) Name() string { return "gated" }

func (p *gatedProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return newFakeStream(llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer %d", n)}), nil
}

// countingProvider answers every request, forever.
type countingProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	return newFakeStream(llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer %d", n)}), nil
}

func newQueueAgent(provider llm.Provider) (*Agent, *ext.Bus) {
	bus := ext.NewBus()
	return New(Config{Provider: provider, Model: "test-model", MaxTurns: 5}, bus, tools.NewRegistry()), bus
}

func TestQueuedMessageJoinsTheRunningRun(t *testing.T) {
	provider := newGatedProvider()
	ag, _ := newQueueAgent(provider)

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := ag.Run(context.Background(), "first")
		done <- outcome{res, err}
	}()

	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the provider")
	}
	if !ag.Running() {
		t.Error("Running() = false during a run")
	}
	// A follow-up typed while the model works is queued, not lost.
	if err := ag.Enqueue("follow up"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got := ag.Queue(); len(got) != 1 || got[0] != "follow up" {
		t.Errorf("Queue() = %v, want one message", got)
	}
	close(provider.release)

	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish")
	}
	if got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}
	if got.res.Queued != 1 {
		t.Errorf("Result.Queued = %d, want 1", got.res.Queued)
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2", provider.calls)
	}
	// The follow-up lands after the first answer, as a user message.
	hist := ag.History()
	if len(hist) != 4 {
		t.Fatalf("history length = %d, want 4: %#v", len(hist), hist)
	}
	if hist[2].Role != llm.RoleUser || hist[2].Content != "follow up" {
		t.Errorf("history[2] = %+v, want the queued user message", hist[2])
	}
	if len(ag.Queue()) != 0 {
		t.Error("the queue still holds a message after the run drained it")
	}
	if ag.Running() {
		t.Error("Running() = true after the run ended")
	}
}

func TestMessageQueuedBeforeTheRunIsPickedUp(t *testing.T) {
	provider := &countingProvider{}
	ag, _ := newQueueAgent(provider)

	// The window between "a run was accepted" and "the run began" must
	// not lose input.
	if err := ag.Enqueue("typed early"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	res, err := ag.Run(context.Background(), "first")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Queued != 1 {
		t.Errorf("Result.Queued = %d, want 1", res.Queued)
	}
	// The queued message joins the run's first turn: it lands right
	// after the input, before the model answers.
	hist := ag.History()
	if len(hist) != 3 || hist[1].Content != "typed early" || hist[2].Role != llm.RoleAssistant {
		t.Fatalf("history = %#v, want the queued message between the input and the answer", hist)
	}
}

func TestEnqueueRejectsAnEmptyMessage(t *testing.T) {
	ag, _ := newQueueAgent(&countingProvider{})
	if err := ag.Enqueue("   "); err == nil {
		t.Error("Enqueue(\"   \") = nil, want an error")
	}
}

func TestSubmitStartsARunWhenIdleAndQueuesWhenBusy(t *testing.T) {
	provider := newGatedProvider()
	ag, _ := newQueueAgent(provider)

	// Busy: Submit queues.
	go func() { _, _ = ag.Run(context.Background(), "first") }()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the provider")
	}
	queued, err := ag.Submit(context.Background(), "second")
	if err != nil {
		t.Fatalf("Submit while busy: %v", err)
	}
	if !queued {
		t.Error("Submit while busy reported a started run")
	}
	close(provider.release)
	// Let the run drain.
	for ag.Running() {
		time.Sleep(time.Millisecond)
	}

	// Idle: Submit runs.
	queued, err = ag.Submit(context.Background(), "third")
	if err != nil {
		t.Fatalf("Submit while idle: %v", err)
	}
	if queued {
		t.Error("Submit while idle reported a queued message")
	}
}

func TestSettleHandlerCanRequestAContinuation(t *testing.T) {
	provider := &countingProvider{}
	ag, bus := newQueueAgent(provider)

	var starts int
	bus.On(ext.EventAgentStart, func(e *ext.Event) error {
		starts++
		return nil
	})
	var settles int
	bus.On(ext.EventAgentBeforeSettle, func(e *ext.Event) error {
		settles++
		if settles == 1 {
			e.Continue = true
		}
		return nil
	})

	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if starts != 2 {
		t.Errorf("agent_start events = %d, want 2", starts)
	}
	if !res.Continued {
		t.Error("Result.Continued = false, want true")
	}
	if res.Turns != 2 {
		t.Errorf("Result.Turns = %d, want 2", res.Turns)
	}
}

func TestSettleHandlerCanInjectAMessage(t *testing.T) {
	provider := &countingProvider{}
	ag, bus := newQueueAgent(provider)

	var once sync.Once
	bus.On(ext.EventAgentBeforeSettle, func(e *ext.Event) error {
		once.Do(func() {
			e.Messages = append(e.Messages, llm.Message{Role: llm.RoleUser, Content: "one more thing"})
		})
		return nil
	})

	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Continued || res.Turns != 2 {
		t.Errorf("res = %+v, want a continuation", res)
	}
	hist := ag.History()
	found := false
	for _, m := range hist {
		if m.Content == "one more thing" {
			found = true
		}
	}
	if !found {
		t.Errorf("history = %#v, want the injected message", hist)
	}
}

func TestSettleContinuationsAreBounded(t *testing.T) {
	provider := &countingProvider{}
	ag, bus := newQueueAgent(provider)

	var logs []string
	ag.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	var starts int
	bus.On(ext.EventAgentStart, func(e *ext.Event) error {
		starts++
		return nil
	})
	// A handler that always asks for more would keep the agent busy
	// forever and never hand the prompt back.
	bus.On(ext.EventAgentBeforeSettle, func(e *ext.Event) error {
		e.Continue = true
		return nil
	})

	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	const want = maxSettleRounds + 1
	if starts != want {
		t.Errorf("agent_start events = %d, want %d", starts, want)
	}
	if provider.calls != want {
		t.Errorf("provider calls = %d, want %d", provider.calls, want)
	}
	if !res.Continued {
		t.Error("Result.Continued = false")
	}
	if len(logs) == 0 {
		t.Error("the bound was reached without a diagnostic")
	}
}

// neverProvider blocks until the context ends.
type neverProvider struct{}

func (p *neverProvider) Name() string { return "never" }

func (p *neverProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAbortedRunSkipsTheSettleBoundary(t *testing.T) {
	ag, bus := newQueueAgent(&neverProvider{})

	var settle, settled, aborted int
	bus.On(ext.EventAgentBeforeSettle, func(e *ext.Event) error {
		settle++
		return nil
	})
	bus.On(ext.EventAgentSettled, func(e *ext.Event) error {
		settled++
		if e.Aborted {
			aborted++
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := ag.Run(ctx, "hi"); err == nil {
		t.Fatal("Run succeeded, want the cancellation to surface")
	}
	if settle != 0 {
		t.Errorf("agent_before_settle fired %d times on an aborted run, want 0", settle)
	}
	if settled != 1 {
		t.Errorf("agent_settled fired %d times, want 1", settled)
	}
	if aborted != 1 {
		t.Errorf("agent_settled aborted flag set %d times, want 1", aborted)
	}
}

func TestAgentSettledFiresOncePerRun(t *testing.T) {
	ag, bus := newQueueAgent(&countingProvider{})
	var settled int
	bus.On(ext.EventAgentSettled, func(e *ext.Event) error {
		settled++
		return nil
	})
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if settled != 1 {
		t.Errorf("agent_settled fired %d times, want 1", settled)
	}
}

func TestDrainRunsWhatWasQueuedAtTheVeryEnd(t *testing.T) {
	provider := &countingProvider{}
	ag, _ := newQueueAgent(provider)

	if err := ag.Drain(context.Background()); err != nil {
		t.Fatalf("Drain on an empty queue: %v", err)
	}
	if provider.calls != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls)
	}

	// A message queued after the run's last drain is picked up here.
	if err := ag.Enqueue("late"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := ag.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
	hist := ag.History()
	if len(hist) != 2 || hist[0].Content != "late" {
		t.Fatalf("history = %#v, want the drained message", hist)
	}
	if len(ag.Queue()) != 0 {
		t.Error("the queue still holds a message")
	}
}

// TestAMessageQueuedAtTheEndIsNotLost stresses the moment a run ends.
//
// Enqueue and the run's final check happen on different goroutines: the
// message must be either drained by that run or left in the queue for
// the caller, never dropped.
func TestAMessageQueuedAtTheEndIsNotLost(t *testing.T) {
	provider := &countingProvider{}
	ag, bus := newQueueAgent(provider)

	var once sync.Once
	bus.On(ext.EventAgentBeforeSettle, func(e *ext.Event) error {
		// Queue right at the last boundary, from another goroutine, so
		// the enqueue can land on either side of the run's final check.
		once.Do(func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = ag.Enqueue("last instant")
			}()
			<-done
		})
		return nil
	})

	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The run either took it (then nothing is left) or it is waiting.
	left := ag.Queue()
	if len(left) > 1 {
		t.Fatalf("queue = %v, want at most one message", left)
	}
	if len(left) == 1 {
		// Not lost: a caller's Drain picks it up.
		if err := ag.Drain(context.Background()); err != nil {
			t.Fatalf("Drain: %v", err)
		}
	}
	found := false
	for _, m := range ag.History() {
		if m.Content == "last instant" {
			found = true
		}
	}
	if !found {
		t.Error("the message queued at the very end was lost")
	}
}
