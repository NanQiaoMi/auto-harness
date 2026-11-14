package telemetrybackend

import (
	"github.com/NanQiaoMi/auto-harness/telemetry"
)

// redactSpans drops the attributes the schema marks sensitive.
//
// The harness's own schema marks nothing sensitive today — the
// collector never records content — but the policy has to be enforced
// at the point where the data leaves the process, and a schema that
// grows a sensitive attribute must be honoured without touching the
// backend.
//
// The spans are copies: the caller's data is not modified.
func redactSpans(spans []telemetry.StoredSpan) []telemetry.StoredSpan {
	out := make([]telemetry.StoredSpan, len(spans))
	for i, span := range spans {
		out[i] = span
		out[i].Attributes = redactAttributes(span.Name, span.Attributes)
		if len(span.Events) > 0 {
			events := make([]telemetry.StoredEvent, len(span.Events))
			for j, event := range span.Events {
				events[j] = event
				events[j].Attributes = redactAttributes(span.Name, event.Attributes)
			}
			out[i].Events = events
		}
	}
	return out
}

// redactAttributes drops the sensitive attributes of one span.
func redactAttributes(name string, attrs telemetry.SpanAttributes) telemetry.SpanAttributes {
	if len(attrs) == 0 {
		return attrs
	}
	definition, ok := telemetry.Schema.Spans[name]
	if !ok {
		return attrs
	}
	out := make(telemetry.SpanAttributes, len(attrs))
	for key, value := range attrs {
		if attr, ok := definition.StartAttributes[key]; ok && attr.Sensitive {
			continue
		}
		if attr, ok := definition.EndAttributes[key]; ok && attr.Sensitive {
			continue
		}
		out[key] = value
	}
	return out
}

// redactMetricPoints drops the sensitive attributes of the metric
// points.
//
// The metric attributes are declared in the schema just like the span
// attributes are, and they carry the same risk: an attribute marked
// sensitive must not leave the process, whichever protocol carries it.
//
// The points are copies: the caller's data is not modified.
func redactMetricPoints(points []MetricPoint) []MetricPoint {
	out := make([]MetricPoint, len(points))
	for i, point := range points {
		out[i] = point
		out[i].Attributes = redactMetricAttributes(point.Name, point.Attributes)
	}
	return out
}

// redactMetricAttributes drops the sensitive attributes of one metric.
func redactMetricAttributes(name string, attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return attrs
	}
	definition, ok := telemetry.Schema.Metrics[name]
	if !ok {
		return attrs
	}
	out := make(map[string]string, len(attrs))
	for key, value := range attrs {
		if attr, ok := definition.Attributes[key]; ok && attr.Sensitive {
			continue
		}
		out[key] = value
	}
	return out
}
