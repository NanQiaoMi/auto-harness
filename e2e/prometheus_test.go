//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimi/auto-harness/agent"
	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/telemetry"
	"github.com/mimi/auto-harness/telemetrybackend"
	"github.com/mimi/auto-harness/telemetrybackend/prompb"
	"github.com/mimi/auto-harness/telemetrybackend/snappy"
	"github.com/mimi/auto-harness/telemetryext"
	"github.com/mimi/auto-harness/tools"
)

// prometheusCollector is a stand-in for a Prometheus server's remote
// write endpoint: it records the compressed bodies it receives.
type prometheusCollector struct {
	mu sync.Mutex
	// bodies are the compressed bodies of the requests.
	bodies [][]byte
	// headers are the headers of the last request.
	headers http.Header
}

// ServeHTTP records one body.
func (c *prometheusCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.headers = r.Header.Clone()
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// request decodes the requests the collector received.
//
// The bodies are snappy-compressed protobuf messages, so they are
// decompressed and decoded the way a Prometheus server decodes them.
func (c *prometheusCollector) request(t *testing.T) *prompb.WriteRequest {
	t.Helper()
	c.mu.Lock()
	bodies := make([][]byte, len(c.bodies))
	copy(bodies, c.bodies)
	c.mu.Unlock()

	merged := &prompb.WriteRequest{}
	for _, body := range bodies {
		raw, err := snappy.Decode(body)
		if err != nil {
			t.Fatalf("the body is not a snappy block: %v", err)
		}
		decoded, err := prompb.Decode(raw)
		if err != nil {
			t.Fatalf("the body is not a write request: %v", err)
		}
		merged.TimeSeries = append(merged.TimeSeries, decoded.TimeSeries...)
		merged.Metadata = append(merged.Metadata, decoded.Metadata...)
	}
	return merged
}

// seriesNames returns the names of the received series, counted.
func (c *prometheusCollector) seriesNames(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, series := range c.request(t).TimeSeries {
		for _, label := range series.Labels {
			if label.Name == "__name__" {
				out[label.Value]++
			}
		}
	}
	return out
}

// TestLivePrometheusBackendShipsTheMetrics verifies the whole path
// against the real model: the Prometheus server receives the run's
// metrics as time series, with the names and labels a dashboard
// expects.
func TestLivePrometheusBackendShipsTheMetrics(t *testing.T) {
	collector := &prometheusCollector{}
	server := httptest.NewServer(collector)
	defer server.Close()

	backend, err := telemetrybackend.NewPrometheus(telemetrybackend.Options{
		Endpoint: server.URL + "/api/v1/write",
	})
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	exporter := telemetrybackend.NewExporter(backend, telemetry.Schema, time.Hour, nil)
	exporter.Start()

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	recorder := telemetry.NewMemoryContext()
	recorder.SetMeasurementSink(exporter.RecordMeasurement)

	telemetryCollector := telemetryext.New(recorder, "openai", testModel)
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())
	if err := telemetryCollector.Setup(api); err != nil {
		t.Fatalf("collector setup: %v", err)
	}

	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  6,
		MaxTokens: 1024,
	}, bus, reg)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart, Reason: "e2e"})

	const marker = "harness-prometheus-5d1c"
	if _, err := ag.Run(context.Background(),
		"Use the bash tool to run exactly: echo "+marker+". Then reply with the output you saw."); err != nil {
		t.Fatalf("Run: %v", err)
	}

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown, Reason: "e2e"})

	// Flush everything, then close.
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	names := collector.seriesNames(t)
	if len(names) == 0 {
		t.Fatal("the Prometheus server received no series")
	}

	// The request duration is a histogram, so it travels as the bucket,
	// count and sum series Prometheus expects.
	for _, want := range []string{
		"pi_harness_request_duration_bucket",
		"pi_harness_request_duration_count",
		"pi_harness_request_duration_sum",
	} {
		if names[want] == 0 {
			t.Errorf("no series named %q; got %v", want, names)
		}
	}

	// The token usage is a counter, so it carries the "_total" suffix.
	var sawCounter bool
	for name := range names {
		if strings.HasSuffix(name, "_total") {
			sawCounter = true
		}
	}
	if !sawCounter {
		t.Errorf("no counter series among %v", names)
	}

	// The headers are the ones a Prometheus server needs.
	collector.mu.Lock()
	headers := collector.headers.Clone()
	collector.mu.Unlock()
	if got := headers.Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want %q", got, "application/x-protobuf")
	}
	if got := headers.Get("Content-Encoding"); got != "snappy" {
		t.Errorf("Content-Encoding = %q, want %q", got, "snappy")
	}
}
