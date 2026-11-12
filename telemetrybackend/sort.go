package telemetrybackend

import (
	"sort"

	"github.com/mimi/auto-harness/telemetry"
)

// sortedKeys returns a map's keys in order.
//
// The OTLP payload is compared byte for byte in tests and diffed by
// humans in a collector, so the attribute order is stable rather than
// the random order a Go map iteration gives.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// sortedAttrKeys returns an attribute map's keys in order.
func sortedAttrKeys(m telemetry.SpanAttributes) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
