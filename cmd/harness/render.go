package main

import (
	"fmt"
	"strings"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// toolNotice renders a tool call and its result for display.
//
// The renderers decide what is shown: an extension that registers a
// renderer for a tool takes over its presentation. A tool without a
// renderer falls back to the generic notice: the tool's name and its
// arguments.
//
// The rendering never fails the call: a renderer that errors falls
// back to the generic notice, so a broken renderer cannot break a
// session.
type toolNotice struct {
	// registry resolves the renderers of a tool.
	registry *tools.Registry
	// cwd is the working directory the calls run in.
	cwd string
}

// call renders the notice of a tool call.
//
// The lines are the call's own; the caller decides how to print them.
func (n *toolNotice) call(call *llm.ToolCall) []string {
	if call == nil {
		return nil
	}
	renderers := n.registry.ResolveRenderers(call.Name)
	if renderers == nil || renderers.RenderCall == nil {
		return []string{fmt.Sprintf("• %s %s", call.Name, compactArgs(call.Arguments))}
	}
	args := tools.DecodeArgs(call.Arguments)
	ctx := tools.RenderContext{
		Args:       args,
		ToolCallID: call.ID,
		CWD:        n.cwd,
	}
	lines, err := renderers.RenderCall(args, ctx)
	if err != nil || len(lines) == 0 {
		return []string{fmt.Sprintf("• %s %s", call.Name, compactArgs(call.Arguments))}
	}
	return lines
}

// result renders the notice of a tool result.
//
// It returns nil when the tool's renderer shows nothing, which is the
// common case: a result the user did not ask to see stays hidden.
func (n *toolNotice) result(call *llm.ToolCall, result *tools.Result, expanded bool) []string {
	if call == nil || result == nil {
		return nil
	}
	renderers := n.registry.ResolveRenderers(call.Name)
	if renderers == nil || renderers.RenderResult == nil {
		if !expanded {
			return nil
		}
		return strings.Split(strings.TrimRight(result.Content, "\n"), "\n")
	}
	args := tools.DecodeArgs(call.Arguments)
	ctx := tools.RenderContext{
		Args:       args,
		ToolCallID: call.ID,
		CWD:        n.cwd,
		IsError:    result.IsError,
		Expanded:   expanded,
	}
	lines, err := renderers.RenderResult(*result, tools.RenderResultOptions{Expanded: expanded}, ctx)
	if err != nil {
		return nil
	}
	return lines
}

// install registers the notice handlers on the bus.
//
// The call notice is shown when the tool starts; the result notice is
// shown when it ends. The result is only shown in the expanded mode,
// so a quiet session stays quiet.
func (n *toolNotice) install(bus *ext.Bus, quiet bool, expanded func() bool, write func(string)) {
	bus.On(ext.EventToolExecutionStart, func(e *ext.Event) error {
		if quiet || e.ToolCall == nil || ext.AgentID(e) != "" {
			return nil
		}
		for _, line := range n.call(e.ToolCall) {
			write(line)
		}
		return nil
	})
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if quiet || e.ToolCall == nil || e.ToolResult == nil || ext.AgentID(e) != "" {
			return nil
		}
		lines := n.result(e.ToolCall, e.ToolResult, expanded())
		if len(lines) == 0 {
			return nil
		}
		for _, line := range lines {
			write(line)
		}
		return nil
	})
}
