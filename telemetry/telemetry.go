// Package telemetry implements a vendor-neutral telemetry contract for
// the harness, modeled after pi's `@earendil-works/pi-telemetry`.
//
// The contract has three parts:
//
//   - A Context starts a Span around a callback. The span records
//     attributes, events and a status; it settles when the callback
//     returns.
//   - Metrics and Propagation are optional adapter capabilities. Every
//     context the adapter hands out carries them, so a nested
//     operation can record without a second configuration.
//   - A Store reads settled traces and metric rows back without
//     depending on a storage backend.
//
// The package ships no exporter and no global "current span" state: a
// host wires in the adapter it wants, or none at all (NoopContext).
// Instrumentation passes telemetry contexts explicitly, so an operation
// belongs where the caller put it.
//
// Design notes:
//
//   - Go interface methods cannot be generic, so StartSpan takes an
//     error-returning callback instead of pi's value-returning one.
//     Results travel out through closure captures.
//   - pi's adapter contract trusts typed inputs: an adapter need not
//     validate measurement values, metadata, attributes, boundaries or
//     carrier identifiers, and what it makes of malformed ones is
//     unspecified.
//   - Every type here is safe for concurrent use by multiple
//     goroutines. pi is single-threaded; Go callers are not.
package telemetry

import (
	"context"
	"sync"
)

// AttributeValue is a single telemetry attribute value.
//
// pi types it as the closed union string | number | boolean | arrays of
// those. Go has no union types, so this is `any` under a documented
// contract: producers store a string, a bool, an int or float kind, or
// a slice of those, and an implementation may drop anything else.
type AttributeValue any

// SpanAttributes names the attribute values on a span or event.
//
// A nil map is pi's absent `attributes`, and a nil value is pi's
// undefined entry; implementations treat both as not present.
type SpanAttributes map[string]AttributeValue

// Carrier is an opaque, JSON-safe string map an adapter injects into
// and extracts from a transport.
//
// The adapter owns its format; W3C traceparent and tracestate fit
// without requiring them.
type Carrier map[string]string

// Propagation carries a parent's identity across a process or transport
// boundary.
//
// Both methods are synchronous.
type Propagation interface {
	// Inject returns a detached carrier for a span or extracted parent
	// the adapter knows, settled or not; nil for its root, a no-op
	// context, or a context it does not recognize.
	Inject(target Context) Carrier
	// Extract returns a receiver-owned parent for carrier. A nil or
	// unknown carrier falls back to the adapter's root; the identifiers
	// of a recognized one are opaque strings, not checked for format.
	Extract(carrier Carrier) Context
}

// SpanLink relates a span to another context without changing its
// parent.
//
// The target is a span or an extracted parent; an adapter ignores a
// root, no-op or unrecognized target.
type SpanLink struct {
	Context    Context
	Attributes SpanAttributes
}

// SpanOptions name a span being started and optionally seed its
// attributes and links.
type SpanOptions struct {
	Name string
	// Attributes seed the span's start attributes; nil starts it bare.
	Attributes SpanAttributes
	// Links relate the span to targets known at creation; Span.AddLink
	// adds later ones.
	Links []SpanLink
}

// StatusCode is the terminal disposition a span reports.
type StatusCode string

const (
	StatusOK    StatusCode = "ok"
	StatusError StatusCode = "error"
)

// SpanError identifies the failure behind an error status.
//
// Name classifies the failure the way a JS error name does — a Go
// caller typically uses the error's concrete type — and Message is its
// rendered text.
type SpanError struct {
	Name    string
	Message string
}

// SpanStatus is the terminal status a span reports.
//
// Error is meaningful only when Code is StatusError.
type SpanStatus struct {
	Code  StatusCode
	Error *SpanError
}

// Context starts spans for whatever telemetry consumer the host
// application wired in.
//
// A host keeps its adapter, which starts root spans; request options
// and invocation contexts carry a Span. The adapter's capabilities stay
// available on every context it hands out — child spans, extracted
// parents, and the inert spans it falls back to.
//
// Implementations must be safe for concurrent use by multiple
// goroutines, and so must every Span, Metrics, instrument and
// Propagation they hand out.
type Context interface {
	// StartSpan opens a span, runs callback inside it — synchronously,
	// exactly once — and closes the span when the callback returns. The
	// callback's error is returned to the caller unchanged.
	StartSpan(options SpanOptions, callback func(span Span) error) error
	// Metrics is the adapter's metric capability, or nil for an adapter
	// that only traces.
	Metrics() Metrics
	// Propagation is the adapter's propagation capability, or nil when
	// it has none.
	Propagation() Propagation
}

// Span is a live span: a Context for starting children, plus the
// mutators that record what happened inside it.
//
// Every mutator is synchronous and ignored once the span settled. A
// Span is safe for concurrent use.
type Span interface {
	Context
	// AddLink relates the span to link.Context without changing its
	// parent.
	AddLink(link SpanLink)
	// AddEvent records a point-in-time event on the span; nil
	// attributes record it bare.
	AddEvent(name string, attributes SpanAttributes)
	// SetAttributes merges attributes onto the span.
	SetAttributes(attributes SpanAttributes)
	// SetStatus sets the span's terminal status.
	SetStatus(status SpanStatus)
}

// noopSpan is the one inert span behind NoopContext.
//
// It is fieldless, so it retains nothing it is handed, and every child
// StartSpan hands back the same shared span.
type noopSpan struct{}

func (s noopSpan) StartSpan(_ SpanOptions, callback func(span Span) error) error {
	return callback(s)
}

func (noopSpan) Metrics() Metrics                { return NoopMetrics }
func (noopSpan) Propagation() Propagation        { return NoopPropagation }
func (noopSpan) AddLink(SpanLink)                {}
func (noopSpan) AddEvent(string, SpanAttributes) {}
func (noopSpan) SetAttributes(SpanAttributes)    {}
func (noopSpan) SetStatus(SpanStatus)            {}

// NoopContext is the shared telemetry context used when an application
// does not provide one.
//
// It is a Span, so it can stand in wherever a parent span is expected.
// It admits every callback synchronously on the caller's goroutine,
// exposes NoopMetrics and NoopPropagation, and discards everything
// else.
var NoopContext Span = noopSpan{}

// Deferred starts a span that settles when the returned release
// function is called.
//
// pi keeps a span open across an asynchronous operation by returning a
// promise from the callback: the span settles when the promise
// resolves. Go's callback is synchronous, so the equivalent is a
// callback that blocks until the work is done — which means StartSpan
// has to run on its own goroutine.
//
// The returned span is ready to use: Deferred waits for the adapter to
// hand it over. The release function settles the span and is safe to
// call more than once; calling it is what ends the span. It returns
// once the span has settled, so a caller that releases a span can read
// the settled state right afterwards.
//
// A nil release means the span never settles, which leaks it. Always
// release, including on the error path.
func Deferred(ctx Context, options SpanOptions) (Span, func()) {
	if ctx == nil {
		return NoopContext, func() {}
	}
	ready := make(chan Span, 1)
	done := make(chan struct{})
	settled := make(chan struct{})
	var once sync.Once
	release := func() {
		once.Do(func() { close(done) })
		<-settled
	}

	go func() {
		defer close(settled)
		_ = ctx.StartSpan(options, func(span Span) error {
			ready <- span
			<-done
			return nil
		})
		// The adapter settled the span without ever handing it over
		// (a contract violation). Unblock the waiter rather than
		// leaking it.
		select {
		case ready <- NoopContext:
		default:
		}
	}()

	span := <-ready
	return span, release
}

// noopPropagation ignores its arguments.
type noopPropagation struct{}

func (noopPropagation) Inject(Context) Carrier  { return nil }
func (noopPropagation) Extract(Carrier) Context { return NoopContext }

// NoopPropagation is the shared propagation capability that ignores its
// arguments: Inject returns nil and Extract returns NoopContext.
var NoopPropagation Propagation = noopPropagation{}

// StoredEvent is one event of a StoredSpan.
//
// TimeMs is its offset from the span's start, in milliseconds.
type StoredEvent struct {
	Name       string
	TimeMs     float64
	Attributes SpanAttributes
}

// StoredLink is one link of a StoredSpan: the target's trace and span
// IDs, and the link's attributes.
type StoredLink struct {
	TraceID    string
	SpanID     string
	Attributes SpanAttributes
}

// StoredSpan is a settled span snapshot, independent of the adapter
// that recorded it.
//
// Times are epoch milliseconds at microsecond resolution, measured with
// a monotonic clock from the span's start, so EndedMs-StartedMs and
// event offsets are exact while wall clocks across processes may
// disagree. Parent relationships, not timestamp containment, define a
// trace's tree.
type StoredSpan struct {
	ID      string
	TraceID string
	// ParentID is the parent span's ID; empty for a root. The parent
	// itself may be missing from a store: remote, expired, or not
	// settled yet.
	ParentID   string
	Name       string
	StartedMs  float64
	EndedMs    float64
	Attributes SpanAttributes
	Events     []StoredEvent
	Links      []StoredLink
	Status     SpanStatus
}

// StoredTrace is the spans a store holds for one trace.
type StoredTrace struct {
	TraceID string
	Spans   []StoredSpan
}

// TraceQuery filters and pages Store.FindTraces.
//
// A trace's first span is its earliest stored one, by start time with
// span ID breaking ties.
type TraceQuery struct {
	// Name, when set, is the first span's name.
	Name string
	// Attributes, when set, must all equal the attributes of one span,
	// which may be anywhere in the trace. Values are scalars.
	Attributes SpanAttributes
	// Errors keeps only the traces with a span whose status is an error.
	Errors bool
	// StartMin and StartMax, when set, are inclusive bounds on the first
	// span's start time.
	StartMin *float64
	StartMax *float64
	// Limit is the page size, positive; 0 is the default of 20. A
	// negative Limit is refused.
	Limit int
	// Cursor continues after the page that returned it; pass the same
	// filters with it. Empty starts at the newest trace.
	Cursor string
}

// TracePage is one page of Store.FindTraces: complete traces, the newest
// first span first and trace ID breaking ties, and the Cursor of the next
// page, empty on the last.
type TracePage struct {
	Traces []StoredTrace
	Cursor string
}

// MetricQuery filters Store.FindMetrics.
type MetricQuery struct {
	Name string
	// StartMin and StartMax, when set, are inclusive bounds on an
	// interval's start time.
	StartMin *float64
	StartMax *float64
	// Attributes, when set, must all equal the row's. Values are
	// scalars.
	Attributes SpanAttributes
}

// StoredMetric is one metric row: the measurements of one series within
// one interval.
type StoredMetric struct {
	// IntervalMs is the interval's start, in epoch milliseconds.
	IntervalMs float64
	Name       string
	Kind       MetricKind
	// Unit is the instrument's; empty is the absent key.
	Unit       string
	Attributes SpanAttributes
	Count      int
	Sum        float64
	Min        float64
	Max        float64
}

// Store reads settled traces and metric rows back without depending on
// a storage backend.
//
// It returns only settled spans, and only those it holds: a child can
// appear before its parent settles, and pages are not a snapshot across
// calls. Results are detached from its state. It must be safe for
// concurrent use.
type Store interface {
	// FindTraces returns one page of the traces query matches.
	FindTraces(ctx context.Context, query TraceQuery) (TracePage, error)
	// GetTrace returns the trace traceID names, or nil when the store
	// holds none of its spans.
	GetTrace(ctx context.Context, traceID string) (*StoredTrace, error)
	// FindMetrics returns the metric rows query matches.
	FindMetrics(ctx context.Context, query MetricQuery) ([]StoredMetric, error)
}
