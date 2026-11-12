package telemetrybackend

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mimi/auto-harness/telemetry"
)

// Aggregator turns raw measurements into the aggregate points a
// collector wants.
//
// A collector wants a rate or a distribution per interval, not every
// raw measurement: shipping one point per measurement would be a
// request per tool call. The aggregator groups the measurements by
// name and attribute set, and sums the counters and buckets the
// histograms.
//
// The aggregator is the piece that turns the contract's metrics into
// OTLP's temporality model. It reports cumulative values, which is
// what a collector that restarts rarely wants; a delta reporter would
// need to remember the last value of every series, which does not
// survive a restart.
type Aggregator struct {
	mu sync.Mutex
	// series holds the aggregate of each metric series.
	series map[string]*aggregate
	// definitions carries the schema's metadata, so a point knows its
	// unit and description.
	definitions map[string]telemetry.MetricDefinition
	// now is the clock.
	now func() time.Time
	// interval is how long an aggregate covers.
	interval time.Duration
	// started is when the current interval began.
	started time.Time
}

// aggregate is one metric series' running total.
type aggregate struct {
	name        string
	kind        telemetry.MetricKind
	attributes  map[string]string
	bounds      []float64
	bucketCount []uint64
	count       int
	sum         float64
	min         float64
	max         float64
	first       time.Time
	last        time.Time
}

// NewAggregator creates an aggregator over the schema's metric
// definitions.
//
// The definitions give each instrument its unit, description and
// histogram boundaries; an instrument the schema does not define is
// still aggregated, with no metadata.
func NewAggregator(schema telemetry.SchemaDefinition, interval time.Duration) *Aggregator {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	definitions := map[string]telemetry.MetricDefinition{}
	for name, definition := range schema.Metrics {
		definitions[name] = definition
	}
	now := time.Now
	return &Aggregator{
		series:      map[string]*aggregate{},
		definitions: definitions,
		now:         now,
		interval:    interval,
		started:     now(),
	}
}

// SetClock replaces the aggregator's clock. Tests use it.
func (a *Aggregator) SetClock(now func() time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now != nil {
		a.now = now
	}
}

// Record adds one measurement.
//
// The kind comes from the schema: an instrument the schema declares as
// a histogram is bucketed, everything else is summed.
func (a *Aggregator) Record(name string, value float64, attributes map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	definition, known := a.definitions[name]
	kind := telemetry.MetricCounter
	var bounds []float64
	if known {
		kind = definition.Kind
		bounds = definition.Boundaries
	}

	key := seriesKey(name, attributes)
	agg, ok := a.series[key]
	if !ok {
		agg = &aggregate{
			name:       name,
			kind:       kind,
			attributes: copyAttributes(attributes),
			bounds:     bounds,
			min:        value,
			max:        value,
			first:      a.now(),
		}
		if kind == telemetry.MetricHistogram {
			agg.bucketCount = make([]uint64, len(bounds)+1)
		}
		a.series[key] = agg
	}
	if agg.kind == telemetry.MetricHistogram {
		agg.bucketCount[bucketIndex(bounds, value)]++
	}
	agg.count++
	agg.sum += value
	if value < agg.min {
		agg.min = value
	}
	if value > agg.max {
		agg.max = value
	}
	agg.last = a.now()
}

// RecordMeasurement adds one measurement the recorder captured.
func (a *Aggregator) RecordMeasurement(meas telemetry.RecordedMeasurement) {
	attrs := make(map[string]string, len(meas.Attributes))
	for key, value := range meas.Attributes {
		attrs[key] = attributeText(value)
	}
	a.Record(meas.Name, meas.Value, attrs)
}

// Points returns the current aggregates and resets them.
//
// The reset is what makes the temporality delta: the next round
// reports only what happened since this one. The values are
// cumulative within the round, which is what OTLP's cumulative
// temporality means to a collector that receives one batch per round.
func (a *Aggregator) Points() []MetricPoint {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]MetricPoint, 0, len(a.series))
	now := a.now()
	for _, agg := range a.series {
		definition := a.definitions[agg.name]
		point := MetricPoint{
			Name:        agg.name,
			Description: definition.Description,
			Unit:        definition.Unit,
			Kind:        agg.kind,
			Count:       agg.count,
			Sum:         agg.sum,
			Min:         agg.min,
			Max:         agg.max,
			Attributes:  agg.attributes,
			StartTime:   agg.first,
			Time:        now,
		}
		if agg.kind == telemetry.MetricHistogram {
			point.Bounds = agg.bounds
			point.BucketCounts = agg.bucketCount
		} else {
			point.Value = agg.sum
		}
		out = append(out, point)
	}

	// Start the next round.
	a.series = map[string]*aggregate{}
	a.started = now

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return seriesKey(out[i].Name, out[i].Attributes) < seriesKey(out[j].Name, out[j].Attributes)
	})
	return out
}

// Pending reports how many series the aggregator holds.
func (a *Aggregator) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.series)
}

// bucketIndex returns the bucket a value falls into.
//
// The buckets are the schema's boundaries: the first bucket holds
// everything up to the first boundary, and the last holds everything
// above the last one.
func bucketIndex(bounds []float64, value float64) int {
	index := sort.SearchFloat64s(bounds, value)
	if index >= len(bounds) {
		return len(bounds)
	}
	return index
}

// seriesKey renders a name and its attributes as a key.
//
// The attributes are sorted, so the same set always yields the same
// key regardless of map order.
func seriesKey(name string, attributes map[string]string) string {
	keys := sortedKeys(attributes)
	key := name
	for _, k := range keys {
		key += "|" + k + "=" + attributes[k]
	}
	return key
}

// copyAttributes copies an attribute map.
func copyAttributes(attrs map[string]string) map[string]string {
	if attrs == nil {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for key, value := range attrs {
		out[key] = value
	}
	return out
}

// attributeText renders an attribute value as text.
//
// The contract allows numbers and booleans as well as strings; a
// metric's attribute set is a string map, so the value is rendered.
func attributeText(value telemetry.AttributeValue) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(v)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return ""
	}
}
