//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/telemetry"
	"github.com/NanQiaoMi/auto-harness/telemetrybackend"
	"github.com/NanQiaoMi/auto-harness/telemetryext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// otlpCollector is a stand-in for an OTLP collector: it records the
// payloads it receives.
type otlpCollector struct {
	mu sync.Mutex
	// traces are the trace payloads.
	traces []map[string]any
	// metrics are the metric payloads.
	metrics []map[string]any
}

// ServeHTTP records one payload.
func (c *otlpCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	if _, ok := payload["resourceSpans"]; ok {
		c.traces = append(c.traces, payload)
	} else if _, ok := payload["resourceMetrics"]; ok {
		c.metrics = append(c.metrics, payload)
	}
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// spans returns the spans the collector received.
func (c *otlpCollector) spans(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, payload := range c.traces {
		for _, rs := range payload["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, span := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, span.(map[string]any))
				}
			}
		}
	}
	return out
}

// metricNames returns the metric names the collector received.
func (c *otlpCollector) metricNames(t *testing.T) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, payload := range c.metrics {
		for _, rm := range payload["resourceMetrics"].([]any) {
			for _, sm := range rm.(map[string]any)["scopeMetrics"].([]any) {
				for _, metric := range sm.(map[string]any)["metrics"].([]any) {
					out = append(out, metric.(map[string]any)["name"].(string))
				}
			}
		}
	}
	return out
}

// TestLiveOTLPBackendShipsTheRun verifies the whole path against the
// real model: the collector receives the spans, with the right
// identifiers and no content in the attributes.
func TestLiveOTLPBackendShipsTheRun(t *testing.T) {
	collector := &otlpCollector{}
	server := httptest.NewServer(collector)
	defer server.Close()

	backend, err := telemetrybackend.NewOTLP(telemetrybackend.Options{
		Endpoint: server.URL + "/v1/traces",
		Resource: telemetrybackend.Resource{Name: "auto-harness", Version: "test"},
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
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
	recorder.SetSink(exporter.SpanSink())
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

	const secret = "{{SECRET_4mz2npbq}}"
	const marker = "harness-otlp-8f2e"
	if _, err := ag.Run(context.Background(),
		"Use the bash tool to run exactly: echo "+marker+". Then reply with the output you saw. "+
			"Also remember the code "+secret+"."); err != nil {
		t.Fatalf("Run: %v", err)
	}

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown, Reason: "e2e"})

	// Flush everything, then close.
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := collector.spans(t)
	if len(spans) == 0 {
		t.Fatal("the collector received no spans")
	}

	// The session, request and tool spans are all there.
	names := map[string]bool{}
	for _, s := range spans {
		names[s["name"].(string)] = true
	}
	for _, want := range []string{"pi.harness.session", "pi.harness.request", "pi.harness.tool"} {
		if !names[want] {
			t.Errorf("no span named %q; got %v", want, names)
		}
	}

	// The identifiers are the hex the OTLP spec wants.
	for _, s := range spans {
		traceID, _ := s["traceId"].(string)
		if len(traceID) != 32 {
			t.Errorf("traceId = %q, want 32 hex characters", traceID)
		}
		spanID, _ := s["spanId"].(string)
		if len(spanID) != 16 {
			t.Errorf("spanId = %q, want 16 hex characters", spanID)
		}
		// The times are decimal strings, not numbers.
		if _, ok := s["startTimeUnixNano"].(string); !ok {
			t.Errorf("startTimeUnixNano = %v (%T), want a decimal string",
				s["startTimeUnixNano"], s["startTimeUnixNano"])
		}
	}

	// The privacy policy holds against a real run: the secret and the
	// tool's output stay out of the attributes.
	for _, s := range spans {
		attrs, _ := s["attributes"].([]any)
		for _, attr := range attrs {
			kv := attr.(map[string]any)
			value := kv["value"].(map[string]any)
			text := strings.ToLower(valueText(value))
			for _, forbidden := range []string{secret, marker} {
				if strings.Contains(text, strings.ToLower(forbidden)) {
					t.Errorf("the span attribute %q leaks content: %q", kv["key"], text)
				}
			}
		}
	}

	// The metrics went out too.
	metrics := collector.metricNames(t)
	if len(metrics) == 0 {
		t.Error("the collector received no metrics")
	}
	var sawDuration bool
	for _, name := range metrics {
		if name == "pi.harness.request.duration" {
			sawDuration = true
		}
	}
	if !sawDuration {
		t.Errorf("the metrics are %v, want the request duration among them", metrics)
	}
}

// valueText renders an OTLP attribute value as text.
func valueText(value map[string]any) string {
	if s, ok := value["stringValue"].(string); ok {
		return s
	}
	if v, ok := value["intValue"].(float64); ok {
		return string(rune(int(v)))
	}
	return ""
}
