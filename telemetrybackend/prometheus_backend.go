package telemetrybackend

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/mimi/auto-harness/telemetry"
	"github.com/mimi/auto-harness/telemetrybackend/prompb"
)

// PrometheusBackend ships telemetry to a Prometheus server over
// remote write.
//
// The protocol is not a trace protocol: Prometheus stores metrics, not
// spans. A backend that has no place for a span says so rather than
// inventing one, so SendTraces is a no-op here. The metrics are the
// part that matters, and they travel as Prometheus's own time series:
// a name, a set of labels and a value.
//
// The body is the protobuf encoding of a write request, snappy
// compressed, which is what a Prometheus server's /api/v1/write
// endpoint expects.
//
// The harness ships no external dependency, so both the protobuf
// encoding and the compression are written here and checked against
// the reference implementations by their own tests.
type PrometheusBackend struct {
	// client is the HTTP client.
	client *http.Client
	// endpoint is where the write requests go.
	endpoint string
	// headers are sent with every request.
	headers map[string]string
	// redact drops the attributes the schema marks sensitive.
	redact bool

	mu sync.Mutex
	// sentMetrics counts the metric points shipped.
	sentMetrics int
	// droppedMetrics counts the metric points a failed request lost.
	droppedMetrics int
	// droppedTraces counts the spans a backend with no place for them
	// let go.
	droppedTraces int
}

// NewPrometheus creates a backend for a Prometheus remote write
// endpoint.
//
// The endpoint is the server's write URL, conventionally
// http://localhost:9090/api/v1/write.
func NewPrometheus(opts Options) (*PrometheusBackend, error) {
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, fmt.Errorf("telemetrybackend: no endpoint")
	}
	endpoint, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("telemetrybackend: endpoint %q: %w", opts.Endpoint, err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("telemetrybackend: endpoint %q: want an http or https URL", opts.Endpoint)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	return &PrometheusBackend{
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		endpoint: endpoint.String(),
		headers:  opts.Headers,
		redact:   opts.Redact,
	}, nil
}

// SendTraces ships a batch of spans.
//
// Prometheus has no traces: a counter is not a trace, and storing one
// as a metric would invent data the contract never recorded. So the
// spans are dropped and the drop is counted.
func (b *PrometheusBackend) SendTraces(ctx context.Context, spans []telemetry.StoredSpan) error {
	if len(spans) == 0 {
		return nil
	}
	b.mu.Lock()
	b.droppedTraces += len(spans)
	b.mu.Unlock()
	return nil
}

// SendMetrics ships a batch of metric points.
//
// The points are redacted here, when they are about to leave the
// process: the backend is the last place that can still keep a
// sensitive attribute from being sent.
func (b *PrometheusBackend) SendMetrics(ctx context.Context, points []MetricPoint) error {
	if len(points) == 0 {
		return nil
	}
	if b.redact {
		points = redactMetricPoints(points)
	}
	request := encodePrometheus(points)
	if len(request.TimeSeries) == 0 {
		return nil
	}
	payload, err := compressPrometheus(request)
	if err != nil {
		return err
	}
	if err := b.post(ctx, payload); err != nil {
		b.mu.Lock()
		b.droppedMetrics += len(points)
		b.mu.Unlock()
		return err
	}
	b.mu.Lock()
	b.sentMetrics += len(points)
	b.mu.Unlock()
	return nil
}

// post sends one write request.
//
// A failed request is retried once: a server that was briefly busy
// should not cost a batch. A second failure drops the batch and
// returns the error.
func (b *PrometheusBackend) post(ctx context.Context, payload []byte) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("telemetrybackend: build request: %w", err)
		}
		// The two headers a Prometheus server needs to know how to
		// read the body.
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "snappy")
		for key, value := range b.headers {
			req.Header.Set(key, value)
		}
		resp, err := b.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("telemetrybackend: send to %s: %w", b.endpoint, err)
			continue
		}
		// The body is drained so the connection can be reused.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("telemetrybackend: %s returned %s: %s", b.endpoint, resp.Status, strings.TrimSpace(string(body)))
		// A 4xx is the payload's fault; retrying it would fail again.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			break
		}
	}
	return lastErr
}

// Flush ships whatever is pending.
//
// The backend holds nothing: batches are sent as they arrive, so a
// flush only waits for the in-flight requests.
func (b *PrometheusBackend) Flush(ctx context.Context) error {
	return nil
}

// Close releases the backend's resources.
func (b *PrometheusBackend) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// Stats returns the backend's counters.
func (b *PrometheusBackend) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{
		SentMetrics:    b.sentMetrics,
		DroppedMetrics: b.droppedMetrics,
		DroppedTraces:  b.droppedTraces,
	}
}

// Endpoint returns the endpoint the metrics go to.
func (b *PrometheusBackend) Endpoint() string { return b.endpoint }

// compile-time check that the backend satisfies the interface.
var _ Backend = (*PrometheusBackend)(nil)

// prometheusMetadata builds the metadata of the metric families in a
// request.
//
// The metadata carries each family's type, name, help text and unit.
//
// It is part of the remote write protocol, so filling it in is correct
// whatever the receiver does with it: Prometheus's own v1 receiver
// ignores the field today, but its v2 path reads it, and other remote
// write receivers use it to show a metric's help text. A family the
// receiver does not know is still better described than not described
// at all.
func prometheusMetadata(points []MetricPoint) []prompb.MetricMetadata {
	seen := map[string]bool{}
	out := []prompb.MetricMetadata{}
	for _, point := range points {
		name := PrometheusMetricName(point.Name)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, prompb.MetricMetadata{
			Type:             prometheusMetricType(point.Kind),
			MetricFamilyName: name,
			Help:             point.Description,
			Unit:             point.Unit,
		})
	}
	return out
}

// prometheusMetricType maps an instrument kind onto Prometheus's own
// types.
//
// A histogram stays a histogram even though it travels as a bucket
// series: the collector assembles the buckets back into a
// distribution.
func prometheusMetricType(kind telemetry.MetricKind) prompb.MetricType {
	switch kind {
	case telemetry.MetricCounter:
		return prompb.MetricTypeCounter
	case telemetry.MetricUpDownCounter:
		return prompb.MetricTypeGauge
	case telemetry.MetricHistogram:
		return prompb.MetricTypeHistogram
	default:
		return prompb.MetricTypeUnknown
	}
}
