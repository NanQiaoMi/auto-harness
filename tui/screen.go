package tui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Terminal abstracts the terminal so the screen can be tested without
// one (pi's ProcessTerminal equivalent).
type Terminal interface {
	// Write sends bytes to the terminal.
	Write(p []byte) (int, error)
	// Size returns the terminal width and height in columns/rows.
	Size() (width, height int)
	// Read reads raw input bytes; it blocks.
	Read(p []byte) (int, error)
	// SetRawMode enables or disables raw mode.
	SetRawMode(on bool) error
}

// Screen renders frames with differential updates.
//
// Two modes mirror pi: main screen writes into the normal buffer and
// keeps the terminal's scrollback; alternate screen uses the alternate
// buffer with an application-owned viewport.
type Screen struct {
	mu sync.Mutex

	term  Terminal
	alt   bool
	theme Theme

	// prev holds the last rendered frame for diffing.
	prev []string
	// width is the render width used for the last frame.
	width int
	// height is the render height used for the last frame.
	height int

	// components is the root component tree.
	root Component
	// dirty marks the frame as needing a redraw.
	dirty bool
	// running marks the render loop as active.
	running bool

	// onRender is called after each frame with the rendered lines.
	onRender func(lines []string)
	// onInput receives raw input when set.
	onInput func(data string)
}

// NewScreen creates a screen over a terminal.
func NewScreen(term Terminal, theme Theme) *Screen {
	return &Screen{term: term, theme: theme, dirty: true}
}

// SetRoot installs the component tree rendered on every frame.
func (s *Screen) SetRoot(c Component) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.root = c
	s.dirty = true
}

// SetTheme replaces the theme.
func (s *Screen) SetTheme(t Theme) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.theme = t
	s.dirty = true
}

// Theme returns the current theme.
func (s *Screen) Theme() Theme {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.theme
}

// SetOnRender installs a callback invoked with each rendered frame.
func (s *Screen) SetOnRender(fn func(lines []string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRender = fn
}

// SetOnInput installs a callback for raw terminal input.
func (s *Screen) SetOnInput(fn func(data string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onInput = fn
}

// RequestRender marks the frame as needing a redraw.
func (s *Screen) RequestRender() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
}

// Start begins rendering. In alternate mode it enters the alternate
// screen buffer; it always restores the terminal on Stop.
func (s *Screen) Start() error {
	s.mu.Lock()
	s.running = true
	alt := s.alt
	s.mu.Unlock()

	if alt {
		// Enter the alternate screen buffer.
		if _, err := s.term.Write([]byte("\x1b[?1049h\x1b[?25l")); err != nil {
			return err
		}
	} else {
		// Main screen: hide the cursor while rendering.
		if _, err := s.term.Write([]byte("\x1b[?25l")); err != nil {
			return err
		}
	}
	return nil
}

// Stop ends rendering and restores the terminal.
func (s *Screen) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	alt := s.alt
	s.mu.Unlock()

	var b strings.Builder
	if alt {
		// Leave the alternate buffer, then print the final document so
		// the conversation is still in the scrollback.
		final := s.renderNow()
		b.WriteString("\x1b[?25h\x1b[?1049l")
		for _, line := range final {
			b.WriteString(line)
			b.WriteString("\r\n")
		}
	} else {
		b.WriteString("\x1b[?25h")
	}
	_, err := s.term.Write([]byte(b.String()))
	return err
}

// UseAltScreen switches to the alternate-screen viewport mode. It takes
// effect on the next Start.
func (s *Screen) UseAltScreen(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alt = on
}

// Render draws the current frame if it changed. It returns the lines
// that were rendered.
func (s *Screen) Render() []string {
	s.mu.Lock()
	if !s.dirty || s.root == nil {
		lines := s.prev
		s.mu.Unlock()
		return lines
	}
	width, height := s.term.Size()
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	lines := s.root.Render(width)
	s.dirty = false
	prev := s.prev
	s.prev = lines
	s.width = width
	s.height = height
	onRender := s.onRender
	s.mu.Unlock()

	out := DiffText(prev, lines, width)
	if out != "" {
		// Wrap the update in a synchronized-output sequence so the
		// terminal applies it atomically (no flicker).
		s.term.Write([]byte(SyncBegin + out + SyncEnd))
	}
	if onRender != nil {
		onRender(lines)
	}
	return lines
}

// renderNow renders a frame without writing to the terminal.
func (s *Screen) renderNow() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil
	}
	width, _ := s.term.Size()
	if width <= 0 {
		width = 80
	}
	return s.root.Render(width)
}

// Run drives the render loop until ctx-like cancellation via Stop, at the
// given interval. It also pumps terminal input to the onInput callback.
func (s *Screen) Run(interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		interval = 33 * time.Millisecond
	}
	go s.pumpInput()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.Render()
		}
	}
}

// pumpInput reads raw terminal input and forwards it.
func (s *Screen) pumpInput() {
	buf := make([]byte, 1024)
	for {
		n, err := s.term.Read(buf)
		if n > 0 {
			s.mu.Lock()
			fn := s.onInput
			s.mu.Unlock()
			if fn != nil {
				fn(string(buf[:n]))
			}
		}
		if err != nil {
			return
		}
	}
}

// Print writes text directly to the terminal, bypassing the diff
// renderer (used for one-off notices).
func (s *Screen) Print(text string) {
	s.term.Write([]byte(text))
}

// Printf writes formatted text directly to the terminal.
func (s *Screen) Printf(format string, args ...any) {
	s.term.Write([]byte(fmt.Sprintf(format, args...)))
}

// ProcessTerminal is a Terminal backed by the process's stdio.
type ProcessTerminal struct {
	In  *os.File
	Out *os.File
	mu  sync.Mutex
	// rawState keeps whether raw mode was enabled.
	raw bool
}

// NewProcessTerminal returns a terminal over the process stdio.
func NewProcessTerminal() *ProcessTerminal {
	return &ProcessTerminal{In: os.Stdin, Out: os.Stdout}
}

func (p *ProcessTerminal) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Out.Write(b)
}

func (p *ProcessTerminal) Read(b []byte) (int, error) {
	return p.In.Read(b)
}

// Size returns the terminal size, falling back to 80x24.
func (p *ProcessTerminal) Size() (int, int) {
	w, h, err := terminalSize(int(p.Out.Fd()))
	if err != nil || w <= 0 {
		w, h = 80, 24
	}
	return w, h
}

// SetRawMode enables or disables raw mode on the input file.
func (p *ProcessTerminal) SetRawMode(on bool) error {
	return setRawMode(int(p.In.Fd()), on)
}
