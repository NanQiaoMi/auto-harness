package prompb

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrCorrupt reports that the input is not a valid protobuf message.
var ErrCorrupt = errors.New("prompb: corrupt input")

// The wire types the decoder handles beyond the ones the encoder
// writes.
const (
	wireFixed32 = 5
)

// Decode reads a write request.
//
// It is the inverse of Marshal: what the harness sends is what it can
// read back. The decoder is strict about the wire format, so a
// malformed message is reported rather than half-read.
func Decode(data []byte) (*WriteRequest, error) {
	request := &WriteRequest{}
	err := eachField(data, func(field int, wire int, value []byte, number uint64) error {
		switch {
		case field == fieldWriteRequestTimeseries && wire == wireBytes:
			series, err := decodeTimeSeries(value)
			if err != nil {
				return err
			}
			request.TimeSeries = append(request.TimeSeries, *series)
		case field == fieldWriteRequestMetadata && wire == wireBytes:
			metadata, err := decodeMetadata(value)
			if err != nil {
				return err
			}
			request.Metadata = append(request.Metadata, *metadata)
		default:
			// An unknown field is skipped: the protocol may grow, and
			// a reader must not break on what it does not know.
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return request, nil
}

// decodeTimeSeries reads one time series.
func decodeTimeSeries(data []byte) (*TimeSeries, error) {
	series := &TimeSeries{}
	err := eachField(data, func(field int, wire int, value []byte, number uint64) error {
		switch {
		case field == fieldTimeSeriesLabels && wire == wireBytes:
			label, err := decodeLabel(value)
			if err != nil {
				return err
			}
			series.Labels = append(series.Labels, *label)
		case field == fieldTimeSeriesSamples && wire == wireBytes:
			sample, err := decodeSample(value)
			if err != nil {
				return err
			}
			series.Samples = append(series.Samples, *sample)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return series, nil
}

// decodeLabel reads one label.
func decodeLabel(data []byte) (*Label, error) {
	label := &Label{}
	err := eachField(data, func(field int, wire int, value []byte, number uint64) error {
		switch {
		case field == fieldLabelName && wire == wireBytes:
			label.Name = string(value)
		case field == fieldLabelValue && wire == wireBytes:
			label.Value = string(value)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return label, nil
}

// decodeSample reads one sample.
func decodeSample(data []byte) (*Sample, error) {
	sample := &Sample{}
	err := eachField(data, func(field int, wire int, value []byte, number uint64) error {
		switch {
		case field == fieldSampleValue && wire == wireFixed64:
			if len(value) != 8 {
				return ErrCorrupt
			}
			sample.Value = math.Float64frombits(binary.LittleEndian.Uint64(value))
		case field == fieldSampleTimestamp && wire == wireVarint:
			sample.Timestamp = int64(number)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sample, nil
}

// decodeMetadata reads one metric metadata.
func decodeMetadata(data []byte) (*MetricMetadata, error) {
	metadata := &MetricMetadata{}
	err := eachField(data, func(field int, wire int, value []byte, number uint64) error {
		switch {
		case field == fieldMetadataType && wire == wireVarint:
			metadata.Type = MetricType(number)
		case field == fieldMetadataFamilyName && wire == wireBytes:
			metadata.MetricFamilyName = string(value)
		case field == fieldMetadataHelp && wire == wireBytes:
			metadata.Help = string(value)
		case field == fieldMetadataUnit && wire == wireBytes:
			metadata.Unit = string(value)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return metadata, nil
}

// eachField walks a protobuf message's fields.
//
// The callback gets the field number, the wire type, the value and the
// number of a varint field. A length-delimited field's value is the
// bytes themselves; the other kinds are skipped, since the messages
// here hold none.
func eachField(data []byte, fn func(field int, wire int, value []byte, number uint64) error) error {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return ErrCorrupt
		}
		data = data[n:]
		field := int(tag >> 3)
		wire := int(tag & 0x07)

		switch wire {
		case wireVarint:
			number, n := binary.Uvarint(data)
			if n <= 0 {
				return ErrCorrupt
			}
			data = data[n:]
			if err := fn(field, wire, nil, number); err != nil {
				return err
			}
		case wireFixed64:
			if len(data) < 8 {
				return ErrCorrupt
			}
			if err := fn(field, wire, data[:8], 0); err != nil {
				return err
			}
			data = data[8:]
		case wireBytes:
			length, n := binary.Uvarint(data)
			if n <= 0 {
				return ErrCorrupt
			}
			data = data[n:]
			if uint64(len(data)) < length {
				return ErrCorrupt
			}
			if err := fn(field, wire, data[:length], 0); err != nil {
				return err
			}
			data = data[length:]
		case wireFixed32:
			if len(data) < 4 {
				return ErrCorrupt
			}
			if err := fn(field, wire, data[:4], 0); err != nil {
				return err
			}
			data = data[4:]
		default:
			return ErrCorrupt
		}
	}
	return nil
}
