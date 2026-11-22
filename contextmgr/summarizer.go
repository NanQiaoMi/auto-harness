package contextmgr

import (
	"context"
	"fmt"
	"strings"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// Summarizer turns older transcript messages into a compact context
// capsule.
type Summarizer interface {
	Summarize(ctx context.Context, messages []llm.Message, maxTokens int) (string, error)
}

// SummarizerFunc adapts a function into a Summarizer.
type SummarizerFunc func(ctx context.Context, messages []llm.Message, maxTokens int) (string, error)

// Summarize implements Summarizer.
func (f SummarizerFunc) Summarize(ctx context.Context, messages []llm.Message, maxTokens int) (string, error) {
	return f(ctx, messages, maxTokens)
}

// LLMSummarizer uses a provider to produce context capsules.
type LLMSummarizer struct {
	Provider llm.Provider
	Model    string
}

// Summarize implements Summarizer.
func (s LLMSummarizer) Summarize(ctx context.Context, messages []llm.Message, maxTokens int) (string, error) {
	if s.Provider == nil {
		return "", fmt.Errorf("contextmgr: summarizer has no provider")
	}
	return SummarizeOnce(ctx, s.Provider, s.Model, messages, maxTokens)
}

// SummarizeOnce renders a transcript and asks the provider for a
// context capsule. It is exported so callers can build a dynamic
// summarizer that follows the currently selected model.
func SummarizeOnce(ctx context.Context, provider llm.Provider, model string, messages []llm.Message, maxTokens int) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("contextmgr: summarizer has no provider")
	}
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	transcript := renderTranscript(messages)
	zero := 0.0
	stream, err := provider.Chat(ctx, llm.ChatRequest{
		Model:       model,
		System:      summarizerSystemPrompt,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: transcript}},
		MaxTokens:   maxTokens,
		Temperature: &zero,
	})
	if err != nil {
		return "", fmt.Errorf("contextmgr: summarize: %w", err)
	}
	msg, _, err := llm.Collect(stream, nil)
	if err != nil {
		return "", fmt.Errorf("contextmgr: summarize: %w", err)
	}
	if msg == nil || strings.TrimSpace(msg.Content) == "" {
		return "", fmt.Errorf("contextmgr: summarizer returned no content")
	}
	return strings.TrimSpace(msg.Content), nil
}

const summarizerSystemPrompt = `You are the context compactor for a coding agent.

Read the transcript and produce a faithful, dense "context capsule" that
another coding agent can use to continue the work without the original
messages. Preserve, in this order:

1. The user's goals, constraints, preferences and acceptance criteria.
2. Decisions made and why; alternatives that were rejected.
3. Files and code areas touched, with exact paths and identifiers.
4. Commands run, tests run, and their results.
5. Errors, failed attempts, and unresolved questions.
6. The current state and the next concrete step.

Keep exact names, paths, versions and numbers. Do not invent facts. Do not
add a preamble. Output only the capsule, using short headings or bullets.`

func renderTranscript(messages []llm.Message) string {
	var b strings.Builder
	for i, m := range messages {
		fmt.Fprintf(&b, "\n[message %d role=%s]\n", i+1, m.Role)
		if m.Content != "" {
			b.WriteString(clipRunes(m.Content, 32000))
			b.WriteByte('\n')
		}
		for _, img := range m.Images {
			b.WriteString(img.Placeholder())
			b.WriteByte('\n')
		}
		for _, call := range m.ToolCalls {
			fmt.Fprintf(&b, "tool_call id=%s name=%s arguments=%s\n",
				call.ID, call.Name, clipRunes(string(call.Arguments), 8000))
		}
		if m.ToolCallID != "" {
			fmt.Fprintf(&b, "tool_call_id=%s error=%t\n", m.ToolCallID, m.IsError)
		}
	}
	return strings.TrimSpace(b.String())
}

func clipRunes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return truncateUTF8(s, maxBytes) + "\n[clipped for summary]"
}
