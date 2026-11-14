package telemetrybackend

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

	"github.com/NanQiaoMi/auto-harness/telemetry"
)

// fakeCollector is a stand-in for an OTLP collector: it records the
// requests it receives.
type fakeCollector struct {
	mu sync.Mutex
	// traces are the trace payloads, as raw JSON.
	traces [][]byte
	// metrics are the metric payloads, as raw JSON.
	metrics [][]byte
	// headers are the headers of the last request.
	headers http.Header
	// status is the status code to return. Zero means 200.
	status int
}

// handler serves the collector's endpoints.
func (f *fakeCollector) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.headers = r.Header.Clone()
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/metrics"):
		f.metrics = append(f.metrics, body)
	case strings.HasSuffix(r.URL.Path, "/v1/traces"):
		f.traces = append(f.traces, body)
	}
	status := f.status
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

// tracePayloads returns the decoded trace payloads.
func (f *fakeCollector) tracePayloads(t *testing.T) []map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.traces))
	for _, body := range f.traces {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("the trace payload is not JSON: %v\n%s", err, body)
		}
		out = append(out, payload)
	}
	return out
}

// metricPayloads returns the decoded metric payloads.
func (f *fakeCollector) metricPayloads(t *testing.T) []map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.metrics))
	for _, body := range f.metrics {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("the metric payload is not JSON: %v\n%s", err, body)
		}
		out = append(out, payload)
	}
	return out
}

// TestOTLPBackendSendsSpans verifies the spans reach the collector's
// traces endpoint.
func TestOTLPBackendSendsSpans(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	span := telemetry.StoredSpan{
		ID: "1", TraceID: "2", Name: "pi.harness.request",
		StartedMs: 1000, EndedMs: 2000,
		Attributes: telemetry.SpanAttributes{"pi.harness.model": "deepseek-v4.1-flash"},
	}
	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{span}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	payloads := collector.tracePayloads(t)
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(payloads))
	}
	payload := payloads[0]
	resourceSpans, _ := payload["resourceSpans"].([]any)
	if len(resourceSpans) != 1 {
		t.Fatalf("resourceSpans = %v", payload["resourceSpans"])
	}
	rs, _ := resourceSpans[0].(map[string]any)
	scopeSpans, _ := rs["scopeSpans"].([]any)
	if len(scopeSpans) != 1 {
		t.Fatalf("scopeSpans = %v", rs["scopeSpans"])
	}
	ss, _ := scopeSpans[0].(map[string]any)
	spans, _ := ss["spans"].([]any)
	if len(spans) != 1 {
		t.Fatalf("spans = %v", ss["spans"])
	}
	encoded, _ := spans[0].(map[string]any)
	if encoded["name"] != "pi.harness.request" {
		t.Errorf("name = %v", encoded["name"])
	}
	// The 64-bit times are decimal strings, not numbers.
	if encoded["startTimeUnixNano"] != "1000000000" {
		t.Errorf("startTimeUnixNano = %v (%T), want the decimal string", encoded["startTimeUnixNano"], encoded["startTimeUnixNano"])
	}
	if encoded["endTimeUnixNano"] != "2000000000" {
		t.Errorf("endTimeUnixNano = %v", encoded["endTimeUnixNano"])
	}
}

// TestOTLPBackendUsesHexIdentifiers verifies the one documented
// deviation from the protobuf JSON mapping: the identifiers are hex,
// not base64.
func TestOTLPBackendUsesHexIdentifiers(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	// An identifier that is already the right hex stays as it is.
	hexTrace := "5b8efff798038103d269b633813fc60c"
	hexSpan := "8c3e7f1a2b4d5e6f"
	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: hexSpan, TraceID: hexTrace, Name: "x",
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	payload := collector.tracePayloads(t)[0]
	encoded := firstSpan(t, payload)
	if encoded["traceId"] != hexTrace {
		t.Errorf("traceId = %v, want the hex string", encoded["traceId"])
	}
	if encoded["spanId"] != hexSpan {
		t.Errorf("spanId = %v, want the hex string", encoded["spanId"])
	}
}

// TestOTLPBackendHashesOpaqueIdentifiers verifies an identifier that
// is not hex becomes a valid hex one of the right length.
//
// The contract's identifiers are opaque strings; a collector rejects
// a trace id that is not 16 bytes.
func TestOTLPBackendHashesOpaqueIdentifiers(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: "7", TraceID: "96a8555455163d4c:1", Name: "x",
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	encoded := firstSpan(t, collector.tracePayloads(t)[0])
	traceID, _ := encoded["traceId"].(string)
	if len(traceID) != 32 {
		t.Errorf("traceId = %q, want 32 hex characters", traceID)
	}
	spanID, _ := encoded["spanId"].(string)
	if len(spanID) != 16 {
		t.Errorf("spanId = %q, want 16 hex characters", spanID)
	}
	for _, c := range traceID + spanID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("the identifier %q is not hex", traceID+spanID)
			break
		}
	}
}

// TestOTLPBackendHashesStably verifies the same identifier always
// becomes the same hex, so a trace keeps its shape.
func TestOTLPBackendHashesStably(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	for i := 0; i < 2; i++ {
		if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
			ID: "7", TraceID: "96a8555455163d4c:1", Name: "x",
		}}); err != nil {
			t.Fatalf("SendTraces: %v", err)
		}
	}
	payloads := collector.tracePayloads(t)
	first := firstSpan(t, payloads[0])
	second := firstSpan(t, payloads[1])
	if first["traceId"] != second["traceId"] {
		t.Errorf("the trace id changed between sends: %v vs %v", first["traceId"], second["traceId"])
	}
	if first["spanId"] != second["spanId"] {
		t.Errorf("the span id changed between sends: %v vs %v", first["spanId"], second["spanId"])
	}
}

// TestOTLPBackendRedactsTheSensitiveAttributes verifies the backend
// honours the schema's sensitive marking.
//
// The harness's schema marks nothing sensitive today, so the test
// adds a marking of its own and checks the backend drops it: the
// policy has to work when the schema grows.
func TestOTLPBackendRedactsTheSensitiveAttributes(t *testing.T) {
	// Mark an attribute of the request span as sensitive.
	definition := telemetry.Schema.Spans["pi.harness.request"]
	attr := definition.StartAttributes["pi.harness.model"]
	attr.Sensitive = true
	definition.StartAttributes["pi.harness.model"] = attr
	telemetry.Schema.Spans["pi.harness.request"] = definition
	defer func() {
		attr.Sensitive = false
		definition.StartAttributes["pi.harness.model"] = attr
		telemetry.Schema.Spans["pi.harness.request"] = definition
	}()

	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces", Redact: true})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: "1", TraceID: "2", Name: "pi.harness.request",
		Attributes: telemetry.SpanAttributes{
			"pi.harness.model": "deepseek-v4.1-flash",
			"pi.harness.tools": 5,
		},
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	span := firstSpan(t, collector.tracePayloads(t)[0])
	attrs, _ := span["attributes"].([]any)
	for _, attr := range attrs {
		kv := attr.(map[string]any)
		if kv["key"] == "pi.harness.model" {
			t.Error("the sensitive attribute was sent")
		}
	}
	// The other attributes survive.
	var sawTools bool
	for _, attr := range attrs {
		kv := attr.(map[string]any)
		if kv["key"] == "pi.harness.tools" {
			sawTools = true
		}
	}
	if !sawTools {
		t.Errorf("the safe attributes were dropped: %v", attrs)
	}
}

// TestOTLPBackendWithoutRedactionKeepsEverything verifies the policy
// is opt-out.
func TestOTLPBackendWithoutRedactionKeepsEverything(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces", Redact: false})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: "1", TraceID: "2", Name: "pi.harness.request",
		Attributes: telemetry.SpanAttributes{"pi.harness.model": "deepseek-v4.1-flash"},
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}
	span := firstSpan(t, collector.tracePayloads(t)[0])
	if !strings.Contains(mustJSON(t, span), "pi.harness.model") {
		t.Errorf("the attribute was dropped: %v", span["attributes"])
	}
}

// mustJSON renders a value as JSON.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// TestOTLPBackendEncodesTheStatus verifies the status codes.
func TestOTLPBackendEncodesTheStatus(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{
		{ID: "1", TraceID: "2", Name: "ok", Status: telemetry.SpanStatus{Code: telemetry.StatusOK}},
		{ID: "3", TraceID: "2", Name: "bad", Status: telemetry.SpanStatus{
			Code:  telemetry.StatusError,
			Error: &telemetry.SpanError{Name: "ToolError", Message: "the tool failed"},
		}},
	}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	spans := allSpans(t, collector.tracePayloads(t)[0])
	okStatus, _ := spans[0]["status"].(map[string]any)
	if okStatus["code"] != float64(1) {
		t.Errorf("the ok status = %v, want 1", okStatus)
	}
	errStatus, _ := spans[1]["status"].(map[string]any)
	if errStatus["code"] != float64(2) {
		t.Errorf("the error status = %v, want 2", errStatus)
	}
	if errStatus["message"] != "the tool failed" {
		t.Errorf("the error message = %v", errStatus["message"])
	}
}

// TestOTLPBackendEncodesTheAttributes verifies the attribute kinds.
func TestOTLPBackendEncodesTheAttributes(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: "1", TraceID: "2", Name: "x",
		Attributes: telemetry.SpanAttributes{
			"pi.harness.model":   "deepseek-v4.1-flash",
			"pi.harness.tools":   5,
			"pi.harness.failed":  true,
			"pi.harness.ratio":   0.5,
			"pi.harness.kinds":   []string{"a", "b"},
			"pi.harness.unknown": struct{}{},
		},
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	span := firstSpan(t, collector.tracePayloads(t)[0])
	attrs, _ := span["attributes"].([]any)
	byKey := map[string]map[string]any{}
	for _, attr := range attrs {
		kv, _ := attr.(map[string]any)
		value, _ := kv["value"].(map[string]any)
		byKey[kv["key"].(string)] = value
	}

	if byKey["pi.harness.model"]["stringValue"] != "deepseek-v4.1-flash" {
		t.Errorf("the model = %v", byKey["pi.harness.model"])
	}
	if byKey["pi.harness.tools"]["intValue"] != float64(5) {
		t.Errorf("the tools = %v", byKey["pi.harness.tools"])
	}
	if byKey["pi.harness.failed"]["boolValue"] != true {
		t.Errorf("the failed flag = %v", byKey["pi.harness.failed"])
	}
	if byKey["pi.harness.ratio"]["doubleValue"] != 0.5 {
		t.Errorf("the ratio = %v", byKey["pi.harness.ratio"])
	}
	array, _ := byKey["pi.harness.kinds"]["arrayValue"].(map[string]any)
	values, _ := array["values"].([]any)
	if len(values) != 2 {
		t.Errorf("the array = %v", array)
	}
	// A value of an unknown kind is dropped, not guessed at.
	if _, present := byKey["pi.harness.unknown"]; present {
		t.Errorf("an unknown attribute kind was encoded: %v", byKey["pi.harness.unknown"])
	}
}

// TestOTLPBackendSendsTheResource verifies the resource attributes.
func TestOTLPBackendSendsTheResource(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{
		Endpoint: server.URL + "/v1/traces",
		Resource: Resource{Name: "auto-harness", Version: "1.2.3",
			Attributes: map[string]string{"deployment.environment": "local"}},
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: "1", TraceID: "2", Name: "x",
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	payload := collector.tracePayloads(t)[0]
	rs, _ := payload["resourceSpans"].([]any)[0].(map[string]any)
	resource, _ := rs["resource"].(map[string]any)
	attrs, _ := resource["attributes"].([]any)
	byKey := map[string]string{}
	for _, attr := range attrs {
		kv, _ := attr.(map[string]any)
		value, _ := kv["value"].(map[string]any)
		byKey[kv["key"].(string)] = value["stringValue"].(string)
	}
	if byKey["service.name"] != "auto-harness" {
		t.Errorf("service.name = %q", byKey["service.name"])
	}
	if byKey["service.version"] != "1.2.3" {
		t.Errorf("service.version = %q", byKey["service.version"])
	}
	if byKey["deployment.environment"] != "local" {
		t.Errorf("deployment.environment = %q", byKey["deployment.environment"])
	}
}

// TestOTLPBackendSendsTheHeaders verifies the configured headers.
func TestOTLPBackendSendsTheHeaders(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{
		Endpoint: server.URL + "/v1/traces",
		Headers:  map[string]string{"Authorization": "Bearer token"},
	})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendTraces(context.Background(), []telemetry.StoredSpan{{
		ID: "1", TraceID: "2", Name: "x",
	}}); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	collector.mu.Lock()
	defer collector.mu.Unlock()
	if got := collector.headers.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := collector.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

// TestOTLPBackendDerivesTheMetricsEndpoint verifies the metrics
// endpoint comes from the traces one.
func TestOTLPBackendDerivesTheMetricsEndpoint(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if !strings.HasSuffix(backend.MetricsEndpoint(), "/v1/metrics") {
		t.Errorf("the metrics endpoint = %q", backend.MetricsEndpoint())
	}
	if !strings.HasSuffix(backend.TracesEndpoint(), "/v1/traces") {
		t.Errorf("the traces endpoint = %q", backend.TracesEndpoint())
	}
}

// TestOTLPBackendSendsMetrics verifies the metrics reach the
// collector's metrics endpoint.
func TestOTLPBackendSendsMetrics(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	now := time.Unix(1700000000, 0)
	point := MetricPoint{
		Name: "pi.harness.tokens", Description: "The model token usage.", Unit: "token",
		Kind: telemetry.MetricCounter, Value: 120, Count: 1, Sum: 120,
		Attributes: map[string]string{"pi.harness.kind": "input"},
		StartTime:  now, Time: now.Add(time.Second),
	}
	if err := backend.SendMetrics(context.Background(), []MetricPoint{point}); err != nil {
		t.Fatalf("SendMetrics: %v", err)
	}

	payloads := collector.metricPayloads(t)
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(payloads))
	}
	rm, _ := payloads[0]["resourceMetrics"].([]any)[0].(map[string]any)
	sm, _ := rm["scopeMetrics"].([]any)[0].(map[string]any)
	metrics, _ := sm["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("metrics = %v", sm["metrics"])
	}
	metric, _ := metrics[0].(map[string]any)
	if metric["name"] != "pi.harness.tokens" {
		t.Errorf("name = %v", metric["name"])
	}
	if metric["unit"] != "token" {
		t.Errorf("unit = %v", metric["unit"])
	}
	sum, _ := metric["sum"].(map[string]any)
	if sum["isMonotonic"] != true {
		t.Errorf("isMonotonic = %v, want true", sum["isMonotonic"])
	}
	if sum["aggregationTemporality"] != float64(2) {
		t.Errorf("aggregationTemporality = %v, want 2 (cumulative)", sum["aggregationTemporality"])
	}
	points, _ := sum["dataPoints"].([]any)
	if len(points) != 1 {
		t.Fatalf("dataPoints = %v", sum["dataPoints"])
	}
	dp, _ := points[0].(map[string]any)
	if dp["asDouble"] != float64(120) {
		t.Errorf("asDouble = %v", dp["asDouble"])
	}
	if dp["timeUnixNano"] != "1700000001000000000" {
		t.Errorf("timeUnixNano = %v", dp["timeUnixNano"])
	}
}

// TestOTLPBackendSendsAHistogram verifies the histogram's buckets.
func TestOTLPBackendSendsAHistogram(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	now := time.Unix(1700000000, 0)
	point := MetricPoint{
		Name: "pi.harness.request.duration", Unit: "ms",
		Kind: telemetry.MetricHistogram, Count: 3, Sum: 450, Min: 50, Max: 300,
		Bounds:       []float64{100, 500, 1000},
		BucketCounts: []uint64{1, 1, 1, 0},
		StartTime:    now, Time: now.Add(time.Second),
	}
	if err := backend.SendMetrics(context.Background(), []MetricPoint{point}); err != nil {
		t.Fatalf("SendMetrics: %v", err)
	}

	payload := collector.metricPayloads(t)[0]
	rm, _ := payload["resourceMetrics"].([]any)[0].(map[string]any)
	sm, _ := rm["scopeMetrics"].([]any)[0].(map[string]any)
	metric, _ := sm["metrics"].([]any)[0].(map[string]any)
	histogram, _ := metric["histogram"].(map[string]any)
	if histogram == nil {
		t.Fatalf("the metric has no histogram: %v", metric)
	}
	dp, _ := histogram["dataPoints"].([]any)[0].(map[string]any)
	if dp["count"] != float64(3) {
		t.Errorf("count = %v, want 3", dp["count"])
	}
	if dp["sum"] != float64(450) {
		t.Errorf("sum = %v, want 450", dp["sum"])
	}
	counts, _ := dp["bucketCounts"].([]any)
	if len(counts) != 4 {
		t.Errorf("bucketCounts = %v, want 4", counts)
	}
	bounds, _ := dp["explicitBounds"].([]any)
	if len(bounds) != 3 {
		t.Errorf("explicitBounds = %v, want 3", bounds)
	}
}

// TestOTLPBackendDropsAnInvalidHistogram verifies a histogram with no
// buckets is not shipped: a collector would reject it.
func TestOTLPBackendDropsAnInvalidHistogram(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	if err := backend.SendMetrics(context.Background(), []MetricPoint{{
		Name: "bad", Kind: telemetry.MetricHistogram, Count: 1,
	}}); err != nil {
		t.Fatalf("SendMetrics: %v", err)
	}
	payload := collector.metricPayloads(t)[0]
	rm, _ := payload["resourceMetrics"].([]any)[0].(map[string]any)
	sm, _ := rm["scopeMetrics"].([]any)[0].(map[string]any)
	metrics, _ := sm["metrics"].([]any)
	if len(metrics) != 0 {
		t.Errorf("metrics = %v, want none", metrics)
	}
}

// TestOTLPBackendCountsADroppedBatch verifies a failed request is
// counted and the error is returned.
func TestOTLPBackendCountsADroppedBatch(t *testing.T) {
	collector := &fakeCollector{status: http.StatusInternalServerError}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	err = backend.SendTraces(context.Background(), []telemetry.StoredSpan{{ID: "1", TraceID: "2", Name: "x"}})
	if err == nil {
		t.Fatal("SendTraces returned no error")
	}
	stats := backend.Stats()
	if stats.DroppedTraces != 1 {
		t.Errorf("dropped = %d, want 1", stats.DroppedTraces)
	}
	if stats.SentTraces != 0 {
		t.Errorf("sent = %d, want 0", stats.SentTraces)
	}
}

// TestOTLPBackendDoesNotRetryAClientError verifies a 4xx is not
// retried: the payload is wrong, and retrying it fails again.
func TestOTLPBackendDoesNotRetryAClientError(t *testing.T) {
	collector := &fakeCollector{status: http.StatusBadRequest}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	_ = backend.SendTraces(context.Background(), []telemetry.StoredSpan{{ID: "1", TraceID: "2", Name: "x"}})
	collector.mu.Lock()
	requests := len(collector.traces)
	collector.mu.Unlock()
	if requests != 1 {
		t.Errorf("requests = %d, want 1 (a 4xx is not retried)", requests)
	}
}

// TestOTLPBackendRetriesAServerError verifies a 5xx is retried once.
func TestOTLPBackendRetriesAServerError(t *testing.T) {
	collector := &fakeCollector{status: http.StatusServiceUnavailable}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces"})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	_ = backend.SendTraces(context.Background(), []telemetry.StoredSpan{{ID: "1", TraceID: "2", Name: "x"}})
	collector.mu.Lock()
	requests := len(collector.traces)
	collector.mu.Unlock()
	if requests != 2 {
		t.Errorf("requests = %d, want 2 (one retry)", requests)
	}
}

// TestOTLPBackendBatchesSpans verifies the batch size splits the
// requests.
func TestOTLPBackendBatchesSpans(t *testing.T) {
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()

	backend, err := NewOTLP(Options{Endpoint: server.URL + "/v1/traces", BatchSize: 2})
	if err != nil {
		t.Fatalf("NewOTLP: %v", err)
	}
	defer backend.Close()

	spans := make([]telemetry.StoredSpan, 0, 5)
	for i := 0; i < 5; i++ {
		spans = append(spans, telemetry.StoredSpan{ID: "1", TraceID: "2", Name: "x"})
	}
	if err := backend.SendTraces(context.Background(), spans); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}
	payloads := collector.tracePayloads(t)
	if len(payloads) != 3 {
		t.Fatalf("payloads = %d, want 3", len(payloads))
	}
	if got := len(allSpans(t, payloads[0])); got != 2 {
		t.Errorf("the first batch has %d spans, want 2", got)
	}
	if got := len(allSpans(t, payloads[2])); got != 1 {
		t.Errorf("the last batch has %d spans, want 1", got)
	}
}

// TestOTLPBackendRejectsABadEndpoint verifies the endpoint is checked.
func TestOTLPBackendRejectsABadEndpoint(t *testing.T) {
	if _, err := NewOTLP(Options{}); err == nil {
		t.Error("an empty endpoint was accepted")
	}
	if _, err := NewOTLP(Options{Endpoint: "ftp://example.com"}); err == nil {
		t.Error("a non-http endpoint was accepted")
	}
}

// firstSpan returns the payload's first span.
func firstSpan(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	spans := allSpans(t, payload)
	if len(spans) == 0 {
		t.Fatal("the payload has no spans")
	}
	return spans[0]
}

// allSpans returns the payload's spans.
func allSpans(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	resourceSpans, _ := payload["resourceSpans"].([]any)
	if len(resourceSpans) == 0 {
		return nil
	}
	rs, _ := resourceSpans[0].(map[string]any)
	scopeSpans, _ := rs["scopeSpans"].([]any)
	if len(scopeSpans) == 0 {
		return nil
	}
	ss, _ := scopeSpans[0].(map[string]any)
	raw, _ := ss["spans"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, span := range raw {
		encoded, _ := span.(map[string]any)
		out = append(out, encoded)
	}
	return out
}
