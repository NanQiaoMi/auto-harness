// Package frontmatter parses the YAML frontmatter of a markdown file.
//
// The harness ships no external dependency, so the frontmatter is parsed
// by hand. The supported subset is what the harness's own resource
// formats need: a flat mapping of scalars, plus literal and folded
// block scalars.
//
// Anything richer — nested mappings, sequences, anchors — is rejected
// rather than guessed at. A file written for another tool then fails
// loudly instead of loading with the wrong values, which is the only
// safe behaviour for a file that carries instructions.
package frontmatter

import (
	"fmt"
	"strings"
)

// Field is one frontmatter entry.
type Field struct {
	// Key is the field's name.
	Key string
	// Value is the field's value. A block scalar keeps its newlines;
	// a plain scalar is trimmed.
	Value string
}

// Parse splits a markdown document into its frontmatter fields and its
// body.
//
// A document without frontmatter is not an error: the fields are empty
// and the body is the whole document.
//
// An error means the frontmatter is present but malformed: it was never
// closed, a line is not a field, or a value is not a scalar this
// parser understands.
func Parse(content string) ([]Field, string, error) {
	normalized := normalize(content)

	// The frontmatter is only a frontmatter when it opens the file.
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, strings.TrimSpace(normalized), nil
	}

	lines := strings.Split(normalized, "\n")
	var block []string
	body := ""
	closed := false
	// The first line is the opening delimiter.
	for i := 1; i < len(lines); i++ {
		if isDelimiter(lines[i]) {
			body = strings.Join(lines[i+1:], "\n")
			closed = true
			break
		}
		block = append(block, lines[i])
	}
	if !closed {
		return nil, "", fmt.Errorf("frontmatter: the block is not closed")
	}

	fields, err := parseBlock(block)
	if err != nil {
		return nil, "", err
	}
	return fields, strings.TrimSpace(body), nil
}

// Get returns the value of a field.
//
// The lookup is exact: YAML keys are case-sensitive, and a file that
// spells a field differently is a file that means something else.
func Get(fields []Field, key string) (string, bool) {
	for _, field := range fields {
		if field.Key == key {
			return field.Value, true
		}
	}
	return "", false
}

// normalize turns the document into the form the parser reads: LF line
// endings, no byte order mark.
func normalize(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	// The byte order mark is a UTF-8 sign, not part of the text.
	return strings.TrimPrefix(content, string([]byte{0xEF, 0xBB, 0xBF}))
}

// isDelimiter reports whether a line closes the frontmatter.
//
// Trailing spaces are allowed: a hand-edited file often has them.
func isDelimiter(line string) bool {
	return strings.TrimRight(line, " \t") == "---"
}

// parseBlock reads the frontmatter's lines into fields.
func parseBlock(lines []string) ([]Field, error) {
	var fields []Field
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("frontmatter: %q is not a field", strings.TrimSpace(line))
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("frontmatter: %q has no field name", strings.TrimSpace(line))
		}
		value = strings.TrimSpace(value)

		switch {
		case value == "":
			// A field with no value: an empty string, not a nested
			// mapping. A nested mapping is not supported, so there is
			// nothing else it could mean.
			fields = append(fields, Field{Key: key})
		case isBlockIndicator(value):
			block, next := collectBlock(lines, i+1)
			fields = append(fields, Field{Key: key, Value: blockValue(value, block)})
			i = next - 1
		default:
			scalar, err := scalarValue(value)
			if err != nil {
				return nil, fmt.Errorf("frontmatter: %q: %w", key, err)
			}
			fields = append(fields, Field{Key: key, Value: scalar})
		}
	}
	return fields, nil
}

// isBlockIndicator reports whether a value starts a block scalar.
//
// The indicator is the pipe or the greater-than sign, with an optional
// chomping marker after it.
func isBlockIndicator(value string) bool {
	switch value {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	}
	return false
}

// collectBlock gathers the indented lines of a block scalar.
//
// The block ends at the first line that is neither indented nor empty.
// The returned index is the first line after the block.
func collectBlock(lines []string, start int) ([]string, int) {
	var block []string
	i := start
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			block = append(block, "")
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		block = append(block, line)
	}
	return block, i
}

// blockValue turns a block scalar's lines into its value.
//
// The pipe keeps the line breaks, the greater-than sign folds them into
// spaces. The chomping marker decides what happens to the trailing
// newline: a dash removes it, a plus keeps every trailing newline, and
// the default keeps one.
func blockValue(indicator string, block []string) string {
	// The trailing empty lines are the block's trailing newline, not
	// part of its text.
	trailing := 0
	for len(block) > 0 && strings.TrimSpace(block[len(block)-1]) == "" {
		block = block[:len(block)-1]
		trailing++
	}
	lines := stripIndent(block)

	var text string
	if strings.HasPrefix(indicator, ">") {
		text = foldLines(lines)
	} else {
		text = strings.Join(lines, "\n")
	}

	switch {
	case strings.HasSuffix(indicator, "-"):
		// The trailing newline is stripped.
	case strings.HasSuffix(indicator, "+"):
		text += strings.Repeat("\n", trailing+1)
	default:
		text += "\n"
	}
	return text
}

// stripIndent removes the common indentation from a block's lines.
//
// The indentation is the smallest one among the non-empty lines, which
// is what YAML measures the block against.
func stripIndent(lines []string) []string {
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		if len(line) >= indent {
			out[i] = line[indent:]
			continue
		}
		out[i] = strings.TrimLeft(line, " \t")
	}
	return out
}

// foldLines joins the lines of a folded block scalar.
//
// Lines that follow each other are joined with a space; an empty line
// becomes a newline of its own.
func foldLines(lines []string) string {
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			if strings.TrimSpace(lines[i-1]) == "" || strings.TrimSpace(line) == "" {
				b.WriteString("\n")
			} else {
				b.WriteString(" ")
			}
		}
		b.WriteString(line)
	}
	return b.String()
}

// scalarValue reads a plain scalar value.
//
// A quoted value loses its quotes. A value that starts a collection is
// rejected: this parser does not read collections, and a half-read
// collection is worse than a clear failure.
func scalarValue(value string) (string, error) {
	if value[0] == '[' || value[0] == '{' {
		return "", fmt.Errorf("collections are not supported")
	}
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first == '"' && last == '"' {
			return unescape(value[1 : len(value)-1]), nil
		}
		if first == '\'' && last == '\'' {
			return value[1 : len(value)-1], nil
		}
	}
	if value[0] == '"' || value[0] == '\'' {
		return "", fmt.Errorf("the quoted value is not closed")
	}
	return value, nil
}

// unescape reads the escapes inside a double-quoted value.
func unescape(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			b.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		default:
			b.WriteByte('\\')
			b.WriteByte(value[i])
		}
	}
	return b.String()
}
