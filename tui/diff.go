// Package tui is a minimal terminal UI layer with differential rendering,
// modeled after pi's pi-tui: components render to lines, and the screen
// only rewrites what changed since the previous frame.
//
// The package is split into a pure rendering core (Diff, Frame) and the
// terminal plumbing (Screen), so the diffing logic is testable without a
// terminal.
package tui

import (
	"strings"
)

// Line is a rendered row of text. It may contain ANSI escape sequences;
// its display width is what matters for layout.
type Line struct {
	// Text is the raw line content, possibly with ANSI escapes.
	Text string
}

// Frame is a full screen of lines.
type Frame []string

// Diff describes the changes needed to turn a previous frame into the
// next one. Only changed lines are rewritten, and only those that moved
// need to be erased first.
type Diff struct {
	// Lines are the new frame's lines (in order).
	Lines []string
	// Changed holds the indices of lines whose content differs.
	Changed []int
	// Unchanged is the number of lines that stayed identical.
	Unchanged int
	// Appended is the count of lines beyond the previous frame's length.
	Appended int
	// Removed is the count of lines beyond the next frame's length.
	Removed int
}

// IsEmpty reports whether the frame needs no redraw.
func (d Diff) IsEmpty() bool { return len(d.Changed) == 0 && d.Appended == 0 && d.Removed == 0 }

// DiffFrames compares two frames line by line and returns the changes
// needed to move from prev to next. prev may be nil (first frame).
func DiffFrames(prev, next []string) Diff {
	d := Diff{Lines: next}

	common := len(prev)
	if len(next) < common {
		common = len(next)
	}
	for i := 0; i < common; i++ {
		if prev[i] == next[i] {
			d.Unchanged++
			continue
		}
		d.Changed = append(d.Changed, i)
	}
	if len(next) > len(prev) {
		d.Appended = len(next) - len(prev)
	}
	if len(prev) > len(next) {
		d.Removed = len(prev) - len(next)
	}
	return d
}

// DiffText renders a diff as the ANSI sequence that applies it, using
// cursor movement to touch only changed lines.
//
// The strategy: for each changed line, move the cursor to that row,
// erase it to end of line, and write the new content. Appended lines are
// written at the bottom. This keeps the diff proportional to the change
// size rather than the screen size.
func DiffText(prev, next []string, width int) string {
	d := DiffFrames(prev, next)
	if d.IsEmpty() {
		return ""
	}
	var b strings.Builder
	for _, idx := range d.Changed {
		// Move to the row (1-based), erase the line, write the content.
		b.WriteString("\x1b[")
		b.WriteString(itoa(idx + 1))
		b.WriteString(";1H\x1b[2K")
		b.WriteString(next[idx])
		b.WriteString("\x1b[K")
	}
	// Appended lines: position at the first new row and write them.
	if d.Appended > 0 {
		start := len(prev)
		b.WriteString("\x1b[")
		b.WriteString(itoa(start + 1))
		b.WriteString(";1H")
		for i := start; i < len(next); i++ {
			b.WriteString("\x1b[2K")
			b.WriteString(next[i])
			b.WriteString("\x1b[K")
			if i < len(next)-1 {
				b.WriteString("\r\n")
			}
		}
	}
	// Removed lines: clear the leftovers so the screen has no ghosts.
	if d.Removed > 0 {
		start := len(next)
		b.WriteString("\x1b[")
		b.WriteString(itoa(start + 1))
		b.WriteString(";1H\x1b[0J")
	}
	return b.String()
}

// SyncBegin and SyncEnd wrap an update in the CSI 2026 synchronized
// output sequence, which makes the terminal apply it atomically (no
// flicker). Terminals that do not support it ignore the sequence.
const (
	SyncBegin = "\x1b[?2026h"
	SyncEnd   = "\x1b[?2026l"
)

// itoa converts a small int to a string without pulling in strconv for
// a hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
