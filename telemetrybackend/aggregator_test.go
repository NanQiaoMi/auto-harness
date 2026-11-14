package telemetrybackend

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/telemetry"
)

// recordingBackend captures what the exporter sends.
type recordingBackend struct {
	mu sync.Mutex
	// spans are the spans shipped.
	spans []telemetry.StoredSpan
	// points are the metric points shipped.
	points []MetricPoint
	// flushes counts the flushes.
	flushes int
	// closed marks the backend as closed.
	closed bool
	// err is returned by every send.
	err error
}

func (b *recordingBackend) SendTraces(ctx context.Context, spans []telemetry.StoredSpan) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.spans = append(b.spans, spans...)
	return nil
}

func (b *recordingBackend) SendMetrics(ctx context.Context, points []MetricPoint) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.points = append(b.points, points...)
	return nil
}

func (b *recordingBackend) Flush(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushes++
	return nil
}

func (b *recordingBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// sentSpans returns the spans shipped.
func (b *recordingBackend) sentSpans() []telemetry.StoredSpan {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]telemetry.StoredSpan(nil), b.spans...)
}

// sentPoints returns the metric points shipped.
func (b *recordingBackend) sentPoints() []MetricPoint {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]MetricPoint(nil), b.points...)
}

// TestAggregatorSumsTheCounters verifies the counters aggregate.
func TestAggregatorSumsTheCounters(t *testing.T) {
	agg := NewAggregator(telemetry.Schema, 30*time.Second)
	agg.Record("pi.harness.tokens", 10, map[string]string{"pi.harness.kind": "input"})
	agg.Record("pi.harness.tokens", 20, map[string]string{"pi.harness.kind": "input"})

	points := agg.Points()
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	point := points[0]
	if point.Value != 30 {
		t.Errorf("value = %v, want 30", point.Value)
	}
	if point.Count != 2 {
		t.Errorf("count = %v, want 2", point.Count)
	}
	if point.Unit != "token" {
		t.Errorf("unit = %q, want token", point.Unit)
	}
	if point.Kind != telemetry.MetricCounter {
		t.Errorf("kind = %v", point.Kind)
	}
}

// TestAggregatorSeparatesTheSeries verifies two attribute sets are two
// series.
func TestAggregatorSeparatesTheSeries(t *testing.T) {
	agg := NewAggregator(telemetry.Schema, 30*time.Second)
	agg.Record("pi.harness.tokens", 10, map[string]string{"pi.harness.kind": "input"})
	agg.Record("pi.harness.tokens", 5, map[string]string{"pi.harness.kind": "output"})

	points := agg.Points()
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2", len(points))
	}
}

// TestAggregatorBucketsTheHistogram verifies the histogram's buckets.
func TestAggregatorBucketsTheHistogram(t *testing.T) {
	agg := NewAggregator(telemetry.Schema, 30*time.Second)
	for _, value := range []float64{50, 150, 600, 5000} {
		agg.Record("pi.harness.request.duration", value, map[string]string{
			"pi.harness.provider": "openai",
		})
	}

	points := agg.Points()
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	point := points[0]
	if point.Kind != telemetry.MetricHistogram {
		t.Errorf("kind = %v, want a histogram", point.Kind)
	}
	if point.Count != 4 {
		t.Errorf("count = %v, want 4", point.Count)
	}
	if point.Sum != 5800 {
		t.Errorf("sum = %v, want 5800", point.Sum)
	}
	if point.Min != 50 || point.Max != 5000 {
		t.Errorf("min/max = %v/%v, want 50/5000", point.Min, point.Max)
	}
	// The schema's boundaries are 100, 500, 1000, 5000, 15000, 60000.
	// 50 lands in the first bucket, 150 and 600 in the second and
	// third, and 5000 in the fourth.
	if len(point.BucketCounts) != len(point.Bounds)+1 {
		t.Fatalf("buckets = %v, bounds = %v", point.BucketCounts, point.Bounds)
	}
	want := []uint64{1, 1, 1, 1, 0, 0, 0}
	for i, count := range want {
		if point.BucketCounts[i] != count {
			t.Errorf("bucket %d = %d, want %d (all: %v)", i, point.BucketCounts[i], count, point.BucketCounts)
			break
		}
	}
}

// TestAggregatorResetsBetweenRounds verifies the points are the round's
// own, not a running total.
func TestAggregatorResetsBetweenRounds(t *testing.T) {
	agg := NewAggregator(telemetry.Schema, 30*time.Second)
	agg.Record("pi.harness.tokens", 10, nil)

	points := agg.Points()
	if len(points) != 1 || points[0].Value != 10 {
		t.Fatalf("the first round = %v", points)
	}
	if agg.Pending() != 0 {
		t.Errorf("the aggregator still holds %d series", agg.Pending())
	}

	// The second round reports only what happened in it.
	agg.Record("pi.harness.tokens", 5, nil)
	points = agg.Points()
	if len(points) != 1 {
		t.Fatalf("the second round = %v", points)
	}
	if points[0].Value != 5 {
		t.Errorf("value = %v, want 5 (the round's own)", points[0].Value)
	}
}

// TestAggregatorRendersTheAttributeValues verifies a metric's
// attributes are strings.
func TestAggregatorRendersTheAttributeValues(t *testing.T) {
	agg := NewAggregator(telemetry.Schema, 30*time.Second)
	agg.RecordMeasurement(telemetry.RecordedMeasurement{
		Name:  "pi.harness.tokens",
		Value: 7,
		Attributes: telemetry.SpanAttributes{
			"pi.harness.kind":  "input",
			"pi.harness.model": "deepseek-v4.1-flash",
			"pi.harness.count": 3,
			"pi.harness.flag":  true,
		},
	})
	points := agg.Points()
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	attrs := points[0].Attributes
	if attrs["pi.harness.kind"] != "input" {
		t.Errorf("kind = %q", attrs["pi.harness.kind"])
	}
	if attrs["pi.harness.count"] != "3" {
		t.Errorf("count = %q, want 3", attrs["pi.harness.count"])
	}
	if attrs["pi.harness.flag"] != "true" {
		t.Errorf("flag = %q, want true", attrs["pi.harness.flag"])
	}
}

// TestExporterBatchesTheSpans verifies the spans go out in batches,
// not one request each.
func TestExporterBatchesTheSpans(t *testing.T) {
	backend := &recordingBackend{}
	exporter := NewExporter(backend, telemetry.Schema, time.Hour, nil)
	exporter.SetBatchSize(4)
	exporter.Start()
	defer exporter.Close()

	sink := exporter.SpanSink()
	for i := 0; i < 10; i++ {
		sink(telemetry.StoredSpan{ID: "1", TraceID: "2", Name: "x"})
	}
	exporter.Flush(context.Background())

	spans := backend.sentSpans()
	if len(spans) != 10 {
		t.Errorf("sent %d spans, want 10", len(spans))
	}
}

// TestExporterShipsEverySpanExactlyOnce verifies the background
// drainer and an explicit flush do not ship the same span twice.
func TestExporterShipsEverySpanExactlyOnce(t *testing.T) {
	backend := &recordingBackend{}
	exporter := NewExporter(backend, telemetry.Schema, time.Hour, nil)
	exporter.SetBatchSize(4)
	exporter.Start()

	sink := exporter.SpanSink()
	for i := 0; i < 20; i++ {
		sink(telemetry.StoredSpan{ID: "1", TraceID: "2", Name: "x"})
	}
	// Flush while the background drainer is running.
	exporter.Flush(context.Background())
	exporter.Flush(context.Background())
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := backend.sentSpans()
	if len(spans) != 20 {
		t.Errorf("sent %d spans, want exactly 20", len(spans))
	}
}

// TestExporterShipsTheMetricsOnFlush verifies the metrics are
// aggregated and shipped.
func TestExporterShipsTheMetricsOnFlush(t *testing.T) {
	backend := &recordingBackend{}
	exporter := NewExporter(backend, telemetry.Schema, time.Hour, nil)
	defer exporter.Close()

	exporter.RecordMeasurement(telemetry.RecordedMeasurement{
		Name:       "pi.harness.tokens",
		Value:      10,
		Attributes: telemetry.SpanAttributes{"pi.harness.kind": "input"},
	})
	exporter.RecordMeasurement(telemetry.RecordedMeasurement{
		Name:       "pi.harness.tokens",
		Value:      20,
		Attributes: telemetry.SpanAttributes{"pi.harness.kind": "input"},
	})
	exporter.FlushMetrics(context.Background())

	points := backend.sentPoints()
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	if points[0].Value != 30 {
		t.Errorf("value = %v, want 30", points[0].Value)
	}
	if exporter.Flushed() != 1 {
		t.Errorf("flushed = %d, want 1", exporter.Flushed())
	}
}

// TestExporterSurvivesABackendFailure verifies a failed backend does
// not stop the exporter.
func TestExporterSurvivesABackendFailure(t *testing.T) {
	backend := &recordingBackend{err: context.DeadlineExceeded}
	exporter := NewExporter(backend, telemetry.Schema, time.Hour, func(string, ...any) {})

	sink := exporter.SpanSink()
	sink(telemetry.StoredSpan{ID: "1", TraceID: "2", Name: "x"})
	exporter.RecordMeasurement(telemetry.RecordedMeasurement{Name: "pi.harness.tokens", Value: 1})
	exporter.Flush(context.Background())
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Nothing was shipped, but nothing panicked either.
	if len(backend.sentSpans()) != 0 {
		t.Error("a failing backend received spans")
	}
}

// TestExporterStopsAfterClose verifies a closed exporter takes no more
// spans.
func TestExporterStopsAfterClose(t *testing.T) {
	backend := &recordingBackend{}
	exporter := NewExporter(backend, telemetry.Schema, time.Hour, nil)
	exporter.Start()

	sink := exporter.SpanSink()
	sink(telemetry.StoredSpan{ID: "1", TraceID: "2", Name: "x"})
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sink(telemetry.StoredSpan{ID: "3", TraceID: "2", Name: "y"})
	exporter.Flush(context.Background())

	for _, span := range backend.sentSpans() {
		if span.ID == "3" {
			t.Error("a closed exporter still shipped a span")
		}
	}
}
