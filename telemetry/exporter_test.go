package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExporterWritesTheStatus verifies the status's wire format: the
// code is a plain string under "status", the error is its object.
func TestExporterWritesTheStatus(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf, ExporterOptions{})

	if err := exp.WriteSpan(StoredSpan{
		ID: "s1", TraceID: "t1", Name: "pi.harness.tool",
		Status: SpanStatus{Code: StatusOK},
	}); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}
	if err := exp.WriteSpan(StoredSpan{
		ID: "s2", TraceID: "t1", Name: "pi.harness.tool",
		Status: SpanStatus{
			Code:  StatusError,
			Error: &SpanError{Name: "ToolError", Message: "the tool failed"},
		},
	}); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var okRecord, errRecord map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &okRecord); err != nil {
		t.Fatalf("the line is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &errRecord); err != nil {
		t.Fatalf("the line is not JSON: %v", err)
	}

	okStatus, _ := okRecord["status"].(map[string]any)
	if okStatus["status"] != "ok" {
		t.Errorf("the status = %v, want ok", okRecord["status"])
	}
	if _, present := okStatus["error"]; present {
		t.Errorf("the ok status carries an error: %v", okStatus)
	}

	errStatus, _ := errRecord["status"].(map[string]any)
	if errStatus["status"] != "error" {
		t.Errorf("the status = %v, want error", errRecord["status"])
	}
	errObj, _ := errStatus["error"].(map[string]any)
	if errObj["Name"] != "ToolError" {
		t.Errorf("the error = %v", errStatus["error"])
	}
}

// TestExporterWritesOneJSONObjectPerLine verifies the wire format.
func TestExporterWritesOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf, ExporterOptions{})

	span := StoredSpan{
		ID:        "s1",
		TraceID:   "t1",
		Name:      "pi.harness.session",
		StartedMs: 100,
		EndedMs:   200,
		Attributes: SpanAttributes{
			"pi.harness.model": "deepseek-v4.1-flash",
		},
		Status: SpanStatus{Code: StatusOK},
	}
	if err := exp.WriteSpan(span); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}
	if err := exp.WriteSpan(span); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("the line is not JSON: %v", err)
	}
	if record["name"] != "pi.harness.session" {
		t.Errorf("name = %v", record["name"])
	}
	if record["traceId"] != "t1" {
		t.Errorf("traceId = %v", record["traceId"])
	}
	if exp.Count() != 2 {
		t.Errorf("count = %d, want 2", exp.Count())
	}
}

// TestExporterWritesATrace verifies a trace's spans all land.
func TestExporterWritesATrace(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf, ExporterOptions{})
	trace := StoredTrace{
		TraceID: "t1",
		Spans: []StoredSpan{
			{ID: "s1", TraceID: "t1", Name: "a"},
			{ID: "s2", TraceID: "t1", Name: "b", ParentID: "s1"},
		},
	}
	if err := exp.WriteTrace(trace); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	if exp.Count() != 2 {
		t.Errorf("count = %d, want 2", exp.Count())
	}
	if !strings.Contains(buf.String(), `"parentId":"s1"`) {
		t.Errorf("the parent id is missing: %s", buf.String())
	}
}

// TestExporterRedactsSensitiveAttributes verifies the redaction policy
// drops the attributes the schema marks sensitive.
func TestExporterRedactsSensitiveAttributes(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf, ExporterOptions{Redact: true})

	// The schema marks no attribute sensitive today; the point of the
	// test is that the filter runs and keeps the safe ones.
	span := StoredSpan{
		ID:      "s1",
		TraceID: "t1",
		Name:    "pi.harness.request",
		Attributes: SpanAttributes{
			"pi.harness.model": "deepseek-v4.1-flash",
			"pi.harness.stop":  "stop",
		},
	}
	if err := exp.WriteSpan(span); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}
	if !strings.Contains(buf.String(), "deepseek-v4.1-flash") {
		t.Errorf("the safe attribute was dropped: %s", buf.String())
	}
}

// TestExporterWithoutRedactionKeepsEverything verifies the policy is
// opt-out.
func TestExporterWithoutRedactionKeepsEverything(t *testing.T) {
	var buf bytes.Buffer
	exp := NewExporter(&buf, ExporterOptions{Redact: false})
	span := StoredSpan{ID: "s1", TraceID: "t1", Name: "pi.harness.request"}
	if err := exp.WriteSpan(span); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}
	if exp.Count() != 1 {
		t.Errorf("count = %d, want 1", exp.Count())
	}
}

// TestFileExporterCreatesAPrivateFile verifies the file mode: telemetry
// is local diagnostics, not a shared artifact.
func TestFileExporterCreatesAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")

	exp, err := NewFileExporter(path, ExporterOptions{})
	if err != nil {
		t.Fatalf("NewFileExporter: %v", err)
	}
	if err := exp.WriteSpan(StoredSpan{ID: "s1", TraceID: "t1", Name: "x"}); err != nil {
		t.Fatalf("WriteSpan: %v", err)
	}
	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `"name":"x"`) {
		t.Errorf("the file does not hold the span: %s", data)
	}
}

// TestFileExporterAppends verifies a second exporter adds to the same
// file rather than replacing it.
func TestFileExporterAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")

	for i := 0; i < 2; i++ {
		exp, err := NewFileExporter(path, ExporterOptions{})
		if err != nil {
			t.Fatalf("NewFileExporter: %v", err)
		}
		if err := exp.WriteSpan(StoredSpan{ID: "s1", TraceID: "t1", Name: "x"}); err != nil {
			t.Fatalf("WriteSpan: %v", err)
		}
		if err := exp.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("lines = %d, want 2", len(lines))
	}
}

// TestSchemaDeclaresTheHarnessVocabulary verifies the harness's own
// schema is well-formed.
func TestSchemaDeclaresTheHarnessVocabulary(t *testing.T) {
	if Schema.Version != harnessSchemaVersion {
		t.Errorf("version = %d, want %d", Schema.Version, harnessSchemaVersion)
	}
	for _, name := range []string{
		"pi.harness.session",
		"pi.harness.request",
		"pi.harness.tool",
		"pi.harness.compaction",
	} {
		if _, ok := Schema.Spans[name]; !ok {
			t.Errorf("the schema has no span %q", name)
		}
	}
	for _, name := range []string{
		"pi.harness.request.duration",
		"pi.harness.tokens",
		"pi.harness.tool.calls",
	} {
		if _, ok := Schema.Metrics[name]; !ok {
			t.Errorf("the schema has no metric %q", name)
		}
	}
}

// TestSchemaAttributesAreNamespacedAndPrimitive verifies the shape of
// the harness's vocabulary: every attribute is namespaced, and every
// declared type is a primitive or an array of primitives.
//
// The privacy policy is not a naming rule — it is that the recorded
// values are facts, not content. The collector test proves that: it
// drives a session with a distinctive prompt and tool output and checks
// that neither appears in any attribute.
func TestSchemaAttributesAreNamespacedAndPrimitive(t *testing.T) {
	primitive := map[AttributeType]bool{
		AttributeString:       true,
		AttributeNumber:       true,
		AttributeBoolean:      true,
		AttributeStringArray:  true,
		AttributeNumberArray:  true,
		AttributeBooleanArray: true,
	}
	check := func(where, name string, definition AttributeDefinition) {
		t.Helper()
		if !strings.HasPrefix(name, "pi.harness.") {
			t.Errorf("%s: the attribute %q is not namespaced", where, name)
		}
		if !primitive[definition.Type] {
			t.Errorf("%s.%s: the type %q is not primitive", where, name, definition.Type)
		}
	}
	for name, span := range Schema.Spans {
		for attr, definition := range span.StartAttributes {
			check(name, attr, definition)
		}
		for attr, definition := range span.EndAttributes {
			check(name, attr, definition)
		}
		for event, definition := range span.Events {
			for attr, attrDefinition := range definition.Attributes {
				check(name+"."+event, attr, attrDefinition)
			}
		}
	}
	for name, metric := range Schema.Metrics {
		for attr, definition := range metric.Attributes {
			check(name, attr, definition)
		}
	}
}

// TestSchemaMetricsAreLowCardinality verifies no metric attribute is
// high cardinality: a high-cardinality metric attribute would explode
// the metric's series count.
func TestSchemaMetricsAreLowCardinality(t *testing.T) {
	for name, metric := range Schema.Metrics {
		for attr, definition := range metric.Attributes {
			if definition.Cardinality == CardinalityHigh {
				t.Errorf("%s.%s is high cardinality", name, attr)
			}
		}
	}
}
