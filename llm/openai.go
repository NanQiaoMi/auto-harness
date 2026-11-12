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

// OpenAIProvider speaks the OpenAI Chat Completions protocol. The same
// wire format is used by DeepSeek, Moonshot, Ollama, vLLM and most
// OpenAI-compatible gateways, so one adapter covers them all via BaseURL.
type OpenAIProvider struct {
	APIKey     string
	BaseURL    string // defaults to https://api.openai.com/v1
	HTTPClient *http.Client
	// ExtraHeaders is merged into every request (e.g. provider-specific auth).
	ExtraHeaders map[string]string
	// Hooks intercepts provider traffic (see llm.Hooks).
	Hooks *Hooks
}

func NewOpenAIProvider(apiKey, baseURL string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 0}, // streaming: no overall timeout
	}
}

func (p *OpenAIProvider) Name() string { return "openai" }

// openAIRequest is the Chat Completions wire format.
type openAIRequest struct {
	Model       string            `json:"model"`
	Messages    []openAIMessage   `json:"messages"`
	Tools       []openAITool      `json:"tools,omitempty"`
	Stream      bool              `json:"stream"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	StreamOpts  *openAIStreamOpts `json:"stream_options,omitempty"`
}

type openAIStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function map[string]any `json:"function"`
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// ReasoningContent carries extended-thinking output on
			// DeepSeek-style gateways ("reasoning_content").
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Chat implements Provider.
func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (Stream, error) {
	if p.Hooks != nil && p.Hooks.BeforeRequest != nil {
		p.Hooks.BeforeRequest(p.Name(), &req)
	}
	wire := openAIRequest{
		Model:       req.Model,
		Stream:      true,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		StreamOpts:  &openAIStreamOpts{IncludeUsage: true},
	}
	if req.System != "" {
		wire.Messages = append(wire.Messages, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			wire.Messages = append(wire.Messages, openAIMessage{Role: "user", Content: m.Content})
		case RoleAssistant:
			om := openAIMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				otc := openAIToolCall{ID: tc.ID, Type: "function"}
				otc.Function.Name = tc.Name
				otc.Function.Arguments = string(tc.Arguments)
				om.ToolCalls = append(om.ToolCalls, otc)
			}
			wire.Messages = append(wire.Messages, om)
		case RoleTool:
			wire.Messages = append(wire.Messages, openAIMessage{
				Role:       "tool",
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
			})
		}
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, openAITool{
			Type: "function",
			Function: map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", UserAgent)
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
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
		return nil, fmt.Errorf("openai: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &openAIStream{body: resp.Body, scanner: scanner}, nil
}

type openAIStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	// pending holds tool calls being assembled by index.
	pending map[int]*ToolCall
	order   []int
	text    strings.Builder
	reason  string
	usage   Usage
	done    bool
	closed  bool
	// queued holds the events a single chunk produced.
	//
	// A chunk can carry both reasoning and text, and the stream
	// protocol has one event per read, so the extra one waits here.
	queued []StreamEvent
}

func (s *openAIStream) Close() error {
	if !s.closed {
		s.closed = true
		return s.body.Close()
	}
	return nil
}

func (s *openAIStream) Recv() (StreamEvent, error) {
	if s.done {
		return StreamEvent{}, io.EOF
	}
	// An event queued by the previous chunk comes first.
	if len(s.queued) > 0 {
		ev := s.queued[0]
		s.queued = s.queued[1:]
		return ev, nil
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // keep-alive / comment
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return s.finish()
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return StreamEvent{}, fmt.Errorf("openai: bad stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			s.usage = Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.ReasoningContent != "" {
			ev := StreamEvent{Type: EventThinkingDelta, Text: choice.Delta.ReasoningContent}
			// A chunk can carry the reasoning and the text together.
			// The text goes on too: dropping it would lose part of
			// the answer.
			if choice.Delta.Content != "" {
				s.text.WriteString(choice.Delta.Content)
				s.queued = append(s.queued, StreamEvent{Type: EventTextDelta, Text: choice.Delta.Content})
			}
			return ev, nil
		}
		if choice.Delta.Content != "" {
			s.text.WriteString(choice.Delta.Content)
			return StreamEvent{Type: EventTextDelta, Text: choice.Delta.Content}, nil
		}
		for _, dtc := range choice.Delta.ToolCalls {
			if s.pending == nil {
				s.pending = map[int]*ToolCall{}
			}
			tc := s.pending[dtc.Index]
			if tc == nil {
				tc = &ToolCall{}
				s.pending[dtc.Index] = tc
				s.order = append(s.order, dtc.Index)
			}
			if dtc.ID != "" {
				tc.ID = dtc.ID
			}
			if dtc.Function.Name != "" {
				tc.Name = dtc.Function.Name
			}
			if dtc.Function.Arguments != "" {
				tc.Arguments = append(tc.Arguments, json.RawMessage(dtc.Function.Arguments)...)
			}
		}
		if choice.FinishReason != nil {
			// Keep reading: the usage chunk (stream_options.include_usage)
			// arrives after the finish_reason chunk and before [DONE].
			s.reason = *choice.FinishReason
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, fmt.Errorf("openai: stream read: %w", err)
	}
	return s.finish()
}

func (s *openAIStream) finish() (StreamEvent, error) {
	s.done = true
	msg := Message{Role: RoleAssistant, Content: s.text.String()}
	for _, idx := range s.order {
		tc := s.pending[idx]
		if len(tc.Arguments) == 0 {
			tc.Arguments = json.RawMessage("{}")
		}
		msg.ToolCalls = append(msg.ToolCalls, *tc)
	}
	return StreamEvent{
		Type:       EventDone,
		Message:    &msg,
		StopReason: s.reason,
		Usage:      s.usage,
	}, nil
}
