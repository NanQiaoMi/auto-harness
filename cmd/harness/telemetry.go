package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/telemetry"
	"github.com/NanQiaoMi/auto-harness/telemetrybackend"
	"github.com/NanQiaoMi/auto-harness/telemetryext"
)

// telemetrySetup wires the telemetry collector into a run.
//
// Telemetry is opt-in: a run without it records nothing, and the kernel
// does not know it exists. The collector is an extension like any other,
// so it survives a reload.
//
// The spans go to the file exporter, to the OTLP backend, to the
// Prometheus backend, or to any combination. The two remote backends
// are separate: Prometheus stores metrics, not spans, so a run that
// ships to Prometheus still sends its spans to OTLP.
type telemetrySetup struct {
	// collector is the extension, nil when telemetry is off.
	collector *telemetryext.Collector
	// recorder holds the spans of the run.
	recorder *telemetry.MemoryContext
	// exporter writes the spans to the JSONL file.
	exporter *telemetry.Exporter
	// backend ships the spans to an OTLP collector.
	backend *telemetrybackend.Exporter
	// prometheus ships the metrics to a Prometheus server.
	prometheus *telemetrybackend.Exporter
	// path is the file the spans go to.
	path string
	// endpoint is the OTLP collector the spans go to.
	endpoint string
	// prometheusEndpoint is the Prometheus server the metrics go to.
	prometheusEndpoint string
}

// loadTelemetry prepares the telemetry collector.
//
// It returns a setup with no collector when telemetry is off, which is
// the default. The caller loads the collector into the extension
// manager.
func loadTelemetry(cfg *config.Config, provider, model string) *telemetrySetup {
	if !cfg.Telemetry {
		return &telemetrySetup{}
	}

	setup := &telemetrySetup{}
	recorder := telemetry.NewMemoryContext()
	setup.recorder = recorder

	// The file exporter: the local, always-available record.
	path := cfg.TelemetryPath
	if path == "" {
		path = filepath.Join(config.Dir(), "telemetry.jsonl")
	}
	setup.path = path

	var fileExporter *telemetry.Exporter
	if cfg.TelemetryFile == nil || *cfg.TelemetryFile {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
		} else {
			exporter, err := telemetry.NewFileExporter(path, telemetry.ExporterOptions{Redact: true})
			if err != nil {
				fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
			} else {
				fileExporter = exporter
				setup.exporter = exporter
			}
		}
	}

	// The OTLP backend: the remote collector.
	var backendExporter *telemetrybackend.Exporter
	if cfg.TelemetryEndpoint != "" {
		backend, err := telemetrybackend.NewOTLP(telemetrybackend.Options{
			Endpoint: cfg.TelemetryEndpoint,
			Headers:  cfg.TelemetryHeaders,
			Timeout:  cfg.TelemetryTimeout(),
			Redact:   true,
			Resource: telemetrybackend.Resource{
				Name:    "auto-harness",
				Version: telemetrybackend.Version,
				Attributes: map[string]string{
					"service.instance.id": cfg.SessionDir,
				},
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
		} else {
			backendExporter = telemetrybackend.NewExporter(backend, telemetry.Schema,
				cfg.TelemetryInterval(), func(format string, args ...any) {
					fmt.Fprintf(os.Stderr, "[telemetry] "+format+"\n", args...)
				})
			backendExporter.Start()
			setup.backend = backendExporter
			setup.endpoint = cfg.TelemetryEndpoint
		}
	}

	// The Prometheus backend: the metrics go to a Prometheus server as
	// time series. Prometheus has no traces, so the spans do not go
	// there.
	var prometheusExporter *telemetrybackend.Exporter
	if cfg.TelemetryPrometheusEndpoint != "" {
		backend, err := telemetrybackend.NewPrometheus(telemetrybackend.Options{
			Endpoint: cfg.TelemetryPrometheusEndpoint,
			Headers:  cfg.TelemetryPrometheusHeaders,
			Timeout:  cfg.TelemetryTimeout(),
			Redact:   true,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
		} else {
			prometheusExporter = telemetrybackend.NewExporter(backend, telemetry.Schema,
				cfg.TelemetryInterval(), func(format string, args ...any) {
					fmt.Fprintf(os.Stderr, "[telemetry] "+format+"\n", args...)
				})
			prometheusExporter.Start()
			setup.prometheus = prometheusExporter
			setup.prometheusEndpoint = cfg.TelemetryPrometheusEndpoint
		}
	}

	// The span sink: the file first, so a run that dies still leaves
	// its spans behind, then the backend.
	recorder.SetSink(func(span telemetry.StoredSpan) {
		if fileExporter != nil {
			if err := fileExporter.WriteSpan(span); err != nil {
				fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
			}
		}
		if backendExporter != nil {
			backendExporter.SpanSink()(span)
		}
	})
	// The measurement sink: the backends aggregate the measurements.
	recorder.SetMeasurementSink(func(meas telemetry.RecordedMeasurement) {
		if backendExporter != nil {
			backendExporter.RecordMeasurement(meas)
		}
		if prometheusExporter != nil {
			prometheusExporter.RecordMeasurement(meas)
		}
	})

	collector := telemetryext.New(recorder, provider, model)
	collector.Logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[telemetry] "+format+"\n", args...)
	}
	setup.collector = collector

	return setup
}

// Close releases the exporters.
func (t *telemetrySetup) Close() {
	if t == nil {
		return
	}
	if t.backend != nil {
		if err := t.backend.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
		}
	}
	if t.prometheus != nil {
		if err := t.prometheus.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
		}
	}
	if t.exporter != nil {
		if err := t.exporter.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] telemetry: %v\n", err)
		}
	}
}

// report prints the summary the /telemetry command shows.
func (t *telemetrySetup) report() {
	if t == nil || t.collector == nil {
		fmt.Println("telemetry is off (set \"telemetry\": true in ~/.harness/config.json)")
		return
	}
	fmt.Println("telemetry is on")
	if t.path != "" {
		fmt.Printf("  file:     %s\n", t.path)
	}
	if t.endpoint != "" {
		fmt.Printf("  endpoint: %s\n", t.endpoint)
	}
	if t.prometheusEndpoint != "" {
		fmt.Printf("  prometheus: %s\n", t.prometheusEndpoint)
	}
	if t.recorder != nil {
		spans := t.recorder.GetSpans()
		fmt.Printf("  spans:    %d\n", len(spans))
		for _, s := range spans {
			fmt.Printf("    %-22s %s\n", s.Name, s.Status.Code)
		}
	}
	if t.backend != nil {
		fmt.Printf("  shipped:  %d spans, %d metric batches\n", t.backend.Sent(), t.backend.Flushed())
	}
	if t.prometheus != nil {
		fmt.Printf("  shipped:  %d metric batches to Prometheus\n", t.prometheus.Flushed())
	}
}

// load installs the collector into the extension manager.
func (t *telemetrySetup) load(mgr *ext.Manager) {
	if t == nil || t.collector == nil {
		return
	}
	if err := mgr.Load(t.collector); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
	}
}
