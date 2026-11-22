package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GeminiProvider speaks the Google Gemini generateContent API. The model
// and method live in the path:
//
//	POST {base}/models/{model}:generateContent
//	POST {base}/models/{model}:streamGenerateContent?alt=sse
//
// BaseURL is the API root including the version, e.g.
// https://generativelanguage.googleapis.com/v1beta.
type GeminiProvider struct {
	APIKey string
	// BaseURL is the API root (default https://generativelanguage.googleapis.com/v1beta).
	BaseURL string
	// HTTPClient is used for requests.
	HTTPClient *http.Client
	// ExtraHeaders is merged into every request.
	ExtraHeaders map[string]string
	// Hooks intercepts provider traffic (see llm.Hooks).
	Hooks *Hooks
}

func NewGeminiProvider(apiKey, baseURL string) *GeminiProvider {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	return &GeminiProvider{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 0},
	}
}

func (p *GeminiProvider) Name() string { return "gemini" }

// geminiRequest is the generateContent wire format.
type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	Tools             []geminiTool     `json:"tools,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
	// FunctionCall requests a tool invocation.
	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
	// FunctionResponse carries a tool result back to the model.
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiGenConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

// geminiResponse covers both the unary and streaming shapes.
type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason,omitempty"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat implements Provider.
func (p *GeminiProvider) Chat(ctx context.Context, req ChatRequest) (Stream, error) {
	if p.Hooks != nil && p.Hooks.BeforeRequest != nil {
		p.Hooks.BeforeRequest(p.Name(), &req)
	}
	wire := geminiRequest{}
	if req.System != "" {
		wire.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: req.System}},
		}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			parts := make([]geminiPart, 0, len(m.Images)+1)
			if m.Content != "" || len(m.Images) == 0 {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, img := range m.Images {
				raw, err := img.Bytes()
				if err != nil {
					return nil, err
				}
				parts = append(parts, geminiImagePart(img, raw))
			}
			wire.Contents = append(wire.Contents, geminiContent{
				Role:  "user",
				Parts: parts,
			})
		case RoleAssistant:
			parts := []geminiPart{}
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := map[string]any{}
				if len(tc.Arguments) > 0 {
					_ = json.Unmarshal(tc.Arguments, &args)
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
					ID: tc.ID, Name: tc.Name, Args: args,
				}})
			}
			if len(parts) > 0 {
				wire.Contents = append(wire.Contents, geminiContent{Role: "model", Parts: parts})
			}
		case RoleTool:
			parts := make([]geminiPart, 0, len(m.Images)+1)
			parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResponse{
				ID:   m.ToolCallID,
				Name: m.ToolCallID,
				Response: map[string]any{
					"content": m.Content,
					"isError": m.IsError,
				},
			}})
			for _, img := range m.Images {
				raw, err := img.Bytes()
				if err != nil {
					return nil, err
				}
				parts = append(parts, geminiImagePart(img, raw))
			}
			wire.Contents = append(wire.Contents, geminiContent{
				Role:  "user",
				Parts: parts,
			})
		}
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, geminiTool{
			FunctionDeclarations: []geminiFunctionDecl{{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			}},
		})
	}
	if req.MaxTokens > 0 || req.Temperature != nil {
		wire.GenerationConfig = &geminiGenConfig{
			MaxOutputTokens: req.MaxTokens,
			Temperature:     req.Temperature,
		}
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", p.BaseURL, req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", UserAgent)
	if p.APIKey != "" {
		httpReq.Header.Set("x-goog-api-key", p.APIKey)
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), resp.Body))
		p.Hooks.AfterResponse(p.Name(), resp.StatusCode, b)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, fmt.Errorf("gemini: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &geminiStream{body: resp.Body, scanner: scanner}, nil
}

func geminiImagePart(img Image, raw []byte) geminiPart {
	return geminiPart{
		InlineData: &geminiInlineData{
			MimeType: img.MediaType,
			Data:     base64.StdEncoding.EncodeToString(raw),
		},
	}
}

type geminiStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	text    strings.Builder
	usage   Usage
	reason  string
	done    bool
	closed  bool
	// pending holds tool calls seen in the stream.
	pending []ToolCall
}

func (s *geminiStream) Close() error {
	if !s.closed {
		s.closed = true
		return s.body.Close()
	}
	return nil
}

func (s *geminiStream) Recv() (StreamEvent, error) {
	if s.done {
		return StreamEvent{}, io.EOF
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var chunk geminiResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return StreamEvent{}, fmt.Errorf("gemini: bad stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return StreamEvent{}, fmt.Errorf("gemini: %s", chunk.Error.Message)
		}
		s.usage = Usage{
			InputTokens:  chunk.UsageMetadata.PromptTokenCount,
			OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
		}
		for _, cand := range chunk.Candidates {
			if cand.FinishReason != "" {
				s.reason = cand.FinishReason
			}
			for _, part := range cand.Content.Parts {
				if part.FunctionCall != nil {
					args := json.RawMessage("{}")
					if part.FunctionCall.Args != nil {
						if b, err := json.Marshal(part.FunctionCall.Args); err == nil {
							args = b
						}
					}
					tc := ToolCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Arguments: args}
					s.pending = append(s.pending, tc)
					return StreamEvent{Type: EventToolCall, ToolCall: &tc}, nil
				}
				if part.Text != "" {
					s.text.WriteString(part.Text)
					return StreamEvent{Type: EventTextDelta, Text: part.Text}, nil
				}
			}
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, fmt.Errorf("gemini: stream read: %w", err)
	}
	return s.finish()
}

func (s *geminiStream) finish() (StreamEvent, error) {
	s.done = true
	msg := Message{Role: RoleAssistant, Content: s.text.String()}
	msg.ToolCalls = append(msg.ToolCalls, s.pending...)
	return StreamEvent{
		Type:       EventDone,
		Message:    &msg,
		StopReason: s.reason,
		Usage:      s.usage,
	}, nil
}
