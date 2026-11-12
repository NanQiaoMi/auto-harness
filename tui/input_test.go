package tui

import (
	"strings"
	"testing"
	"time"
)

// TestLineInputCollectsLines verifies the raw byte collector
// reassembles the lines.
func TestLineInputCollectsLines(t *testing.T) {
	in := NewLineInput()

	go func() {
		in.Feed("hello")
		in.Feed(" world\r\n")
		in.Feed("second\n")
	}()

	line, ok := in.ReadLine()
	if !ok || line != "hello world" {
		t.Errorf("line = %q, ok = %v; want 'hello world'", line, ok)
	}
	line, ok = in.ReadLine()
	if !ok || line != "second" {
		t.Errorf("line = %q, ok = %v; want 'second'", line, ok)
	}
}

// TestLineInputHandlesBackspace verifies the backspace key edits the
// line being typed.
func TestLineInputHandlesBackspace(t *testing.T) {
	in := NewLineInput()

	go func() {
		in.Feed("abc")
		in.Feed("\x7f") // backspace
		in.Feed("\n")
	}()

	line, ok := in.ReadLine()
	if !ok || line != "ab" {
		t.Errorf("line = %q, ok = %v; want 'ab'", line, ok)
	}
}

// TestLineInputReportsPartial verifies the typed text is reported as
// it arrives, so the editor can show it.
func TestLineInputReportsPartial(t *testing.T) {
	in := NewLineInput()

	var partials []string
	in.SetOnPartial(func(text string) {
		partials = append(partials, text)
	})
	in.Feed("ab")
	in.Feed("c\n")

	if len(partials) == 0 {
		t.Fatal("no partial text was reported")
	}
	if partials[len(partials)-1] != "" {
		t.Errorf("last partial = %q, want the cleared line", partials[len(partials)-1])
	}
}

// TestLineInputCloseWakesWaiters verifies closing the input does not
// leave a reader hanging.
func TestLineInputCloseWakesWaiters(t *testing.T) {
	in := NewLineInput()

	done := make(chan bool, 1)
	go func() {
		_, ok := in.ReadLine()
		done <- ok
	}()

	time.Sleep(10 * time.Millisecond)
	in.Close()

	select {
	case ok := <-done:
		if ok {
			t.Error("the reader reported a line after the close")
		}
	case <-time.After(time.Second):
		t.Fatal("the reader was left hanging")
	}
}

// TestTranscriptWriterSplitsLines verifies the writer turns the
// prompt's text into transcript lines.
func TestTranscriptWriterSplitsLines(t *testing.T) {
	tr := NewTranscript()
	w := NewTranscriptWriter(tr, nil)

	if _, err := w.Write([]byte("permission required: bash\n    allow? [y/N] ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	lines := tr.Lines()
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %v", len(lines), lines)
	}
	if lines[0] != "permission required: bash" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "allow?") {
		t.Errorf("line 1 = %q", lines[1])
	}
}
