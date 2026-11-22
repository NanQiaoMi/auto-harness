package llm

import (
	"fmt"
	"os"
	"strings"
)

// Model describes known model metadata. The catalog is intentionally
// small and overridable — like pi's model catalog, it is a starting
// point, not a contract.
type Model struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Provider      string   `json:"provider"` // "openai" | "anthropic"
	Protocol      string   `json:"protocol"` // wire format: openai | anthropic | gemini
	ContextWindow int      `json:"context_window"`
	MaxOutput     int      `json:"max_output"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`
}

// SupportsImage reports whether the model accepts image inputs. Models
// without declared input modalities default to true so custom gateway
// models are not blocked.
func (m Model) SupportsImage() bool {
	if len(m.Input) == 0 {
		return true
	}
	for _, kind := range m.Input {
		if kind == "image" {
			return true
		}
	}
	return false
}

// Catalog lists built-in known models.
var Catalog = []Model{
	// Anthropic
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", Provider: "anthropic", Protocol: "anthropic", ContextWindow: 200000, MaxOutput: 64000, Reasoning: true, Input: []string{"text", "image"}},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Provider: "anthropic", Protocol: "anthropic", ContextWindow: 200000, MaxOutput: 64000, Reasoning: true, Input: []string{"text", "image"}},
	{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5", Provider: "anthropic", Protocol: "anthropic", ContextWindow: 200000, MaxOutput: 64000, Reasoning: true, Input: []string{"text", "image"}},
	// OpenAI-compatible (representative; any model id works via BaseURL gateways)
	{ID: "gpt-5.2", Name: "GPT 5.2", Provider: "openai", Protocol: "openai", ContextWindow: 400000, MaxOutput: 128000, Reasoning: true, Input: []string{"text", "image"}},
	{ID: "deepseek-chat", Name: "DeepSeek Chat", Provider: "openai", Protocol: "openai", ContextWindow: 128000, MaxOutput: 8192, Input: []string{"text"}},
	{ID: "qwen3-coder", Name: "Qwen3 Coder", Provider: "openai", Protocol: "openai", ContextWindow: 256000, MaxOutput: 65536, Input: []string{"text"}},
}

// Lookup returns catalog metadata for a model ID.
func Lookup(id string) (*Model, bool) {
	for i := range Catalog {
		if Catalog[i].ID == id {
			return &Catalog[i], true
		}
	}
	return nil, false
}

// GuessProvider infers the provider for a model ID when it is not in the
// catalog: ids containing "claude" map to Anthropic, everything else to
// the OpenAI-compatible protocol.
func GuessProvider(modelID string) string {
	if m, ok := Lookup(modelID); ok {
		return m.Provider
	}
	if strings.Contains(strings.ToLower(modelID), "claude") {
		return "anthropic"
	}
	return "openai"
}

// NewProvider builds a provider by name. API keys come from arguments or
// the conventional environment variables.
func NewProvider(name, baseURL, apiKey string) (Provider, error) {
	switch name {
	case "openai", "":
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		if baseURL == "" {
			baseURL = os.Getenv("OPENAI_BASE_URL")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("llm: no API key for openai-compatible provider (set OPENAI_API_KEY)")
		}
		return NewOpenAIProvider(apiKey, baseURL), nil
	case "anthropic":
		if apiKey == "" {
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		if baseURL == "" {
			baseURL = os.Getenv("ANTHROPIC_BASE_URL")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("llm: no API key for anthropic provider (set ANTHROPIC_API_KEY)")
		}
		return NewAnthropicProvider(apiKey, baseURL), nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want openai or anthropic)", name)
	}
}
