package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ServerConfig describes an MCP server to connect to.
//
// A server is either a stdio server (Command set) or an HTTP server
// (URL set).
type ServerConfig struct {
	// Name identifies the server; it namespaces its tools.
	Name string `json:"name"`
	// Command is the executable of a stdio server.
	Command string `json:"command,omitempty"`
	// Args are the command's arguments.
	Args []string `json:"args,omitempty"`
	// Env adds environment variables to the server process.
	Env map[string]string `json:"env,omitempty"`
	// CWD is the server's working directory.
	CWD string `json:"cwd,omitempty"`
	// URL is the endpoint of an HTTP server.
	URL string `json:"url,omitempty"`
	// Headers are sent with every HTTP request.
	Headers map[string]string `json:"headers,omitempty"`
	// Type selects the transport: "stdio" or "http". It is optional:
	// a command selects stdio and a URL selects HTTP.
	Type string `json:"type,omitempty"`
	// Description explains what the server offers.
	Description string `json:"description,omitempty"`
	// Enabled is false to keep the entry without connecting.
	Enabled *bool `json:"enabled,omitempty"`
	// Timeout is the per-request timeout in seconds (default 60).
	Timeout int `json:"timeout,omitempty"`
}

// IsEnabled reports whether the server should be connected.
func (c ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Client is a connection to one MCP server.
type Client struct {
	config ServerConfig
	// transport carries the JSON-RPC messages.
	transport Transport

	mu     sync.Mutex
	info   InitializeResult
	tools  []Tool
	closed bool
	// stderr collects the server's diagnostics, shown when a
	// connection fails.
	stderr *strings.Builder
}

// Connect starts a server and completes the MCP handshake.
//
// The handshake is: initialize, then the initialized notification, then
// the tool list.
func Connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("mcp: server needs a name")
	}
	if cfg.Type == "sse" {
		return nil, fmt.Errorf("mcp: %s: the SSE transport is not supported", cfg.Name)
	}

	var transport Transport
	switch {
	case cfg.URL != "" && cfg.Type != "stdio":
		transport = NewHTTPTransport(cfg.URL, cfg.Headers)
	case cfg.Command != "":
		t, err := NewStdioTransport(ctx, cfg.Command, cfg.Args, envSlice(cfg.Env), cfg.CWD)
		if err != nil {
			return nil, fmt.Errorf("mcp: %s: %w", cfg.Name, err)
		}
		transport = t
	default:
		return nil, fmt.Errorf("mcp: %s: needs a command or a url", cfg.Name)
	}

	c := &Client{config: cfg, transport: transport, stderr: &strings.Builder{}}
	if err := c.handshake(ctx); err != nil {
		transport.Close()
		return nil, err
	}
	return c, nil
}

// envSlice converts an env map into the "KEY=VALUE" form exec wants.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// handshake runs the MCP initialization sequence.
func (c *Client) handshake(ctx context.Context) error {
	timeout := time.Duration(c.config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var result InitializeResult
	err := c.transport.Call(ctx, MethodInitialize, InitializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientInfo: Implementation{
			Name:    "auto-harness",
			Title:   "auto-harness coding agent",
			Version: "0.1.5",
		},
	}, &result)
	if err != nil {
		return fmt.Errorf("mcp: %s: initialize: %w", c.config.Name, err)
	}
	c.mu.Lock()
	c.info = result
	c.mu.Unlock()

	// The initialized notification tells the server the handshake is
	// done and it may start sending requests.
	if err := c.transport.Notify(ctx, MethodInitialized, nil); err != nil {
		return fmt.Errorf("mcp: %s: initialized: %w", c.config.Name, err)
	}

	// Load the tool list.
	if err := c.refreshTools(ctx); err != nil {
		return err
	}
	return nil
}

// refreshTools lists the server's tools, following pagination.
func (c *Client) refreshTools(ctx context.Context) error {
	var all []Tool
	cursor := ""
	for {
		var page ToolListResult
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		if err := c.transport.Call(ctx, MethodToolsList, params, &page); err != nil {
			return fmt.Errorf("mcp: %s: tools/list: %w", c.config.Name, err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	c.mu.Lock()
	c.tools = all
	c.mu.Unlock()
	return nil
}

// Tools returns the server's tools.
func (c *Client) Tools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// ServerInfo returns the server's identity from the handshake.
func (c *Client) ServerInfo() Implementation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.ServerInfo
}

// HasToolsCapability reports whether the server declared the tools
// capability during the handshake.
func (c *Client) HasToolsCapability() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.HasCapability("tools")
}

// HasResourcesCapability reports whether the server declared the
// resources capability.
func (c *Client) HasResourcesCapability() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.HasCapability("resources")
}

// Instructions returns the server's instructions, if any.
func (c *Client) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.Instructions
}

// Description returns the server's description, falling back to the
// first line of its instructions.
func (c *Client) Description() string {
	if c.config.Description != "" {
		return c.config.Description
	}
	if instr := c.Instructions(); instr != "" {
		if i := strings.IndexByte(instr, '\n'); i >= 0 {
			return instr[:i]
		}
		return instr
	}
	return ""
}

// CallTool invokes a tool on the server.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	timeout := time.Duration(c.config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var result CallToolResult
	err := c.transport.Call(ctx, MethodToolsCall, CallToolParams{Name: name, Arguments: args}, &result)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: tools/call %s: %w", c.config.Name, name, err)
	}
	return &result, nil
}

// Close disconnects from the server.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.transport.Close()
}
