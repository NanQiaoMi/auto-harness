package main

import (
	"io"
	"sync"
)

// commandOutput lets the REPL replace its command writer after the
// builtins are registered. The TUI uses this to put command output in
// its transcript instead of writing over the rendered screen.
type commandOutput struct {
	mu sync.Mutex
	w  io.Writer
}

func newCommandOutput(w io.Writer) *commandOutput {
	if w == nil {
		w = io.Discard
	}
	return &commandOutput{w: w}
}

func (o *commandOutput) Set(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	o.mu.Lock()
	o.w = w
	o.mu.Unlock()
}

func (o *commandOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.w.Write(p)
}
