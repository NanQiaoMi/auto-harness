// Package telemetryext instruments the harness with the vendor-neutral
// telemetry contract.
//
// The collector is an extension, like everything else in the harness: it
// subscribes to the event bus and turns the events into spans. Nothing
// in the kernel knows telemetry exists.
//
// The instrumentation follows the privacy policy the schema encodes: no
// prompt, completion, tool argument, tool output, file path or provider
// payload is ever recorded. Only facts the harness already has — names,
// counts, durations, token counts — become attributes.
package telemetryext

import (
	"sync"
	"time"

	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/llm"
	"github.com/mimi/auto-harness/telemetry"
)

// Collector turns the harness's events into telemetry spans.
//
// It is an extension: Setup installs the hooks, and a reload undoes
// them like any other extension's registrations.
//
// The harness's operations are bracketed by events, not by a function
// call, so the collector holds each span open itself and settles it
// when the operation ends.
type Collector struct {
	// Context starts the spans. A nil context means no telemetry.
	//
	// It is the adapter's root context: the session span starts under
	// it, and every later span starts under the session. NoopContext is
	// the root of a run with no telemetry.
	Context telemetry.Context
	// Provider and Model name the session's starting model.
	Provider string
	Model    string
	// Logf receives non-fatal diagnostics.
	Logf func(format string, args ...any)

	mu sync.Mutex
	// session is the root span, open for the session's lifetime.
	session telemetry.Span
	// releaseSession settles the root span.
	releaseSession func()
	// turnIndex counts the turns of the current run.
	turnIndex int
	// toolCalls counts the tool calls of the session.
	toolCalls int
	// request is the span of the request in flight, if any.
	request telemetry.Span
	// releaseRequest settles the request span.
	releaseRequest func()
	// requestStarted is when the request began.
	requestStarted time.Time
	// toolSpans holds the tool spans of the current turn.
	toolSpans []heldSpan
	// compaction is the span of the compaction in flight, if any.
	compaction telemetry.Span
	// releaseCompaction settles the compaction span.
	releaseCompaction func()
}

// heldSpan is a span and the function that settles it.
type heldSpan struct {
	span    telemetry.Span
	release func()
	// blocked records that the permission layer blocked the call.
	blocked bool
}

// New creates a collector over a telemetry context.
func New(ctx telemetry.Context, provider, model string) *Collector {
	if ctx == nil {
		ctx = telemetry.NoopContext
	}
	return &Collector{Context: ctx, Provider: provider, Model: model}
}

// Name implements ext.Extension.
func (c *Collector) Name() string { return "telemetry" }

// Setup installs the collector's hooks.
func (c *Collector) Setup(api *ext.API) error {
	api.On(ext.EventSessionStart, func(e *ext.Event) error {
		c.startSession()
		return nil
	})
	api.On(ext.EventSessionShutdown, func(e *ext.Event) error {
		c.endSession()
		return nil
	})
	api.On(ext.EventRequestStart, func(e *ext.Event) error {
		c.startRequest(e.Request)
		return nil
	})
	api.On(ext.EventTurnEnd, func(e *ext.Event) error {
		c.endRequest(e.Request)
		return nil
	})
	api.On(ext.EventToolCall, func(e *ext.Event) error {
		c.startTool(e)
		return nil
	})
	api.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		c.endTool(e)
		return nil
	})
	api.On(ext.EventSessionBeforeCompact, func(e *ext.Event) error {
		c.startCompaction(len(e.Messages))
		return nil
	})
	api.On(ext.EventSessionCompact, func(e *ext.Event) error {
		c.endCompaction(len(e.Messages))
		return nil
	})
	return nil
}

// startSession opens the root span.
func (c *Collector) startSession() {
	c.mu.Lock()
	if c.session != nil {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	span, release := c.start(telemetry.SpanOptions{
		Name: "pi.harness.session",
		Attributes: telemetry.SpanAttributes{
			"pi.harness.provider": c.Provider,
			"pi.harness.model":    c.Model,
		},
	}, nil)

	c.mu.Lock()
	c.session = span
	c.releaseSession = release
	c.turnIndex = 0
	c.toolCalls = 0
	c.mu.Unlock()
}

// endSession settles the root span.
func (c *Collector) endSession() {
	c.mu.Lock()
	span := c.session
	release := c.releaseSession
	c.session = nil
	c.releaseSession = nil
	turns := c.turnIndex
	calls := c.toolCalls
	c.mu.Unlock()

	if span == nil {
		return
	}
	span.SetAttributes(telemetry.SpanAttributes{
		"pi.harness.turns":      turns,
		"pi.harness.tool_calls": calls,
	})
	span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	if release != nil {
		release()
	}
}

// startRequest opens a span for the model request.
//
// A request that never ends (the run was cancelled) leaves its span
// open; the teardown settles it.
func (c *Collector) startRequest(info ext.Request) {
	c.mu.Lock()
	parent := c.session
	c.turnIndex++
	turn := c.turnIndex
	// A request still in flight means the previous one never ended.
	stale := c.releaseRequest
	c.mu.Unlock()
	if stale != nil {
		stale()
	}

	span, release := c.start(telemetry.SpanOptions{
		Name: "pi.harness.request",
		Attributes: telemetry.SpanAttributes{
			"pi.harness.provider": info.Provider,
			"pi.harness.model":    info.Model,
			"pi.harness.tools":    info.Tools,
		},
	}, parent)

	c.mu.Lock()
	c.request = span
	c.releaseRequest = release
	c.requestStarted = time.Now()
	c.mu.Unlock()

	// The turn is an event on the session span: the request span
	// belongs to the request, the turn belongs to the session.
	if parent != nil {
		parent.AddEvent("pi.harness.turn", telemetry.SpanAttributes{
			"pi.harness.turn_index": turn,
		})
	}
}

// endRequest settles the request span and records its measurements.
func (c *Collector) endRequest(info ext.Request) {
	c.mu.Lock()
	span := c.request
	release := c.releaseRequest
	c.request = nil
	c.releaseRequest = nil
	started := c.requestStarted
	c.mu.Unlock()

	if span == nil {
		return
	}
	span.AddEvent("pi.harness.first_chunk", nil)
	span.SetAttributes(telemetry.SpanAttributes{
		"pi.harness.stop":              info.StopReason,
		"pi.harness.usage.input":       info.Usage.InputTokens,
		"pi.harness.usage.output":      info.Usage.OutputTokens,
		"pi.harness.usage.cache_read":  info.Usage.CacheReadTokens,
		"pi.harness.usage.cache_write": info.Usage.CacheWriteTokens,
	})
	span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	if release != nil {
		release()
	}

	// The measurements.
	elapsed := info.Duration
	if elapsed == 0 && !started.IsZero() {
		elapsed = time.Since(started)
	}
	c.RecordDuration(info.Provider, info.Model, elapsed)
	c.RecordUsage(info.Model, info.Usage)
}

// startTool opens a span for the tool call.
//
// A call the permission layer blocked is recorded as blocked: the
// block flag rides on the tool_call event.
func (c *Collector) startTool(e *ext.Event) {
	if e.ToolCall == nil {
		return
	}
	c.mu.Lock()
	parent := c.session
	if c.request != nil {
		parent = c.request
	}
	c.mu.Unlock()

	span, release := c.start(telemetry.SpanOptions{
		Name: "pi.harness.tool",
		Attributes: telemetry.SpanAttributes{
			"pi.harness.tool":         e.ToolCall.Name,
			"pi.harness.tool_blocked": e.Block,
		},
	}, parent)

	c.mu.Lock()
	c.toolSpans = append(c.toolSpans, heldSpan{span: span, release: release, blocked: e.Block})
	c.mu.Unlock()

	// The call is an event on the session span: the tool span belongs
	// to the call, the event belongs to the session's tally.
	if session := c.sessionSpan(); session != nil {
		session.AddEvent("pi.harness.tool_call", telemetry.SpanAttributes{
			"pi.harness.tool":         e.ToolCall.Name,
			"pi.harness.tool_error":   false,
			"pi.harness.tool_blocked": e.Block,
		})
	}
}

// sessionSpan returns the session span, or nil when there is none.
func (c *Collector) sessionSpan() telemetry.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// endTool settles the tool span.
func (c *Collector) endTool(e *ext.Event) {
	c.mu.Lock()
	if len(c.toolSpans) == 0 {
		c.mu.Unlock()
		return
	}
	held := c.toolSpans[len(c.toolSpans)-1]
	c.toolSpans = c.toolSpans[:len(c.toolSpans)-1]
	c.toolCalls++
	c.mu.Unlock()

	span := held.span
	if span == nil {
		return
	}
	failed := e.ToolResult != nil && e.ToolResult.IsError
	span.SetAttributes(telemetry.SpanAttributes{
		"pi.harness.tool_error":   failed,
		"pi.harness.tool_blocked": held.blocked,
	})
	if failed {
		span.SetStatus(telemetry.SpanStatus{
			Code:  telemetry.StatusError,
			Error: &telemetry.SpanError{Name: "ToolError", Message: "the tool returned an error result"},
		})
	} else {
		span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	}
	if held.release != nil {
		held.release()
	}
	if e.ToolCall != nil {
		c.RecordToolCallBlocked(e.ToolCall.Name, failed, held.blocked)
	}
}

// startCompaction opens a span for the compaction.
func (c *Collector) startCompaction(before int) {
	c.mu.Lock()
	parent := c.session
	c.mu.Unlock()

	span, release := c.start(telemetry.SpanOptions{
		Name: "pi.harness.compaction",
		Attributes: telemetry.SpanAttributes{
			"pi.harness.messages_before": before,
		},
	}, parent)

	c.mu.Lock()
	c.compaction = span
	c.releaseCompaction = release
	c.mu.Unlock()
}

// endCompaction settles the compaction span.
func (c *Collector) endCompaction(after int) {
	c.mu.Lock()
	span := c.compaction
	release := c.releaseCompaction
	c.compaction = nil
	c.releaseCompaction = nil
	c.mu.Unlock()

	if span == nil {
		return
	}
	span.SetAttributes(telemetry.SpanAttributes{
		"pi.harness.messages_after": after,
	})
	span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	if release != nil {
		release()
	}
}

// start opens a span under the parent and returns it with the release
// function that settles it.
//
// The span outlives the callback that opened it: the harness's
// operations are bracketed by events, not by a function call, so the
// collector settles the span itself.
//
// A nil parent means the collector's root context.
func (c *Collector) start(options telemetry.SpanOptions, parent telemetry.Span) (telemetry.Span, func()) {
	if parent != nil {
		return telemetry.Deferred(parent, options)
	}
	return c.startRoot(options)
}

// startRoot opens a span under the collector's root context.
func (c *Collector) startRoot(options telemetry.SpanOptions) (telemetry.Span, func()) {
	if c.Context == nil {
		return telemetry.NoopContext, func() {}
	}
	return telemetry.Deferred(c.Context, options)
}

// Teardown settles the spans still open.
func (c *Collector) Teardown() error {
	c.mu.Lock()
	session := c.session
	releaseSession := c.releaseSession
	c.session = nil
	c.releaseSession = nil
	request := c.request
	releaseRequest := c.releaseRequest
	c.request = nil
	c.releaseRequest = nil
	tools := c.toolSpans
	c.toolSpans = nil
	compaction := c.compaction
	releaseCompaction := c.releaseCompaction
	c.compaction = nil
	c.releaseCompaction = nil
	c.mu.Unlock()

	for _, held := range tools {
		if held.span != nil {
			held.span.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
		}
		if held.release != nil {
			held.release()
		}
	}
	if request != nil {
		request.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	}
	if releaseRequest != nil {
		releaseRequest()
	}
	if compaction != nil {
		compaction.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	}
	if releaseCompaction != nil {
		releaseCompaction()
	}
	if session != nil {
		session.SetStatus(telemetry.SpanStatus{Code: telemetry.StatusOK})
	}
	if releaseSession != nil {
		releaseSession()
	}
	return nil
}

// RecordUsage records the token usage of a request.
//
// The harness's providers report usage at the end of a stream; the
// collector records it as metrics, which is where usage belongs.
func (c *Collector) RecordUsage(model string, usage llm.Usage) {
	if c.Context == nil {
		return
	}
	metrics := telemetry.NewTypedMetrics(c.Context.Metrics(), telemetry.Schema)
	tokens := metrics.Counter("pi.harness.tokens")
	for _, kind := range []struct {
		name  string
		value int
	}{
		{"input", usage.InputTokens},
		{"output", usage.OutputTokens},
		{"cache_read", usage.CacheReadTokens},
		{"cache_write", usage.CacheWriteTokens},
	} {
		if kind.value == 0 {
			continue
		}
		tokens.Add(float64(kind.value), telemetry.SpanAttributes{
			"pi.harness.kind":  kind.name,
			"pi.harness.model": model,
		})
	}
}

// RecordDuration records the duration of one request.
func (c *Collector) RecordDuration(provider, model string, elapsed time.Duration) {
	if c.Context == nil {
		return
	}
	metrics := telemetry.NewTypedMetrics(c.Context.Metrics(), telemetry.Schema)
	duration := metrics.Histogram("pi.harness.request.duration")
	duration.Record(float64(elapsed.Milliseconds()), telemetry.SpanAttributes{
		"pi.harness.provider": provider,
		"pi.harness.model":    model,
	}, nil)
}

// RecordToolCall counts one tool call.
func (c *Collector) RecordToolCall(tool string, failed bool) {
	c.RecordToolCallBlocked(tool, failed, false)
}

// RecordToolCallBlocked counts one tool call and whether a permission
// rule blocked it.
func (c *Collector) RecordToolCallBlocked(tool string, failed, blocked bool) {
	if c.Context == nil {
		return
	}
	metrics := telemetry.NewTypedMetrics(c.Context.Metrics(), telemetry.Schema)
	calls := metrics.Counter("pi.harness.tool.calls")
	calls.Add(1, telemetry.SpanAttributes{
		"pi.harness.tool":         tool,
		"pi.harness.tool_error":   failed,
		"pi.harness.tool_blocked": blocked,
	})
}

// compile-time check that the collector satisfies the extension
// interface.
var _ ext.Extension = (*Collector)(nil)
