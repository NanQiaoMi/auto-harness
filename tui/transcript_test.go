package tui

import (
	"strings"
	"testing"
)

// TestTranscriptPendingLifecycle mirrors how the REPL streams model output:
// the pending line grows in place, then is committed as a finished entry.
func TestTranscriptPendingLifecycle(t *testing.T) {
	tr := NewTranscript()
	tr.Append("> hello")

	// While streaming, the pending line is the last rendered line.
	tr.SetPending("partial ans")
	lines := tr.Render(80)
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[1] != "partial ans" {
		t.Errorf("pending line = %q", lines[1])
	}

	// Growing the pending line changes only that line.
	tr.SetPending("partial answer")
	lines = tr.Render(80)
	if lines[1] != "partial answer" {
		t.Errorf("grown pending = %q", lines[1])
	}

	// Committing turns it into a permanent entry; the rendered output is
	// unchanged (same text, same position).
	before := len(tr.Render(80))
	tr.Append(tr.Pending())
	tr.SetPending("")
	after := tr.Render(80)
	if len(after) != before {
		t.Errorf("commit changed line count: %d -> %d", before, len(after))
	}
	if after[1] != "partial answer" {
		t.Errorf("committed line = %q", after[1])
	}
}

// TestTranscriptStreamingOnlyRewritesPendingLine proves the differential
// renderer rewrites one row while the model streams.
func TestTranscriptStreamingOnlyRewritesPendingLine(t *testing.T) {
	tr := NewTranscript()
	tr.Append("> question")

	tr.SetPending("a")
	first := tr.Render(80)
	tr.SetPending("ab")
	second := tr.Render(80)

	d := DiffFrames(first, second)
	if len(d.Changed) != 1 {
		t.Fatalf("changed lines = %v, want exactly 1", d.Changed)
	}
	if d.Changed[0] != 1 {
		t.Errorf("changed index = %d, want 1 (the pending row)", d.Changed[0])
	}
}

func TestEditorRenders(t *testing.T) {
	e := NewEditor("> ")
	e.Style = Style{Bold: true}
	lines := e.Render(40)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], "> ") {
		t.Errorf("editor line = %q", lines[0])
	}
	if !strings.Contains(lines[0], "type a message") {
		t.Errorf("placeholder missing: %q", lines[0])
	}

	e.Value = "actual input"
	lines = e.Render(40)
	if !strings.Contains(lines[0], "actual input") {
		t.Errorf("value missing: %q", lines[0])
	}
	if strings.Contains(lines[0], "type a message") {
		t.Errorf("placeholder should be hidden: %q", lines[0])
	}
}

// TestContainerTranscriptEditorComposition verifies the REPL's component
// tree renders in the expected order.
func TestContainerTranscriptEditorComposition(t *testing.T) {
	tr := NewTranscript()
	tr.Append("header")
	tr.Append("")
	tr.Append("> question")
	tr.SetPending("answer so far")

	ed := NewEditor("> ")
	root := &Container{}
	root.Add(tr)
	root.Add(ed)

	lines := root.Render(80)
	if len(lines) != 5 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0] != "header" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if lines[2] != "> question" {
		t.Errorf("line 2 = %q", lines[2])
	}
	if lines[3] != "answer so far" {
		t.Errorf("line 3 (pending) = %q", lines[3])
	}
	if !strings.Contains(lines[4], "> ") {
		t.Errorf("line 4 (editor) = %q", lines[4])
	}

	// The editor row must not shift while the pending line grows.
	before := root.Render(80)
	tr.SetPending("answer so far, and more")
	after := root.Render(80)
	if len(before) != len(after) {
		t.Fatalf("line count changed while streaming")
	}
	if before[4] != after[4] {
		t.Errorf("editor row changed while streaming: %q -> %q", before[4], after[4])
	}
}
