package telemetrybackend

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mimi/auto-harness/telemetry"
	"github.com/mimi/auto-harness/telemetrybackend/prompb"
	"github.com/mimi/auto-harness/telemetrybackend/snappy"
)

// The label Prometheus gives a metric its name.
const prometheusNameLabel = "__name__"

// The label that holds a histogram bucket's upper bound.
const prometheusBucketLabel = "le"

// Prometheus's metric names and label names allow only letters, digits
// and underscores, and must not start with a digit. The harness's names
// use dots ("pi.harness.request.duration"), which Prometheus does not
// accept, so they are translated.
//
// Prometheus does allow a colon in a metric name — it is reserved for
// recording rules — but not in a label name. The colon is replaced in
// both: the names the harness produces have no recording rules, and one
// rule for both is one rule to get right.
var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// PrometheusMetricName translates a contract metric name into a
// Prometheus one.
//
// Prometheus's names are restricted to letters, digits and underscores,
// so the dots of the harness's names become underscores. A leading digit
// is prefixed, since Prometheus rejects those.
//
// The translation is stable: the same contract name always yields the
// same Prometheus name, so a dashboard keeps working across runs.
func PrometheusMetricName(name string) string {
	translated := invalidNameChars.ReplaceAllString(name, "_")
	if translated == "" {
		return "unnamed_metric"
	}
	if translated[0] >= '0' && translated[0] <= '9' {
		translated = "_" + translated
	}
	return translated
}

// PrometheusLabelName translates a contract attribute name into a
// Prometheus label name.
//
// The rules are the same as the metric name's: letters, digits and
// underscores only.
func PrometheusLabelName(name string) string {
	translated := invalidNameChars.ReplaceAllString(name, "_")
	if translated == "" {
		return "unnamed_label"
	}
	if translated[0] >= '0' && translated[0] <= '9' {
		translated = "_" + translated
	}
	return translated
}

// prometheusLabels turns a contract attribute set into Prometheus
// labels.
//
// The labels are sorted, so the same attribute set always produces the
// same bytes: a remote write request is then reproducible, which makes
// it testable.
func prometheusLabels(attributes map[string]string) []prompb.Label {
	if len(attributes) == 0 {
		return nil
	}
	names := sortedKeys(attributes)
	labels := make([]prompb.Label, 0, len(names))
	for _, name := range names {
		labels = append(labels, prompb.Label{
			Name:  PrometheusLabelName(name),
			Value: attributes[name],
		})
	}
	return labels
}

// namedLabels returns the labels of a series, with the metric's name
// among them.
//
// Prometheus carries a metric's name as a label, the "__name__" one.
// The name label comes first, as Prometheus's own writers put it, and
// the attribute labels follow in order.
func namedLabels(name string, attributes map[string]string) []prompb.Label {
	labels := make([]prompb.Label, 0, len(attributes)+1)
	labels = append(labels, prompb.Label{Name: prometheusNameLabel, Value: name})
	labels = append(labels, prometheusLabels(attributes)...)
	return labels
}

// encodePrometheus turns the contract's metric points into a remote
// write request.
//
// The translation is the interesting part, because Prometheus's data
// model is not the contract's:
//
//   - A counter becomes a Prometheus counter, whose name carries the
//     "_total" suffix Prometheus expects.
//   - An up-down counter is a gauge: it can go up and down, so it is
//     not a counter.
//   - A histogram becomes one series per bucket, named "<metric>_bucket"
//     with a "le" label, plus the "_sum" and "_count" series. That is
//     the classic Prometheus histogram layout.
//
// A point with no samples is skipped: Prometheus rejects an empty
// series.
func encodePrometheus(points []MetricPoint) *prompb.WriteRequest {
	request := &prompb.WriteRequest{}

	// The series are grouped by their label set: one series per
	// distinct attribute set, with every point's value in it.
	type seriesKey struct {
		name   string
		labels string
	}
	order := []seriesKey{}
	series := map[seriesKey]*prompb.TimeSeries{}

	seriesFor := func(key seriesKey, labels []prompb.Label) *prompb.TimeSeries {
		existing, ok := series[key]
		if ok {
			return existing
		}
		existing = &prompb.TimeSeries{Labels: labels}
		series[key] = existing
		order = append(order, key)
		return existing
	}

	for _, point := range points {
		timestamp := point.Time.UnixMilli()
		base := PrometheusMetricName(point.Name)

		switch point.Kind {
		case telemetry.MetricHistogram:
			// A histogram needs its bounds and one count per bucket.
			// A point without them is not a histogram a collector
			// could use, so it is skipped.
			if len(point.Bounds) == 0 || len(point.BucketCounts) != len(point.Bounds)+1 {
				continue
			}
			// The buckets, the sum and the count.
			for i, bound := range point.Bounds {
				bucketLabels := appendLabel(namedLabels(base+"_bucket", point.Attributes), prometheusBucketLabel, formatBound(bound))
				key := seriesKey{
					name:   base + "_bucket",
					labels: labelKey(bucketLabels),
				}
				target := seriesFor(key, bucketLabels)
				target.Samples = append(target.Samples, prompb.Sample{
					Value:     float64(cumulativeCount(point.BucketCounts, i)),
					Timestamp: timestamp,
				})
			}
			// The last bucket is the overflow one: everything above
			// the last bound.
			overflowLabels := appendLabel(namedLabels(base+"_bucket", point.Attributes), prometheusBucketLabel, "+Inf")
			key := seriesKey{
				name:   base + "_bucket",
				labels: labelKey(overflowLabels),
			}
			target := seriesFor(key, overflowLabels)
			target.Samples = append(target.Samples, prompb.Sample{
				Value:     float64(cumulativeCount(point.BucketCounts, len(point.Bounds))),
				Timestamp: timestamp,
			})

			// The count is the number of observations.
			countLabels := namedLabels(base+"_count", point.Attributes)
			key = seriesKey{
				name:   base + "_count",
				labels: labelKey(countLabels),
			}
			target = seriesFor(key, countLabels)
			target.Samples = append(target.Samples, prompb.Sample{
				Value:     float64(point.Count),
				Timestamp: timestamp,
			})

			// The sum is the total of the observations.
			sumLabels := namedLabels(base+"_sum", point.Attributes)
			key = seriesKey{
				name:   base + "_sum",
				labels: labelKey(sumLabels),
			}
			target = seriesFor(key, sumLabels)
			target.Samples = append(target.Samples, prompb.Sample{
				Value:     point.Sum,
				Timestamp: timestamp,
			})

		case telemetry.MetricUpDownCounter:
			// An up-down counter can go down, so it is a gauge.
			labels := namedLabels(base, point.Attributes)
			key := seriesKey{name: base, labels: labelKey(labels)}
			target := seriesFor(key, labels)
			target.Samples = append(target.Samples, prompb.Sample{
				Value:     point.Value,
				Timestamp: timestamp,
			})

		default:
			// A counter. Prometheus expects the "_total" suffix.
			name := base
			if !strings.HasSuffix(name, "_total") {
				name += "_total"
			}
			labels := namedLabels(name, point.Attributes)
			key := seriesKey{name: name, labels: labelKey(labels)}
			target := seriesFor(key, labels)
			target.Samples = append(target.Samples, prompb.Sample{
				Value:     point.Value,
				Timestamp: timestamp,
			})
		}
	}

	// The series are emitted in a stable order.
	sort.Slice(order, func(i, j int) bool {
		if order[i].name != order[j].name {
			return order[i].name < order[j].name
		}
		return order[i].labels < order[j].labels
	})
	for _, key := range order {
		target := series[key]
		if len(target.Samples) == 0 {
			continue
		}
		request.TimeSeries = append(request.TimeSeries, *target)
	}
	request.Metadata = prometheusMetadata(points)
	return request
}

// cumulativeCount returns the number of observations at or below the
// i-th bound.
//
// The aggregator counts each observation in exactly one bucket, but a
// Prometheus bucket holds every observation up to its bound: the counts
// are cumulative.
func cumulativeCount(bucketCounts []uint64, index int) uint64 {
	var total uint64
	for i := 0; i <= index && i < len(bucketCounts); i++ {
		total += bucketCounts[i]
	}
	return total
}

// appendLabel returns the labels with one more.
//
// The extra label is the metric's name, which Prometheus carries as a
// label rather than a field.
func appendLabel(labels []prompb.Label, name, value string) []prompb.Label {
	out := make([]prompb.Label, 0, len(labels)+1)
	out = append(out, labels...)
	out = append(out, prompb.Label{Name: name, Value: value})
	return out
}

// labelKey renders a label set as a key, so two label sets can be
// compared.
func labelKey(labels []prompb.Label) string {
	var b strings.Builder
	for _, label := range labels {
		b.WriteString(label.Name)
		b.WriteString("=")
		b.WriteString(label.Value)
		b.WriteString(",")
	}
	return b.String()
}

// formatBound renders a bucket boundary the way Prometheus expects.
//
// The number is rendered the way Prometheus's own text format renders
// it, so a bucket boundary reads the same in a dashboard and in a
// scrape.
func formatBound(bound float64) string {
	return strconv.FormatFloat(bound, 'g', -1, 64)
}

// compressPrometheus encodes and compresses a remote write request.
//
// The body is the protobuf encoding of the request, snappy-compressed:
// that is what a Prometheus server expects, with the
// "application/x-protobuf" and "snappy" content types.
func compressPrometheus(request *prompb.WriteRequest) ([]byte, error) {
	encoded := request.Marshal()
	compressed, err := snappy.Encode(encoded)
	if err != nil {
		return nil, fmt.Errorf("telemetrybackend: compress: %w", err)
	}
	return compressed, nil
}
