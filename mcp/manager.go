package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/mimi/auto-harness/tools"
)

// Manager connects to the configured MCP servers and exposes their
// tools through the harness's tool registry.
//
// Every server tool is registered as mcp__<server>__<tool>, so tools
// from different servers cannot collide.
type Manager struct {
	mu      sync.RWMutex
	clients map[string]*Client
	// order keeps the server names in configuration order.
	order []string
	// registry is where the bridged tools are registered.
	registry *tools.Registry
	// onToolsChanged is called when a server's tool list changes.
	onToolsChanged func(server string)
}

// NewManager creates a manager that registers bridged tools in the
// given registry.
func NewManager(registry *tools.Registry) *Manager {
	return &Manager{
		clients:  map[string]*Client{},
		registry: registry,
	}
}

// SetToolsChangedHandler installs a callback invoked when a server
// announces a changed tool list.
func (m *Manager) SetToolsChangedHandler(fn func(server string)) {
	m.mu.Lock()
	m.onToolsChanged = fn
	m.mu.Unlock()
}

// Connect starts a server and registers its tools.
//
// A server that fails to connect is reported, but the others still
// connect: one broken server must not break the session.
func (m *Manager) Connect(ctx context.Context, cfg ServerConfig) error {
	if !cfg.IsEnabled() {
		return nil
	}
	client, err := Connect(ctx, cfg)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if _, exists := m.clients[cfg.Name]; !exists {
		m.order = append(m.order, cfg.Name)
	}
	m.clients[cfg.Name] = client
	m.mu.Unlock()

	for _, t := range client.Tools() {
		if err := m.registerTool(cfg.Name, client, t); err != nil {
			// A name collision with an existing tool is not fatal: the
			// rest of the server's tools still work.
			continue
		}
	}
	return nil
}

// ConnectAll connects every server in order, collecting the failures.
func (m *Manager) ConnectAll(ctx context.Context, configs []ServerConfig) error {
	var errs []error
	for _, cfg := range configs {
		if !cfg.IsEnabled() {
			continue
		}
		if err := m.Connect(ctx, cfg); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	msg := errs[0].Error()
	for _, e := range errs[1:] {
		msg += "; " + e.Error()
	}
	return fmt.Errorf("mcp: %s", msg)
}

// registerTool registers one server tool as a harness tool.
func (m *Manager) registerTool(server string, client *Client, t Tool) error {
	name := ToolName(server, t.Name)
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	tool := &tools.Tool{
		Name:        name,
		Label:       t.Title,
		Description: t.Description,
		Parameters:  schema,
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			parsed := map[string]any{}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &parsed); err != nil {
					return tools.Result{
						Content: fmt.Sprintf("invalid arguments for %s: %v", name, err),
						IsError: true,
					}, nil
				}
			}
			res, err := client.CallTool(ctx, t.Name, parsed)
			if err != nil {
				return tools.Result{Content: err.Error(), IsError: true}, nil
			}
			text := res.TextResult()
			if res.StructuredContent != nil && text == "" {
				text = string(res.StructuredContent)
			}
			return tools.Result{Content: text, IsError: res.IsError}, nil
		},
	}
	return m.registry.Register(tool)
}

// Servers returns the names of the connected servers, in configuration
// order.
func (m *Manager) Servers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// ResourceServers returns the connected servers that offer resources.
//
// The list is sorted by name, so the listing tools return a stable
// order.
func (m *Manager) ResourceServers() []ResourceServer {
	m.mu.RLock()
	names := make([]string, len(m.order))
	copy(names, m.order)
	m.mu.RUnlock()

	var out []ResourceServer
	for _, name := range names {
		c, ok := m.Client(name)
		if !ok || !c.HasResourcesCapability() {
			continue
		}
		out = append(out, ResourceServer{Name: name, Client: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RegisterResourceTools registers the resource tools: the two
// listings and the read.
//
// The tools cover every connected server with resources, so the model
// never has to name a server to find out what is there. A server that
// cannot be listed is reported in the listing's errors, the way pi
// does, so one broken server does not hide the others.
func (m *Manager) RegisterResourceTools() error {
	if len(m.ResourceServers()) == 0 {
		return nil
	}
	for _, tool := range m.resourceTools() {
		if err := m.registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// resourceTools builds the three resource tools.
func (m *Manager) resourceTools() []*tools.Tool {
	stringProperty := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	listParams := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": stringProperty("MCP server name. Omit to list every server with resources."),
			"cursor": stringProperty("Opaque cursor from a previous call with the same server; omit for the first page."),
		},
	}
	readParams := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": stringProperty("MCP server name exactly as configured."),
			"uri":    stringProperty("Resource URI to read."),
		},
		"required": []string{"server", "uri"},
	}

	list := func(kind string) *tools.Tool {
		return &tools.Tool{
			Name:        ResourceToolName(kind),
			Label:       "List MCP " + kind,
			Description: "Lists the " + kind + " provided by the connected MCP servers. Resources let a server share data that gives the model context: files, schemas or application data.",
			Parameters:  listParams,
			Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
				parsed := tools.DecodeArgs(args)
				server := tools.ArgString(parsed, "server")
				listing, err := m.listResources(ctx, kind, server)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				payload, err := json.MarshalIndent(listing, "", "  ")
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				return tools.Result{Content: string(payload)}, nil
			},
		}
	}

	return []*tools.Tool{
		list("resources"),
		list("resource_templates"),
		{
			Name:        ReadResourceTool,
			Label:       "Read MCP Resource",
			Description: "Reads one resource of an MCP server, given the server name and the resource URI.",
			Parameters:  readParams,
			Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
				parsed := tools.DecodeArgs(args)
				server := tools.ArgString(parsed, "server")
				uri := tools.ArgString(parsed, "uri")
				if server == "" {
					return tools.Result{Content: "server must be provided", IsError: true}, nil
				}
				if uri == "" {
					return tools.Result{Content: "uri must be provided", IsError: true}, nil
				}
				client, ok := m.Client(server)
				if !ok {
					return tools.Result{Content: fmt.Sprintf("no MCP server named %q", server), IsError: true}, nil
				}
				result, err := client.ReadResource(ctx, uri)
				if err != nil {
					return tools.Result{Content: err.Error(), IsError: true}, nil
				}
				return tools.Result{Content: result.TextResult()}, nil
			},
		},
	}
}

// listResources lists the resources or the templates of one server, or
// of every server with resources.
func (m *Manager) listResources(ctx context.Context, kind, server string) (ResourceListing, error) {
	listing := ResourceListing{Server: server}

	if server != "" {
		client, ok := m.Client(server)
		if !ok {
			return listing, fmt.Errorf("no MCP server named %q", server)
		}
		if kind == "resources" {
			resources, err := client.ListResources(ctx)
			if err != nil {
				return listing, err
			}
			for _, r := range resources {
				listing.Resources = append(listing.Resources, listedResource(server, r))
			}
			return listing, nil
		}
		templates, err := client.ListResourceTemplates(ctx)
		if err != nil {
			return listing, err
		}
		for _, t := range templates {
			listing.ResourceTemplates = append(listing.ResourceTemplates, ListedResourceTemplate{
				Server:      server,
				URITemplate: t.URITemplate,
				Name:        t.Name,
				Title:       t.Title,
				Description: t.Description,
				MimeType:    t.MimeType,
			})
		}
		return listing, nil
	}

	// Every server. One that fails is reported, the others are still
	// listed.
	for _, s := range m.ResourceServers() {
		if kind == "resources" {
			resources, err := s.Client.ListResources(ctx)
			if err != nil {
				listing.Errors = append(listing.Errors, ListingError{Server: s.Name, Error: err.Error()})
				continue
			}
			for _, r := range resources {
				listing.Resources = append(listing.Resources, listedResource(s.Name, r))
			}
			continue
		}
		templates, err := s.Client.ListResourceTemplates(ctx)
		if err != nil {
			listing.Errors = append(listing.Errors, ListingError{Server: s.Name, Error: err.Error()})
			continue
		}
		for _, t := range templates {
			listing.ResourceTemplates = append(listing.ResourceTemplates, ListedResourceTemplate{
				Server:      s.Name,
				URITemplate: t.URITemplate,
				Name:        t.Name,
				Title:       t.Title,
				Description: t.Description,
				MimeType:    t.MimeType,
			})
		}
	}
	return listing, nil
}

// listedResource tags a resource with its server.
func listedResource(server string, r Resource) ListedResource {
	return ListedResource{
		Server:      server,
		URI:         r.URI,
		Name:        r.Name,
		Title:       r.Title,
		Description: r.Description,
		MimeType:    r.MimeType,
		Size:        r.Size,
	}
}

// Client returns the connection to a server.
func (m *Manager) Client(name string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[name]
	return c, ok
}

// ToolCount returns the number of tools a server offers.
func (m *Manager) ToolCount(name string) int {
	c, ok := m.Client(name)
	if !ok {
		return 0
	}
	return len(c.Tools())
}

// Status describes a server's state for the /mcp listing.
type Status struct {
	Name        string
	Connected   bool
	Tools       int
	Description string
	Error       error
}

// Statuses returns the state of every configured server, sorted by
// name.
func (m *Manager) Statuses() []Status {
	m.mu.RLock()
	names := make([]string, len(m.order))
	copy(names, m.order)
	m.mu.RUnlock()

	out := make([]Status, 0, len(names))
	for _, n := range names {
		c, ok := m.Client(n)
		if !ok {
			continue
		}
		out = append(out, Status{
			Name:        n,
			Connected:   true,
			Tools:       len(c.Tools()),
			Description: c.Description(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close disconnects every server.
func (m *Manager) Close() {
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.order = nil
	m.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}
