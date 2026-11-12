package tui

import (
	"sync"
	"unicode/utf8"
)

// LineInput collects lines of raw terminal input.
//
// The TUI reads the terminal in raw mode, so the bytes arrive one at a
// time with no line discipline. LineInput reassembles them into lines
// for whoever needs them (the REPL, the permission prompt) while the
// editor shows what is being typed.
//
// A line that completes before anyone reads it is queued, so the
// keystrokes that arrive between the reader's turns are not lost.
type LineInput struct {
	mu sync.Mutex
	// buf holds the characters of the line being assembled.
	buf []byte
	// ready holds the completed lines nobody has read yet.
	ready []string
	// waiters are the goroutines waiting for a complete line.
	waiters []chan string
	// closed marks the end of the input.
	closed bool
	// onPartial is called with the line being typed, for the editor.
	onPartial func(string)
}

// NewLineInput returns an empty collector.
func NewLineInput() *LineInput { return &LineInput{} }

// SetOnPartial installs a callback invoked with the text typed so far,
// after every keystroke.
func (l *LineInput) SetOnPartial(fn func(string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onPartial = fn
}

// Feed adds raw terminal bytes to the collector.
func (l *LineInput) Feed(data string) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	var partial bool
	for _, r := range data {
		switch r {
		case '\r':
			// Ignore the carriage return of CRLF.
		case '\n':
			line := string(l.buf)
			l.buf = nil
			partial = true
			l.deliver(line)
		case 0x7f, 0x08:
			// Backspace: drop the last character.
			if n := len(l.buf); n > 0 {
				l.buf = l.buf[:n-1]
			}
			partial = true
		default:
			if r >= 0x20 {
				l.buf = utf8.AppendRune(l.buf, r)
			}
			partial = true
		}
	}
	onPartial := l.onPartial
	current := string(l.buf)
	l.mu.Unlock()

	if partial && onPartial != nil {
		onPartial(current)
	}
}

// deliver hands a completed line to a waiting reader, or queues it.
//
// The caller holds the lock.
func (l *LineInput) deliver(line string) {
	if len(l.waiters) > 0 {
		ch := l.waiters[0]
		l.waiters = l.waiters[1:]
		ch <- line
		return
	}
	l.ready = append(l.ready, line)
}

// ReadLine blocks until a complete line arrives. It returns false at
// the end of the input.
func (l *LineInput) ReadLine() (string, bool) {
	l.mu.Lock()
	if len(l.ready) > 0 {
		line := l.ready[0]
		l.ready = l.ready[1:]
		l.mu.Unlock()
		return line, true
	}
	if l.closed {
		l.mu.Unlock()
		return "", false
	}
	ch := make(chan string, 1)
	l.waiters = append(l.waiters, ch)
	l.mu.Unlock()

	line, ok := <-ch
	return line, ok
}

// Close ends the input and wakes every waiter.
func (l *LineInput) Close() {
	l.mu.Lock()
	l.closed = true
	waiters := l.waiters
	l.waiters = nil
	l.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}
