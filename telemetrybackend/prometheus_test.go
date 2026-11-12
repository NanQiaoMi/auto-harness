package telemetrybackend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimi/auto-harness/telemetry"
	"github.com/mimi/auto-harness/telemetrybackend/prompb"
	"github.com/mimi/auto-harness/telemetrybackend/snappy"
)

// fakePrometheus is a stand-in for a Prometheus server's remote write
// endpoint: it records the requests it receives.
type fakePrometheus struct {
	mu sync.Mutex
	// bodies are the compressed bodies of the requests.
	bodies [][]byte
	// headers are the headers of the last request.
	headers http.Header
	// status is the status code to return. Zero means 200.
	status int
}

// handler serves the write endpoint.
func (f *fakePrometheus) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.headers = r.Header.Clone()
	status := f.status
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

// requests returns the number of requests received.
func (f *fakePrometheus) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// lastHeaders returns the headers of the last request.
func (f *fakePrometheus) lastHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers
}

// point builds a metric point.
func point(name string, kind telemetry.MetricKind, value float64, attributes map[string]string) MetricPoint {
	return MetricPoint{
		Name:       name,
		Kind:       kind,
		Value:      value,
		Attributes: attributes,
		Time:       time.Unix(1700000000, 0),
	}
}

// TestPrometheusBackendSendsAWriteRequest checks that the backend posts
// a remote write request with the headers a Prometheus server expects.
func TestPrometheusBackendSendsAWriteRequest(t *testing.T) {
	server := &fakePrometheus{}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{Endpoint: httpServer.URL + "/api/v1/write"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	points := []MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 42, map[string]string{
			"pi.harness.kind":  "input",
			"pi.harness.model": "deepseek-v4.1-flash",
		}),
	}
	if err := backend.SendMetrics(context.Background(), points); err != nil {
		t.Fatalf("send metrics: %v", err)
	}
	if server.requests() != 1 {
		t.Fatalf("requests = %d, want 1", server.requests())
	}

	headers := server.lastHeaders()
	if got := headers.Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want %q", got, "application/x-protobuf")
	}
	if got := headers.Get("Content-Encoding"); got != "snappy" {
		t.Errorf("Content-Encoding = %q, want %q", got, "snappy")
	}
}

// TestPrometheusBackendRejectsABadEndpoint checks the endpoint's
// validation.
func TestPrometheusBackendRejectsABadEndpoint(t *testing.T) {
	if _, err := NewPrometheus(Options{}); err == nil {
		t.Error("an empty endpoint was accepted")
	}
	if _, err := NewPrometheus(Options{Endpoint: "ftp://example.com"}); err == nil {
		t.Error("a non-http endpoint was accepted")
	}
	if _, err := NewPrometheus(Options{Endpoint: "http://example.com/write"}); err != nil {
		t.Errorf("a valid endpoint was rejected: %v", err)
	}
}

// TestPrometheusBackendDropsTheSpans checks that a backend with no
// place for spans says so rather than inventing one.
func TestPrometheusBackendDropsTheSpans(t *testing.T) {
	server := &fakePrometheus{}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{Endpoint: httpServer.URL + "/api/v1/write"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	spans := []telemetry.StoredSpan{{Name: "pi.harness.request"}}
	if err := backend.SendTraces(context.Background(), spans); err != nil {
		t.Fatalf("send traces: %v", err)
	}
	if server.requests() != 0 {
		t.Errorf("requests = %d, want 0: Prometheus has no traces", server.requests())
	}
	if stats := backend.Stats(); stats.DroppedTraces != 1 {
		t.Errorf("DroppedTraces = %d, want 1", stats.DroppedTraces)
	}
}

// TestPrometheusMetricName checks the name translation.
func TestPrometheusMetricName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pi.harness.tokens", "pi_harness_tokens"},
		{"pi.harness.request.duration", "pi_harness_request_duration"},
		{"tokens", "tokens"},
		{"9lives", "_9lives"},
		{"", "unnamed_metric"},
	}
	for _, c := range cases {
		if got := PrometheusMetricName(c.in); got != c.want {
			t.Errorf("PrometheusMetricName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPrometheusLabelName checks the label name translation.
func TestPrometheusLabelName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pi.harness.model", "pi_harness_model"},
		{"model", "model"},
		{"9lives", "_9lives"},
		{"", "unnamed_label"},
	}
	for _, c := range cases {
		if got := PrometheusLabelName(c.in); got != c.want {
			t.Errorf("PrometheusLabelName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEncodePrometheusNamesTheSeries checks that the metric's name
// travels as the "__name__" label, which is how Prometheus carries it.
func TestEncodePrometheusNamesTheSeries(t *testing.T) {
	request := encodePrometheus([]MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 42, map[string]string{
			"pi.harness.model": "deepseek-v4.1-flash",
		}),
	})
	if len(request.TimeSeries) != 1 {
		t.Fatalf("series = %d, want 1", len(request.TimeSeries))
	}
	series := request.TimeSeries[0]
	if len(series.Labels) != 2 {
		t.Fatalf("labels = %d, want 2", len(series.Labels))
	}
	if series.Labels[0].Name != "__name__" {
		t.Errorf("the first label is %q, want %q", series.Labels[0].Name, "__name__")
	}
	// A counter carries the "_total" suffix Prometheus expects.
	if series.Labels[0].Value != "pi_harness_tokens_total" {
		t.Errorf("the name is %q, want %q", series.Labels[0].Value, "pi_harness_tokens_total")
	}
	if series.Labels[1].Name != "pi_harness_model" {
		t.Errorf("the second label is %q, want %q", series.Labels[1].Name, "pi_harness_model")
	}
	if len(series.Samples) != 1 || series.Samples[0].Value != 42 {
		t.Errorf("the samples are %v, want one sample of 42", series.Samples)
	}
	// The timestamp is in milliseconds since the epoch.
	if series.Samples[0].Timestamp != 1700000000000 {
		t.Errorf("the timestamp is %d, want %d", series.Samples[0].Timestamp, 1700000000000)
	}
}

// TestEncodePrometheusMakesAnUpDownCounterAGauge checks that a counter
// that can go down is a gauge.
func TestEncodePrometheusMakesAnUpDownCounterAGauge(t *testing.T) {
	request := encodePrometheus([]MetricPoint{
		point("pi.harness.queue.depth", telemetry.MetricUpDownCounter, 3, nil),
	})
	if len(request.TimeSeries) != 1 {
		t.Fatalf("series = %d, want 1", len(request.TimeSeries))
	}
	name := request.TimeSeries[0].Labels[0].Value
	if name != "pi_harness_queue_depth" {
		t.Errorf("the name is %q, want %q: an up-down counter is a gauge", name, "pi_harness_queue_depth")
	}
}

// TestEncodePrometheusExpandsAHistogram checks the classic Prometheus
// histogram layout: one series per bucket, plus the sum and the count.
func TestEncodePrometheusExpandsAHistogram(t *testing.T) {
	p := point("pi.harness.request.duration", telemetry.MetricHistogram, 0, map[string]string{
		"pi.harness.provider": "openai",
	})
	p.Bounds = []float64{100, 500}
	// Three observations: one below 100, one between 100 and 500, one
	// above both.
	p.BucketCounts = []uint64{1, 1, 1}
	p.Count = 3
	p.Sum = 700

	request := encodePrometheus([]MetricPoint{p})
	if len(request.TimeSeries) != 5 {
		t.Fatalf("series = %d, want 5 (two buckets, the overflow bucket, the count and the sum)", len(request.TimeSeries))
	}

	// The buckets are cumulative: each one holds every observation up
	// to its bound.
	buckets := map[string]float64{}
	counts := map[string]float64{}
	for _, series := range request.TimeSeries {
		name := series.Labels[0].Value
		value := series.Samples[0].Value
		switch {
		case strings.HasSuffix(name, "_bucket"):
			le := ""
			for _, label := range series.Labels {
				if label.Name == "le" {
					le = label.Value
				}
			}
			buckets[le] = value
		case strings.HasSuffix(name, "_count"):
			counts["count"] = value
		case strings.HasSuffix(name, "_sum"):
			counts["sum"] = value
		default:
			t.Errorf("unexpected series %q", name)
		}
	}
	if buckets["100"] != 1 {
		t.Errorf("the 100 bucket is %v, want 1", buckets["100"])
	}
	if buckets["500"] != 2 {
		t.Errorf("the 500 bucket is %v, want 2", buckets["500"])
	}
	if buckets["+Inf"] != 3 {
		t.Errorf("the +Inf bucket is %v, want 3", buckets["+Inf"])
	}
	if counts["count"] != 3 {
		t.Errorf("the count is %v, want 3", counts["count"])
	}
	if counts["sum"] != 700 {
		t.Errorf("the sum is %v, want 700", counts["sum"])
	}
}

// TestEncodePrometheusSkipsABrokenHistogram checks that a histogram
// without usable buckets is dropped rather than shipped as a lie.
func TestEncodePrometheusSkipsABrokenHistogram(t *testing.T) {
	p := point("pi.harness.request.duration", telemetry.MetricHistogram, 0, nil)
	p.Bounds = []float64{100, 500}
	p.BucketCounts = []uint64{1}

	request := encodePrometheus([]MetricPoint{p})
	if len(request.TimeSeries) != 0 {
		t.Errorf("series = %d, want 0", len(request.TimeSeries))
	}
}

// TestEncodePrometheusIsStable checks that the same points always
// produce the same bytes.
func TestEncodePrometheusIsStable(t *testing.T) {
	points := []MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 42, map[string]string{
			"pi.harness.model": "deepseek-v4.1-flash",
			"pi.harness.kind":  "input",
		}),
		point("pi.harness.tool.calls", telemetry.MetricCounter, 7, map[string]string{
			"pi.harness.tool": "read",
		}),
	}
	first := encodePrometheus(points).Marshal()
	for i := 0; i < 20; i++ {
		if got := encodePrometheus(points).Marshal(); string(got) != string(first) {
			t.Fatalf("the encoding changed between rounds")
		}
	}
}

// TestPrometheusMetadataDescribesTheFamilies checks that the request
// carries the metric families' metadata.
func TestPrometheusMetadataDescribesTheFamilies(t *testing.T) {
	p := point("pi.harness.tokens", telemetry.MetricCounter, 42, nil)
	p.Description = "The model token usage."
	p.Unit = "token"

	request := encodePrometheus([]MetricPoint{p})
	if len(request.Metadata) != 1 {
		t.Fatalf("metadata = %d, want 1", len(request.Metadata))
	}
	metadata := request.Metadata[0]
	if metadata.Type != prompb.MetricTypeCounter {
		t.Errorf("the type is %v, want a counter", metadata.Type)
	}
	if metadata.MetricFamilyName != "pi_harness_tokens" {
		t.Errorf("the family is %q, want %q", metadata.MetricFamilyName, "pi_harness_tokens")
	}
	if metadata.Help != "The model token usage." {
		t.Errorf("the help is %q, want %q", metadata.Help, "The model token usage.")
	}
	if metadata.Unit != "token" {
		t.Errorf("the unit is %q, want %q", metadata.Unit, "token")
	}
}

// TestPrometheusBackendReportsAFailedRequest checks that a server that
// refuses the write is reported, and that the metrics are counted as
// dropped.
func TestPrometheusBackendReportsAFailedRequest(t *testing.T) {
	server := &fakePrometheus{status: http.StatusBadRequest}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{Endpoint: httpServer.URL + "/api/v1/write"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	points := []MetricPoint{point("pi.harness.tokens", telemetry.MetricCounter, 1, nil)}
	if err := backend.SendMetrics(context.Background(), points); err == nil {
		t.Fatal("the failed request was not reported")
	}
	if stats := backend.Stats(); stats.DroppedMetrics != 1 {
		t.Errorf("DroppedMetrics = %d, want 1", stats.DroppedMetrics)
	}
	// A 4xx is the payload's fault, so it is not retried.
	if server.requests() != 1 {
		t.Errorf("requests = %d, want 1: a 4xx is not retried", server.requests())
	}
}

// TestPrometheusBackendRetriesAServerError checks that a server that
// is briefly busy does not cost the batch.
func TestPrometheusBackendRetriesAServerError(t *testing.T) {
	server := &fakePrometheus{status: http.StatusServiceUnavailable}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{Endpoint: httpServer.URL + "/api/v1/write"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	points := []MetricPoint{point("pi.harness.tokens", telemetry.MetricCounter, 1, nil)}
	if err := backend.SendMetrics(context.Background(), points); err == nil {
		t.Fatal("the failed request was not reported")
	}
	if server.requests() != 2 {
		t.Errorf("requests = %d, want 2: a 5xx is retried once", server.requests())
	}
}

// TestPrometheusBackendSendsTheHeaders checks that the configured
// headers are sent.
func TestPrometheusBackendSendsTheHeaders(t *testing.T) {
	server := &fakePrometheus{}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{
		Endpoint: httpServer.URL + "/api/v1/write",
		Headers:  map[string]string{"X-Scope-OrgID": "harness"},
	})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	if err := backend.SendMetrics(context.Background(), []MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 1, nil),
	}); err != nil {
		t.Fatalf("send metrics: %v", err)
	}
	if got := server.lastHeaders().Get("X-Scope-OrgID"); got != "harness" {
		t.Errorf("X-Scope-OrgID = %q, want %q", got, "harness")
	}
}

// TestPrometheusBodyMatchesTheReferenceDecoder checks that the bytes
// the backend sends decode the way a Prometheus server decodes them.
//
// The decoder is the reference implementation: the harness's own
// encoder is only worth having if a real server can read it.
func TestPrometheusBodyMatchesTheReferenceDecoder(t *testing.T) {
	decoder, cleanup := buildReferenceDecoder(t)
	defer cleanup()

	points := []MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 42, map[string]string{
			"pi.harness.model": "deepseek-v4.1-flash",
		}),
		point("pi.harness.queue.depth", telemetry.MetricUpDownCounter, 3, nil),
	}
	histogram := point("pi.harness.request.duration", telemetry.MetricHistogram, 0, map[string]string{
		"pi.harness.provider": "openai",
	})
	histogram.Bounds = []float64{100, 500}
	histogram.BucketCounts = []uint64{1, 1, 1}
	histogram.Count = 3
	histogram.Sum = 700
	points = append(points, histogram)

	request := encodePrometheus(points)
	body, err := compressPrometheus(request)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}

	decoded := decodeWithReference(t, decoder, body)
	if len(decoded.Timeseries) != len(request.TimeSeries) {
		t.Fatalf("the reference decoded %d series, want %d", len(decoded.Timeseries), len(request.TimeSeries))
	}
	for i, series := range request.TimeSeries {
		got := decoded.Timeseries[i]
		if len(got.Labels) != len(series.Labels) {
			t.Errorf("series %d: the reference decoded %d labels, want %d", i, len(got.Labels), len(series.Labels))
			continue
		}
		for j, label := range series.Labels {
			if got.Labels[j].Name != label.Name || got.Labels[j].Value != label.Value {
				t.Errorf("series %d: label %d is %q=%q, want %q=%q", i, j,
					got.Labels[j].Name, got.Labels[j].Value, label.Name, label.Value)
			}
		}
		if len(got.Samples) != len(series.Samples) {
			t.Errorf("series %d: the reference decoded %d samples, want %d", i, len(got.Samples), len(series.Samples))
			continue
		}
		for j, sample := range series.Samples {
			if got.Samples[j].Value != sample.Value || got.Samples[j].Timestamp != sample.Timestamp {
				t.Errorf("series %d: sample %d is %v/%d, want %v/%d", i, j,
					got.Samples[j].Value, got.Samples[j].Timestamp, sample.Value, sample.Timestamp)
			}
		}
	}
}

// referenceWriteRequest is the reference decoder's output.
type referenceWriteRequest struct {
	Timeseries []struct {
		Labels []struct {
			Name  string
			Value string
		}
		Samples []struct {
			Value     float64
			Timestamp int64
		}
	} `json:"series"`
}

// buildReferenceDecoder builds a program that decodes a remote write
// request with the reference implementation.
//
// The harness ships no external dependency, so the encoder and the
// compression here are hand-written. The reference is the oracle: what
// it decodes is what a Prometheus server sees.
func buildReferenceDecoder(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "prometheus-decoder")
	if err != nil {
		t.Fatalf("make the decoder directory: %v", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	module := `module decoder

go 1.22

require (
	github.com/golang/snappy v0.0.4
	github.com/prometheus/prometheus v0.316.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o644); err != nil {
		cleanup()
		t.Fatalf("write the decoder module: %v", err)
	}

	source := filepath.Join(dir, "main.go")
	program := `package main

import (
	"encoding/json"
	"os"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

type label struct {
	Name  string ` + "`json:\"name\"`" + `
	Value string ` + "`json:\"value\"`" + `
}

type sampleValue struct {
	Value     float64 ` + "`json:\"value\"`" + `
	Timestamp int64   ` + "`json:\"timestamp\"`" + `
}

type series struct {
	Labels  []label      ` + "`json:\"labels\"`" + `
	Samples []sampleValue ` + "`json:\"samples\"`" + `
}

type request struct {
	Timeseries []series ` + "`json:\"series\"`" + `
}

func main() {
	compressed, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	raw, err := snappy.Decode(nil, compressed)
	if err != nil {
		panic(err)
	}
	var decoded prompb.WriteRequest
	if err := decoded.Unmarshal(raw); err != nil {
		panic(err)
	}
	out := request{}
	for _, s := range decoded.Timeseries {
		entry := series{}
		for _, l := range s.Labels {
			entry.Labels = append(entry.Labels, label{Name: l.Name, Value: l.Value})
		}
		for _, sample := range s.Samples {
			entry.Samples = append(entry.Samples, sampleValue{Value: sample.Value, Timestamp: sample.Timestamp})
		}
		out.Timeseries = append(out.Timeseries, entry)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(encoded)
}
`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		cleanup()
		t.Fatalf("write the decoder: %v", err)
	}

	binary := filepath.Join(dir, "decoder")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY="+oracleProxy(), "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		cleanup()
		t.Fatalf("build the decoder: %v\n%s", err, out)
	}
	return binary, cleanup
}

// TestPrometheusBackendRedactsTheSensitiveAttributes verifies the
// backend honours the schema's sensitive marking for the metric
// attributes too.
//
// The harness's schema marks nothing sensitive today, so the test adds
// a marking of its own and checks the backend drops it: the policy has
// to work when the schema grows, and it has to work for whichever
// protocol carries the data.
func TestPrometheusBackendRedactsTheSensitiveAttributes(t *testing.T) {
	// Mark an attribute of the token metric as sensitive.
	definition := telemetry.Schema.Metrics["pi.harness.tokens"]
	attr := definition.Attributes["pi.harness.model"]
	attr.Sensitive = true
	definition.Attributes["pi.harness.model"] = attr
	telemetry.Schema.Metrics["pi.harness.tokens"] = definition
	defer func() {
		attr.Sensitive = false
		definition.Attributes["pi.harness.model"] = attr
		telemetry.Schema.Metrics["pi.harness.tokens"] = definition
	}()

	server := &fakePrometheus{}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{Endpoint: httpServer.URL + "/api/v1/write", Redact: true})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	if err := backend.SendMetrics(context.Background(), []MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 42, map[string]string{
			"pi.harness.model": "deepseek-v4.1-flash",
			"pi.harness.kind":  "input",
		}),
	}); err != nil {
		t.Fatalf("send metrics: %v", err)
	}

	// The body is decoded the way a Prometheus server decodes it.
	raw, err := snappy.Decode(server.bodies[0])
	if err != nil {
		t.Fatalf("the body is not a snappy block: %v", err)
	}
	request, err := prompb.Decode(raw)
	if err != nil {
		t.Fatalf("the body is not a write request: %v", err)
	}
	if len(request.TimeSeries) != 1 {
		t.Fatalf("series = %d, want 1", len(request.TimeSeries))
	}
	for _, label := range request.TimeSeries[0].Labels {
		if label.Name == "pi_harness_model" {
			t.Error("the sensitive attribute was sent")
		}
	}
	// The other attributes survive.
	var sawKind bool
	for _, label := range request.TimeSeries[0].Labels {
		if label.Name == "pi_harness_kind" {
			sawKind = true
		}
	}
	if !sawKind {
		t.Errorf("the safe attributes were dropped: %v", request.TimeSeries[0].Labels)
	}
}

// TestPrometheusBackendWithoutRedactionKeepsEverything verifies the
// policy is opt-out.
func TestPrometheusBackendWithoutRedactionKeepsEverything(t *testing.T) {
	definition := telemetry.Schema.Metrics["pi.harness.tokens"]
	attr := definition.Attributes["pi.harness.model"]
	attr.Sensitive = true
	definition.Attributes["pi.harness.model"] = attr
	telemetry.Schema.Metrics["pi.harness.tokens"] = definition
	defer func() {
		attr.Sensitive = false
		definition.Attributes["pi.harness.model"] = attr
		telemetry.Schema.Metrics["pi.harness.tokens"] = definition
	}()

	server := &fakePrometheus{}
	httpServer := httptest.NewServer(http.HandlerFunc(server.handler))
	defer httpServer.Close()

	backend, err := NewPrometheus(Options{Endpoint: httpServer.URL + "/api/v1/write", Redact: false})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer backend.Close()

	if err := backend.SendMetrics(context.Background(), []MetricPoint{
		point("pi.harness.tokens", telemetry.MetricCounter, 42, map[string]string{
			"pi.harness.model": "deepseek-v4.1-flash",
		}),
	}); err != nil {
		t.Fatalf("send metrics: %v", err)
	}

	raw, err := snappy.Decode(server.bodies[0])
	if err != nil {
		t.Fatalf("the body is not a snappy block: %v", err)
	}
	request, err := prompb.Decode(raw)
	if err != nil {
		t.Fatalf("the body is not a write request: %v", err)
	}
	var sawModel bool
	for _, label := range request.TimeSeries[0].Labels {
		if label.Name == "pi_harness_model" {
			sawModel = true
		}
	}
	if !sawModel {
		t.Errorf("the attribute was dropped without the redaction policy: %v", request.TimeSeries[0].Labels)
	}
}

// oracleProxy is the module proxy the reference build uses.
//
// The environment's own proxy is used when it sets one; otherwise a
// mirror is picked, because the default proxy is unreachable from
// here.
func oracleProxy() string {
	if proxy := os.Getenv("GOPROXY"); proxy != "" {
		return proxy
	}
	return "https://goproxy.cn,direct"
}

// decodeWithReference decodes a write request with the reference
// implementation.
func decodeWithReference(t *testing.T, decoder string, body []byte) referenceWriteRequest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write the body: %v", err)
	}
	out, err := exec.Command(decoder, path).Output()
	if err != nil {
		t.Fatalf("run the decoder: %v", err)
	}
	var decoded referenceWriteRequest
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode the decoder's output: %v\n%s", err, out)
	}
	return decoded
}
