package snappy

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// oraclePath is the reference implementation's encoder, built by the
// test from the module cache.
//
// The harness ships no external dependency, so the snappy encoder here
// is hand-written. The reference implementation is the oracle: the
// bytes must match it exactly, because a Prometheus server decodes the
// block with that same format.
var oraclePath string

// TestMain builds the oracle once for the whole package.
//
// The oracle is built in its own module, so the harness's go.mod keeps
// its promise of no external dependency.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "snappy-oracle")
	if err != nil {
		fmt.Fprintln(os.Stderr, "snappy: cannot make the oracle directory:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	module := `module oracle

go 1.22

require github.com/golang/snappy v0.0.4
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "snappy: cannot write the oracle module:", err)
		os.Exit(1)
	}

	source := filepath.Join(dir, "main.go")
	program := `package main

import (
	"os"

	"github.com/golang/snappy"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(snappy.Encode(nil, data))
}
`
	if err := os.WriteFile(source, []byte(program), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "snappy: cannot write the oracle:", err)
		os.Exit(1)
	}

	oraclePath = filepath.Join(dir, "oracle")
	build := exec.Command("go", "build", "-o", oraclePath, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY="+oracleProxy(), "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "snappy: cannot build the oracle: %v\n%s\n", err, out)
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

// oracle encodes with the reference implementation.
func oracle(t *testing.T, data []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	out, err := exec.Command(oraclePath, path).Output()
	if err != nil {
		t.Fatalf("run the oracle: %v", err)
	}
	return out
}

// checkSame compares the encoder's output with the reference.
func checkSame(t *testing.T, name string, data []byte) {
	t.Helper()
	got, err := Encode(data)
	if err != nil {
		t.Fatalf("%s: encode: %v", name, err)
	}
	want := oracle(t, data)
	if !bytes.Equal(got, want) {
		t.Errorf("%s: the encoding differs from the reference\n got %x\nwant %x", name, got, want)
	}
}

// TestEncodeMatchesTheReference checks the encoder against the
// reference implementation on inputs that exercise every branch of the
// algorithm.
func TestEncodeMatchesTheReference(t *testing.T) {
	// The empty input.
	checkSame(t, "empty", nil)
	checkSame(t, "one byte", []byte("x"))

	// Inputs shorter than the minimum copy size: all literal.
	for size := 1; size < 20; size++ {
		checkSame(t, fmt.Sprintf("%d bytes", size), bytes.Repeat([]byte("a"), size))
	}

	// A run of one byte: the copy path with a small offset.
	checkSame(t, "a run", bytes.Repeat([]byte("a"), 1000))

	// Two alternating bytes: matches at odd offsets.
	alt := make([]byte, 5000)
	for i := range alt {
		if i%2 == 0 {
			alt[i] = 'a'
		} else {
			alt[i] = 'b'
		}
	}
	checkSame(t, "alternating", alt)

	// A repeated block: long copies with a large offset.
	block := bytes.Repeat([]byte("0123456789"), 100)
	checkSame(t, "a repeated block", append(block, block...))

	// Text-like data: the case the format is made for.
	checkSame(t, "text", []byte("the quick brown fox jumps over the lazy dog. the quick brown fox jumps over the lazy dog.\n"))

	// Incompressible data: every byte a literal.
	random := make([]byte, 4096)
	rand.New(rand.NewSource(1)).Read(random)
	checkSame(t, "random", random)

	// A large block, past the 64 KiB block limit: the encoder splits
	// it.
	large := bytes.Repeat([]byte("some repeated content that compresses well. "), 4000)
	checkSame(t, "large", large)
}

// TestEncodeRoundTrip checks that the block decodes back to the
// original.
//
// The decoder is written here as well, so the test checks the format
// rather than the reference's behaviour.
func TestEncodeRoundTrip(t *testing.T) {
	inputs := [][]byte{
		nil,
		[]byte("x"),
		bytes.Repeat([]byte("a"), 1000),
		[]byte("the quick brown fox jumps over the lazy dog"),
	}
	random := make([]byte, 5000)
	rand.New(rand.NewSource(2)).Read(random)
	inputs = append(inputs, random)

	for i, data := range inputs {
		encoded, err := Encode(data)
		if err != nil {
			t.Fatalf("input %d: encode: %v", i, err)
		}
		decoded, err := Decode(encoded)
		if err != nil {
			t.Fatalf("input %d: decode: %v", i, err)
		}
		if !bytes.Equal(decoded, data) {
			t.Errorf("input %d: the round trip changed the data", i)
		}
	}
}

// TestEncodeCompresses checks that the encoder actually compresses
// compressible data.
func TestEncodeCompresses(t *testing.T) {
	data := bytes.Repeat([]byte("compress me. "), 1000)
	encoded, err := Encode(data)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) >= len(data) {
		t.Errorf("encoded = %d bytes, want fewer than %d", len(encoded), len(data))
	}
}
