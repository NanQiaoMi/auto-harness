// Package telemetrybackend sends telemetry to a real collector.
//
// The telemetry contract in the telemetry package says what to record;
// this package says where the records go. The split matters: the
// contract stays vendor-neutral, and a backend is a translation of the
// contract's spans and metrics into one protocol's wire format.
//
// The protocol is OTLP over HTTP with a JSON payload, which every
// OpenTelemetry collector accepts and which needs no protobuf
// dependency.
//
// The OTLP JSON encoding has two rules that a hand-written encoder must
// respect, and both differ from what a generic protobuf JSON encoder
// would produce:
//
//   - 64-bit integers are decimal strings, not numbers. A collector
//     accepts either, but a string is what the spec mandates.
//   - trace and span identifiers are hex strings, not base64. This is
//     the one documented deviation from the protobuf JSON mapping.
//
// Both are handled in the encoder, and both are covered by tests.
package telemetrybackend

import (
	"context"
	"time"

	"github.com/NanQiaoMi/auto-harness/telemetry"
)

// Backend ships telemetry somewhere.
//
// A backend receives batches and sends them. It is called from the
// collector's goroutine, so it must be safe for concurrent use and
// should not block for long: a slow collector must not slow the agent.
type Backend interface {
	// SendTraces ships a batch of spans.
	SendTraces(ctx context.Context, spans []telemetry.StoredSpan) error
	// SendMetrics ships a batch of metric points.
	SendMetrics(ctx context.Context, points []MetricPoint) error
	// Flush ships whatever is pending and waits for it to land.
	Flush(ctx context.Context) error
	// Close releases the backend's resources. It flushes first.
	Close() error
}

// Resource describes the entity producing the telemetry.
//
// It becomes the OTLP resource, which every collector keys its data by.
// The attributes are the standard OpenTelemetry service attributes
// (service.name, service.version, …) plus whatever the caller adds.
type Resource struct {
	// Name identifies the service, e.g. "auto-harness".
	Name string
	// Version is the service version.
	Version string
	// Attributes are extra resource attributes.
	Attributes map[string]string
}

// Scope identifies the instrumentation that produced the telemetry.
//
// It becomes the OTLP instrumentation scope: the name of the package
// and its version.
type Scope struct {
	// Name is the instrumentation scope name, e.g. "github.com/NanQiaoMi/auto-harness".
	Name string
	// Version is the instrumentation version.
	Version string
}

// MetricPoint is one aggregated metric data point.
//
// A backend aggregates measurements before shipping them: a collector
// wants a rate or a distribution per interval, not every raw
// measurement.
type MetricPoint struct {
	// Name is the metric's name.
	Name string
	// Description is the metric's description.
	Description string
	// Unit is the metric's unit.
	Unit string
	// Kind is the instrument kind: counter, upDownCounter or histogram.
	Kind telemetry.MetricKind
	// Value is the aggregated value of a counter or up-down counter.
	Value float64
	// Count is the number of measurements in the aggregate.
	Count int
	// Sum is the sum of the measurements.
	Sum float64
	// Min is the smallest measurement, when the point has one.
	Min float64
	// Max is the largest measurement, when the point has one.
	Max float64
	// Bounds are the histogram's bucket boundaries, strictly increasing.
	Bounds []float64
	// BucketCounts are the histogram's bucket counts: one more than the
	// number of bounds.
	BucketCounts []uint64
	// Attributes identify the series.
	Attributes map[string]string
	// StartTime is when the aggregate started.
	StartTime time.Time
	// Time is when the aggregate ended.
	Time time.Time
}

// Options configure a backend.
type Options struct {
	// Endpoint is the collector's OTLP/HTTP traces endpoint. The
	// metrics endpoint is derived from it by replacing "/v1/traces"
	// with "/v1/metrics"; an endpoint without that suffix is used as
	// the traces endpoint and the metrics endpoint is derived from it.
	Endpoint string
	// Headers are sent with every request.
	Headers map[string]string
	// Timeout bounds one request.
	Timeout time.Duration
	// BatchSize is how many spans one request carries. Zero means the
	// default.
	BatchSize int
	// Resource describes the producer.
	Resource Resource
	// Scope identifies the instrumentation.
	Scope Scope
	// Redact drops the attributes the schema marks sensitive.
	//
	// The backend does the redaction itself: it is the last place
	// before the data leaves the process, so it is the last place that
	// can still keep a sensitive attribute from being sent.
	Redact bool
}

// DefaultBatchSize is how many spans one request carries when the
// options do not say.
const DefaultBatchSize = 512

// DefaultTimeout bounds one request when the options do not say.
const DefaultTimeout = 10 * time.Second

// defaultResource is the resource of a harness run that did not
// configure one.
func defaultResource() Resource {
	return Resource{
		Name:    "auto-harness",
		Version: Version,
	}
}

// defaultScope is the instrumentation scope of the harness.
func defaultScope() Scope {
	return Scope{
		Name:    "github.com/NanQiaoMi/auto-harness",
		Version: Version,
	}
}

// Version is the harness's version, reported in the OTLP resource and
// scope.
var Version = "0.1.3"
