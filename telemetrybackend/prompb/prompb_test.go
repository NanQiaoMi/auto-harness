package prompb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// oraclePath is the reference implementation's encoder, built by the
// test from the module cache.
//
// The harness ships no external dependency, so the protobuf encoding
// here is hand-written. The reference implementation is the oracle: the
// bytes must match it exactly, because that is what a Prometheus
// server decodes.
var oraclePath string

// TestMain builds the oracle once for the whole package.
//
// The oracle is built in its own module, so the harness's go.mod keeps
// its promise of no external dependency.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "prompb-oracle")
	if err != nil {
		fmt.Fprintln(os.Stderr, "prompb: cannot make the oracle directory:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	module := `module oracle

go 1.22

require github.com/prometheus/prometheus v0.316.0
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "prompb: cannot write the oracle module:", err)
		os.Exit(1)
	}

	source := filepath.Join(dir, "main.go")
	program := `package main

import (
	"encoding/json"
	"os"

	"github.com/prometheus/prometheus/prompb"
)

type input struct {
	Series   []series   ` + "`json:\"series\"`" + `
	Metadata []metadata ` + "`json:\"metadata\"`" + `
}

type series struct {
	Labels  []label ` + "`json:\"labels\"`" + `
	Samples []sample ` + "`json:\"samples\"`" + `
}

type label struct {
	Name  string ` + "`json:\"name\"`" + `
	Value string ` + "`json:\"value\"`" + `
}

type sample struct {
	Value     float64 ` + "`json:\"value\"`" + `
	Timestamp int64   ` + "`json:\"timestamp\"`" + `
}

type metadata struct {
	Type             int32  ` + "`json:\"type\"`" + `
	MetricFamilyName string ` + "`json:\"family\"`" + `
	Help             string ` + "`json:\"help\"`" + `
	Unit             string ` + "`json:\"unit\"`" + `
}

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var in input
	if err := json.Unmarshal(data, &in); err != nil {
		panic(err)
	}
	request := &prompb.WriteRequest{}
	for _, s := range in.Series {
		ts := prompb.TimeSeries{}
		for _, l := range s.Labels {
			ts.Labels = append(ts.Labels, prompb.Label{Name: l.Name, Value: l.Value})
		}
		for _, sample := range s.Samples {
			ts.Samples = append(ts.Samples, prompb.Sample{Value: sample.Value, Timestamp: sample.Timestamp})
		}
		request.Timeseries = append(request.Timeseries, ts)
	}
	for _, m := range in.Metadata {
		request.Metadata = append(request.Metadata, prompb.MetricMetadata{
			Type:             prompb.MetricMetadata_MetricType(m.Type),
			MetricFamilyName: m.MetricFamilyName,
			Help:             m.Help,
			Unit:             m.Unit,
		})
	}
	out, err := request.Marshal()
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(out)
}
`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "prompb: cannot write the oracle:", err)
		os.Exit(1)
	}

	oraclePath = filepath.Join(dir, "oracle")
	build := exec.Command("go", "build", "-o", oraclePath, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY="+oracleProxy(), "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "prompb: cannot build the oracle: %v\n%s\n", err, out)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// oracleProxy is the module proxy the oracle build uses.
//
// The environment's own proxy is used when it sets one; otherwise a
// mirror is picked, because the default proxy is unreachable from
// here.
func oracleProxy() string {
	if proxy := os.Getenv("GOPROXY"); proxy != "" {
		return proxy
	}
	return "https://goproxy.cn,direct"
}

// oracleInput is the JSON description the oracle reads.
type oracleInput struct {
	Series   []oracleSeries   `json:"series"`
	Metadata []oracleMetadata `json:"metadata"`
}

type oracleSeries struct {
	Labels  []oracleLabel  `json:"labels"`
	Samples []oracleSample `json:"samples"`
}

type oracleLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type oracleSample struct {
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
}

type oracleMetadata struct {
	Type             int32  `json:"type"`
	MetricFamilyName string `json:"family"`
	Help             string `json:"help"`
	Unit             string `json:"unit"`
}

// oracle encodes with the reference implementation.
func oracle(t *testing.T, request *WriteRequest) []byte {
	t.Helper()
	description := oracleInput{}
	for _, series := range request.TimeSeries {
		entry := oracleSeries{}
		for _, label := range series.Labels {
			entry.Labels = append(entry.Labels, oracleLabel{Name: label.Name, Value: label.Value})
		}
		for _, sample := range series.Samples {
			entry.Samples = append(entry.Samples, oracleSample{Value: sample.Value, Timestamp: sample.Timestamp})
		}
		description.Series = append(description.Series, entry)
	}
	for _, metadata := range request.Metadata {
		description.Metadata = append(description.Metadata, oracleMetadata{
			Type:             int32(metadata.Type),
			MetricFamilyName: metadata.MetricFamilyName,
			Help:             metadata.Help,
			Unit:             metadata.Unit,
		})
	}

	data, err := json.Marshal(description)
	if err != nil {
		t.Fatalf("marshal the description: %v", err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write the description: %v", err)
	}
	out, err := exec.Command(oraclePath, path).Output()
	if err != nil {
		t.Fatalf("run the oracle: %v", err)
	}
	return out
}

// checkSame compares the encoder's output with the reference.
func checkSame(t *testing.T, name string, request *WriteRequest) {
	t.Helper()
	got := request.Marshal()
	want := oracle(t, request)
	if !bytes.Equal(got, want) {
		t.Errorf("%s: the encoding differs from the reference\n got %x\nwant %x", name, got, want)
	}
}

// TestMarshalMatchesTheReference checks the encoder against the
// reference implementation.
func TestMarshalMatchesTheReference(t *testing.T) {
	// An empty request.
	checkSame(t, "empty", &WriteRequest{})

	// One series with the usual labels.
	checkSame(t, "one series", &WriteRequest{
		TimeSeries: []TimeSeries{{
			Labels: []Label{
				{Name: "__name__", Value: "harness_requests_total"},
				{Name: "provider", Value: "openai"},
			},
			Samples: []Sample{
				{Value: 42, Timestamp: 1700000000000},
			},
		}},
	})

	// Several series and several samples each.
	checkSame(t, "many series", &WriteRequest{
		TimeSeries: []TimeSeries{
			{
				Labels:  []Label{{Name: "__name__", Value: "a_total"}, {Name: "x", Value: "1"}},
				Samples: []Sample{{Value: 1, Timestamp: 1000}, {Value: 2, Timestamp: 2000}},
			},
			{
				Labels:  []Label{{Name: "__name__", Value: "b_total"}},
				Samples: []Sample{{Value: -3.5, Timestamp: 3000}},
			},
		},
	})

	// The default values: proto3 omits them, so the reference must
	// omit them too.
	checkSame(t, "a zero value and a zero timestamp", &WriteRequest{
		TimeSeries: []TimeSeries{{
			Labels:  []Label{{Name: "__name__", Value: "z"}},
			Samples: []Sample{{Value: 0, Timestamp: 0}},
		}},
	})

	// A negative timestamp: the varint is ten bytes, which is the
	// encoding's edge case.
	checkSame(t, "a negative timestamp", &WriteRequest{
		TimeSeries: []TimeSeries{{
			Labels:  []Label{{Name: "__name__", Value: "n"}},
			Samples: []Sample{{Value: 1, Timestamp: -1}},
		}},
	})

	// A large timestamp.
	checkSame(t, "a large timestamp", &WriteRequest{
		TimeSeries: []TimeSeries{{
			Labels:  []Label{{Name: "__name__", Value: "t"}},
			Samples: []Sample{{Value: 1e300, Timestamp: 1 << 62}},
		}},
	})

	// The metadata: the metric family's type, name, help and unit.
	checkSame(t, "metadata", &WriteRequest{
		TimeSeries: []TimeSeries{{
			Labels:  []Label{{Name: "__name__", Value: "harness_requests_total"}},
			Samples: []Sample{{Value: 1, Timestamp: 1000}},
		}},
		Metadata: []MetricMetadata{
			{Type: MetricTypeCounter, MetricFamilyName: "harness_requests", Help: "The requests.", Unit: "call"},
			{Type: MetricTypeHistogram, MetricFamilyName: "harness_latency", Help: "The latency.", Unit: "ms"},
		},
	})

	// The metadata's default type: proto3 omits it.
	checkSame(t, "an unknown metadata type", &WriteRequest{
		Metadata: []MetricMetadata{
			{Type: MetricTypeUnknown, MetricFamilyName: "unknown_family"},
		},
	})
}

// TestMarshalEmptySeries checks that a series without samples still
// carries its labels.
func TestMarshalEmptySeries(t *testing.T) {
	request := &WriteRequest{
		TimeSeries: []TimeSeries{{
			Labels: []Label{{Name: "__name__", Value: "empty"}},
		}},
	}
	got := request.Marshal()
	want := oracle(t, request)
	if !bytes.Equal(got, want) {
		t.Errorf("the encoding differs from the reference\n got %x\nwant %x", got, want)
	}
}

// TestDecodeRoundTrip checks that what the encoder writes, the decoder
// reads back.
//
// The decoder is the harness's own, so the check is that the two agree:
// the bytes that leave the process are the bytes the harness can read.
func TestDecodeRoundTrip(t *testing.T) {
	request := &WriteRequest{
		TimeSeries: []TimeSeries{
			{
				Labels: []Label{
					{Name: "__name__", Value: "harness_requests_total"},
					{Name: "provider", Value: "openai"},
				},
				Samples: []Sample{
					{Value: 42, Timestamp: 1700000000000},
					{Value: 43, Timestamp: 1700000001000},
				},
			},
			{
				Labels:  []Label{{Name: "__name__", Value: "harness_latency_bucket"}, {Name: "le", Value: "+Inf"}},
				Samples: []Sample{{Value: -3.5, Timestamp: 1700000000000}},
			},
		},
		Metadata: []MetricMetadata{
			{Type: MetricTypeCounter, MetricFamilyName: "harness_requests", Help: "The requests.", Unit: "call"},
			{Type: MetricTypeHistogram, MetricFamilyName: "harness_latency", Help: "The latency.", Unit: "ms"},
		},
	}

	decoded, err := Decode(request.Marshal())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.TimeSeries) != len(request.TimeSeries) {
		t.Fatalf("series = %d, want %d", len(decoded.TimeSeries), len(request.TimeSeries))
	}
	for i, series := range request.TimeSeries {
		got := decoded.TimeSeries[i]
		if len(got.Labels) != len(series.Labels) {
			t.Errorf("series %d: labels = %d, want %d", i, len(got.Labels), len(series.Labels))
			continue
		}
		for j, label := range series.Labels {
			if got.Labels[j] != label {
				t.Errorf("series %d: label %d is %+v, want %+v", i, j, got.Labels[j], label)
			}
		}
		if len(got.Samples) != len(series.Samples) {
			t.Errorf("series %d: samples = %d, want %d", i, len(got.Samples), len(series.Samples))
			continue
		}
		for j, sample := range series.Samples {
			if got.Samples[j] != sample {
				t.Errorf("series %d: sample %d is %+v, want %+v", i, j, got.Samples[j], sample)
			}
		}
	}
	if len(decoded.Metadata) != len(request.Metadata) {
		t.Fatalf("metadata = %d, want %d", len(decoded.Metadata), len(request.Metadata))
	}
	for i, metadata := range request.Metadata {
		if decoded.Metadata[i] != metadata {
			t.Errorf("metadata %d is %+v, want %+v", i, decoded.Metadata[i], metadata)
		}
	}
}

// TestDecodeRejectsCorruptInput checks that a malformed message is
// reported rather than half-read.
func TestDecodeRejectsCorruptInput(t *testing.T) {
	cases := [][]byte{
		// A truncated varint.
		{0xff},
		// A length that runs past the end.
		{0x0a, 0x10, 0x01},
		// An unknown wire type.
		{0x0e},
	}
	for i, data := range cases {
		if _, err := Decode(data); err == nil {
			t.Errorf("case %d: the corrupt input was accepted", i)
		}
	}
}
