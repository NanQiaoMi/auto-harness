package tui

import (
	"io"
	"strings"
	"testing"
)

func TestDiffFramesDetectsChanges(t *testing.T) {
	prev := []string{"a", "b", "c"}
	next := []string{"a", "B", "c"}
	d := DiffFrames(prev, next)
	if len(d.Changed) != 1 || d.Changed[0] != 1 {
		t.Errorf("changed = %v, want [1]", d.Changed)
	}
	if d.Unchanged != 2 {
		t.Errorf("unchanged = %d, want 2", d.Unchanged)
	}
	if d.Appended != 0 || d.Removed != 0 {
		t.Errorf("appended/removed = %d/%d", d.Appended, d.Removed)
	}
	if d.IsEmpty() {
		t.Error("diff should not be empty")
	}
}

func TestDiffFramesAppendAndRemove(t *testing.T) {
	d := DiffFrames([]string{"a"}, []string{"a", "b", "c"})
	if d.Appended != 2 {
		t.Errorf("appended = %d, want 2", d.Appended)
	}
	if len(d.Changed) != 0 {
		t.Errorf("changed = %v, want none", d.Changed)
	}

	d = DiffFrames([]string{"a", "b", "c"}, []string{"a"})
	if d.Removed != 2 {
		t.Errorf("removed = %d, want 2", d.Removed)
	}
}

func TestDiffFramesIdentical(t *testing.T) {
	d := DiffFrames([]string{"a", "b"}, []string{"a", "b"})
	if !d.IsEmpty() {
		t.Errorf("identical frames should produce an empty diff: %+v", d)
	}
}

func TestDiffTextOnlyRewritesChangedLines(t *testing.T) {
	prev := []string{"one", "two", "three"}
	next := []string{"one", "TWO", "three"}
	out := DiffText(prev, next, 80)

	if out == "" {
		t.Fatal("expected diff output")
	}
	// Only the changed line's content should be present.
	if !strings.Contains(out, "TWO") {
		t.Errorf("missing new content: %q", out)
	}
	if strings.Contains(out, "one") || strings.Contains(out, "three") {
		t.Errorf("unchanged lines were rewritten: %q", out)
	}
	// The cursor must move to the changed row (row 2 -> "\x1b[2;1H").
	if !strings.Contains(out, "\x1b[2;1H") {
		t.Errorf("missing cursor move to row 2: %q", out)
	}
}

func TestDiffTextAppendsNewLines(t *testing.T) {
	out := DiffText([]string{"a"}, []string{"a", "b"}, 80)
	if !strings.Contains(out, "b") {
		t.Errorf("missing appended line: %q", out)
	}
	if !strings.Contains(out, "\x1b[2;1H") {
		t.Errorf("append should position at the new row: %q", out)
	}
}

func TestDiffTextClearsRemovedLines(t *testing.T) {
	out := DiffText([]string{"a", "b", "c"}, []string{"a"}, 80)
	if !strings.Contains(out, "\x1b[0J") {
		t.Errorf("removed lines should be cleared: %q", out)
	}
}

func TestDisplayWidthIgnoresANSI(t *testing.T) {
	if got := DisplayWidth("abc"); got != 3 {
		t.Errorf("plain width = %d, want 3", got)
	}
	if got := DisplayWidth("\x1b[31mabc\x1b[0m"); got != 3 {
		t.Errorf("ansi width = %d, want 3", got)
	}
	if got := DisplayWidth("中文"); got != 4 {
		t.Errorf("cjk width = %d, want 4", got)
	}
}

func TestWrapRespectsWidth(t *testing.T) {
	lines := Wrap("the quick brown fox jumps over the lazy dog", 12)
	for i, l := range lines {
		if DisplayWidth(l) > 12 {
			t.Errorf("line %d too wide (%d): %q", i, DisplayWidth(l), l)
		}
	}
	if len(lines) < 2 {
		t.Errorf("expected multiple lines, got %v", lines)
	}
	// Words must be preserved.
	joined := strings.Join(lines, " ")
	for _, w := range []string{"quick", "brown", "lazy"} {
		if !strings.Contains(joined, w) {
			t.Errorf("word %q lost in wrap: %q", w, joined)
		}
	}
}

func TestWrapHardSplitsLongWords(t *testing.T) {
	lines := Wrap(strings.Repeat("x", 25), 10)
	if len(lines) < 3 {
		t.Fatalf("expected hard split, got %v", lines)
	}
	for i, l := range lines {
		if DisplayWidth(l) > 10 {
			t.Errorf("line %d too wide: %q", i, l)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("hello", 10); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	got := Truncate("hello world", 8)
	if DisplayWidth(got) > 8 {
		t.Errorf("truncated too wide: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got)
	}
}

func TestTextRenderPrefixAndWrap(t *testing.T) {
	txt := &Text{Content: "hello world this is a long line", Prefix: "> "}
	lines := txt.Render(20)
	if len(lines) < 2 {
		t.Fatalf("expected wrapping, got %v", lines)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, "> ") {
			t.Errorf("line %d missing prefix: %q", i, l)
		}
		if DisplayWidth(l) > 20 {
			t.Errorf("line %d too wide (%d): %q", i, DisplayWidth(l), l)
		}
	}
}

func TestHStackSideBySide(t *testing.T) {
	h := &HStack{Children: []Component{
		NewText("ab"),
		NewText("cde"),
	}}
	lines := h.Render(40)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0] != "ab cde" {
		t.Errorf("hstack line = %q, want %q", lines[0], "ab cde")
	}
}

func TestStyleApply(t *testing.T) {
	s := Style{Bold: true, FG: "36"}
	out := s.Apply("hi")
	if !strings.HasPrefix(out, "\x1b[") || !strings.Contains(out, "hi") {
		t.Errorf("styled output = %q", out)
	}
	if !strings.HasSuffix(out, "\x1b[0m") {
		t.Errorf("missing reset: %q", out)
	}
	if DisplayWidth(out) != 2 {
		t.Errorf("styled width = %d, want 2", DisplayWidth(out))
	}
	if got := (Style{}).Apply("hi"); got != "hi" {
		t.Errorf("empty style changed output: %q", got)
	}
}

// fakeTerminal records writes for screen tests.
type fakeTerminal struct {
	buf      strings.Builder
	width    int
	height   int
	input    chan []byte
	rawMode  bool
	writeErr error
}

func (f *fakeTerminal) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}

func (f *fakeTerminal) Size() (int, int) { return f.width, f.height }

func (f *fakeTerminal) Read(p []byte) (int, error) {
	b, ok := <-f.input
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (f *fakeTerminal) SetRawMode(on bool) error {
	f.rawMode = on
	return nil
}

func TestScreenRendersDiffOnly(t *testing.T) {
	term := &fakeTerminal{width: 40, height: 10, input: make(chan []byte)}
	s := NewScreen(term, DefaultTheme())

	root := &Container{}
	txt := NewText("hello")
	root.Add(txt)
	s.SetRoot(root)

	s.Render()
	first := term.buf.String()
	if !strings.Contains(first, "hello") {
		t.Errorf("first render missing content: %q", first)
	}

	// An unchanged tree produces no further output.
	term.buf.Reset()
	s.Render()
	if term.buf.String() != "" {
		t.Errorf("unchanged frame wrote output: %q", term.buf.String())
	}

	// Changing the text rewrites the line.
	txt.Content = "goodbye"
	s.RequestRender()
	term.buf.Reset()
	s.Render()
	out := term.buf.String()
	if !strings.Contains(out, "goodbye") {
		t.Errorf("update missing new content: %q", out)
	}
}

func TestScreenSyncSequence(t *testing.T) {
	term := &fakeTerminal{width: 40, height: 10, input: make(chan []byte)}
	s := NewScreen(term, DefaultTheme())
	root := &Container{}
	root.Add(NewText("x"))
	s.SetRoot(root)

	s.Render()
	out := term.buf.String()
	if !strings.Contains(out, SyncBegin) || !strings.Contains(out, SyncEnd) {
		t.Errorf("missing synchronized output markers: %q", out)
	}
}
