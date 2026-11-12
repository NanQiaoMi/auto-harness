package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestNoopContextRunsTheCallback verifies the no-op context admits the
// callback synchronously and returns its error.
func TestNoopContextRunsTheCallback(t *testing.T) {
	called := false
	err := NoopContext.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("StartSpan: %v", err)
	}
	if !called {
		t.Error("the callback was not called")
	}
}

// TestNoopContextRetainsNothing verifies the no-op span ignores
// everything it is handed.
func TestNoopContextRetainsNothing(t *testing.T) {
	err := NoopContext.StartSpan(SpanOptions{
		Name:       "x",
		Attributes: SpanAttributes{"a": 1},
	}, func(span Span) error {
		span.AddEvent("e", SpanAttributes{"b": 2})
		span.SetAttributes(SpanAttributes{"c": 3})
		span.SetStatus(SpanStatus{Code: StatusError})
		span.AddLink(SpanLink{Context: NoopContext})
		return nil
	})
	if err != nil {
		t.Fatalf("StartSpan: %v", err)
	}
}

// TestNoopContextReturnsTheCallbackError verifies the error travels
// back unchanged.
func TestNoopContextReturnsTheCallbackError(t *testing.T) {
	want := errors.New("boom")
	got := NoopContext.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		return want
	})
	if !errors.Is(got, want) {
		t.Errorf("error = %v, want %v", got, want)
	}
}

// TestNoopMetricsDiscard verifies the no-op instruments keep nothing.
func TestNoopMetricsDiscard(t *testing.T) {
	counter := NoopMetrics.Counter("c", InstrumentOptions{})
	counter.Add(1, nil)
	histogram := NoopMetrics.Histogram("h", HistogramOptions{})
	histogram.Record(1, nil, nil)
}

// TestNoopPropagation verifies the no-op propagation ignores its
// arguments.
func TestNoopPropagation(t *testing.T) {
	if carrier := NoopPropagation.Inject(NoopContext); carrier != nil {
		t.Errorf("carrier = %v, want nil", carrier)
	}
	if got := NoopPropagation.Extract(Carrier{"a": "b"}); got != NoopContext {
		t.Errorf("context = %v, want the no-op context", got)
	}
}

// TestMemoryRecordsTheSpan verifies the reference adapter records what
// the callback did.
func TestMemoryRecordsTheSpan(t *testing.T) {
	m := NewMemoryContext()
	err := m.StartSpan(SpanOptions{
		Name:       "example.read",
		Attributes: SpanAttributes{"example.resource": "account"},
	}, func(span Span) error {
		span.AddEvent("example.cache", SpanAttributes{"example.cache.hit": true})
		span.SetAttributes(SpanAttributes{"example.item_count": 3})
		return nil
	})
	if err != nil {
		t.Fatalf("StartSpan: %v", err)
	}

	spans := m.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name != "example.read" {
		t.Errorf("name = %q", s.Name)
	}
	if !s.Settled {
		t.Error("the span is not settled")
	}
	if s.Attributes["example.resource"] != "account" {
		t.Errorf("attributes = %v", s.Attributes)
	}
	if s.Attributes["example.item_count"] != 3 {
		t.Errorf("attributes = %v, want the end attribute merged", s.Attributes)
	}
	if len(s.Events) != 1 || s.Events[0].Name != "example.cache" {
		t.Errorf("events = %v", s.Events)
	}
	if s.Status.Code != StatusOK {
		t.Errorf("status = %v", s.Status)
	}
}

// TestMemoryErrorStatus verifies a failing callback settles the span
// with an error status.
func TestMemoryErrorStatus(t *testing.T) {
	m := NewMemoryContext()
	want := errors.New("boom")
	err := m.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	spans := m.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Status.Code != StatusError {
		t.Errorf("status = %v, want an error", spans[0].Status)
	}
	if spans[0].Status.Error == nil {
		t.Fatal("the error status carries no error")
	}
	if spans[0].Status.Error.Name == "" {
		t.Error("the error has no name")
	}
}

// TestMemoryExplicitStatusWins verifies an explicit status survives a
// failing callback.
func TestMemoryExplicitStatusWins(t *testing.T) {
	m := NewMemoryContext()
	_ = m.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		span.SetStatus(SpanStatus{Code: StatusOK})
		return errors.New("boom")
	})
	spans := m.GetSpans()
	if spans[0].Status.Code != StatusOK {
		t.Errorf("status = %v, want ok (the explicit status)", spans[0].Status)
	}
}

// TestMemoryNestsSpans verifies the parent-child relationship.
func TestMemoryNestsSpans(t *testing.T) {
	m := NewMemoryContext()
	_ = m.StartSpan(SpanOptions{Name: "parent"}, func(parent Span) error {
		return parent.StartSpan(SpanOptions{Name: "child"}, func(child Span) error {
			return nil
		})
	})
	spans := m.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if spans[0].Name != "parent" || spans[0].ParentID != 0 {
		t.Errorf("parent = %+v", spans[0])
	}
	if spans[1].Name != "child" || spans[1].ParentID != spans[0].ID {
		t.Errorf("child = %+v, want the parent's id", spans[1])
	}
}

// TestMemoryAttributeMerging verifies later attributes replace earlier
// ones and undefined values are ignored.
func TestMemoryAttributeMerging(t *testing.T) {
	m := NewMemoryContext()
	_ = m.StartSpan(SpanOptions{
		Name:       "x",
		Attributes: SpanAttributes{"a": 1, "b": 2},
	}, func(span Span) error {
		span.SetAttributes(SpanAttributes{"a": 3})
		span.SetAttributes(SpanAttributes{"b": nil})
		return nil
	})
	spans := m.GetSpans()
	if spans[0].Attributes["a"] != 3 {
		t.Errorf("a = %v, want 3", spans[0].Attributes["a"])
	}
	if spans[0].Attributes["b"] != 2 {
		t.Errorf("b = %v, want the original 2", spans[0].Attributes["b"])
	}
}

// TestMemoryPostSettlementCallsAreInert verifies a settled span ignores
// further calls.
func TestMemoryPostSettlementCallsAreInert(t *testing.T) {
	m := NewMemoryContext()
	var held Span
	_ = m.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		held = span
		return nil
	})
	held.AddEvent("late", nil)
	held.SetAttributes(SpanAttributes{"late": true})
	held.SetStatus(SpanStatus{Code: StatusError})

	spans := m.GetSpans()
	if len(spans[0].Events) != 0 {
		t.Errorf("events = %v, want none after settlement", spans[0].Events)
	}
	if spans[0].Attributes["late"] != nil {
		t.Errorf("attributes = %v, want none after settlement", spans[0].Attributes)
	}
	if spans[0].Status.Code != StatusOK {
		t.Errorf("status = %v, want ok (the calls were inert)", spans[0].Status)
	}
}

// TestMemoryLastStatusWins verifies repeated status calls.
func TestMemoryLastStatusWins(t *testing.T) {
	m := NewMemoryContext()
	_ = m.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		span.SetStatus(SpanStatus{Code: StatusError})
		span.SetStatus(SpanStatus{Code: StatusOK})
		return nil
	})
	if m.GetSpans()[0].Status.Code != StatusOK {
		t.Errorf("status = %v, want ok", m.GetSpans()[0].Status.Code)
	}
}

// TestMemoryRecordsMeasurements verifies the metric instruments record
// what they are given.
func TestMemoryRecordsMeasurements(t *testing.T) {
	m := NewMemoryContext()
	counter := m.Metrics().Counter("calls", InstrumentOptions{Description: "d", Unit: "u"})
	counter.Add(2, SpanAttributes{"a": "b"})
	histogram := m.Metrics().Histogram("dur", HistogramOptions{InstrumentOptions: InstrumentOptions{Unit: "ms"}})
	histogram.Record(12.5, nil, nil)

	meas := m.GetMeasurements()
	if len(meas) != 2 {
		t.Fatalf("measurements = %d, want 2", len(meas))
	}
	if meas[0].Kind != MetricCounter || meas[0].Value != 2 {
		t.Errorf("measurement = %+v", meas[0])
	}
	if meas[0].Attributes["a"] != "b" {
		t.Errorf("attributes = %v", meas[0].Attributes)
	}
	if meas[1].Kind != MetricHistogram || meas[1].Value != 12.5 {
		t.Errorf("measurement = %+v", meas[1])
	}
}

// TestMemoryPropagationRoundTrip verifies a carrier survives the trip
// and comes back as a parent.
func TestMemoryPropagationRoundTrip(t *testing.T) {
	caller := NewMemoryContext()
	provider := NewMemoryContext()

	var serverSpan Span
	_ = caller.StartSpan(SpanOptions{Name: "rpc.client"}, func(client Span) error {
		carrier := caller.Propagation().Inject(client)
		if carrier == nil {
			t.Fatal("the carrier is nil")
		}
		parent := provider.Propagation().Extract(carrier)
		return parent.StartSpan(SpanOptions{Name: "rpc.server"}, func(s Span) error {
			serverSpan = s
			return nil
		})
	})
	if serverSpan == nil {
		t.Fatal("the server span was not started")
	}
	// The extracted parent's trace is the caller's.
	spans := provider.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("provider spans = %d, want 1", len(spans))
	}
	if spans[0].ParentContext == nil {
		t.Error("the server span has no parent context")
	}
}

// TestMemoryUnknownCarrierFallsBackToRoot verifies an unrecognized
// carrier yields the adapter's root.
func TestMemoryUnknownCarrierFallsBackToRoot(t *testing.T) {
	m := NewMemoryContext()
	parent := m.Propagation().Extract(Carrier{"unknown": "x"})
	_ = parent.StartSpan(SpanOptions{Name: "x"}, func(span Span) error {
		return nil
	})
	spans := m.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].ParentID != 0 {
		t.Errorf("parent id = %d, want 0 (the root)", spans[0].ParentID)
	}
}

// TestMemoryLinks verifies a link records the target's carrier.
func TestMemoryLinks(t *testing.T) {
	m := NewMemoryContext()
	var target Span
	_ = m.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	})
	_ = m.StartSpan(SpanOptions{Name: "source"}, func(span Span) error {
		span.AddLink(SpanLink{Context: target, Attributes: SpanAttributes{"reason": "trigger"}})
		return nil
	})
	spans := m.GetSpans()
	var source RecordedSpan
	for _, s := range spans {
		if s.Name == "source" {
			source = s
		}
	}
	if len(source.Links) != 1 {
		t.Fatalf("links = %v, want one", source.Links)
	}
	if source.Links[0].Attributes["reason"] != "trigger" {
		t.Errorf("link attributes = %v", source.Links[0].Attributes)
	}
}

// TestStoreFindsTraces verifies the store's query.
func TestStoreFindsTraces(t *testing.T) {
	m := NewMemoryContext()
	_ = m.StartSpan(SpanOptions{Name: "one", Attributes: SpanAttributes{"k": "v"}}, func(span Span) error {
		return nil
	})
	_ = m.StartSpan(SpanOptions{Name: "two"}, func(span Span) error {
		return errors.New("boom")
	})

	page, err := m.FindTraces(context.Background(), TraceQuery{})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 2 {
		t.Fatalf("traces = %d, want 2", len(page.Traces))
	}

	// The name filter.
	page, err = m.FindTraces(context.Background(), TraceQuery{Name: "one"})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 1 || page.Traces[0].Spans[0].Name != "one" {
		t.Errorf("traces = %v", page.Traces)
	}

	// The error filter.
	page, err = m.FindTraces(context.Background(), TraceQuery{Errors: true})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 1 {
		t.Errorf("traces = %d, want 1", len(page.Traces))
	}

	// The attribute filter.
	page, err = m.FindTraces(context.Background(), TraceQuery{Attributes: SpanAttributes{"k": "v"}})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 1 {
		t.Errorf("traces = %d, want 1", len(page.Traces))
	}
}

// TestStorePaginates verifies the cursor.
func TestStorePaginates(t *testing.T) {
	m := NewMemoryContext()
	for i := 0; i < 5; i++ {
		_ = m.StartSpan(SpanOptions{Name: fmt.Sprintf("span%d", i)}, func(span Span) error {
			return nil
		})
	}
	page, err := m.FindTraces(context.Background(), TraceQuery{Limit: 2})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 2 {
		t.Fatalf("traces = %d, want 2", len(page.Traces))
	}
	if page.Cursor == "" {
		t.Fatal("the page has no cursor")
	}
	page2, err := m.FindTraces(context.Background(), TraceQuery{Limit: 2, Cursor: page.Cursor})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page2.Traces) != 2 {
		t.Errorf("traces = %d, want 2", len(page2.Traces))
	}
	if page2.Traces[0].TraceID == page.Traces[0].TraceID {
		t.Error("the second page repeats the first")
	}
}

// TestStoreReturnsOnlySettledSpans verifies an open span is not
// returned.
func TestStoreReturnsOnlySettledSpans(t *testing.T) {
	m := NewMemoryContext()
	span, release := Deferred(m, SpanOptions{Name: "open"})
	_ = span
	page, err := m.FindTraces(context.Background(), TraceQuery{})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 0 {
		t.Errorf("traces = %d, want none (the span is open)", len(page.Traces))
	}
	release()
	page, err = m.FindTraces(context.Background(), TraceQuery{})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 1 {
		t.Errorf("traces = %d, want 1", len(page.Traces))
	}
}

// TestStoreFindsMetrics verifies the metric rows.
func TestStoreFindsMetrics(t *testing.T) {
	m := NewMemoryContext()
	counter := m.Metrics().Counter("calls", InstrumentOptions{})
	counter.Add(2, nil)
	counter.Add(3, nil)

	rows, err := m.FindMetrics(context.Background(), MetricQuery{Name: "calls"})
	if err != nil {
		t.Fatalf("FindMetrics: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Count != 2 || rows[0].Sum != 5 {
		t.Errorf("row = %+v", rows[0])
	}
	if rows[0].Min != 2 || rows[0].Max != 3 {
		t.Errorf("row = %+v", rows[0])
	}
}

// TestStoreRootHasNoParentID verifies a root span's parent id is
// empty, the way the contract says.
//
// An exporter that reads the parent id as a string would otherwise
// write a bogus parent for the root.
func TestStoreRootHasNoParentID(t *testing.T) {
	m := NewMemoryContext()
	_ = m.StartSpan(SpanOptions{Name: "root"}, func(span Span) error {
		return span.StartSpan(SpanOptions{Name: "child"}, func(child Span) error {
			return nil
		})
	})
	page, err := m.FindTraces(context.Background(), TraceQuery{})
	if err != nil {
		t.Fatalf("FindTraces: %v", err)
	}
	if len(page.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(page.Traces))
	}
	spans := page.Traces[0].Spans
	for _, s := range spans {
		if s.Name == "root" && s.ParentID != "" {
			t.Errorf("the root's parent id = %q, want empty", s.ParentID)
		}
		if s.Name == "child" && s.ParentID == "" {
			t.Error("the child's parent id is empty")
		}
	}
}

// TestDeferredHoldsTheSpan verifies a deferred span stays open until it
// is released.
func TestDeferredHoldsTheSpan(t *testing.T) {
	m := NewMemoryContext()
	span, release := Deferred(m, SpanOptions{Name: "held"})
	span.SetAttributes(SpanAttributes{"a": 1})

	spans := m.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Settled {
		t.Error("the span settled before the release")
	}

	release()
	spans = m.GetSpans()
	if !spans[0].Settled {
		t.Error("the span did not settle after the release")
	}
	if spans[0].Attributes["a"] != 1 {
		t.Errorf("attributes = %v", spans[0].Attributes)
	}
}

// TestDeferredReleaseIsIdempotent verifies a second release is harmless.
func TestDeferredReleaseIsIdempotent(t *testing.T) {
	m := NewMemoryContext()
	_, release := Deferred(m, SpanOptions{Name: "held"})
	release()
	release()
}

// TestTypedMetricsPassesTheSchemaMetadata verifies the schema's
// description and unit reach the adapter.
func TestTypedMetricsPassesTheSchemaMetadata(t *testing.T) {
	m := NewMemoryContext()
	metrics := NewTypedMetrics(m.Metrics(), Schema)
	metrics.Histogram("pi.harness.request.duration").Record(1, nil, nil)

	meas := m.GetMeasurements()
	if len(meas) != 1 {
		t.Fatalf("measurements = %d, want 1", len(meas))
	}
	if meas[0].Unit != "ms" {
		t.Errorf("unit = %q, want ms", meas[0].Unit)
	}
	if meas[0].Description == "" {
		t.Error("the measurement has no description")
	}
	if len(meas[0].Boundaries) == 0 {
		t.Error("the histogram has no boundaries")
	}
}
