package perm

import (
	"context"
	"strings"
	"testing"
)

// TestLineReaderSharesTheStream verifies two consumers of the same
// reader each get their own lines, in order.
func TestLineReaderSharesTheStream(t *testing.T) {
	lr := NewLineReader(strings.NewReader("first\nsecond\nthird\n"))

	line, ok := lr.ReadLine()
	if !ok || line != "first" {
		t.Fatalf("line = %q, ok = %v; want first", line, ok)
	}
	line, ok = lr.ReadLine()
	if !ok || line != "second" {
		t.Fatalf("line = %q, ok = %v; want second", line, ok)
	}
	line, ok = lr.ReadLine()
	if !ok || line != "third" {
		t.Fatalf("line = %q, ok = %v; want third", line, ok)
	}
	if _, ok := lr.ReadLine(); ok {
		t.Error("expected the end of the input")
	}
}

// TestScannerOverLineReader verifies the scanner reads the same lines
// as the reader.
func TestScannerOverLineReader(t *testing.T) {
	lr := NewLineReader(strings.NewReader("alpha\nbeta\n"))
	scanner := NewScanner(lr)

	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("lines = %v, want [alpha beta]", got)
	}
}

// TestPromptApproverSharesTheReader verifies the approver takes its
// answer from the shared reader, so it does not steal the REPL's next
// line.
func TestPromptApproverSharesTheReader(t *testing.T) {
	lr := NewLineReader(strings.NewReader("y\nnext command\n"))
	var out strings.Builder
	p := &PromptApprover{
		Lines:   lr,
		Out:     &out,
		Workdir: t.TempDir(),
	}

	allowed, err := p.Approve(context.Background(), Call{Tool: "bash"}, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !allowed {
		t.Error("the call was not approved")
	}
	// The next line is still there for the REPL.
	line, ok := lr.ReadLine()
	if !ok || line != "next command" {
		t.Errorf("line = %q, ok = %v; want the REPL's line", line, ok)
	}
}

// TestApproverDeniesAtEndOfInput verifies a missing answer declines
// rather than hanging.
func TestApproverDeniesAtEndOfInput(t *testing.T) {
	lr := NewLineReader(strings.NewReader(""))
	p := &PromptApprover{Lines: lr, Out: &strings.Builder{}}

	allowed, err := p.Approve(context.Background(), Call{Tool: "bash"}, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if allowed {
		t.Error("the call was approved without an answer")
	}
}

// TestDenyApproverBlocks verifies the fail-safe approver.
func TestDenyApproverBlocks(t *testing.T) {
	d := DenyApprover{Reason: "no session"}
	allowed, err := d.Approve(context.Background(), Call{Tool: "bash"}, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if allowed {
		t.Error("the call was approved")
	}
}
