package telemetry

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Exporter writes settled spans to a stream.
//
// pi's telemetry package deliberately ships no exporter: the host
// application chooses a backend. The harness ships one anyway, because
// a local JSONL file is the most useful thing a self-hosted agent can
// do with its own telemetry — and it stays opt-in.
//
// The wire format is one JSON object per line.
type Exporter struct {
	mu sync.Mutex
	// out is where the lines go.
	out io.Writer
	// closer closes the underlying file, when there is one.
	closer io.Closer
	// count is the number of spans written.
	count int
	// redact drops attributes the schema marks sensitive.
	redact bool
}

// ExporterOptions configures an Exporter.
type ExporterOptions struct {
	// Redact drops the attributes the schema marks sensitive, and
	// replaces the values of high-cardinality attributes with a
	// placeholder. It is on by default: telemetry is diagnostics, and
	// an attribute bag is the easiest place to leak a prompt.
	Redact bool
}

// NewExporter writes spans to w.
func NewExporter(w io.Writer, opts ExporterOptions) *Exporter {
	return &Exporter{out: w, redact: opts.Redact}
}

// NewFileExporter opens path for appending and writes spans to it.
func NewFileExporter(path string, opts ExporterOptions) (*Exporter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Exporter{out: f, closer: f, redact: opts.Redact}, nil
}

// exporterRecord is one line of the export.
type exporterRecord struct {
	// TraceID groups the spans of one trace.
	TraceID string `json:"traceId"`
	// SpanID is the span's own identifier.
	SpanID string `json:"spanId"`
	// ParentID is the enclosing span, empty for a root.
	ParentID string `json:"parentId,omitempty"`
	// Name is the span's name.
	Name string `json:"name"`
	// StartedMs and EndedMs are epoch milliseconds.
	StartedMs float64 `json:"startedMs"`
	EndedMs   float64 `json:"endedMs"`
	// Attributes are the span's merged attributes.
	Attributes SpanAttributes `json:"attributes,omitempty"`
	// Events are the span's point-in-time events.
	Events []StoredEvent `json:"events,omitempty"`
	// Status is the span's terminal status.
	Status spanStatusJSON `json:"status"`
}

// spanStatusJSON is the on-disk form of a status.
//
// The status code is a plain string under "status", the way pi's
// contract spells it, and the error is its object.
type spanStatusJSON struct {
	Code  string     `json:"status"`
	Error *SpanError `json:"error,omitempty"`
}

// statusJSON converts a status to its on-disk form.
func statusJSON(status SpanStatus) spanStatusJSON {
	return spanStatusJSON{Code: string(status.Code), Error: status.Error}
}

// WriteSpan writes one settled span.
//
// The span's name must be one the schema declares; an unknown name is
// written as-is, since the exporter does not validate.
func (e *Exporter) WriteSpan(span StoredSpan) error {
	record := exporterRecord{
		TraceID:    span.TraceID,
		SpanID:     span.ID,
		ParentID:   span.ParentID,
		Name:       span.Name,
		StartedMs:  span.StartedMs,
		EndedMs:    span.EndedMs,
		Attributes: e.filterAttributes(span.Name, span.Attributes),
		Events:     span.Events,
		Status:     statusJSON(span.Status),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("telemetry: export span %q: %w", span.Name, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.out.Write(append(data, '\n')); err != nil {
		return err
	}
	e.count++
	return nil
}

// filterAttributes drops the attributes the redaction policy covers.
func (e *Exporter) filterAttributes(name string, attrs SpanAttributes) SpanAttributes {
	if !e.redact || len(attrs) == 0 {
		return attrs
	}
	definition, ok := Schema.Spans[name]
	if !ok {
		return attrs
	}
	out := make(SpanAttributes, len(attrs))
	for key, value := range attrs {
		if attr, ok := definition.EndAttributes[key]; ok && attr.Sensitive {
			continue
		}
		if attr, ok := definition.StartAttributes[key]; ok && attr.Sensitive {
			continue
		}
		out[key] = value
	}
	return out
}

// WriteTrace writes every span of a trace.
func (e *Exporter) WriteTrace(trace StoredTrace) error {
	for _, span := range trace.Spans {
		if err := e.WriteSpan(span); err != nil {
			return err
		}
	}
	return nil
}

// Count returns the number of spans written.
func (e *Exporter) Count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count
}

// Close flushes and closes the underlying file, when there is one.
func (e *Exporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closer == nil {
		return nil
	}
	return e.closer.Close()
}
