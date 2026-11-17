// Package tools defines the harness tool system, modeled after pi's
// registerTool: a tool is a name, a JSON Schema parameter description and
// an execute function. Built-in tools live alongside user tools in a
// Registry that controls which are exposed to the model.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// Update is streamed progress from a long-running tool execution.
type Update struct {
	Content string
}

// Result is the outcome of a tool execution. Content is returned to the
// model verbatim.
type Result struct {
	Content string
	IsError bool
	// ExitCode is the command's exit status, for the tools that run
	// commands. It is zero for the tools that do not.
	ExitCode int
}

// Tool is a callable tool definition (pi's ToolDefinition + execute).
type Tool struct {
	Name        string
	Label       string
	Description string
	// PromptSnippet is an optional one-liner surfaced in the system prompt.
	PromptSnippet string
	// PromptGuidelines are optional usage rules surfaced to the model.
	PromptGuidelines []string
	// Parameters is a JSON Schema object describing the arguments.
	Parameters map[string]any
	// Execute runs the tool. args is the raw JSON arguments object.
	// onUpdate may be called to stream progress; it may be nil.
	Execute func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error)
	// Renderers present the call and its result. They are optional:
	// a tool without them is shown with the generic rendering.
	Renderers *Renderers
}

// RenderersOf returns the tool's renderers, merged with the built-in
// ones for its name.
//
// A tool that brings its own renderer keeps it; the built-in one only
// fills the gap. It mirrors pi's withBuiltInRenderers.
func (t *Tool) RenderersOf() *Renderers {
	if t == nil {
		return nil
	}
	return t.Renderers.WithBuiltIn(BuiltInRenderer(t.Name))
}

// Definition converts the tool to its LLM-facing form.
func (t *Tool) Definition() llm.ToolDefinition {
	params := t.Parameters
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return llm.ToolDefinition{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  params,
	}
}

// Registry holds registered tools and the active-tool subset, mirroring
// pi.setActiveTools / getAllTools / getActiveTools.
type Registry struct {
	mu     sync.RWMutex
	tools  map[string]*Tool
	active map[string]bool // nil means "all active"
	// renderers are the renderer resolvers, tried in registration
	// order. They are how an extension takes over the presentation of
	// a tool it does not own.
	renderers []rendererEntry
	// nextRendererID identifies the resolvers, so one can be removed
	// again on a hot reload.
	nextRendererID int
}

// rendererEntry is one registered resolver. The id lets the registry
// remove it again on a hot reload, where a function value cannot be
// compared.
type rendererEntry struct {
	id       int
	resolver RendererResolver
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]*Tool{}}
}

// AddRenderer adds a renderer resolver.
//
// The resolvers are chained in registration order, the way pi's
// extension runner chains them: each one may return its own renderers
// or defer to the next, and the registered tool's own renderers are
// the fallback at the end of the chain.
//
// It returns a function that removes the resolver again, so a hot
// reload can undo what the old extension registered.
func (r *Registry) AddRenderer(resolver RendererResolver) func() {
	if resolver == nil {
		return func() {}
	}
	r.mu.Lock()
	r.nextRendererID++
	entry := rendererEntry{id: r.nextRendererID, resolver: resolver}
	r.renderers = append(r.renderers, entry)
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, e := range r.renderers {
			if e.id == entry.id {
				r.renderers = append(r.renderers[:i], r.renderers[i+1:]...)
				return
			}
		}
	}
}

// ResolveRenderers resolves the renderers of a tool.
//
// The chain runs the registered resolvers in order and ends with the
// tool's own renderers. A resolver that returns nil passes the
// question on.
func (r *Registry) ResolveRenderers(name string) *Renderers {
	r.mu.RLock()
	resolvers := make([]RendererResolver, 0, len(r.renderers))
	for _, e := range r.renderers {
		resolvers = append(resolvers, e.resolver)
	}
	r.mu.RUnlock()

	base := func() *Renderers {
		t, ok := r.Get(name)
		if !ok {
			return nil
		}
		return t.RenderersOf()
	}
	return ResolveRenderers(name, resolvers, base)
}

// Register adds a tool. Duplicate names are rejected.
func (r *Registry) Register(t *Tool) error {
	if t == nil || t.Name == "" {
		return fmt.Errorf("tools: tool must have a name")
	}
	if t.Execute == nil {
		return fmt.Errorf("tools: tool %q has no execute function", t.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("tools: tool %q already registered", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Unregister removes a tool by name. It is used when an extension is
// unloaded (hot reload) so its tools disappear from the model's tool
// list. Returns an error if the tool is unknown.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return fmt.Errorf("tools: no such tool %q", name)
	}
	delete(r.tools, name)
	// The tool may have been pinned as active; drop it from that set so
	// the active subset never names a missing tool. An empty (but
	// non-nil) set means "no tools active" — it must not degrade to
	// "all tools active", which is what a nil set means.
	if r.active != nil {
		delete(r.active, name)
	}
	return nil
}

// SetActiveTools restricts exposure to the named tools. Empty input
// exposes all registered tools.
func (r *Registry) SetActiveTools(names []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(names) == 0 {
		r.active = nil
		return nil
	}
	active := map[string]bool{}
	for _, n := range names {
		if _, ok := r.tools[n]; !ok {
			return fmt.Errorf("tools: cannot activate unknown tool %q", n)
		}
		active[n] = true
	}
	r.active = active
	return nil
}

// SetActiveToolsExact restricts exposure to exactly the named tools.
// Unlike SetActiveTools, an empty list means no tools, which lets an
// environment profile expose a deliberately tool-free agent.
func (r *Registry) SetActiveToolsExact(names []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := map[string]bool{}
	for _, n := range names {
		if _, ok := r.tools[n]; !ok {
			return fmt.Errorf("tools: cannot activate unknown tool %q", n)
		}
		active[n] = true
	}
	r.active = active
	return nil
}

// GetAllTools returns every registered tool, sorted by name.
func (r *Registry) GetAllTools() []*Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetActiveTools returns the tools currently exposed to the model.
func (r *Registry) GetActiveTools() []*Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		out := make([]*Tool, 0, len(r.tools))
		for _, t := range r.tools {
			out = append(out, t)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	out := make([]*Tool, 0, len(r.active))
	for name := range r.active {
		out = append(out, r.tools[name])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Definitions returns the LLM-facing definitions of active tools.
func (r *Registry) Definitions() []llm.ToolDefinition {
	active := r.GetActiveTools()
	defs := make([]llm.ToolDefinition, 0, len(active))
	for _, t := range active {
		defs = append(defs, t.Definition())
	}
	return defs
}
