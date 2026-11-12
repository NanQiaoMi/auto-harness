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
)

// OTLPBackend ships telemetry to an OpenTelemetry collector over HTTP.
//
// It batches: the spans of a batch go in one request, and a request
// that fails is retried once. A collector that is down must not slow
// the agent down, so a failed batch is dropped after its retry and
// counted.
type OTLPBackend struct {
	// client is the HTTP client.
	client *http.Client
	// tracesEndpoint is where the spans go.
	tracesEndpoint string
	// metricsEndpoint is where the metrics go.
	metricsEndpoint string
	// headers are sent with every request.
	headers map[string]string
	// batchSize is how many spans one request carries.
	batchSize int
	// resource describes the producer.
	resource Resource
	// scope identifies the instrumentation.
	scope Scope
	// redact drops the attributes the schema marks sensitive.
	redact bool

	mu sync.Mutex
	// droppedTraces counts the spans a failed request lost.
	droppedTraces int
	// droppedMetrics counts the metric points a failed request lost.
	droppedMetrics int
	// sentTraces counts the spans shipped.
	sentTraces int
	// sentMetrics counts the metric points shipped.
	sentMetrics int
}

// NewOTLP creates a backend for an OTLP/HTTP collector.
//
// The endpoint is the collector's traces endpoint, conventionally
// http://localhost:4318/v1/traces. The metrics endpoint is derived
// from it. An endpoint without the "/v1/traces" suffix is used as the
// traces endpoint, and the metrics endpoint is its sibling.
func NewOTLP(opts Options) (*OTLPBackend, error) {
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
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	resource := opts.Resource
	if resource.Name == "" {
		resource.Name = defaultResource().Name
	}
	scope := opts.Scope
	if scope.Name == "" {
		scope = defaultScope()
	}

	return &OTLPBackend{
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		tracesEndpoint:  endpoint.String(),
		metricsEndpoint: metricsEndpoint(endpoint),
		headers:         opts.Headers,
		batchSize:       batchSize,
		resource:        resource,
		scope:           scope,
		redact:          opts.Redact,
	}, nil
}

// metricsEndpoint derives the metrics endpoint from the traces one.
func metricsEndpoint(traces *url.URL) string {
	metrics := *traces
	path := metrics.Path
	switch {
	case strings.HasSuffix(path, "/v1/traces"):
		metrics.Path = strings.TrimSuffix(path, "/v1/traces") + "/v1/metrics"
	case strings.HasSuffix(path, "/v1/metrics"):
		metrics.Path = strings.TrimSuffix(path, "/v1/metrics") + "/v1/traces"
	case path == "" || path == "/":
		metrics.Path = "/v1/metrics"
	default:
		metrics.Path = strings.TrimSuffix(path, "/") + "/v1/metrics"
	}
	return metrics.String()
}

// SendTraces ships a batch of spans.
//
// The spans are redacted here, when they are about to leave the
// process: the backend is the last place that can still keep a
// sensitive attribute from being sent.
func (b *OTLPBackend) SendTraces(ctx context.Context, spans []telemetry.StoredSpan) error {
	if len(spans) == 0 {
		return nil
	}
	for start := 0; start < len(spans); start += b.batchSize {
		end := start + b.batchSize
		if end > len(spans) {
			end = len(spans)
		}
		batch := spans[start:end]
		if b.redact {
			batch = redactSpans(batch)
		}
		payload, err := encodeTraces(batch, b.resource, b.scope)
		if err != nil {
			return err
		}
		if err := b.post(ctx, b.tracesEndpoint, payload); err != nil {
			b.mu.Lock()
			b.droppedTraces += len(batch)
			b.mu.Unlock()
			return err
		}
		b.mu.Lock()
		b.sentTraces += len(batch)
		b.mu.Unlock()
	}
	return nil
}

// SendMetrics ships a batch of metric points.
//
// The points are redacted here, when they are about to leave the
// process: the backend is the last place that can still keep a
// sensitive attribute from being sent.
func (b *OTLPBackend) SendMetrics(ctx context.Context, points []MetricPoint) error {
	if len(points) == 0 {
		return nil
	}
	if b.redact {
		points = redactMetricPoints(points)
	}
	payload, err := encodeMetrics(points, b.resource, b.scope)
	if err != nil {
		return err
	}
	if err := b.post(ctx, b.metricsEndpoint, payload); err != nil {
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

// post sends one payload.
//
// A failed request is retried once: a collector that was briefly busy
// should not cost a batch. A second failure drops the batch and
// returns the error.
func (b *OTLPBackend) post(ctx context.Context, endpoint string, payload []byte) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("telemetrybackend: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for key, value := range b.headers {
			req.Header.Set(key, value)
		}
		resp, err := b.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("telemetrybackend: send to %s: %w", endpoint, err)
			continue
		}
		// The body is drained so the connection can be reused.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("telemetrybackend: %s returned %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
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
func (b *OTLPBackend) Flush(ctx context.Context) error {
	return nil
}

// Close releases the backend's resources.
func (b *OTLPBackend) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// Stats reports what the backend shipped and what it lost.
type Stats struct {
	// SentTraces is the number of spans shipped.
	SentTraces int
	// SentMetrics is the number of metric points shipped.
	SentMetrics int
	// DroppedTraces is the number of spans a failed request lost.
	DroppedTraces int
	// DroppedMetrics is the number of metric points a failed request
	// lost.
	DroppedMetrics int
}

// Stats returns the backend's counters.
func (b *OTLPBackend) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{
		SentTraces:     b.sentTraces,
		SentMetrics:    b.sentMetrics,
		DroppedTraces:  b.droppedTraces,
		DroppedMetrics: b.droppedMetrics,
	}
}

// TracesEndpoint returns the endpoint the spans go to.
func (b *OTLPBackend) TracesEndpoint() string { return b.tracesEndpoint }

// MetricsEndpoint returns the endpoint the metrics go to.
func (b *OTLPBackend) MetricsEndpoint() string { return b.metricsEndpoint }

// compile-time check that the backend satisfies the interface.
var _ Backend = (*OTLPBackend)(nil)
