//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimi/auto-harness/agent"
	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/telemetry"
	"github.com/mimi/auto-harness/telemetryext"
	"github.com/mimi/auto-harness/tools"
)

// TestLiveTelemetryRecordsTheRun verifies the telemetry contract against
// the real model: the spans come out, the attributes carry facts, and
// nothing the user said ends up in them.
func TestLiveTelemetryRecordsTheRun(t *testing.T) {
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}

	provider := newProvider(t)
	recorder := telemetry.NewMemoryContext()
	collector := telemetryext.New(recorder, provider.Name(), testModel)

	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())
	if err := collector.Setup(api); err != nil {
		t.Fatalf("collector setup: %v", err)
	}

	ag := agent.New(agent.Config{
		Provider:  provider,
		Model:     testModel,
		MaxTurns:  6,
		MaxTokens: 1024,
	}, bus, reg)

	// The session lifecycle the collector brackets.
	if err := bus.Emit(&ext.Event{Type: ext.EventSessionStart, Reason: "e2e"}); err != nil {
		t.Fatalf("session start: %v", err)
	}

	const secret = "{{SECRET_q4nmmfzk}}"
	const marker = "harness-telemetry-3c9d"
	if _, err := ag.Run(context.Background(),
		"Use the bash tool to run exactly: echo "+marker+". Then reply with the output you saw. "+
			"Also remember the code "+secret+"."); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := bus.Emit(&ext.Event{Type: ext.EventSessionShutdown, Reason: "e2e"}); err != nil {
		t.Fatalf("session shutdown: %v", err)
	}

	spans := recorder.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans were recorded")
	}

	// The session, request and tool spans are all there.
	names := map[string]bool{}
	for _, s := range spans {
		names[s.Name] = true
	}
	for _, want := range []string{"pi.harness.session", "pi.harness.request", "pi.harness.tool"} {
		if !names[want] {
			t.Errorf("no span named %q; got %v", want, names)
		}
	}

	// The request span carries the facts about the request.
	var request telemetry.RecordedSpan
	for _, s := range spans {
		if s.Name == "pi.harness.request" {
			request = s
			break
		}
	}
	if request.Attributes["pi.harness.model"] != testModel {
		t.Errorf("model = %v, want %q", request.Attributes["pi.harness.model"], testModel)
	}
	if request.Attributes["pi.harness.provider"] != provider.Name() {
		t.Errorf("provider = %v, want %q", request.Attributes["pi.harness.provider"], provider.Name())
	}
	if request.Attributes["pi.harness.tools"] == nil {
		t.Error("the request span has no tool count")
	}
	if request.Attributes["pi.harness.usage.input"] == nil {
		t.Error("the request span has no token usage")
	}
	if !request.Settled {
		t.Error("the request span is not settled")
	}

	// The tool span names the tool, and nothing else.
	for _, s := range spans {
		if s.Name != "pi.harness.tool" {
			continue
		}
		if s.Attributes["pi.harness.tool"] != "bash" {
			t.Errorf("tool = %v, want bash", s.Attributes["pi.harness.tool"])
		}
	}

	// The privacy policy holds against a real run: the secret the model
	// was told, and the tool's output, stay out of the telemetry.
	for _, s := range spans {
		for name, value := range s.Attributes {
			assertNoTelemetryLeak(t, s.Name+"."+name, value, secret, marker)
		}
		for _, e := range s.Events {
			for name, value := range e.Attributes {
				assertNoTelemetryLeak(t, s.Name+"."+e.Name+"."+name, value, secret, marker)
			}
		}
	}
	for _, m := range recorder.GetMeasurements() {
		for name, value := range m.Attributes {
			assertNoTelemetryLeak(t, m.Name+"."+name, value, secret, marker)
		}
	}
}

// assertNoTelemetryLeak fails the test when the value carries a secret
// or the tool's output.
func assertNoTelemetryLeak(t *testing.T, where string, value any, secret, marker string) {
	t.Helper()
	text := strings.ToLower(toTextValue(value))
	for _, forbidden := range []string{secret, marker} {
		if forbidden != "" && strings.Contains(text, strings.ToLower(forbidden)) {
			t.Errorf("%s leaks content: %q", where, toTextValue(value))
		}
	}
}

// toTextValue renders an attribute value as text.
func toTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return fmt.Sprintf("%v", value)
	}
}

// TestLiveTelemetryExportsToJSONL verifies the exporter's output on a
// real run.
func TestLiveTelemetryExportsToJSONL(t *testing.T) {
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewBashTool()); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	exporter, err := telemetry.NewFileExporter(path, telemetry.ExporterOptions{Redact: true})
	if err != nil {
		t.Fatalf("NewFileExporter: %v", err)
	}
	defer exporter.Close()

	recorder := telemetry.NewMemoryContext()
	recorder.SetSink(func(span telemetry.StoredSpan) {
		if err := exporter.WriteSpan(span); err != nil {
			t.Errorf("WriteSpan: %v", err)
		}
	})
	collector := telemetryext.New(recorder, "opencode-go", testModel)
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())
	if err := collector.Setup(api); err != nil {
		t.Fatalf("collector setup: %v", err)
	}

	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  4,
		MaxTokens: 512,
	}, bus, reg)

	_ = bus.Emit(&ext.Event{Type: ext.EventSessionStart, Reason: "e2e"})
	if _, err := ag.Run(context.Background(), "Reply with exactly: TELEMETRY_OK"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = bus.Emit(&ext.Event{Type: ext.EventSessionShutdown, Reason: "e2e"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("lines = %d, want at least 2:\n%s", len(lines), data)
	}
	if !strings.Contains(string(data), "pi.harness.session") {
		t.Errorf("the file has no session span:\n%s", data)
	}
	if !strings.Contains(string(data), "pi.harness.request") {
		t.Errorf("the file has no request span:\n%s", data)
	}
	// Every line is a JSON object.
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			t.Errorf("line %d is not a JSON object: %q", i+1, line)
		}
	}
}
