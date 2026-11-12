// Package mcp implements a Model Context Protocol client: it connects
// to MCP servers and exposes their tools to the harness.
//
// The protocol is JSON-RPC 2.0 over a transport. Two transports are
// supported: stdio (a child process speaking newline-delimited JSON)
// and streamable HTTP.
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProtocolVersion is the MCP revision this client speaks. Servers
// negotiate the version during initialization; a server that asks for a
// different version still works, since the differences are additive.
const ProtocolVersion = "2025-06-18"

// Protocol method names.
const (
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodToolsList   = "tools/list"
	MethodToolsCall   = "tools/call"
	MethodPing        = "ping"
	// MethodToolsListChanged is sent by servers whose tool list changed.
	MethodToolsListChanged = "notifications/tools/list_changed"
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (a request without an id).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error payload of a JSON-RPC response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp: %d %s", e.Code, e.Message)
}

// InitializeParams are the parameters of the initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    Capabilities   `json:"capabilities"`
	ClientInfo      Implementation `json:"clientInfo"`
}

// Capabilities describes what the client supports.
type Capabilities struct {
	Roots struct {
		ListChanged bool `json:"listChanged"`
	} `json:"roots"`
}

// Implementation identifies a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

// HasCapability reports whether the server declared a capability.
func (r *InitializeResult) HasCapability(name string) bool {
	_, ok := r.Capabilities[name]
	return ok
}

// ToolListResult is the response to tools/list.
type ToolListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Tool is a tool offered by an MCP server.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
	// OutputSchema is the optional JSON Schema of the result.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// Annotations are hints about the tool's behavior. They are
	// untrusted metadata from the server.
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are the optional behavior hints of a tool.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// CallToolParams are the parameters of tools/call.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// CallToolResult is the response to tools/call.
type CallToolResult struct {
	// Content holds the result blocks (text, image, resource).
	Content []Content `json:"content"`
	// StructuredContent is the optional machine-readable result.
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	// IsError marks a failed call.
	IsError bool `json:"isError,omitempty"`
}

// Content is one block of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// TextResult renders a call result as plain text for the model.
//
// Text blocks are concatenated; image and other binary blocks are
// described, since the harness's message model carries text only.
func (r *CallToolResult) TextResult() string {
	var b strings.Builder
	for _, c := range r.Content {
		switch c.Type {
		case "text", "":
			b.WriteString(c.Text)
			b.WriteString("\n")
		case "image":
			b.WriteString("[image content omitted]\n")
		case "resource":
			b.WriteString("[resource ")
			if c.URI != "" {
				b.WriteString(c.URI)
			}
			b.WriteString(" omitted]\n")
		default:
			b.WriteString("[" + c.Type + " content omitted]\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ToolName returns the harness-facing name of a server tool.
//
// MCP servers are namespaced so tools from different servers cannot
// collide: mcp__<server>__<tool>. Characters outside letters, digits
// and underscore become underscores.
func ToolName(server, tool string) string {
	return "mcp__" + sanitizeName(server) + "__" + sanitizeName(tool)
}

// sanitizeName replaces every character that is not a letter, digit or
// underscore with an underscore.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
