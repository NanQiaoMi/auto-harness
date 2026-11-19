package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

func TestIsRetryableClassifiesFailures(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		// Transient: worth another attempt.
		{"overloaded_error: server is overloaded", true},
		{"429 Too Many Requests", true},
		{"provider returned 503 service unavailable", true},
		{"connection reset by peer", true},
		{"unexpected EOF", true},
		{"stream was interrupted", true},
		{"ECONNRESET", true},
		// Permanent: the same call fails the same way.
		{"insufficient_quota", false},
		{"GoUsageLimitError: usage limit reached", false},
		{"invalid api key", false},
		{"unauthorized", false},
		{"model not found: deepseek-nope", false},
		{"context length exceeded", false},
		{"invalid_request_error", false},
		// A permanent reason wins over a transient-looking word.
		{"429 rate limited: insufficient_quota", false},
		{"overloaded: permission denied", false},
		// Nothing recognizable is not retried.
		{"something went sideways", false},
	}
	for _, c := range cases {
		if got := isRetryable(errors.New(c.err)); got != c.want {
			t.Errorf("isRetryable(%q) = %v, want %v", c.err, got, c.want)
		}
	}
	if isRetryable(nil) {
		t.Error("isRetryable(nil) = true")
	}
	if isRetryable(context.Canceled) {
		t.Error("a cancelled context must not be retried")
	}
	if isRetryable(context.DeadlineExceeded) {
		t.Error("an expired deadline must not be retried")
	}
}

func TestRetryDelayDoublesAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	max := 400 * time.Millisecond
	want := []time.Duration{100, 200, 400, 400, 400}
	for i, w := range want {
		if got := retryDelay(base, max, i+1); got != w*time.Millisecond {
			t.Errorf("retryDelay(attempt %d) = %v, want %v", i+1, got, w*time.Millisecond)
		}
	}
	// An attempt below 1 is treated as the first.
	if got := retryDelay(base, max, 0); got != base {
		t.Errorf("retryDelay(0) = %v, want %v", got, base)
	}
}

// flakyProvider fails the first failures requests with err, then answers
// like fakeProvider.
type flakyProvider struct {
	failures int
	err      error
	msg      llm.Message
	calls    int
	mu       sync.Mutex
}

func (p *flakyProvider) Name() string { return "flaky" }

func (p *flakyProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n <= p.failures {
		return nil, p.err
	}
	return newFakeStream(p.msg), nil
}

func newRetryTestAgent(t *testing.T, provider llm.Provider, maxRetries int) (*Agent, *ext.Bus) {
	t.Helper()
	bus := ext.NewBus()
	ag := New(Config{
		Provider:       provider,
		Model:          "test-model",
		MaxTurns:       3,
		MaxRetries:     maxRetries,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  2 * time.Millisecond,
	}, bus, tools.NewRegistry())
	return ag, bus
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	provider := &flakyProvider{
		failures: 2,
		err:      errors.New("503 service unavailable"),
		msg:      llm.Message{Role: llm.RoleAssistant, Content: "done"},
	}
	ag, bus := newRetryTestAgent(t, provider, 3)

	var retries []*ext.Event
	bus.On(ext.EventRetry, func(e *ext.Event) error {
		retries = append(retries, e)
		return nil
	})
	var requestStarts int
	bus.On(ext.EventRequestStart, func(e *ext.Event) error {
		requestStarts++
		return nil
	})

	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "done" {
		t.Errorf("Final = %q, want %q", res.Final, "done")
	}
	if provider.calls != 3 {
		t.Errorf("provider calls = %d, want 3", provider.calls)
	}
	if requestStarts != 3 {
		t.Errorf("request_start events = %d, want one per attempt (3)", requestStarts)
	}
	if len(retries) != 2 {
		t.Fatalf("retry events = %d, want 2", len(retries))
	}
	for i, e := range retries {
		if e.Reason == "" {
			t.Errorf("retry %d has no reason", i)
		}
		if got, _ := e.Data["attempt"].(int); got != i+1 {
			t.Errorf("retry %d attempt = %v, want %d", i, e.Data["attempt"], i+1)
		}
		if got, _ := e.Data["maxAttempts"].(int); got != 3 {
			t.Errorf("retry %d maxAttempts = %v, want 3", i, e.Data["maxAttempts"])
		}
		if got, _ := e.Data["delayMs"].(int64); got <= 0 {
			t.Errorf("retry %d delayMs = %v, want a positive delay", i, e.Data["delayMs"])
		}
	}
	// The history holds the user message and one answer: the failed
	// attempts left nothing behind.
	history := ag.History()
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2: %#v", len(history), history)
	}
}

func TestRetryStopsAtBudget(t *testing.T) {
	provider := &flakyProvider{
		failures: 100,
		err:      errors.New("overloaded"),
	}
	ag, bus := newRetryTestAgent(t, provider, 2)

	var retries int
	bus.On(ext.EventRetry, func(e *ext.Event) error {
		retries++
		return nil
	})

	_, err := ag.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("Run succeeded, want the failure to surface")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("error = %v, want the provider's error", err)
	}
	// Two retries means three attempts.
	if provider.calls != 3 {
		t.Errorf("provider calls = %d, want 3", provider.calls)
	}
	if retries != 2 {
		t.Errorf("retry events = %d, want 2", retries)
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	provider := &flakyProvider{
		failures: 100,
		err:      errors.New("insufficient_quota"),
	}
	ag, bus := newRetryTestAgent(t, provider, 5)

	var retries int
	bus.On(ext.EventRetry, func(e *ext.Event) error {
		retries++
		return nil
	})

	if _, err := ag.Run(context.Background(), "hi"); err == nil {
		t.Fatal("Run succeeded, want the failure to surface")
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no retry)", provider.calls)
	}
	if retries != 0 {
		t.Errorf("retry events = %d, want 0", retries)
	}
}

func TestRetriesCanBeDisabled(t *testing.T) {
	provider := &flakyProvider{
		failures: 100,
		err:      errors.New("overloaded"),
	}
	ag, _ := newRetryTestAgent(t, provider, -1)

	if _, err := ag.Run(context.Background(), "hi"); err == nil {
		t.Fatal("Run succeeded, want the failure to surface")
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (retries disabled)", provider.calls)
	}
}

// midStreamProvider streams one delta and then fails.
type midStreamProvider struct {
	msg llm.Message
}

func (p *midStreamProvider) Name() string { return "midstream" }

func (p *midStreamProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	return &failingStream{events: []llm.StreamEvent{
		{Type: llm.EventTextDelta, Text: "partial "},
	}, failAfter: errors.New("stream was interrupted")}, nil
}

type failingStream struct {
	events    []llm.StreamEvent
	pos       int
	failAfter error
}

func (s *failingStream) Recv() (llm.StreamEvent, error) {
	if s.pos >= len(s.events) {
		return llm.StreamEvent{}, s.failAfter
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *failingStream) Close() error { return nil }

func TestNoRetryAfterContentStreamed(t *testing.T) {
	bus := ext.NewBus()
	provider := &midStreamProvider{}
	ag := New(Config{
		Provider:       provider,
		Model:          "test-model",
		MaxTurns:       2,
		MaxRetries:     3,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  time.Millisecond,
	}, bus, tools.NewRegistry())

	var retries, starts int
	bus.On(ext.EventRetry, func(e *ext.Event) error { retries++; return nil })
	bus.On(ext.EventRequestStart, func(e *ext.Event) error { starts++; return nil })

	_, err := ag.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("Run succeeded, want the mid-stream failure to surface")
	}
	// A second attempt would print "partial " twice.
	if starts != 1 {
		t.Errorf("request_start events = %d, want 1", starts)
	}
	if retries != 0 {
		t.Errorf("retry events = %d, want 0", retries)
	}
}

func TestRetrySleepHonoursCancellation(t *testing.T) {
	provider := &flakyProvider{failures: 100, err: errors.New("overloaded")}
	bus := ext.NewBus()
	ag := New(Config{
		Provider:       provider,
		Model:          "test-model",
		MaxTurns:       2,
		MaxRetries:     5,
		RetryBaseDelay: time.Hour, // long enough that only the context ends it
		RetryMaxDelay:  time.Hour,
	}, bus, tools.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	bus.On(ext.EventRetry, func(e *ext.Event) error {
		cancel()
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := ag.Run(ctx, "hi")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded, want cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
