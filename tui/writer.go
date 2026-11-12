package tui

import (
	"strings"
	"sync"
)

// TranscriptWriter writes to a Transcript, one line at a time.
//
// The permission prompt asks the user a question while the screen is
// rendering. Writing it straight to stderr would interleave with the
// differential updates; going through the transcript keeps the screen
// consistent.
type TranscriptWriter struct {
	mu sync.Mutex
	// transcript receives the lines.
	transcript *Transcript
	// onWrite is called after a line is appended, so the caller can ask
	// for a repaint.
	onWrite func()
}

// NewTranscriptWriter returns a writer over a transcript.
func NewTranscriptWriter(t *Transcript, onWrite func()) *TranscriptWriter {
	return &TranscriptWriter{transcript: t, onWrite: onWrite}
}

// Write appends the text as transcript lines.
//
// A trailing fragment without a newline becomes a line of its own, so
// a prompt that ends with "allow? [y/N] " is still visible.
func (w *TranscriptWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	text := string(p)
	segments := strings.Split(text, "\n")
	// A text ending in a newline has an empty last segment, which is
	// the terminator rather than a line.
	if len(segments) > 1 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	for _, s := range segments {
		w.transcript.Append(strings.TrimRight(s, "\r"))
	}
	if w.onWrite != nil {
		w.onWrite()
	}
	return len(p), nil
}
