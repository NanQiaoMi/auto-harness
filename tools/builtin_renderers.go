package tools

import (
	"fmt"
	"strings"
)

// previewLines is how many lines of a tool's output the collapsed
// view shows.
const previewLines = 10

// BuiltInRenderers returns the renderers of the built-in tools, keyed
// by tool name.
//
// The presentation lives apart from the tools themselves, the way it
// does in pi: a process that only displays tool output does not need
// the execution path.
func BuiltInRenderers() map[string]*Renderers {
	return map[string]*Renderers{
		"read":  readRenderers(),
		"write": writeRenderers(),
		"edit":  editRenderers(),
		"bash":  shellRenderers("$"),
	}
}

// BuiltInRenderer returns the built-in renderers of one tool, or nil
// when the tool has none.
func BuiltInRenderer(name string) *Renderers {
	return BuiltInRenderers()[name]
}

// readRenderers renders the read tool.
func readRenderers() *Renderers {
	return &Renderers{
		RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
			path := ArgPath(args)
			rangeText := readRange(args)
			if path == "" {
				return []string{"read (no path)"}, nil
			}
			return []string{"read " + path + rangeText}, nil
		},
		RenderResult: func(result Result, options RenderResultOptions, ctx RenderContext) ([]string, error) {
			if result.IsError {
				return resultLines(result.Content), nil
			}
			// The collapsed view hides the content: the model just
			// read it, the user did not ask to see it.
			if !options.Expanded {
				return nil, nil
			}
			return resultLines(result.Content), nil
		},
	}
}

// readRange describes the line range a read covers.
func readRange(args map[string]any) string {
	offset := ArgInt(args, "offset")
	limit := ArgInt(args, "limit")
	switch {
	case offset > 0 && limit > 0:
		return fmt.Sprintf(" (lines %d-%d)", offset, offset+limit-1)
	case offset > 0:
		return fmt.Sprintf(" (from line %d)", offset)
	case limit > 0:
		return fmt.Sprintf(" (first %d lines)", limit)
	default:
		return ""
	}
}

// writeRenderers renders the write tool.
func writeRenderers() *Renderers {
	return &Renderers{
		RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
			path := ArgPath(args)
			if path == "" {
				return []string{"write (no path)"}, nil
			}
			return []string{"write " + path}, nil
		},
		RenderResult: func(result Result, options RenderResultOptions, ctx RenderContext) ([]string, error) {
			if result.IsError {
				return resultLines(result.Content), nil
			}
			return resultLines(result.Content), nil
		},
	}
}

// editRenderers renders the edit tool.
//
// The call shows the diff the edit will make, which is the part the
// user actually wants to see.
func editRenderers() *Renderers {
	return &Renderers{
		RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
			path := ArgPath(args)
			if path == "" {
				return []string{"edit (no path)"}, nil
			}
			return []string{"edit " + path}, nil
		},
		RenderResult: func(result Result, options RenderResultOptions, ctx RenderContext) ([]string, error) {
			if result.IsError {
				return resultLines(result.Content), nil
			}
			return resultLines(result.Content), nil
		},
	}
}

// shellRenderers renders the shell tools.
//
// The bash and powershell tools differ only in the prompt they show.
func shellRenderers(prompt string) *Renderers {
	return &Renderers{
		RenderCall: func(args map[string]any, ctx RenderContext) ([]string, error) {
			command := ArgString(args, "command")
			if command == "" {
				return []string{prompt + " (no command)"}, nil
			}
			return []string{prompt + " " + command}, nil
		},
		RenderResult: func(result Result, options RenderResultOptions, ctx RenderContext) ([]string, error) {
			output := strings.TrimRight(result.Content, "\n")
			if output == "" {
				return nil, nil
			}
			if !options.Expanded {
				return previewLinesOf(output, previewLines), nil
			}
			return resultLines(output), nil
		},
	}
}

// resultLines splits a tool's output into display lines, dropping the
// trailing newline.
func resultLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(content, "\n"), "\n")
}

// previewLinesOf returns the last lines of the output, with a marker
// saying how many were hidden.
func previewLinesOf(output string, limit int) []string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) <= limit {
		return lines
	}
	hidden := len(lines) - limit
	kept := lines[hidden:]
	out := make([]string, 0, len(kept)+1)
	out = append(out, fmt.Sprintf("... (%d earlier lines)", hidden))
	out = append(out, kept...)
	return out
}
