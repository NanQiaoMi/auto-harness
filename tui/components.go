package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Component renders itself to a list of lines (pi's Component interface).
type Component interface {
	// Render returns one string per line. Each line must not exceed
	// width in display columns.
	Render(width int) []string
	// Invalidate clears cached render state so the next Render starts
	// fresh.
	Invalidate()
}

// Base provides the no-op Invalidate for simple components.
type Base struct{}

// Invalidate satisfies Component.
func (Base) Invalidate() {}

// Text is a simple block of text that wraps to the given width.
type Text struct {
	Base
	Content string
	// Prefix is placed before the first line (e.g. "> ").
	Prefix string
	// Style wraps each line (ANSI). Optional.
	Style func(string) string
}

func (t *Text) Render(width int) []string {
	content := t.Content
	if content == "" {
		return []string{""}
	}
	var out []string
	for _, raw := range strings.Split(content, "\n") {
		wrapped := Wrap(raw, width-len(t.Prefix))
		for i, line := range wrapped {
			if i == 0 {
				line = t.Prefix + line
			} else {
				line = t.Prefix + line
			}
			if t.Style != nil {
				line = t.Style(line)
			}
			out = append(out, line)
		}
	}
	return out
}

// NewText returns a Text component.
func NewText(content string) *Text { return &Text{Content: content} }

// Spacer renders blank lines.
type Spacer struct {
	Base
	Lines int
}

func (s *Spacer) Render(width int) []string {
	n := s.Lines
	if n <= 0 {
		n = 1
	}
	out := make([]string, n)
	return out
}

// Container groups components and renders them in order.
type Container struct {
	Base
	Children []Component
}

func (c *Container) Render(width int) []string {
	var out []string
	for _, child := range c.Children {
		out = append(out, child.Render(width)...)
	}
	return out
}

// Add appends a child.
func (c *Container) Add(child Component) { c.Children = append(c.Children, child) }

// VStack stacks children vertically (pi's VStack).
type VStack struct {
	Base
	Children []Component
}

func (v *VStack) Render(width int) []string {
	var out []string
	for _, child := range v.Children {
		out = append(out, child.Render(width)...)
	}
	return out
}

// HStack places children side by side, each in a column (pi's HStack).
type HStack struct {
	Base
	Children []Component
	// Gap is the number of spaces between columns.
	Gap int
}

func (h *HStack) Render(width int) []string {
	if len(h.Children) == 0 {
		return nil
	}
	// Render every child, then interleave the lines side by side.
	cols := make([][]string, 0, len(h.Children))
	widths := make([]int, 0, len(h.Children))
	for _, child := range h.Children {
		lines := child.Render(width)
		cols = append(cols, lines)
		w := 0
		for _, l := range lines {
			if d := DisplayWidth(l); d > w {
				w = d
			}
		}
		widths = append(widths, w)
	}
	height := 0
	for _, lines := range cols {
		if len(lines) > height {
			height = len(lines)
		}
	}
	gap := h.Gap
	if gap == 0 {
		gap = 1
	}
	out := make([]string, 0, height)
	for row := 0; row < height; row++ {
		var b strings.Builder
		for ci, lines := range cols {
			if ci > 0 {
				b.WriteString(strings.Repeat(" ", gap))
			}
			if row < len(lines) {
				b.WriteString(lines[row])
				if pad := widths[ci] - DisplayWidth(lines[row]); pad > 0 {
					b.WriteString(strings.Repeat(" ", pad))
				}
			} else {
				b.WriteString(strings.Repeat(" ", widths[ci]))
			}
		}
		out = append(out, b.String())
	}
	return out
}

// Wrap breaks text into lines of at most width display columns, splitting
// on word boundaries where possible.
func Wrap(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	if text == "" {
		return []string{""}
	}
	if DisplayWidth(text) <= width {
		return []string{text}
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if DisplayWidth(para) <= width {
			out = append(out, para)
			continue
		}
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		cur := ""
		for _, w := range words {
			candidate := w
			if cur != "" {
				candidate = cur + " " + w
			}
			if DisplayWidth(candidate) <= width {
				cur = candidate
				continue
			}
			if cur != "" {
				out = append(out, cur)
			}
			// A single word longer than the width is hard-split.
			for DisplayWidth(w) > width {
				cut := runeSlice(w, width)
				out = append(out, cut)
				w = w[len(cut):]
			}
			cur = w
		}
		if cur != "" {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// runeSlice returns the longest prefix of s whose display width is at
// most width, and the byte length of that prefix.
func runeSlice(s string, width int) string {
	used := 0
	for i, r := range s {
		w := RuneWidth(r)
		if used+w > width {
			return s[:i]
		}
		used += w
	}
	return s
}

// DisplayWidth returns the display width of a string, ignoring ANSI
// escape sequences.
func DisplayWidth(s string) int {
	width := 0
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == 0x1b {
			// Skip the whole escape sequence: CSI ("ESC [") runs until a
			// byte in 0x40-0x7e; other forms (ESC O, ESC ( …) have a
			// short fixed length.
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && s[j] < 0x40 {
					j++
				}
				if j < len(s) {
					j++ // the final byte
				}
			} else if j < len(s) {
				switch s[j] {
				case ']', 'P', 'X', '^', '_':
					// String sequences end with ST (ESC \) or BEL.
					j++
					for j < len(s) && s[j] != 0x07 {
						if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
							j++
							break
						}
						j++
					}
					if j < len(s) {
						j++
					}
				default:
					j++
				}
			}
			i = j
			continue
		}
		width += RuneWidth(r)
		i += size
	}
	return width
}

// RuneWidth returns the display width of a rune. Wide CJK characters
// count as two columns; combining marks count as zero.
func RuneWidth(r rune) int {
	switch {
	case r == '\t':
		return 4
	case r < 32:
		return 0
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF, // CJK radicals .. Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE6F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	default:
		return 1
	}
}

// Truncate shortens a string to at most width display columns, adding an
// ellipsis when it had to cut.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if DisplayWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	cut := runeSlice(s, width-1)
	return cut + "…"
}

// Padding right-pads a string to the given display width.
func Padding(s string, width int) string {
	pad := width - DisplayWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// Style is an ANSI style descriptor.
type Style struct {
	Bold      bool
	Dim       bool
	Italic    bool
	Underline bool
	Reverse   bool
	// FG and BG are SGR color codes (e.g. 38;5;208), without the
	// leading escape sequence.
	FG string
	BG string
}

// SGR renders the style as an escape sequence, or "" when empty.
func (s Style) SGR() string {
	if s == (Style{}) {
		return ""
	}
	codes := make([]string, 0, 6)
	if s.Bold {
		codes = append(codes, "1")
	}
	if s.Dim {
		codes = append(codes, "2")
	}
	if s.Italic {
		codes = append(codes, "3")
	}
	if s.Underline {
		codes = append(codes, "4")
	}
	if s.Reverse {
		codes = append(codes, "7")
	}
	if s.FG != "" {
		codes = append(codes, s.FG)
	}
	if s.BG != "" {
		codes = append(codes, s.BG)
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// Apply wraps text in the style and resets afterwards.
func (s Style) Apply(text string) string {
	sgr := s.SGR()
	if sgr == "" {
		return text
	}
	return sgr + text + "\x1b[0m"
}

// Theme holds the styles used by the built-in components.
type Theme struct {
	// User is the style for user input lines.
	User Style
	// Assistant is the style for model output.
	Assistant Style
	// Tool is the style for tool notices.
	Tool Style
	// Error is the style for error messages.
	Error Style
	// Dim is the style for secondary text.
	Dim Style
}

// DefaultTheme returns a sensible default theme.
func DefaultTheme() Theme {
	return Theme{
		User:      Style{Bold: true, FG: "36"},
		Assistant: Style{},
		Tool:      Style{FG: "33"},
		Error:     Style{FG: "31", Bold: true},
		Dim:       Style{FG: "90"},
	}
}

// fmtLines is a helper for building multi-line output.
func fmtLines(format string, args ...any) []string {
	return strings.Split(fmt.Sprintf(format, args...), "\n")
}

// utf8Len is used by tests to sanity-check rune handling.
var _ = utf8.RuneCountInString
