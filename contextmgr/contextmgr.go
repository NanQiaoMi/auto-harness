// Package contextmgr keeps an agent request inside a model's context
// window while preserving the parts of the transcript that matter.
//
// It is a small, pluggable foundation rather than a single summarizer:
// an Estimator decides how large a request is, an ArtifactStore can
// move oversized tool output out of the prompt, and a Summarizer turns
// older messages into a context capsule. The manager applies those
// layers in order and always leaves a deterministic hard-trim fallback.
package contextmgr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NanQiaoMi/auto-harness/llm"
)

const (
	DefaultContextWindow     = 128000
	DefaultMaxOutputTokens   = 8192
	DefaultReserveTokens     = 512
	DefaultCompactAt         = 0.75
	DefaultRecentMessages    = 8
	DefaultSummaryMaxTokens  = 2048
	DefaultSummaryChunkToken = 16000
	DefaultArtifactThreshold = 8000
	DefaultArtifactPreview   = 1500
)

// Options configures a Manager.
type Options struct {
	// ContextWindow is the model's total window. Zero falls back to the
	// input or DefaultContextWindow.
	ContextWindow int
	// MaxOutputTokens is reserved for the completion.
	MaxOutputTokens int
	// ReserveTokens is an extra safety margin for provider tokenizers.
	ReserveTokens int
	// CompactAt is the fraction of the prompt budget at which compaction
	// starts. Zero means DefaultCompactAt.
	CompactAt float64
	// KeepRecentTokens is the approximate recent transcript budget.
	// Zero derives a conservative share of the prompt budget.
	KeepRecentTokens int
	// KeepRecentMessages is the minimum number of recent units kept.
	KeepRecentMessages int
	// SummaryMaxTokens bounds one summary completion.
	SummaryMaxTokens int
	// SummaryChunkTokens bounds one summarizer input chunk.
	SummaryChunkTokens int
	// ArtifactThreshold is the byte size at which tool output is moved
	// to the artifact store.
	ArtifactThreshold int
	// ArtifactPreview is the number of bytes shown in the prompt.
	ArtifactPreview int
}

func (o Options) withDefaults() Options {
	if o.ContextWindow <= 0 {
		o.ContextWindow = DefaultContextWindow
	}
	if o.MaxOutputTokens <= 0 {
		o.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if o.ReserveTokens <= 0 {
		o.ReserveTokens = DefaultReserveTokens
	}
	if o.CompactAt <= 0 || o.CompactAt >= 1 {
		o.CompactAt = DefaultCompactAt
	}
	if o.KeepRecentMessages <= 0 {
		o.KeepRecentMessages = DefaultRecentMessages
	}
	if o.SummaryMaxTokens <= 0 {
		o.SummaryMaxTokens = DefaultSummaryMaxTokens
	}
	if o.SummaryChunkTokens <= 0 {
		o.SummaryChunkTokens = DefaultSummaryChunkToken
	}
	if o.ArtifactThreshold <= 0 {
		o.ArtifactThreshold = DefaultArtifactThreshold
	}
	if o.ArtifactPreview <= 0 {
		o.ArtifactPreview = DefaultArtifactPreview
	}
	return o
}

// Input is one request to size and, if needed, compact.
type Input struct {
	SystemPrompt    string
	Messages        []llm.Message
	Tools           []llm.ToolDefinition
	Model           string
	ContextWindow   int
	MaxOutputTokens int
}

// Result is the request to send and, when a summary was produced, the
// new persistent history that should replace the old one.
type Result struct {
	Messages     []llm.Message
	NewHistory   []llm.Message
	Compacted    bool
	Summary      string
	TokensBefore int
	TokensAfter  int
	PromptBudget int
	Warnings     []string
	Artifacts    []string
}

// CompactionStatus describes the most recent successful compaction.
type CompactionStatus struct {
	// Automatic distinguishes automatic request compaction from the
	// explicit /compact command.
	Automatic bool `json:"automatic"`
	// TokensBefore and TokensAfter are the request estimates on either
	// side of the compaction.
	TokensBefore int `json:"tokensBefore"`
	TokensAfter  int `json:"tokensAfter"`
	// SummaryBytes is the size of the generated context capsule.
	SummaryBytes int `json:"summaryBytes"`
	// Artifacts counts the large outputs moved out of the prompt.
	Artifacts int `json:"artifacts"`
	// At is when the compaction completed.
	At time.Time `json:"at"`
}

// Status is a read-only snapshot for UI and RPC clients. It deliberately
// excludes message and summary content: the inspector answers "how much
// room is left", not "what did the user say".
type Status struct {
	Model                  string            `json:"model,omitempty"`
	ContextWindow          int               `json:"contextWindow"`
	MaxOutputTokens        int               `json:"maxOutputTokens"`
	ReserveTokens          int               `json:"reserveTokens"`
	PromptBudget           int               `json:"promptBudget"`
	EstimatedTokens        int               `json:"estimatedTokens"`
	CompactAt              float64           `json:"compactAt"`
	CompactThresholdTokens int               `json:"compactThresholdTokens"`
	KeepRecentTokens       int               `json:"keepRecentTokens"`
	KeepRecentMessages     int               `json:"keepRecentMessages"`
	SummaryMaxTokens       int               `json:"summaryMaxTokens"`
	ArtifactDir            string            `json:"artifactDir,omitempty"`
	LastCompaction         *CompactionStatus `json:"lastCompaction,omitempty"`
}

// Builder is the interface the agent depends on, so an application can
// replace the default manager with its own policy.
type Builder interface {
	Build(ctx context.Context, in Input) (Result, error)
}

// Inspector is implemented by context builders that expose budget
// diagnostics without changing the request.
type Inspector interface {
	Status(in Input) Status
}

// Manager is the default context budget and compression pipeline.
type Manager struct {
	opts       Options
	estimator  Estimator
	summarizer Summarizer
	artifacts  ArtifactStore

	mu          sync.Mutex
	lastCompact *CompactionStatus
}

// NewManager creates a context manager. Nil components receive safe
// defaults: the heuristic estimator and no summarizer/artifact store.
func NewManager(opts Options, estimator Estimator, summarizer Summarizer, artifacts ArtifactStore) *Manager {
	if estimator == nil {
		estimator = HeuristicEstimator{}
	}
	return &Manager{
		opts:       opts.withDefaults(),
		estimator:  estimator,
		summarizer: summarizer,
		artifacts:  artifacts,
	}
}

// Build sizes a request and applies compression when needed.
func (m *Manager) Build(ctx context.Context, in Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	_, _, budget := m.limits(in)
	overhead := m.estimator.EstimateText(in.SystemPrompt) + m.estimator.EstimateTools(in.Tools)
	working := cloneMessages(in.Messages)
	before := overhead + m.estimator.EstimateMessages(working)

	res := Result{
		Messages:     working,
		TokensBefore: before,
		PromptBudget: budget,
	}
	limit := int(float64(budget) * m.opts.CompactAt)
	if before <= limit {
		res.TokensAfter = before
		m.observe(res)
		return res, nil
	}

	units := groupUnits(working)
	keepStart := keepStartForBudget(units, m.estimator, budget-overhead,
		m.recentTokens(budget-overhead), m.opts.KeepRecentMessages)

	if keepStart > 0 && m.summarizer != nil {
		summary, err := m.summarizeUnits(ctx, units[:keepStart], budget, &res)
		if err != nil {
			res.Warnings = append(res.Warnings, "context summary failed: "+err.Error())
		} else if strings.TrimSpace(summary) != "" {
			newHistory := append([]llm.Message{summaryMessage(summary)},
				flattenUnits(units[keepStart:])...)
			newHistory = m.fitHistory(ctx, newHistory, budget-overhead, &res)
			res.NewHistory = newHistory
			res.Summary = summary
			res.Compacted = true
			working = newHistory
		}
	} else if keepStart > 0 && m.summarizer == nil {
		res.Warnings = append(res.Warnings,
			"no summarizer configured; old context may be dropped if the request exceeds its budget")
	}

	if overhead+m.estimator.EstimateMessages(working) > budget {
		working = m.fitHistory(ctx, working, budget-overhead, &res)
	}

	res.Messages = working
	res.TokensAfter = overhead + m.estimator.EstimateMessages(working)
	if res.TokensAfter > budget {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("context still exceeds budget: %d > %d tokens", res.TokensAfter, budget))
	}
	m.observe(res)
	return res, nil
}

// Estimate returns the prompt budget and the current request estimate
// without modifying anything.
func (m *Manager) Estimate(in Input) (budget, total int) {
	_, _, budget = m.limits(in)
	total = m.estimator.EstimateText(in.SystemPrompt) +
		m.estimator.EstimateTools(in.Tools) +
		m.estimator.EstimateMessages(in.Messages)
	return budget, total
}

// Status returns the current budget estimate and the last successful
// compaction. It is safe to call while a run is in progress.
func (m *Manager) Status(in Input) Status {
	window, maxOut, budget := m.limits(in)
	total := m.estimator.EstimateText(in.SystemPrompt) +
		m.estimator.EstimateTools(in.Tools) +
		m.estimator.EstimateMessages(in.Messages)

	out := Status{
		Model:                  in.Model,
		ContextWindow:          window,
		MaxOutputTokens:        maxOut,
		ReserveTokens:          m.opts.ReserveTokens,
		PromptBudget:           budget,
		EstimatedTokens:        total,
		CompactAt:              m.opts.CompactAt,
		CompactThresholdTokens: int(float64(budget) * m.opts.CompactAt),
		KeepRecentTokens:       m.recentTokens(budget),
		KeepRecentMessages:     m.opts.KeepRecentMessages,
		SummaryMaxTokens:       m.opts.SummaryMaxTokens,
		ArtifactDir:            artifactDirectory(m.artifacts),
	}

	m.mu.Lock()
	if m.lastCompact != nil {
		copy := *m.lastCompact
		out.LastCompaction = &copy
	}
	m.mu.Unlock()
	return out
}

func (m *Manager) observe(res Result) {
	m.mu.Lock()
	if res.Compacted {
		m.lastCompact = &CompactionStatus{
			Automatic:    true,
			TokensBefore: res.TokensBefore,
			TokensAfter:  res.TokensAfter,
			SummaryBytes: len(res.Summary),
			Artifacts:    len(res.Artifacts),
			At:           time.Now().UTC(),
		}
	}
	m.mu.Unlock()
}

func (m *Manager) limits(in Input) (window, maxOut, budget int) {
	window = in.ContextWindow
	if window <= 0 {
		window = m.opts.ContextWindow
	}
	if window <= 0 {
		window = DefaultContextWindow
	}
	maxOut = in.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = m.opts.MaxOutputTokens
	}
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputTokens
	}
	if maxOut >= window/2 {
		maxOut = window / 4
	}
	if maxOut < 1 {
		maxOut = 1
	}
	budget = window - maxOut - m.opts.ReserveTokens
	if budget < 1 {
		budget = 1
	}
	return window, maxOut, budget
}

func (m *Manager) summarizeUnits(ctx context.Context, units []unit, budget int, res *Result) (string, error) {
	messages := flattenUnits(units)
	if len(messages) == 0 {
		return "", nil
	}
	chunkTokens := m.opts.SummaryChunkTokens
	if budget > 0 {
		limit := budget * 3 / 4
		if limit < 64 {
			limit = 64
		}
		if chunkTokens <= 0 || chunkTokens > limit {
			chunkTokens = limit
		}
	}
	if chunkTokens < 1 {
		chunkTokens = 1
	}
	summaryMax := m.summaryMaxTokens(budget)

	messages = m.prepareForSummary(ctx, messages, chunkTokens, res)
	chunks := chunkUnits(groupUnits(messages), m.estimator, chunkTokens)
	var parts []string
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		part, err := m.summarizer.Summarize(ctx, chunk, summaryMax)
		if err != nil {
			return "", err
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("contextmgr: summarizer returned no content")
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return m.mergeSummaries(ctx, parts, summaryMax)
}

func (m *Manager) prepareForSummary(ctx context.Context, messages []llm.Message, chunkTokens int, res *Result) []llm.Message {
	out := cloneMessages(messages)
	for i := range out {
		if m.estimator.EstimateMessage(out[i]) <= chunkTokens {
			continue
		}
		if m.artifacts != nil {
			path, err := m.artifacts.Put(ctx, fmt.Sprintf("summary-%d", i), out[i].Content)
			if err == nil {
				preview := truncateUTF8(out[i].Content, m.opts.ArtifactPreview)
				out[i].Content = fmt.Sprintf(
					"[large message stored as artifact: %s; %d bytes]\nPreview:\n%s",
					path, len(out[i].Content), preview)
				appendArtifact(res, path)
				continue
			}
			res.Warnings = append(res.Warnings, "artifact write failed: "+err.Error())
		}
		out[i].Content = truncateUTF8(out[i].Content, chunkTokens*4) +
			"\n[clipped before summarization]"
	}
	return out
}

func (m *Manager) mergeSummaries(ctx context.Context, parts []string, maxTokens int) (string, error) {
	for len(parts) > 1 {
		var next []string
		for i := 0; i < len(parts); i += 2 {
			var msgs []llm.Message
			msgs = append(msgs, llm.Message{
				Role:    llm.RoleUser,
				Content: "Partial context summary 1:\n" + parts[i],
			})
			if i+1 < len(parts) {
				msgs = append(msgs, llm.Message{
					Role:    llm.RoleUser,
					Content: "Partial context summary 2:\n" + parts[i+1],
				})
			}
			merged, err := m.summarizer.Summarize(ctx, msgs, maxTokens)
			if err != nil {
				return "", err
			}
			merged = strings.TrimSpace(merged)
			if merged != "" {
				next = append(next, merged)
			}
		}
		if len(next) == 0 {
			return "", fmt.Errorf("contextmgr: merge summaries returned no content")
		}
		parts = next
	}
	return parts[0], nil
}

func (m *Manager) summaryMaxTokens(budget int) int {
	maxTokens := m.opts.SummaryMaxTokens
	if budget <= 0 {
		return maxTokens
	}
	limit := budget / 4
	if limit < 128 {
		limit = 128
	}
	if maxTokens > limit {
		maxTokens = limit
	}
	if maxTokens < 64 {
		maxTokens = 64
	}
	return maxTokens
}

func (m *Manager) fitHistory(ctx context.Context, messages []llm.Message, budget int, res *Result) []llm.Message {
	messages = cloneMessages(messages)
	if budget < 1 {
		budget = 1
	}
	if m.estimator.EstimateMessages(messages) <= budget {
		return messages
	}
	if m.artifacts != nil {
		messages = m.artifactizeToolResults(ctx, messages, res)
		if m.estimator.EstimateMessages(messages) <= budget {
			return messages
		}
	}

	var summary []llm.Message
	if len(messages) > 0 && isSummaryMessage(messages[0]) {
		summary = []llm.Message{messages[0]}
		messages = messages[1:]
	}
	units := groupUnits(messages)
	keepStart := keepStartForBudget(units, m.estimator, budget-summaryTokens(summary, m.estimator),
		m.recentTokens(budget), m.opts.KeepRecentMessages)
	kept := append(cloneMessages(summary), flattenUnits(units[keepStart:])...)

	lastUser := lastUserUnit(units)
	for m.estimator.EstimateMessages(kept) > budget && len(units)-keepStart > 1 {
		if lastUser >= 0 && keepStart >= lastUser {
			break
		}
		keepStart++
		kept = append(cloneMessages(summary), flattenUnits(units[keepStart:])...)
	}
	if m.estimator.EstimateMessages(kept) > budget {
		kept = m.truncateToFit(kept, budget)
	}
	return kept
}

func (m *Manager) artifactizeToolResults(ctx context.Context, messages []llm.Message, res *Result) []llm.Message {
	for i := range messages {
		if messages[i].Role != llm.RoleTool {
			continue
		}
		if len(messages[i].Content) < m.opts.ArtifactThreshold {
			continue
		}
		name := fmt.Sprintf("tool-%s-%d", messages[i].ToolCallID, i)
		path, err := m.artifacts.Put(ctx, name, messages[i].Content)
		if err != nil {
			res.Warnings = append(res.Warnings, "artifact write failed: "+err.Error())
			continue
		}
		preview := truncateUTF8(messages[i].Content, m.opts.ArtifactPreview)
		messages[i].Content = fmt.Sprintf(
			"[tool output stored as artifact: %s; %d bytes]\nPreview:\n%s",
			path, len(messages[i].Content), preview)
		appendArtifact(res, path)
	}
	return messages
}

func appendArtifact(res *Result, path string) {
	if path == "" {
		return
	}
	for _, existing := range res.Artifacts {
		if existing == path {
			return
		}
	}
	res.Artifacts = append(res.Artifacts, path)
}

func (m *Manager) truncateToFit(messages []llm.Message, budget int) []llm.Message {
	for m.estimator.EstimateMessages(messages) > budget {
		idx := -1
		for i := 1; i < len(messages)-1; i++ {
			if len(messages[i].Content) > 128 {
				idx = i
				break
			}
		}
		if idx < 0 {
			for i := len(messages) - 1; i >= 0; i-- {
				if isSummaryMessage(messages[i]) {
					continue
				}
				if len(messages[i].Content) > 0 {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			break
		}
		oldLen := len(messages[idx].Content)
		newLen := oldLen / 2
		if newLen < 64 {
			newLen = 64
		}
		messages[idx].Content = truncateUTF8(messages[idx].Content, newLen) +
			fmt.Sprintf("\n[truncated to fit context; %d bytes omitted]", oldLen-newLen)
		if len(messages[idx].Content) >= oldLen {
			break
		}
	}
	// A single summary can be the only message left. Preserve its
	// leading structure by truncating it only after everything else
	// has failed.
	for m.estimator.EstimateMessages(messages) > budget && len(messages) == 1 {
		oldLen := len(messages[0].Content)
		if oldLen <= 64 {
			break
		}
		newLen := oldLen / 2
		if newLen < 64 {
			newLen = 64
		}
		messages[0].Content = truncateUTF8(messages[0].Content, newLen) +
			fmt.Sprintf("\n[truncated to fit context; %d bytes omitted]", oldLen-newLen)
	}
	return messages
}

func keepStartForBudget(units []unit, estimator Estimator, budget, recentTokens, recentMessages int) int {
	if len(units) == 0 {
		return 0
	}
	if recentMessages <= 0 {
		recentMessages = DefaultRecentMessages
	}
	start := len(units)
	used := 0
	count := 0
	for i := len(units) - 1; i >= 0; i-- {
		if start < len(units) && (count >= recentMessages || (recentTokens > 0 && used >= recentTokens)) {
			break
		}
		tokens := estimator.EstimateMessages(units[i].msgs)
		if start < len(units) && budget > 0 && used+tokens > budget {
			break
		}
		start = i
		used += tokens
		count++
	}
	if start == len(units) {
		start = len(units) - 1
	}
	if lastUser := lastUserUnit(units); lastUser >= 0 && lastUser < start {
		start = lastUser
	}
	return start
}

func (m *Manager) recentTokens(budget int) int {
	if m.opts.KeepRecentTokens > 0 {
		return m.opts.KeepRecentTokens
	}
	if budget <= 0 {
		return 0
	}
	recent := budget / 3
	if recent < 1024 {
		recent = 1024
	}
	if recent > 32000 {
		recent = 32000
	}
	if recent > budget {
		recent = budget
	}
	return recent
}

func chunkUnits(units []unit, estimator Estimator, maxTokens int) [][]llm.Message {
	var chunks [][]llm.Message
	var current []llm.Message
	used := 0
	for _, u := range units {
		tokens := estimator.EstimateMessages(u.msgs)
		if len(current) > 0 && used+tokens > maxTokens {
			chunks = append(chunks, current)
			current = nil
			used = 0
		}
		current = append(current, u.msgs...)
		used += tokens
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

func artifactDirectory(store ArtifactStore) string {
	if locator, ok := store.(interface{ Directory() string }); ok {
		return locator.Directory()
	}
	return ""
}

func summaryTokens(messages []llm.Message, estimator Estimator) int {
	return estimator.EstimateMessages(messages)
}

func cloneMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if m.ToolCalls != nil {
			out[i].ToolCalls = append([]llm.ToolCall(nil), m.ToolCalls...)
		}
	}
	return out
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && maxBytes < len(s) && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
