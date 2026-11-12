package tools

import (
	"encoding/json"
)

// RenderResultOptions carries the state of the result being rendered.
type RenderResultOptions struct {
	// Expanded asks for the full output instead of a preview.
	Expanded bool
}

// RenderContext carries everything a renderer may look at.
//
// It mirrors pi's ToolRenderContext: the arguments of the call and
// the state of the display.
type RenderContext struct {
	// Args is the decoded arguments of the call.
	Args map[string]any
	// ToolCallID identifies the call.
	ToolCallID string
	// CWD is the working directory the call runs in.
	CWD string
	// Expanded asks for the full output.
	Expanded bool
	// IsError reports that the call failed.
	IsError bool
}

// RenderCall renders a tool call: the line shown when the tool starts.
type RenderCall func(args map[string]any, ctx RenderContext) ([]string, error)

// RenderResult renders a tool result: the lines shown when it ends.
type RenderResult func(result Result, options RenderResultOptions, ctx RenderContext) ([]string, error)

// Renderers are the presentation hooks of a tool (pi's ToolRenderers).
type Renderers struct {
	// RenderCall renders the call.
	RenderCall RenderCall
	// RenderResult renders the result.
	RenderResult RenderResult
}

// WithBuiltIn fills the missing hooks from the built-in renderers.
//
// A tool that supplies its own renderer keeps it; the built-in one
// only fills the gap. It mirrors pi's withBuiltInRenderers.
func (r *Renderers) WithBuiltIn(builtIn *Renderers) *Renderers {
	if builtIn == nil {
		return r
	}
	if r == nil {
		return builtIn
	}
	out := *r
	if out.RenderCall == nil {
		out.RenderCall = builtIn.RenderCall
	}
	if out.RenderResult == nil {
		out.RenderResult = builtIn.RenderResult
	}
	return &out
}

// RendererResolver chooses the renderers of a tool.
//
// It mirrors pi's ToolRendererResolver: the resolver may return its
// own renderers, or call next to get the ones the following
// resolvers (and finally the registered tool) would use.
type RendererResolver func(toolName string, next func() *Renderers) *Renderers

// ResolveRenderers resolves the renderers of a tool through a chain of
// resolvers.
//
// The resolvers are tried in order; each one may return its own
// renderers or defer to the next. The base function is the fallback:
// it returns the renderers the registered tool would use.
//
// It mirrors pi's resolveToolRenderers, where the tool name reaches
// every resolver so it can pick the tools it cares about.
func ResolveRenderers(toolName string, resolvers []RendererResolver, base func() *Renderers) *Renderers {
	var resolve func(index int) *Renderers
	resolve = func(index int) *Renderers {
		if index >= len(resolvers) {
			if base == nil {
				return nil
			}
			return base()
		}
		return resolvers[index](toolName, func() *Renderers { return resolve(index + 1) })
	}
	return resolve(0)
}

// DecodeArgs decodes a tool call's raw JSON arguments.
//
// It is exported so the renderers and the extensions can read the
// arguments of a call without repeating the decoding.
func DecodeArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ArgString reads a string argument, returning "" when it is absent
// or of another type.
func ArgString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

// ArgInt reads an integer argument, returning 0 when it is absent or of
// another type.
func ArgInt(args map[string]any, key string) int {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(n)
	default:
		return 0
	}
}

// ArgPath reads the path argument of a file tool.
//
// It accepts "path" and "file_path", the two names the built-in tools
// and pi's tools use.
func ArgPath(args map[string]any) string {
	if p := ArgString(args, "path"); p != "" {
		return p
	}
	return ArgString(args, "file_path")
}
