package telemetrybackend

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/mimi/auto-harness/telemetry"
)

// The OTLP JSON payload types.
//
// The field names are the protobuf JSON names (lowerCamelCase), and the
// integer types are the spec's: 64-bit integers are decimal strings,
// and trace and span identifiers are hex strings.

// otlpAnyValue is a protobuf AnyValue: one of the scalar kinds.
//
// The union is a set of optional pointers, so an absent kind is absent
// from the JSON rather than a zero value.
type otlpAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	IntValue    *int64          `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
}

// otlpArrayValue is a list of AnyValues.
type otlpArrayValue struct {
	Values []otlpAnyValue `json:"values"`
}

// otlpKeyValue is one attribute.
type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpInstrumentationScope identifies the producer of the data.
type otlpInstrumentationScope struct {
	Name       string         `json:"name"`
	Version    string         `json:"version,omitempty"`
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpResource describes the entity producing the data.
type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpResourceSpans groups spans by resource.
type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
	SchemaURL  string           `json:"schemaUrl,omitempty"`
}

// otlpScopeSpans groups spans by instrumentation scope.
type otlpScopeSpans struct {
	Scope     otlpInstrumentationScope `json:"scope"`
	Spans     []otlpSpan               `json:"spans"`
	SchemaURL string                   `json:"schemaUrl,omitempty"`
}

// otlpSpan is one span.
type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	TraceState        string         `json:"traceState,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Events            []otlpEvent    `json:"events,omitempty"`
	Links             []otlpLink     `json:"links,omitempty"`
	Status            otlpStatus     `json:"status"`
}

// otlpEvent is a point-in-time event on a span.
type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpLink relates a span to another.
type otlpLink struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	TraceState string         `json:"traceState,omitempty"`
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

// otlpStatus is a span's terminal status.
type otlpStatus struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"code"`
}

// The OTLP status codes.
const (
	otlpStatusUnset = 0
	otlpStatusOK    = 1
	otlpStatusError = 2
)

// The OTLP span kinds. The harness's spans are all internal work.
const otlpSpanKindInternal = 1

// otlpTracesData is the payload of the traces endpoint.
type otlpTracesData struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// The OTLP metrics payload types.

// otlpMetricsData is the payload of the metrics endpoint.
type otlpMetricsData struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

// otlpResourceMetrics groups metrics by resource.
type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
	SchemaURL    string             `json:"schemaUrl,omitempty"`
}

// otlpScopeMetrics groups metrics by instrumentation scope.
type otlpScopeMetrics struct {
	Scope     otlpInstrumentationScope `json:"scope"`
	Metrics   []otlpMetric             `json:"metrics"`
	SchemaURL string                   `json:"schemaUrl,omitempty"`
}

// otlpMetric is one metric.
type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

// otlpSum is a scalar sum.
type otlpSum struct {
	DataPoints             []otlpNumberDataPoint `json:"dataPoints"`
	AggregationTemporality int                   `json:"aggregationTemporality"`
	IsMonotonic            bool                  `json:"isMonotonic"`
}

// otlpHistogram is a distribution.
type otlpHistogram struct {
	DataPoints             []otlpHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                      `json:"aggregationTemporality"`
}

// otlpNumberDataPoint is one scalar point.
type otlpNumberDataPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsDouble          float64        `json:"asDouble"`
}

// otlpHistogramDataPoint is one distribution point.
type otlpHistogramDataPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             uint64         `json:"count"`
	Sum               float64        `json:"sum,omitempty"`
	BucketCounts      []uint64       `json:"bucketCounts,omitempty"`
	ExplicitBounds    []float64      `json:"explicitBounds,omitempty"`
	Min               *float64       `json:"min,omitempty"`
	Max               *float64       `json:"max,omitempty"`
}

// The OTLP aggregation temporalities.
const (
	otlpTemporalityDelta      = 1
	otlpTemporalityCumulative = 2
)

// encodeTraces renders a batch of spans as an OTLP traces payload.
func encodeTraces(spans []telemetry.StoredSpan, resource Resource, scope Scope) ([]byte, error) {
	encoded := make([]otlpSpan, 0, len(spans))
	for _, span := range spans {
		encoded = append(encoded, encodeSpan(span))
	}
	data := otlpTracesData{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{Attributes: resourceAttributes(resource)},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpInstrumentationScope{Name: scope.Name, Version: scope.Version},
				Spans: encoded,
			}},
		}},
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("telemetrybackend: encode traces: %w", err)
	}
	return out, nil
}

// encodeSpan renders one span.
func encodeSpan(span telemetry.StoredSpan) otlpSpan {
	out := otlpSpan{
		TraceID:           hexID(span.TraceID, 16),
		SpanID:            hexID(span.ID, 8),
		ParentSpanID:      hexID(span.ParentID, 8),
		Name:              span.Name,
		Kind:              otlpSpanKindInternal,
		StartTimeUnixNano: strconv.FormatInt(millisToNanos(span.StartedMs), 10),
		EndTimeUnixNano:   strconv.FormatInt(millisToNanos(span.EndedMs), 10),
		Attributes:        encodeAttributes(span.Attributes),
		Status:            encodeStatus(span.Status),
	}
	for _, event := range span.Events {
		out.Events = append(out.Events, otlpEvent{
			TimeUnixNano: strconv.FormatInt(millisToNanos(span.StartedMs), 10),
			Name:         event.Name,
			Attributes:   encodeAttributes(event.Attributes),
		})
	}
	for _, link := range span.Links {
		out.Links = append(out.Links, otlpLink{
			TraceID:    hexID(link.TraceID, 16),
			SpanID:     hexID(link.SpanID, 8),
			Attributes: encodeAttributes(link.Attributes),
		})
	}
	return out
}

// encodeStatus renders a span's status.
func encodeStatus(status telemetry.SpanStatus) otlpStatus {
	switch status.Code {
	case telemetry.StatusOK:
		return otlpStatus{Code: otlpStatusOK}
	case telemetry.StatusError:
		out := otlpStatus{Code: otlpStatusError}
		if status.Error != nil {
			out.Message = status.Error.Message
		}
		return out
	default:
		return otlpStatus{Code: otlpStatusUnset}
	}
}

// encodeMetrics renders a batch of metric points as an OTLP metrics
// payload.
//
// Counters become sums and histograms become histograms; an up-down
// counter is a non-monotonic sum.
func encodeMetrics(points []MetricPoint, resource Resource, scope Scope) ([]byte, error) {
	metrics := make([]otlpMetric, 0, len(points))
	for _, point := range points {
		metric, ok := encodeMetricPoint(point)
		if !ok {
			continue
		}
		metrics = append(metrics, metric)
	}
	data := otlpMetricsData{
		ResourceMetrics: []otlpResourceMetrics{{
			Resource: otlpResource{Attributes: resourceAttributes(resource)},
			ScopeMetrics: []otlpScopeMetrics{{
				Scope:   otlpInstrumentationScope{Name: scope.Name, Version: scope.Version},
				Metrics: metrics,
			}},
		}},
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("telemetrybackend: encode metrics: %w", err)
	}
	return out, nil
}

// encodeMetricPoint renders one metric point.
//
// A point with neither a value nor buckets is skipped: a collector
// would reject it.
func encodeMetricPoint(point MetricPoint) (otlpMetric, bool) {
	metric := otlpMetric{
		Name:        point.Name,
		Description: point.Description,
		Unit:        point.Unit,
	}
	switch point.Kind {
	case telemetry.MetricHistogram:
		if len(point.Bounds) == 0 || len(point.BucketCounts) != len(point.Bounds)+1 {
			return otlpMetric{}, false
		}
		dataPoint := otlpHistogramDataPoint{
			Attributes:        encodeStringAttributes(point.Attributes),
			StartTimeUnixNano: strconv.FormatInt(point.StartTime.UnixNano(), 10),
			TimeUnixNano:      strconv.FormatInt(point.Time.UnixNano(), 10),
			Count:             uint64(point.Count),
			Sum:               point.Sum,
			BucketCounts:      point.BucketCounts,
			ExplicitBounds:    point.Bounds,
		}
		if point.Count > 0 {
			min, max := point.Min, point.Max
			dataPoint.Min = &min
			dataPoint.Max = &max
		}
		metric.Histogram = &otlpHistogram{
			DataPoints:             []otlpHistogramDataPoint{dataPoint},
			AggregationTemporality: otlpTemporalityCumulative,
		}
		return metric, true
	case telemetry.MetricCounter, telemetry.MetricUpDownCounter:
		metric.Sum = &otlpSum{
			DataPoints: []otlpNumberDataPoint{{
				Attributes:        encodeStringAttributes(point.Attributes),
				StartTimeUnixNano: strconv.FormatInt(point.StartTime.UnixNano(), 10),
				TimeUnixNano:      strconv.FormatInt(point.Time.UnixNano(), 10),
				AsDouble:          point.Value,
			}},
			AggregationTemporality: otlpTemporalityCumulative,
			IsMonotonic:            point.Kind == telemetry.MetricCounter,
		}
		return metric, true
	default:
		return otlpMetric{}, false
	}
}

// resourceAttributes renders the resource's attributes.
//
// The service attributes come first, in the order OpenTelemetry
// documents them.
func resourceAttributes(resource Resource) []otlpKeyValue {
	attrs := []otlpKeyValue{}
	if resource.Name != "" {
		attrs = append(attrs, otlpKeyValue{
			Key:   "service.name",
			Value: otlpAnyValue{StringValue: strPtr(resource.Name)},
		})
	}
	if resource.Version != "" {
		attrs = append(attrs, otlpKeyValue{
			Key:   "service.version",
			Value: otlpAnyValue{StringValue: strPtr(resource.Version)},
		})
	}
	for _, key := range sortedKeys(resource.Attributes) {
		attrs = append(attrs, otlpKeyValue{
			Key:   key,
			Value: otlpAnyValue{StringValue: strPtr(resource.Attributes[key])},
		})
	}
	return attrs
}

// encodeAttributes renders a span's attributes.
//
// The contract allows strings, numbers, booleans and arrays of those;
// OTLP's AnyValue covers the same set. A value of another kind is
// dropped rather than guessed at.
func encodeAttributes(attrs telemetry.SpanAttributes) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, 0, len(attrs))
	for _, key := range sortedAttrKeys(attrs) {
		value, ok := encodeAnyValue(attrs[key])
		if !ok {
			continue
		}
		out = append(out, otlpKeyValue{Key: key, Value: value})
	}
	return out
}

// encodeStringAttributes renders a metric point's attributes, which are
// always strings.
func encodeStringAttributes(attrs map[string]string) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, 0, len(attrs))
	for _, key := range sortedKeys(attrs) {
		out = append(out, otlpKeyValue{
			Key:   key,
			Value: otlpAnyValue{StringValue: strPtr(attrs[key])},
		})
	}
	return out
}

// encodeAnyValue converts one contract attribute value into an OTLP
// AnyValue.
func encodeAnyValue(value telemetry.AttributeValue) (otlpAnyValue, bool) {
	switch v := value.(type) {
	case string:
		return otlpAnyValue{StringValue: strPtr(v)}, true
	case bool:
		return otlpAnyValue{BoolValue: &v}, true
	case int:
		return otlpAnyValue{IntValue: intPtr(int64(v))}, true
	case int32:
		return otlpAnyValue{IntValue: intPtr(int64(v))}, true
	case int64:
		return otlpAnyValue{IntValue: intPtr(v)}, true
	case uint:
		return otlpAnyValue{IntValue: intPtr(int64(v))}, true
	case uint32:
		return otlpAnyValue{IntValue: intPtr(int64(v))}, true
	case uint64:
		return otlpAnyValue{IntValue: intPtr(int64(v))}, true
	case float32:
		return otlpAnyValue{DoubleValue: floatPtr(float64(v))}, true
	case float64:
		return otlpAnyValue{DoubleValue: floatPtr(v)}, true
	case []string:
		array := otlpArrayValue{Values: make([]otlpAnyValue, 0, len(v))}
		for _, item := range v {
			array.Values = append(array.Values, otlpAnyValue{StringValue: strPtr(item)})
		}
		return otlpAnyValue{ArrayValue: &array}, true
	case []bool:
		array := otlpArrayValue{Values: make([]otlpAnyValue, 0, len(v))}
		for _, item := range v {
			item := item
			array.Values = append(array.Values, otlpAnyValue{BoolValue: &item})
		}
		return otlpAnyValue{ArrayValue: &array}, true
	case []int:
		array := otlpArrayValue{Values: make([]otlpAnyValue, 0, len(v))}
		for _, item := range v {
			array.Values = append(array.Values, otlpAnyValue{IntValue: intPtr(int64(item))})
		}
		return otlpAnyValue{ArrayValue: &array}, true
	case []int64:
		array := otlpArrayValue{Values: make([]otlpAnyValue, 0, len(v))}
		for _, item := range v {
			array.Values = append(array.Values, otlpAnyValue{IntValue: intPtr(item)})
		}
		return otlpAnyValue{ArrayValue: &array}, true
	case []float64:
		array := otlpArrayValue{Values: make([]otlpAnyValue, 0, len(v))}
		for _, item := range v {
			array.Values = append(array.Values, otlpAnyValue{DoubleValue: floatPtr(item)})
		}
		return otlpAnyValue{ArrayValue: &array}, true
	default:
		return otlpAnyValue{}, false
	}
}

// hexID renders an identifier as the hex string OTLP wants.
//
// The contract's identifiers are opaque strings, not the 16-byte and
// 8-byte hex the spec requires. A collector rejects a trace or span id
// of the wrong length, so an identifier that is not already hex is
// hashed into one of the right size.
//
// A hashed identifier is stable: the same input always yields the same
// id, so the same span keeps its identity across a run and a trace
// keeps its shape.
func hexID(id string, size int) string {
	if id == "" {
		return ""
	}
	if decoded, err := hex.DecodeString(id); err == nil && len(decoded) == size {
		return id
	}
	return hashToHex(id, size)
}

// hashToHex hashes an identifier into a hex string of the given byte
// size.
func hashToHex(id string, size int) string {
	sum := fnv128a(id)
	return hex.EncodeToString(sum[:size])
}

// fnv128a computes the FNV-1a 128-bit hash of s.
//
// FNV-1a is not cryptographic, and does not need to be: the input is an
// opaque identifier, and the requirement is a stable one-to-one-ish
// mapping into the right byte width.
func fnv128a(s string) [16]byte {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	// The 128-bit variant uses two independent 64-bit lanes.
	var high, low uint64 = offset64, offset64
	for i := 0; i < len(s); i++ {
		low ^= uint64(s[i])
		low *= prime64
		high ^= uint64(s[i]) + uint64(low)
		high *= prime64
	}
	var out [16]byte
	for i := 0; i < 8; i++ {
		out[i] = byte(low >> (8 * i))
		out[8+i] = byte(high >> (8 * i))
	}
	return out
}

// millisToNanos converts epoch milliseconds to nanoseconds.
func millisToNanos(ms float64) int64 {
	return int64(ms * float64(time.Millisecond))
}

// strPtr returns a pointer to a string.
func strPtr(s string) *string { return &s }

// intPtr returns a pointer to an int64.
func intPtr(v int64) *int64 { return &v }

// floatPtr returns a pointer to a float64.
func floatPtr(v float64) *float64 { return &v }
