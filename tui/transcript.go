package tui

import (
	"fmt"
)

// Transcript is a scrollback component holding conversation lines. It is
// the component behind the harness REPL: user turns, model output and
// tool notices are appended as they arrive.
type Transcript struct {
	Base
	// entries are the committed lines of the conversation.
	entries []string
	// pending is the in-progress model output (the last line), rendered
	// live while the stream arrives.
	pending string
}

// NewTranscript returns an empty transcript.
func NewTranscript() *Transcript { return &Transcript{} }

// Append adds a committed line.
func (t *Transcript) Append(line string) {
	t.entries = append(t.entries, line)
}

// Appendf adds a formatted committed line.
func (t *Transcript) Appendf(format string, args ...any) {
	t.entries = append(t.entries, fmt.Sprintf(format, args...))
}

// SetPending replaces the in-progress line (empty clears it).
func (t *Transcript) SetPending(line string) {
	t.pending = line
}

// Pending returns the in-progress line.
func (t *Transcript) Pending() string { return t.pending }

// Lines returns the committed lines.
func (t *Transcript) Lines() []string {
	out := make([]string, len(t.entries))
	copy(out, t.entries)
	return out
}

// Render returns the transcript lines plus the pending line.
func (t *Transcript) Render(width int) []string {
	out := make([]string, 0, len(t.entries)+1)
	for _, e := range t.entries {
		for _, l := range Wrap(e, width) {
			out = append(out, l)
		}
	}
	if t.pending != "" {
		for _, l := range Wrap(t.pending, width) {
			out = append(out, l)
		}
	}
	return out
}

// Editor is the input line component of the REPL.
type Editor struct {
	Base
	// Value is the current input text.
	Value string
	// Prompt is shown before the input.
	Prompt string
	// Placeholder is shown when Value is empty.
	Placeholder string
	// Style applies to the rendered line.
	Style Style
}

// NewEditor returns an editor with the given prompt.
func NewEditor(prompt string) *Editor {
	return &Editor{Prompt: prompt, Placeholder: "type a message, /help for commands"}
}

// Render returns the single input line.
func (e *Editor) Render(width int) []string {
	text := e.Value
	if text == "" {
		text = e.Placeholder
	}
	line := e.Prompt + text
	if DisplayWidth(line) > width {
		line = Truncate(line, width)
	}
	return []string{e.Style.Apply(line)}
}
