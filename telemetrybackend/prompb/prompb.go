// Package prompb encodes the Prometheus remote write protobuf messages.
//
// The messages are small and stable, so the encoding is written by hand
// rather than pulled in as a dependency: the harness ships no external
// modules. The wire format is protobuf's, and the tests check the
// bytes against the reference implementation, so the two cannot drift
// apart.
//
// Only the messages remote write needs are here:
//
//	WriteRequest   repeated TimeSeries timeseries = 1
//	               repeated MetricMetadata metadata = 3
//	TimeSeries     repeated Label labels         = 1
//	               repeated Sample samples       = 2
//	Label          string name                    = 1
//	               string value                   = 2
//	Sample         double value                   = 1
//	               int64 timestamp                = 2
//	MetricMetadata MetricType type                = 1
//	               string metric_family_name      = 2
//	               string help                    = 4
//	               string unit                    = 5
//
// The encoding rules are protobuf's:
//
//   - A field is a tag (the field number shifted left by three, plus
//     the wire type) followed by the value.
//   - A repeated message field is one tag-length-value group per item.
//   - A scalar is written in the wire type that matches it: varint for
//     integers and bools, 64-bit for doubles and fixed64, and
//     length-prefixed for strings and messages.
//
// Proto3 omits fields that hold their default value, so a zero
// timestamp or an empty label is not written at all.
package prompb

import (
	"encoding/binary"
	"math"
)

// The wire types of the protobuf encoding.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// The field numbers of the messages above.
const (
	fieldWriteRequestTimeseries = 1
	fieldWriteRequestMetadata   = 3
	fieldTimeSeriesLabels       = 1
	fieldTimeSeriesSamples      = 2
	fieldLabelName              = 1
	fieldLabelValue             = 2
	fieldSampleValue            = 1
	fieldSampleTimestamp        = 2
	fieldMetadataType           = 1
	fieldMetadataFamilyName     = 2
	fieldMetadataHelp           = 4
	fieldMetadataUnit           = 5
)

// MetricType is the kind of a metric family.
//
// The values are the ones Prometheus's own metadata uses.
type MetricType int32

// The metric types Prometheus knows.
const (
	// MetricTypeUnknown is an untyped metric.
	MetricTypeUnknown MetricType = 0
	// MetricTypeCounter is a monotonically increasing counter.
	MetricTypeCounter MetricType = 1
	// MetricTypeGauge is a value that goes up and down.
	MetricTypeGauge MetricType = 2
	// MetricTypeHistogram is a distribution.
	MetricTypeHistogram MetricType = 3
)

// Label is one label of a time series.
type Label struct {
	// Name is the label's name.
	Name string
	// Value is the label's value.
	Value string
}

// Sample is one data point of a time series.
type Sample struct {
	// Value is the sample's value.
	Value float64
	// Timestamp is when the sample was taken, in milliseconds since
	// the epoch.
	Timestamp int64
}

// TimeSeries is a labeled series of samples.
type TimeSeries struct {
	// Labels identify the series. The metric's name is the "__name__"
	// label.
	Labels []Label
	// Samples are the series' data points.
	Samples []Sample
}

// WriteRequest is the body of a remote write request.
type WriteRequest struct {
	// TimeSeries are the series the request carries.
	TimeSeries []TimeSeries
	// Metadata describes the metric families the series belong to.
	Metadata []MetricMetadata
}

// MetricMetadata describes one metric family.
//
// A collector shows the metadata as the metric's help text and unit,
// and uses the type to decide how to display it.
type MetricMetadata struct {
	// Type is the family's type.
	Type MetricType
	// MetricFamilyName is the family's name, without the suffixes
	// Prometheus's own naming convention adds.
	MetricFamilyName string
	// Help is the family's description.
	Help string
	// Unit is the family's unit.
	Unit string
}

// Marshal encodes the write request.
//
// The result is the protobuf encoding of the message, before the
// snappy compression.
func (w *WriteRequest) Marshal() []byte {
	var out []byte
	for _, series := range w.TimeSeries {
		out = appendMessage(out, fieldWriteRequestTimeseries, series.marshal())
	}
	for _, metadata := range w.Metadata {
		out = appendMessage(out, fieldWriteRequestMetadata, metadata.marshal())
	}
	return out
}

// marshal encodes one metric metadata.
func (m *MetricMetadata) marshal() []byte {
	var out []byte
	// A zero type is the default, and proto3 omits it.
	if m.Type != 0 {
		out = appendVarintField(out, fieldMetadataType, uint64(m.Type))
	}
	out = appendString(out, fieldMetadataFamilyName, m.MetricFamilyName)
	out = appendString(out, fieldMetadataHelp, m.Help)
	out = appendString(out, fieldMetadataUnit, m.Unit)
	return out
}

// marshal encodes one time series.
func (t *TimeSeries) marshal() []byte {
	var out []byte
	for _, label := range t.Labels {
		out = appendMessage(out, fieldTimeSeriesLabels, label.marshal())
	}
	for _, sample := range t.Samples {
		out = appendMessage(out, fieldTimeSeriesSamples, sample.marshal())
	}
	return out
}

// marshal encodes one label.
func (l *Label) marshal() []byte {
	var out []byte
	out = appendString(out, fieldLabelName, l.Name)
	out = appendString(out, fieldLabelValue, l.Value)
	return out
}

// marshal encodes one sample.
func (s *Sample) marshal() []byte {
	var out []byte
	// A zero value is the default, and proto3 omits it.
	if s.Value != 0 {
		out = appendFixed64(out, fieldSampleValue, math.Float64bits(s.Value))
	}
	if s.Timestamp != 0 {
		out = appendVarintField(out, fieldSampleTimestamp, uint64(s.Timestamp))
	}
	return out
}

// appendMessage appends a length-prefixed message field.
func appendMessage(dst []byte, field int, body []byte) []byte {
	dst = appendTag(dst, field, wireBytes)
	dst = binary.AppendUvarint(dst, uint64(len(body)))
	return append(dst, body...)
}

// appendString appends a length-prefixed string field.
//
// An empty string is the default value, and proto3 omits it.
func appendString(dst []byte, field int, value string) []byte {
	if value == "" {
		return dst
	}
	dst = appendTag(dst, field, wireBytes)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

// appendFixed64 appends a 64-bit field.
func appendFixed64(dst []byte, field int, value uint64) []byte {
	dst = appendTag(dst, field, wireFixed64)
	return binary.LittleEndian.AppendUint64(dst, value)
}

// appendVarintField appends a varint field.
func appendVarintField(dst []byte, field int, value uint64) []byte {
	dst = appendTag(dst, field, wireVarint)
	return binary.AppendUvarint(dst, value)
}

// appendTag appends a field's tag.
func appendTag(dst []byte, field int, wireType int) []byte {
	return binary.AppendUvarint(dst, uint64(field)<<3|uint64(wireType))
}
