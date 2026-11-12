package perm

import (
	"bufio"
	"io"
	"os"
	"sync"
)

// LineReader reads one line at a time from a shared stream.
//
// The REPL and the permission prompt both need to read from the
// terminal, and two buffered readers over the same stream steal each
// other's input: the prompt would swallow the user's next command, or
// the REPL would swallow the answer. One reader, one lock, shared
// between them.
type LineReader interface {
	// ReadLine returns the next line without its terminator. The
	// second result is false at the end of the input.
	ReadLine() (string, bool)
}

// scannerReader is a LineReader over a bufio.Scanner.
type scannerReader struct {
	mu      sync.Mutex
	scanner *bufio.Scanner
}

// ReadLine returns the next line.
func (s *scannerReader) ReadLine() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.scanner.Scan() {
		return "", false
	}
	return s.scanner.Text(), true
}

// NewLineReader wraps a reader as a LineReader.
func NewLineReader(r io.Reader) LineReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &scannerReader{scanner: scanner}
}

// StdinLineReader returns a LineReader over the process's standard
// input.
func StdinLineReader() LineReader {
	return NewLineReader(os.Stdin)
}

// NewScanner returns a scanner over a LineReader, so the REPL can
// read lines the same way the permission prompt does.
func NewScanner(lr LineReader) *bufio.Scanner {
	scanner := bufio.NewScanner(&lineAdapter{lr: lr})
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return scanner
}

// lineAdapter turns a LineReader into an io.Reader.
type lineAdapter struct {
	lr LineReader
	// buf holds the remainder of the current line.
	buf []byte
}

// Read fills p from the current line.
func (a *lineAdapter) Read(p []byte) (int, error) {
	if len(a.buf) == 0 {
		line, ok := a.lr.ReadLine()
		if !ok {
			return 0, io.EOF
		}
		a.buf = append([]byte(line), '\n')
	}
	n := copy(p, a.buf)
	a.buf = a.buf[n:]
	return n, nil
}

// IsTerminal reports whether a file is attached to a terminal, so the
// caller can decide whether asking the user is possible at all.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
