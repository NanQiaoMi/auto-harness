package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicProvider speaks the Anthropic Messages API.
type AnthropicProvider struct {
	APIKey     string
	BaseURL    string // defaults to https://api.anthropic.com
	HTTPClient *http.Client
	// APIVersion is the anthropic-version header (default 2023-06-01).
	APIVersion   string
	ExtraHeaders map[string]string
	// Hooks intercepts provider traffic (see llm.Hooks).
	Hooks *Hooks
}

func NewAnthropicProvider(apiKey, baseURL string) *AnthropicProvider {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &AnthropicProvider{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 0},
		APIVersion: "2023-06-01",
	}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

// anthropicRequest is the Messages API wire format.
type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicEvent is the SSE event payload shape.
type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index,omitempty"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	ContentBlock *struct {
		Type     string          `json:"type"`
		ID       string          `json:"id,omitempty"`
		Name     string          `json:"name,omitempty"`
		Text     string          `json:"text,omitempty"`
		Thinking string          `json:"thinking,omitempty"`
		Input    json.RawMessage `json:"input,omitempty"`
	} `json:"content_block,omitempty"`
	Message *struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message,omitempty"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// Chat implements Provider.
func (p *AnthropicProvider) Chat(ctx context.Context, req ChatRequest) (Stream, error) {
	if p.Hooks != nil && p.Hooks.BeforeRequest != nil {
		p.Hooks.BeforeRequest(p.Name(), &req)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	wire := anthropicRequest{
		Model:     req.Model,
		System:    req.System,
		MaxTokens: maxTokens,
		Stream:    true,
	}
	if req.Thinking {
		wire.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: 4096}
	}
	// Anthropic requires messages to alternate starting with "user"; we
	// fold consecutive same-role messages by merging their blocks.
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			wire.Messages = appendMessage(wire.Messages, "user", anthropicBlock{Type: "text", Text: m.Content})
		case RoleAssistant:
			blocks := []anthropicBlock{}
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: tc.Arguments,
				})
			}
			for _, b := range blocks {
				wire.Messages = appendMessage(wire.Messages, "assistant", b)
			}
		case RoleTool:
			wire.Messages = appendMessage(wire.Messages, "user", anthropicBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
				IsError:   m.IsError,
			})
		}
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", UserAgent)
	httpReq.Header.Set("x-api-key", p.APIKey)
	// Gateways that front Anthropic-compatible APIs (e.g. OpenCode Go)
	// authenticate with Bearer instead of x-api-key; send both.
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	httpReq.Header.Set("anthropic-version", p.APIVersion)
	for k, v := range p.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}
	if p.Hooks != nil && p.Hooks.BeforeHeaders != nil {
		headers := map[string]string{}
		for k := range httpReq.Header {
			headers[k] = httpReq.Header.Get(k)
		}
		p.Hooks.BeforeHeaders(p.Name(), headers)
		for k, v := range headers {
			httpReq.Header.Set(k, v)
		}
	}

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if p.Hooks != nil && p.Hooks.AfterResponse != nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), resp.Body))
		p.Hooks.AfterResponse(p.Name(), resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &anthropicStream{body: resp.Body, scanner: scanner}, nil
}

// appendMessage merges block into the last message when it has the same
// role, otherwise appends a new message (Anthropic wants alternating roles).
func appendMessage(msgs []anthropicMessage, role string, block anthropicBlock) []anthropicMessage {
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, block)
		return msgs
	}
	return append(msgs, anthropicMessage{Role: role, Content: []anthropicBlock{block}})
}

type anthropicStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner

	// assembly state for the current content block
	blockType string // "text" | "tool_use" | "thinking"
	toolID    string
	toolName  string
	toolJSON  strings.Builder
	text      strings.Builder
	reason    string
	usage     Usage
	done      bool
	closed    bool
	pending   []*ToolCall
}

func (s *anthropicStream) Close() error {
	if !s.closed {
		s.closed = true
		return s.body.Close()
	}
	return nil
}

func (s *anthropicStream) Recv() (StreamEvent, error) {
	if s.done {
		return StreamEvent{}, io.EOF
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		var data string
		if strings.HasPrefix(line, "event:") {
			continue // event name line; the data line carries the payload
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		} else {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return StreamEvent{}, fmt.Errorf("anthropic: bad stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				s.usage.InputTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			s.blockType = ev.ContentBlock.Type
			switch ev.ContentBlock.Type {
			case "tool_use":
				s.toolID = ev.ContentBlock.ID
				s.toolName = ev.ContentBlock.Name
				s.toolJSON.Reset()
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					s.text.WriteString(ev.Delta.Text)
					return StreamEvent{Type: EventTextDelta, Text: ev.Delta.Text}, nil
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					return StreamEvent{Type: EventThinkingDelta, Text: ev.Delta.Thinking}, nil
				}
			case "input_json_delta":
				s.toolJSON.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			if s.blockType == "tool_use" {
				args := json.RawMessage(s.toolJSON.String())
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				tc := &ToolCall{ID: s.toolID, Name: s.toolName, Arguments: args}
				s.pending = append(s.pending, tc)
				s.blockType = ""
				return StreamEvent{Type: EventToolCall, ToolCall: tc}, nil
			}
			s.blockType = ""
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				s.reason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				s.usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			return s.finish()
		case "error":
			return StreamEvent{}, fmt.Errorf("anthropic: stream error event")
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, fmt.Errorf("anthropic: stream read: %w", err)
	}
	return s.finish()
}

func (s *anthropicStream) finish() (StreamEvent, error) {
	s.done = true
	msg := Message{Role: RoleAssistant, Content: s.text.String()}
	for _, tc := range s.pending {
		msg.ToolCalls = append(msg.ToolCalls, *tc)
	}
	return StreamEvent{
		Type:       EventDone,
		Message:    &msg,
		StopReason: s.reason,
		Usage:      s.usage,
	}, nil
}
