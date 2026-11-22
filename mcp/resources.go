package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// Protocol method names for the resource methods.
const (
	// MethodResourcesList lists a server's resources.
	MethodResourcesList = "resources/list"
	// MethodResourcesRead reads one resource.
	MethodResourcesRead = "resources/read"
	// MethodResourceTemplatesList lists a server's resource templates.
	MethodResourceTemplatesList = "resources/templates/list"
)

// Resource is a resource a server lists in resources/list.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        *int   `json:"size,omitempty"`
}

// ResourceTemplate is a family of resources, addressed by an RFC 6570
// URI template, from resources/templates/list.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ListResourcesResult is the response to resources/list.
type ListResourcesResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ListResourceTemplatesResult is the response to
// resources/templates/list.
type ListResourceTemplatesResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}

// ReadResourceParams are the parameters of resources/read.
type ReadResourceParams struct {
	URI string `json:"uri"`
}

// ResourceContents is the content of one resource.
//
// A resource carries either text or a base64 blob, never both.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ReadResourceResult is the response to resources/read.
type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
}

// TextResult renders the resource's contents as plain text for the
// model.
//
// Text contents are returned verbatim. A binary resource is described
// rather than dumped: the harness's message model carries text only,
// and a base64 blob would cost the model a fortune to read.
func (r *ReadResourceResult) TextResult() string {
	var b strings.Builder
	for i, c := range r.Contents {
		if i > 0 {
			b.WriteString("\n")
		}
		switch {
		case c.Text != "":
			b.WriteString(c.Text)
		case c.Blob != "":
			b.WriteString(fmt.Sprintf("[binary resource %s omitted (%d bytes)]", c.URI, len(c.Blob)))
		default:
			b.WriteString(fmt.Sprintf("[empty resource %s]", c.URI))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ExtractImages decodes image blobs (PNG, JPEG, WebP, GIF) from the
// resource result, returning both a model-friendly text summary and the
// decoded llm.Image attachments. Non-image binary blobs remain omitted.
func (r *ReadResourceResult) ExtractImages() (string, []llm.Image) {
	var b strings.Builder
	var images []llm.Image
	for i, c := range r.Contents {
		if i > 0 {
			b.WriteString("\n")
		}
		switch {
		case c.Text != "":
			b.WriteString(c.Text)
		case c.Blob != "" && llm.IsSupportedImageMIME(c.MimeType):
			raw, err := base64.StdEncoding.DecodeString(c.Blob)
			if err != nil {
				b.WriteString(fmt.Sprintf("[image resource %s error: invalid base64: %v]", c.URI, err))
				continue
			}
			img, err := llm.ImageFromBytes(raw, c.MimeType, c.URI)
			if err != nil {
				b.WriteString(fmt.Sprintf("[image resource %s error: %v]", c.URI, err))
				continue
			}
			images = append(images, img)
			b.WriteString(fmt.Sprintf("[image resource %s (%s, %dx%d)]", c.URI, img.MediaType, img.Width, img.Height))
		case c.Blob != "":
			b.WriteString(fmt.Sprintf("[binary resource %s omitted (%d bytes)]", c.URI, len(c.Blob)))
		default:
			b.WriteString(fmt.Sprintf("[empty resource %s]", c.URI))
		}
	}
	return strings.TrimRight(b.String(), "\n"), images
}

// ListResources lists the server's resources, following pagination.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var all []Resource
	cursor := ""
	for {
		var page ListResourcesResult
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		if err := c.transport.Call(ctx, MethodResourcesList, params, &page); err != nil {
			return nil, fmt.Errorf("mcp: %s: resources/list: %w", c.config.Name, err)
		}
		all = append(all, page.Resources...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

// ListResourceTemplates lists the server's resource templates,
// following pagination.
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	var all []ResourceTemplate
	cursor := ""
	for {
		var page ListResourceTemplatesResult
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		if err := c.transport.Call(ctx, MethodResourceTemplatesList, params, &page); err != nil {
			return nil, fmt.Errorf("mcp: %s: resources/templates/list: %w", c.config.Name, err)
		}
		all = append(all, page.ResourceTemplates...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

// ReadResource reads one resource.
func (c *Client) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error) {
	timeout := time.Duration(c.config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var result ReadResourceResult
	err := c.transport.Call(ctx, MethodResourcesRead, ReadResourceParams{URI: uri}, &result)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: resources/read %s: %w", c.config.Name, uri, err)
	}
	return &result, nil
}

// ResourceServer is a connected server that offers resources.
type ResourceServer struct {
	// Name is the server's name, as configured.
	Name string
	// Client is the connection to the server.
	Client *Client
}

// ResourceToolName returns the name of the tool that lists a
// server's resources.
//
// The names follow pi's, so a model trained on the Codex tools uses
// them unchanged.
func ResourceToolName(kind string) string {
	switch kind {
	case "resources":
		return ListResourcesTool
	case "resource_templates":
		return ListResourceTemplatesTool
	default:
		return "list_mcp_" + kind
	}
}

// The names of the resource tools.
const (
	// ListResourcesTool lists the resources of every server.
	ListResourcesTool = "list_mcp_resources"
	// ListResourceTemplatesTool lists the resource templates of every
	// server.
	ListResourceTemplatesTool = "list_mcp_resource_templates"
	// ReadResourceTool reads one resource of one server.
	ReadResourceTool = "read_mcp_resource"
)

// ListedResource is one resource in a listing, tagged with its
// server.
type ListedResource struct {
	Server      string `json:"server"`
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        *int   `json:"size,omitempty"`
}

// ListedResourceTemplate is one resource template in a listing,
// tagged with its server.
type ListedResourceTemplate struct {
	Server      string `json:"server"`
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceListing is the JSON a listing tool returns.
type ResourceListing struct {
	Server            string                   `json:"server,omitempty"`
	Resources         []ListedResource         `json:"resources"`
	ResourceTemplates []ListedResourceTemplate `json:"resourceTemplates,omitempty"`
	Errors            []ListingError           `json:"errors,omitempty"`
}

// ListingError reports a server that could not be listed.
type ListingError struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

// MarshalJSON renders the listing as the JSON the model reads.
func (l ResourceListing) MarshalJSON() ([]byte, error) {
	type alias ResourceListing
	out := alias(l)
	if out.Resources == nil {
		out.Resources = []ListedResource{}
	}
	return json.Marshal(out)
}
