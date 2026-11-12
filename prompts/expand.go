// Package prompts implements pi's prompt templates: markdown files
// that become slash commands.
//
// A template is a markdown file with optional YAML frontmatter. The
// file's name is the command's name, so review.md becomes /review. The
// body is the text that enters the conversation, with the arguments
// substituted into it.
//
// The arguments are substituted with shell-like rules:
//
//	$1, $2, ...      one positional argument
//	$@, $ARGUMENTS  every argument, joined with spaces
//	${1:-default}   one argument, or a default when it is missing
//	${@:-default}   every argument, or a default when there are none
//	${@:N}          the arguments from the N-th on
//	${@:N:L}        L arguments, starting at the N-th
//
// The arguments themselves are split with shell quoting rules, so
// /review "API compatibility" is one argument, not two.
//
// The templates are data, not code: a template cannot run anything, it
// can only put text in front of the model. That is what makes a
// template file safe to take from somewhere else.
package prompts

import (
	"fmt"
	"strconv"
	"strings"
)

// SplitArgs splits a command's argument string into arguments.
//
// The splitting follows shell quoting rules: a double or single quote
// groups the words it covers, and a backslash escapes the next
// character. An unclosed quote is an error rather than a guess: a
// half-quoted argument would silently change the meaning of the
// command.
//
// The result keeps the empty arguments between two words, so
// `a "" b` is three arguments.
func SplitArgs(s string) ([]string, error) {
	var args []string
	var current strings.Builder
	// started tells an empty argument from a word that never began.
	started := false
	// quote is the quote character that is open, or zero.
	var quote byte

	flush := func() {
		if started {
			args = append(args, current.String())
			current.Reset()
			started = false
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			// Inside double quotes a backslash still escapes, so a
			// path with a quote in it can be written.
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				current.WriteByte(s[i])
				continue
			}
			current.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			started = true
		case c == '\\' && i+1 < len(s):
			i++
			current.WriteByte(s[i])
			started = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			current.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("prompts: unclosed %c quote", quote)
	}
	flush()
	return args, nil
}

// Expand substitutes the arguments into a template's body.
//
// The substitution is a single pass over the text, so a substituted
// argument is never itself expanded: the model's own output cannot
// turn into a new placeholder.
func Expand(body string, args []string) string {
	var out strings.Builder
	out.Grow(len(body))

	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '$' {
			out.WriteByte(c)
			continue
		}
		// A dollar at the end of the text is just a dollar.
		if i+1 >= len(body) {
			out.WriteByte(c)
			continue
		}

		switch {
		case body[i+1] == '{':
			end := strings.IndexByte(body[i+2:], '}')
			if end < 0 {
				// An unclosed brace is left alone: a template with a
				// stray brace should still expand its other
				// placeholders.
				out.WriteByte(c)
				continue
			}
			expr := body[i+2 : i+2+end]
			out.WriteString(expandBraced(expr, args))
			i = i + 2 + end
		case body[i+1] == '@':
			out.WriteString(joinArgs(args))
			i++
		case body[i+1] == 'A' && strings.HasPrefix(body[i+1:], "ARGUMENTS"):
			// The spelled-out form of $@. The "$" is at i, so the
			// name runs from i+1 to i+1+len("ARGUMENTS")-1.
			out.WriteString(joinArgs(args))
			i += len("ARGUMENTS")
		case isDigit(body[i+1]):
			// A positional argument. The number may have more than
			// one digit: $10 is the tenth argument.
			j := i + 1
			for j < len(body) && isDigit(body[j]) {
				j++
			}
			index, err := strconv.Atoi(body[i+1 : j])
			if err != nil {
				out.WriteByte(c)
				continue
			}
			out.WriteString(argument(args, index))
			i = j - 1
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// expandBraced substitutes a braced expression.
//
// The expression is a name, a name with a default, or a slice of the
// arguments.
func expandBraced(expr string, args []string) string {
	// The default form comes first: "@:-" also starts with "@:", and
	// the two mean different things.
	if strings.HasPrefix(expr, "@:-") {
		if len(args) == 0 {
			return expr[3:]
		}
		return joinArgs(args)
	}
	// The slice form: ${@:N} or ${@:N:L}.
	if strings.HasPrefix(expr, "@:") {
		rest := expr[2:]
		start := 0
		length := -1
		if colon := strings.IndexByte(rest, ':'); colon >= 0 {
			start = parseNumber(rest[:colon])
			length = parseNumber(rest[colon+1:])
		} else {
			start = parseNumber(rest)
		}
		return joinArgs(sliceArgs(args, start, length))
	}

	// The default form: ${name:-default}.
	name, fallback := expr, ""
	if colon := strings.Index(expr, ":-"); colon >= 0 {
		name = expr[:colon]
		fallback = expr[colon+2:]
	}

	switch name {
	case "@":
		if len(args) == 0 {
			return fallback
		}
		return joinArgs(args)
	case "$ARGUMENTS":
		if len(args) == 0 {
			return fallback
		}
		return joinArgs(args)
	default:
		if index, err := strconv.Atoi(name); err == nil {
			value := argument(args, index)
			if value == "" {
				return fallback
			}
			return value
		}
		// An expression that is neither a name nor a number is left
		// alone, so a template with a shell variable in it still
		// expands its other placeholders.
		return "${" + expr + "}"
	}
}

// argument returns the n-th argument, or an empty string when there is
// none.
func argument(args []string, index int) string {
	if index < 1 || index > len(args) {
		return ""
	}
	return args[index-1]
}

// sliceArgs returns the arguments from the start index, at most length
// of them.
//
// A length of -1 means "all of them". The indices are one-based, as
// the substitution syntax is.
func sliceArgs(args []string, start, length int) []string {
	if start < 1 {
		start = 1
	}
	if start > len(args) {
		return nil
	}
	rest := args[start-1:]
	if length >= 0 && length < len(rest) {
		rest = rest[:length]
	}
	return rest
}

// joinArgs renders the arguments the way $@ does: joined with spaces.
func joinArgs(args []string) string {
	return strings.Join(args, " ")
}

// parseNumber parses a number, returning zero when it is not one.
func parseNumber(s string) int {
	value, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return value
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}
