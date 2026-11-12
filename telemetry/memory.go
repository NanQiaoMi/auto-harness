package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// RecordedEvent is one point-in-time event captured on a recorded span.
type RecordedEvent struct {
	Name       string
	Attributes SpanAttributes
}

// RecordedLink is one link captured on a recorded span: the target's
// carrier and the link's attributes.
type RecordedLink struct {
	Context    Carrier
	Attributes SpanAttributes
}

// RecordedSpan is a detached snapshot of one span an InMemoryContext
// observed.
//
// The snapshot carries no timestamps: they are read back through the
// Store interface instead.
type RecordedSpan struct {
	// ID is assigned in span-start order, from 1.
	ID int
	// ParentID is the enclosing span's ID when this recorder owns it, or
	// 0 for a root, a parent owned by another recorder, or an extracted
	// parent whose span this recorder does not hold.
	ParentID int
	// Context is the span's own carrier, what Inject returns for it.
	Context Carrier
	// ParentContext is the parent's carrier, or nil for a root. A remote
	// parent has a ParentContext but no ParentID.
	ParentContext Carrier
	Links         []RecordedLink
	Name          string
	// Attributes are the start attributes merged with every
	// SetAttributes call the span accepted.
	Attributes SpanAttributes
	Events     []RecordedEvent
	Status     SpanStatus
	Settled    bool
	// EndSequence orders settlement across spans, from 1, and is 0
	// while the span is still open.
	EndSequence int
}

// RecordedMeasurement is one measurement an InMemoryContext's
// instruments recorded.
//
// It is a log entry, not an aggregate; the recorder keeps its time for
// FindMetrics, not here.
type RecordedMeasurement struct {
	Kind MetricKind
	Name string
	// Description and Unit are the instrument's.
	Description string
	Unit        string
	// Boundaries are a histogram's bucket hints; nil when it has none.
	Boundaries []float64
	Value      float64
	Attributes SpanAttributes
	// Context is the exemplar span's carrier, or nil.
	Context Carrier
}

// The in-memory carrier: the recorder's scope, the span's id, and the
// trace it belongs to.
//
// The keys are an implementation detail, not a wire format for other
// adapters.
const (
	memoryScopeKey = "pi-memory-scope"
	memorySpanKey  = "pi-memory-span"
	memoryTraceKey = "pi-memory-trace"
)

// carrierID is a carrier's globally unique span ID, "<scope>:<span>".
func carrierID(carrier Carrier) string {
	if carrier == nil {
		return ""
	}
	return carrier[memoryScopeKey] + ":" + carrier[memorySpanKey]
}

// Now returns the current time as milliseconds since the epoch.
func Now() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Millisecond)
}

// MemoryContext is the backend-neutral reference adapter.
//
// It is useful for tests, local diagnostics, and applications that
// intentionally want process-local capture without an exporter. Its
// storage is unbounded and process-local: create a fresh instance to
// isolate tests or recording scopes.
type MemoryContext struct {
	mu sync.Mutex

	// scope identifies this recorder, so a carrier from one instance is
	// not mistaken for another's.
	scope string
	// spans holds the recorded spans by ID.
	spans map[int]*memorySpan
	// order holds the span IDs in start order.
	order []int
	// nextID assigns span IDs.
	nextID int
	// measurements holds the recorded metric measurements in order.
	measurements []recordedMeasurement
	// endSequence orders settlement across spans.
	endSequence int
	// now is the clock the recorder stamps spans with; tests replace it.
	now func() float64
	// sink receives every span as it settles. It is called outside the
	// lock, so a sink may read the recorder back.
	sink func(StoredSpan)
	// measurementSink receives every measurement as it is recorded.
	measurementSink func(RecordedMeasurement)
}

// recordedMeasurement is a measurement plus the time the recorder keeps
// for its store queries.
type recordedMeasurement struct {
	RecordedMeasurement
	// timeMs is the measurement's wall-clock time.
	timeMs float64
}

// memorySpan is the live state of one recorded span.
type memorySpan struct {
	// snapshot is the recorded data.
	RecordedSpan
	// startedMs is the wall-clock start time.
	startedMs float64
	// ended is the wall-clock end time, 0 while the span is open.
	ended float64
	// live holds the attributes, events and links while the span is
	// open.
	live bool
	// explicitStatus records that the callback set the status itself, so
	// the automatic error status does not overwrite it.
	explicitStatus bool
}

// NewMemoryContext creates an in-memory recorder.
func NewMemoryContext() *MemoryContext {
	return &MemoryContext{
		scope:  newScopeID(),
		spans:  map[int]*memorySpan{},
		nextID: 1,
		now:    Now,
	}
}

// newScopeID returns a random identifier for a recorder scope.
func newScopeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A scope collision only merges two recorders' traces; a
		// time-based fallback is good enough.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// SetClock replaces the recorder's clock. Tests use it to make span
// times deterministic.
func (m *MemoryContext) SetClock(now func() float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now != nil {
		m.now = now
	}
}

// SetSink installs a function that receives every span as it settles.
//
// The sink is called outside the recorder's lock, so it may read the
// recorder back. It is how a host exports spans without polling: the
// recorder stays the owner of the data, and the sink decides where it
// goes.
//
// A nil sink removes the current one.
func (m *MemoryContext) SetSink(sink func(StoredSpan)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = sink
}

// SetMeasurementSink installs a function that receives every
// measurement as it is recorded.
//
// It is the metric counterpart of SetSink: a host that ships metrics
// somewhere gets them as they happen rather than by polling.
//
// A nil sink removes the current one.
func (m *MemoryContext) SetMeasurementSink(sink func(RecordedMeasurement)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.measurementSink = sink
}

// Metrics returns the recorder's metric capability.
func (m *MemoryContext) Metrics() Metrics { return &memoryMetrics{ctx: m} }

// Propagation returns the recorder's propagation capability.
func (m *MemoryContext) Propagation() Propagation { return &memoryPropagation{ctx: m} }

// StartSpan opens a span, runs the callback inside it, and settles the
// span when the callback returns.
//
// The callback runs synchronously, exactly once. Its error is returned
// unchanged, and the span's status becomes an error status unless the
// callback set one explicitly.
func (m *MemoryContext) StartSpan(options SpanOptions, callback func(span Span) error) error {
	if callback == nil {
		return fmt.Errorf("telemetry: StartSpan needs a callback")
	}
	span := m.start(options, nil)
	return m.run(span, callback)
}

// start creates the span record and returns its live handle.
//
// The links are resolved before the lock: a link's carrier never changes
// once its span exists.
func (m *MemoryContext) start(options SpanOptions, parent *memorySpan) *memorySpan {
	links := make([]RecordedLink, 0, len(options.Links))
	for _, link := range options.Links {
		if recorded, ok := copyLink(link); ok {
			links = append(links, recorded)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.nextID
	m.nextID++
	started := m.now()

	s := &memorySpan{
		startedMs: started,
		live:      true,
	}
	s.ID = id
	s.Name = options.Name
	s.Attributes = copyAttributes(options.Attributes)
	s.Status = SpanStatus{Code: StatusOK}
	s.Links = links

	if parent != nil {
		s.ParentID = parent.ID
		s.ParentContext = copyCarrier(parent.Context)
		s.Context = m.carrierFor(id, parent.traceID())
	} else {
		s.Context = m.carrierFor(id, "")
	}

	m.spans[id] = s
	m.order = append(m.order, id)
	return s
}

// copyLink records a link, or reports false when its target is not a
// context this recorder knows.
func copyLink(link SpanLink) (RecordedLink, bool) {
	if link.Context == nil {
		return RecordedLink{}, false
	}
	target, ok := link.Context.(interface{ carrier() Carrier })
	if !ok {
		return RecordedLink{}, false
	}
	carrier := target.carrier()
	if carrier == nil {
		return RecordedLink{}, false
	}
	return RecordedLink{Context: copyCarrier(carrier), Attributes: copyAttributes(link.Attributes)}, true
}

// traceID returns the trace a span belongs to.
func (s *memorySpan) traceID() string {
	if s.Context == nil {
		return ""
	}
	return s.Context[memoryTraceKey]
}

// carrierFor builds the carrier for a span id in a trace.
func (m *MemoryContext) carrierFor(spanID int, traceID string) Carrier {
	carrier := Carrier{
		memoryScopeKey: m.scope,
		memorySpanKey:  fmt.Sprintf("%d", spanID),
	}
	if traceID != "" {
		carrier[memoryTraceKey] = traceID
	} else {
		carrier[memoryTraceKey] = carrierID(carrier)
	}
	return carrier
}

// run executes the callback and settles the span.
//
// A panic settles the span with an error status and keeps unwinding:
// Go's panic is pi's throw.
func (m *MemoryContext) run(span *memorySpan, callback func(span Span) error) error {
	returned := false
	defer func() {
		if returned {
			return
		}
		r := recover()
		if r != nil {
			m.settle(span, panicError{value: r})
			panic(r)
		}
		m.settle(span, nil)
	}()
	err := callback(&memorySpanHandle{ctx: m, span: span})
	returned = true
	m.settle(span, err)
	return err
}

// panicError carries a recovered panic into the status derivation, so a
// panicking span is recorded as failed.
type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// settle closes the span, assigning the automatic status when the
// callback did not set one.
//
// A status the callback set explicitly survives a failing callback: the
// callback knew more about what happened than the error does.
func (m *MemoryContext) settle(span *memorySpan, err error) {
	m.mu.Lock()
	if !span.live {
		// A settled span ignores further calls.
		m.mu.Unlock()
		return
	}
	span.live = false
	m.endSequence++
	span.EndSequence = m.endSequence
	span.Settled = true
	span.ended = m.now()
	if err != nil && !span.explicitStatus {
		span.Status = SpanStatus{
			Code:  StatusError,
			Error: &SpanError{Name: errorName(err), Message: err.Error()},
		}
	}
	stored := span.stored()
	sink := m.sink
	m.mu.Unlock()

	// The sink runs outside the lock: it may read the recorder back.
	if sink != nil {
		sink(stored)
	}
}

// errorName classifies an error the way a JS error name does: the
// concrete type of the error.
func errorName(err error) string {
	if err == nil {
		return ""
	}
	var pe panicError
	if errors.As(err, &pe) {
		return "panic"
	}
	return fmt.Sprintf("%T", err)
}

// memorySpanHandle is the Span the callback receives.
type memorySpanHandle struct {
	ctx  *MemoryContext
	span *memorySpan
}

// inertSpan is what a child of a settled parent runs on: nothing is
// recorded, its own children are inert too, and it keeps the recorder's
// capabilities.
type inertSpan struct{ ctx *MemoryContext }

func (s inertSpan) StartSpan(_ SpanOptions, callback func(span Span) error) error {
	return callback(s)
}

func (s inertSpan) Metrics() Metrics              { return s.ctx.Metrics() }
func (s inertSpan) Propagation() Propagation      { return s.ctx.Propagation() }
func (inertSpan) AddLink(SpanLink)                {}
func (inertSpan) AddEvent(string, SpanAttributes) {}
func (inertSpan) SetAttributes(SpanAttributes)    {}
func (inertSpan) SetStatus(SpanStatus)            {}

// StartSpan starts a child span of the handle.
//
// A child of an already-settled parent is not recorded: it runs on an
// inert span, so the recorder keeps its capabilities without gaining a
// span that outlives its parent.
func (h *memorySpanHandle) StartSpan(options SpanOptions, callback func(span Span) error) error {
	if callback == nil {
		return fmt.Errorf("telemetry: StartSpan needs a callback")
	}
	h.ctx.mu.Lock()
	settled := !h.span.live
	h.ctx.mu.Unlock()
	if settled {
		return inertSpan{ctx: h.ctx}.StartSpan(options, callback)
	}
	child := h.ctx.start(options, h.span)
	handle := &memorySpanHandle{ctx: h.ctx, span: child}
	return h.ctx.run(child, func(Span) error { return callback(handle) })
}

// Metrics returns the recorder's metric capability.
func (h *memorySpanHandle) Metrics() Metrics { return h.ctx.Metrics() }

// Propagation returns the recorder's propagation capability.
func (h *memorySpanHandle) Propagation() Propagation { return h.ctx.Propagation() }

// AddLink relates the span to a target without changing its parent.
func (h *memorySpanHandle) AddLink(link SpanLink) {
	// The link's carrier is resolved before the lock: taking it here
	// would deadlock against a link to this very span.
	recorded, ok := copyLink(link)
	if !ok {
		return
	}
	h.ctx.mu.Lock()
	defer h.ctx.mu.Unlock()
	if !h.span.live {
		return
	}
	h.span.Links = append(h.span.Links, recorded)
}

// carrier exposes the span's carrier for link and propagation.
func (h *memorySpanHandle) carrier() Carrier {
	h.ctx.mu.Lock()
	defer h.ctx.mu.Unlock()
	return copyCarrier(h.span.Context)
}

// AddEvent records a point-in-time event on the span.
func (h *memorySpanHandle) AddEvent(name string, attributes SpanAttributes) {
	h.ctx.mu.Lock()
	defer h.ctx.mu.Unlock()
	if !h.span.live {
		return
	}
	h.span.Events = append(h.span.Events, RecordedEvent{
		Name:       name,
		Attributes: copyAttributes(attributes),
	})
}

// SetAttributes merges attributes onto the span.
//
// A later defined value replaces an earlier one; an undefined value is
// ignored.
func (h *memorySpanHandle) SetAttributes(attributes SpanAttributes) {
	h.ctx.mu.Lock()
	defer h.ctx.mu.Unlock()
	if !h.span.live {
		return
	}
	if h.span.Attributes == nil {
		h.span.Attributes = SpanAttributes{}
	}
	for name, value := range attributes {
		if value == nil {
			continue
		}
		h.span.Attributes[name] = value
	}
}

// SetStatus sets the span's terminal status.
//
// Repeated calls are last-write-wins, and the status survives a failing
// callback.
func (h *memorySpanHandle) SetStatus(status SpanStatus) {
	h.ctx.mu.Lock()
	defer h.ctx.mu.Unlock()
	if !h.span.live {
		return
	}
	h.span.Status = status
	h.span.explicitStatus = true
}

// GetSpans returns detached snapshots in span-start order.
func (m *MemoryContext) GetSpans() []RecordedSpan {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedSpan, 0, len(m.order))
	for _, id := range m.order {
		s := m.spans[id]
		if s == nil {
			continue
		}
		out = append(out, s.snapshot())
	}
	return out
}

// snapshot copies the recorded data.
func (s *memorySpan) snapshot() RecordedSpan {
	snap := s.RecordedSpan
	snap.Attributes = copyAttributes(s.Attributes)
	snap.Events = make([]RecordedEvent, len(s.Events))
	for i, e := range s.Events {
		snap.Events[i] = RecordedEvent{Name: e.Name, Attributes: copyAttributes(e.Attributes)}
	}
	snap.Links = make([]RecordedLink, len(s.Links))
	for i, l := range s.Links {
		snap.Links[i] = RecordedLink{Context: copyCarrier(l.Context), Attributes: copyAttributes(l.Attributes)}
	}
	snap.Context = copyCarrier(s.Context)
	snap.ParentContext = copyCarrier(s.ParentContext)
	return snap
}

// GetMeasurements returns the recorded measurements in order.
func (m *MemoryContext) GetMeasurements() []RecordedMeasurement {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedMeasurement, 0, len(m.measurements))
	for _, meas := range m.measurements {
		snap := meas.RecordedMeasurement
		snap.Attributes = copyAttributes(meas.Attributes)
		snap.Context = copyCarrier(meas.Context)
		out = append(out, snap)
	}
	return out
}

// memoryMetrics is the recorder's metric capability.
type memoryMetrics struct {
	ctx *MemoryContext
}

// Counter creates a counter.
func (m *memoryMetrics) Counter(name string, options InstrumentOptions) Counter {
	return &memoryCounter{metrics: m, name: name, options: options}
}

// UpDownCounter creates an up-down counter.
func (m *memoryMetrics) UpDownCounter(name string, options InstrumentOptions) UpDownCounter {
	return &memoryCounter{metrics: m, name: name, options: options}
}

// Histogram creates a histogram.
func (m *memoryMetrics) Histogram(name string, options HistogramOptions) Histogram {
	return &memoryHistogram{metrics: m, name: name, options: options}
}

// memoryCounter is a counter and an up-down counter.
type memoryCounter struct {
	metrics *memoryMetrics
	name    string
	options InstrumentOptions
}

// Add records an increment.
func (c *memoryCounter) Add(value float64, attributes SpanAttributes) {
	c.metrics.ctx.record(RecordedMeasurement{
		Kind:        MetricCounter,
		Name:        c.name,
		Description: c.options.Description,
		Unit:        c.options.Unit,
		Value:       value,
		Attributes:  copyAttributes(attributes),
	})
}

// memoryHistogram is a histogram.
type memoryHistogram struct {
	metrics *memoryMetrics
	name    string
	options HistogramOptions
}

// Record records a measurement, optionally correlated with a span.
func (h *memoryHistogram) Record(value float64, attributes SpanAttributes, context Context) {
	var carrier Carrier
	if context != nil {
		if c, ok := context.(interface{ carrier() Carrier }); ok {
			carrier = copyCarrier(c.carrier())
		}
	}
	h.metrics.ctx.record(RecordedMeasurement{
		Kind:        MetricHistogram,
		Name:        h.name,
		Description: h.options.Description,
		Unit:        h.options.Unit,
		Boundaries:  h.options.Boundaries,
		Value:       value,
		Attributes:  copyAttributes(attributes),
		Context:     carrier,
	})
}

// record appends a measurement.
func (m *MemoryContext) record(meas RecordedMeasurement) {
	m.mu.Lock()
	m.measurements = append(m.measurements, recordedMeasurement{
		RecordedMeasurement: meas,
		timeMs:              m.now(),
	})
	sink := m.measurementSink
	m.mu.Unlock()

	// The sink runs outside the lock: it may read the recorder back.
	if sink != nil {
		sink(meas)
	}
}

// memoryPropagation is the recorder's propagation capability.
type memoryPropagation struct {
	ctx *MemoryContext
}

// Inject returns a detached carrier for the target.
func (p *memoryPropagation) Inject(target Context) Carrier {
	if target == nil {
		return nil
	}
	c, ok := target.(interface{ carrier() Carrier })
	if !ok {
		return nil
	}
	carrier := c.carrier()
	if carrier == nil {
		return nil
	}
	return copyCarrier(carrier)
}

// Extract returns a parent for the carrier.
//
// A nil carrier, or one missing the recorder's keys, falls back to the
// recorder's root. The values are opaque and not checked; a carrier from
// another scope names a parent this recorder does not own, which is the
// point of propagation.
func (p *memoryPropagation) Extract(carrier Carrier) Context {
	if carrier == nil {
		return p.root()
	}
	if carrier[memoryScopeKey] == "" || carrier[memorySpanKey] == "" || carrier[memoryTraceKey] == "" {
		return p.root()
	}
	parent := &remoteParent{ctx: p.ctx, extracted: copyCarrier(carrier)}
	// A carrier that names one of this recorder's own spans has a local
	// parent id; a carrier from another scope does not.
	p.ctx.mu.Lock()
	if carrier[memoryScopeKey] == p.ctx.scope {
		if n, err := strconv.Atoi(carrier[memorySpanKey]); err == nil {
			parent.localID = n
		}
	}
	p.ctx.mu.Unlock()
	return parent
}

// root returns the recorder's root context.
func (p *memoryPropagation) root() Context {
	return &remoteParent{ctx: p.ctx}
}

// remoteParent is a parent this recorder does not own: an extracted
// carrier, or the recorder's root.
type remoteParent struct {
	ctx       *MemoryContext
	extracted Carrier
	// localID is the extracted span's id in this recorder, when the
	// carrier names one of its own spans; 0 otherwise.
	localID int
}

// StartSpan starts a span under the remote parent.
//
// The child's parent context is the extracted carrier, and it joins the
// extracted trace: the work belongs to the caller's trace even though
// this recorder owns only the child.
func (r *remoteParent) StartSpan(options SpanOptions, callback func(span Span) error) error {
	if callback == nil {
		return fmt.Errorf("telemetry: StartSpan needs a callback")
	}
	child := r.ctx.start(options, nil)
	// The remote parent's carrier becomes the child's parent context.
	if r.extracted != nil {
		r.ctx.mu.Lock()
		child.ParentContext = copyCarrier(r.extracted)
		child.Context[memoryTraceKey] = r.extracted[memoryTraceKey]
		child.ParentID = r.localID
		r.ctx.mu.Unlock()
	}
	handle := &memorySpanHandle{ctx: r.ctx, span: child}
	return r.ctx.run(child, func(Span) error { return callback(handle) })
}

// Metrics returns the recorder's metric capability.
func (r *remoteParent) Metrics() Metrics { return r.ctx.Metrics() }

// Propagation returns the recorder's propagation capability.
func (r *remoteParent) Propagation() Propagation { return r.ctx.Propagation() }

// carrier returns the parent's carrier, or nil for the root.
func (r *remoteParent) carrier() Carrier { return copyCarrier(r.extracted) }

// copyAttributes copies an attribute map, dropping nil values.
func copyAttributes(attrs SpanAttributes) SpanAttributes {
	if attrs == nil {
		return nil
	}
	out := make(SpanAttributes, len(attrs))
	for name, value := range attrs {
		if value == nil {
			continue
		}
		out[name] = value
	}
	return out
}

// copyCarrier copies a carrier.
func copyCarrier(c Carrier) Carrier {
	if c == nil {
		return nil
	}
	out := make(Carrier, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// FindTraces returns one page of the traces the query matches.
//
// Only settled spans are returned. The newest trace start comes first,
// with the trace ID breaking ties.
func (m *MemoryContext) FindTraces(ctx context.Context, query TraceQuery) (TracePage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit < 0 {
		return TracePage{}, fmt.Errorf("telemetry: negative page size %d", query.Limit)
	}

	traces := m.collectTraces(query)

	// The cursor is the index of the next trace, as a decimal string.
	start := 0
	if query.Cursor != "" {
		var idx int
		if _, err := fmt.Sscanf(query.Cursor, "%d", &idx); err == nil {
			start = idx
		}
	}
	if start > len(traces) {
		start = len(traces)
	}
	end := start + limit
	if end > len(traces) {
		end = len(traces)
	}
	page := TracePage{Traces: traces[start:end]}
	if end < len(traces) {
		page.Cursor = fmt.Sprintf("%d", end)
	}
	return page, nil
}

// collectTraces gathers the traces that match the query.
func (m *MemoryContext) collectTraces(query TraceQuery) []StoredTrace {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Group the settled spans by trace.
	byTrace := map[string][]StoredSpan{}
	for _, id := range m.order {
		s := m.spans[id]
		if s == nil || !s.Settled {
			continue
		}
		stored := s.stored()
		byTrace[stored.TraceID] = append(byTrace[stored.TraceID], stored)
	}

	out := make([]StoredTrace, 0, len(byTrace))
	for _, spans := range byTrace {
		trace := StoredTrace{TraceID: spans[0].TraceID, Spans: spans}
		if !traceMatches(trace, query) {
			continue
		}
		out = append(out, trace)
	}
	// The newest trace start first; the trace ID breaks ties.
	sort.Slice(out, func(i, j int) bool {
		si, sj := traceStart(out[i]), traceStart(out[j])
		if si != sj {
			return si > sj
		}
		return out[i].TraceID < out[j].TraceID
	})
	return out
}

// traceStart returns the earliest span start in a trace.
func traceStart(trace StoredTrace) float64 {
	best := 0.0
	for i, s := range trace.Spans {
		if i == 0 || s.StartedMs < best {
			best = s.StartedMs
		}
	}
	return best
}

// traceMatches reports whether a trace satisfies the query.
func traceMatches(trace StoredTrace, query TraceQuery) bool {
	// The first span is the earliest stored one.
	first := trace.Spans[0]
	for _, s := range trace.Spans {
		if s.StartedMs < first.StartedMs {
			first = s
		}
	}
	if query.Name != "" && first.Name != query.Name {
		return false
	}
	if query.StartMin != nil && first.StartedMs < *query.StartMin {
		return false
	}
	if query.StartMax != nil && first.StartedMs > *query.StartMax {
		return false
	}
	if query.Errors {
		found := false
		for _, s := range trace.Spans {
			if s.Status.Code == StatusError {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// All the requested attributes must match one span, anywhere in the
	// trace.
	if len(query.Attributes) > 0 {
		matched := false
		for _, s := range trace.Spans {
			if attributesMatch(s.Attributes, query.Attributes) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// attributesMatch reports whether every wanted attribute equals the
// span's.
func attributesMatch(have, want SpanAttributes) bool {
	for name, value := range want {
		got, ok := have[name]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", value) {
			return false
		}
	}
	return true
}

// GetTrace returns the trace the ID names, or nil when the recorder
// holds none of its spans.
func (m *MemoryContext) GetTrace(ctx context.Context, traceID string) (*StoredTrace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var spans []StoredSpan
	for _, id := range m.order {
		s := m.spans[id]
		if s == nil || !s.Settled {
			continue
		}
		stored := s.stored()
		if stored.TraceID == traceID {
			spans = append(spans, stored)
		}
	}
	if len(spans) == 0 {
		return nil, nil
	}
	return &StoredTrace{TraceID: traceID, Spans: spans}, nil
}

// stored builds the timed snapshot of a span.
func (s *memorySpan) stored() StoredSpan {
	events := make([]StoredEvent, len(s.Events))
	for i, e := range s.Events {
		events[i] = StoredEvent{Name: e.Name, Attributes: copyAttributes(e.Attributes)}
	}
	links := make([]StoredLink, len(s.Links))
	for i, l := range s.Links {
		links[i] = StoredLink{
			TraceID:    l.Context[memoryTraceKey],
			SpanID:     l.Context[memorySpanKey],
			Attributes: copyAttributes(l.Attributes),
		}
	}
	// The parent id is empty for a root, the way the contract says.
	parentID := ""
	if s.ParentID != 0 {
		parentID = fmt.Sprintf("%d", s.ParentID)
	}
	return StoredSpan{
		ID:         fmt.Sprintf("%d", s.ID),
		TraceID:    s.traceID(),
		ParentID:   parentID,
		Name:       s.Name,
		StartedMs:  s.startedMs,
		EndedMs:    s.endedMs(),
		Attributes: copyAttributes(s.Attributes),
		Events:     events,
		Links:      links,
		Status:     s.Status,
	}
}

// endedMs returns the span's end time.
func (s *memorySpan) endedMs() float64 {
	if s.ended > 0 {
		return s.ended
	}
	return s.startedMs
}

// FindMetrics returns the metric rows the query matches.
//
// Each raw measurement maps to a count-one row in its ten-second
// interval.
func (m *MemoryContext) FindMetrics(ctx context.Context, query MetricQuery) ([]StoredMetric, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows := map[string]*StoredMetric{}
	for _, meas := range m.measurements {
		if query.Name != "" && meas.Name != query.Name {
			continue
		}
		interval := float64(int(meas.timeMs/10000) * 10000)
		if query.StartMin != nil && interval < *query.StartMin {
			continue
		}
		if query.StartMax != nil && interval > *query.StartMax {
			continue
		}
		key := fmt.Sprintf("%s|%v|%v", meas.Name, interval, attributeKey(meas.Attributes))
		row, ok := rows[key]
		if !ok {
			row = &StoredMetric{
				IntervalMs: interval,
				Name:       meas.Name,
				Kind:       meas.Kind,
				Unit:       meas.Unit,
				Attributes: copyAttributes(meas.Attributes),
				Count:      0,
				Min:        meas.Value,
				Max:        meas.Value,
			}
			rows[key] = row
		}
		row.Count++
		row.Sum += meas.Value
		if meas.Value < row.Min {
			row.Min = meas.Value
		}
		if meas.Value > row.Max {
			row.Max = meas.Value
		}
	}
	out := make([]StoredMetric, 0, len(rows))
	for _, row := range rows {
		if len(query.Attributes) > 0 && !attributesMatch(row.Attributes, query.Attributes) {
			continue
		}
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IntervalMs != out[j].IntervalMs {
			return out[i].IntervalMs < out[j].IntervalMs
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// attributeKey renders an attribute map as a stable key.
func attributeKey(attrs SpanAttributes) string {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	var b []byte
	for _, name := range names {
		b = append(b, name...)
		b = append(b, '=')
		b = append(b, fmt.Sprintf("%v", attrs[name])...)
		b = append(b, ';')
	}
	return string(b)
}
