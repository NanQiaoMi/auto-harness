package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/NanQiaoMi/auto-harness/llm"
)

func largeText(bytes int) string {
	return strings.Repeat("x", bytes)
}

func conversation(pairs, bytes int) []llm.Message {
	var messages []llm.Message
	for i := 0; i < pairs; i++ {
		messages = append(messages,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("user-%d %s", i, largeText(bytes))},
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("assistant-%d %s", i, largeText(bytes))},
		)
	}
	return messages
}

func copyInput(messages []llm.Message) Input {
	return Input{
		SystemPrompt:    "system",
		Messages:        messages,
		Model:           "test-model",
		ContextWindow:   6000,
		MaxOutputTokens: 500,
	}
}

func testOptions() Options {
	return Options{
		ContextWindow:      6000,
		MaxOutputTokens:    500,
		ReserveTokens:      100,
		CompactAt:          0.5,
		KeepRecentMessages: 2,
		KeepRecentTokens:   500,
		SummaryMaxTokens:   200,
		SummaryChunkTokens: 1000,
		ArtifactThreshold:  100000,
		ArtifactPreview:    100,
	}
}

type recordingSummarizer struct {
	mu      sync.Mutex
	calls   [][]llm.Message
	replies []string
	err     error
}

func (s *recordingSummarizer) Summarize(_ context.Context, messages []llm.Message, _ int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, cloneMessages(messages))
	if s.err != nil {
		return "", s.err
	}
	if len(s.replies) > 0 {
		reply := s.replies[0]
		s.replies = s.replies[1:]
		return reply, nil
	}
	return "CAPSULE", nil
}

func TestBuildLeavesSmallRequestsUnchanged(t *testing.T) {
	summarizer := &recordingSummarizer{}
	m := NewManager(testOptions(), nil, summarizer, nil)
	input := copyInput([]llm.Message{
		{Role: llm.RoleUser, Content: "one"},
		{Role: llm.RoleAssistant, Content: "two"},
	})

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Compacted {
		t.Fatal("a small request was compacted")
	}
	if !reflect.DeepEqual(got.Messages, input.Messages) {
		t.Fatalf("messages changed: %#v", got.Messages)
	}
	if got.TokensBefore != got.TokensAfter {
		t.Fatalf("tokens changed without compaction: %d -> %d", got.TokensBefore, got.TokensAfter)
	}
	if len(summarizer.calls) != 0 {
		t.Fatal("the summarizer ran below the threshold")
	}
}

func TestBuildSummarizesOldHistoryAndKeepsLatestUser(t *testing.T) {
	summarizer := &recordingSummarizer{}
	m := NewManager(testOptions(), nil, summarizer, nil)
	input := copyInput(conversation(10, 600))
	input.Messages = append(input.Messages, llm.Message{
		Role: llm.RoleUser, Content: "LATEST USER INTENT",
	})

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compacted || got.Summary != "CAPSULE" {
		t.Fatalf("compaction = %v, summary = %q", got.Compacted, got.Summary)
	}
	if len(summarizer.calls) == 0 {
		t.Fatal("the summarizer never ran")
	}
	if got.TokensAfter >= got.TokensBefore {
		t.Fatalf("compaction did not reduce tokens: %d -> %d", got.TokensBefore, got.TokensAfter)
	}
	if len(got.NewHistory) == 0 || !isSummaryMessage(got.NewHistory[0]) {
		t.Fatalf("new history does not start with a summary: %#v", got.NewHistory[:1])
	}
	if !containsContent(got.NewHistory, "LATEST USER INTENT") {
		t.Fatal("compaction dropped the latest user intent")
	}
	if containsContent(got.NewHistory, "user-0 ") {
		t.Fatal("compaction retained the oldest history alongside its summary")
	}

	status := m.Status(Input{
		SystemPrompt:    input.SystemPrompt,
		Messages:        got.Messages,
		Model:           input.Model,
		ContextWindow:   input.ContextWindow,
		MaxOutputTokens: input.MaxOutputTokens,
	})
	if status.LastCompaction == nil {
		t.Fatal("status does not report the compaction")
	}
	if status.LastCompaction.TokensBefore != got.TokensBefore ||
		status.LastCompaction.TokensAfter != got.TokensAfter {
		t.Fatalf("status compaction = %+v", status.LastCompaction)
	}
}

func TestBuildChunksByMessageUnitAndMergesSummaries(t *testing.T) {
	summarizer := &recordingSummarizer{}
	opts := testOptions()
	opts.ContextWindow = 60000
	opts.MaxOutputTokens = 1000
	opts.SummaryChunkTokens = 240
	opts.KeepRecentTokens = 300
	m := NewManager(opts, nil, summarizer, nil)
	input := copyInput(conversation(40, 1500))
	input.ContextWindow = opts.ContextWindow
	input.MaxOutputTokens = opts.MaxOutputTokens

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compacted {
		t.Fatal("the request was not compacted")
	}
	if len(summarizer.calls) < 4 {
		t.Fatalf("large history used %d summary calls, want chunking", len(summarizer.calls))
	}
	for _, chunk := range summarizer.calls {
		assertNoOrphanToolResult(t, chunk)
	}
}

func TestBuildDropsOldContextWithoutSummarizer(t *testing.T) {
	m := NewManager(testOptions(), nil, nil, nil)
	input := copyInput(conversation(20, 600))

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Compacted {
		t.Fatal("compaction without a summarizer must not claim a capsule")
	}
	if len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, "\n"), "no summarizer") {
		t.Fatalf("warnings = %v", got.Warnings)
	}
	if len(got.Messages) >= len(input.Messages) {
		t.Fatal("the oversized request was not shortened")
	}
	if got.TokensAfter > got.PromptBudget {
		t.Fatalf("request still exceeds budget: %d > %d", got.TokensAfter, got.PromptBudget)
	}
	if !containsContent(got.Messages, "user-19") {
		t.Fatal("hard trimming dropped the most recent user message")
	}
}

func TestBuildArtifactizesLargeToolResults(t *testing.T) {
	store := NewFileArtifactStore(t.TempDir())
	opts := testOptions()
	opts.ContextWindow = 2500
	opts.ArtifactThreshold = 200
	opts.ArtifactPreview = 40
	m := NewManager(opts, nil, nil, store)

	payload := "HUGE-TOOL-OUTPUT-" + largeText(12000)
	input := copyInput([]llm.Message{
		{Role: llm.RoleUser, Content: "read the build log"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call-1", Name: "read", Arguments: []byte(`{"path":"build.log"}`),
		}}},
		{Role: llm.RoleTool, ToolCallID: "call-1", Content: payload},
	})
	input.ContextWindow = opts.ContextWindow
	input.MaxOutputTokens = opts.MaxOutputTokens

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("artifacts = %v", got.Artifacts)
	}
	path := got.Artifacts[0]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Fatal("the artifact does not contain the original tool output")
	}
	if !containsContent(got.Messages, "stored as artifact") || !containsContent(got.Messages, path) {
		t.Fatalf("the prompt has no artifact reference: %#v", got.Messages)
	}
	if containsContent(got.Messages, largeText(1000)) {
		t.Fatal("the oversized output is still in the prompt")
	}
	assertNoOrphanToolResult(t, got.Messages)
}

func TestBuildSurvivesSummarizerFailure(t *testing.T) {
	summarizer := &recordingSummarizer{err: errors.New("provider unavailable")}
	m := NewManager(testOptions(), nil, summarizer, nil)
	input := copyInput(conversation(20, 600))

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Compacted {
		t.Fatal("a failed summary was reported as compaction")
	}
	if !strings.Contains(strings.Join(got.Warnings, "\n"), "context summary failed") {
		t.Fatalf("warnings = %v", got.Warnings)
	}
	if got.TokensAfter > got.PromptBudget {
		t.Fatalf("fallback still exceeds budget: %d > %d", got.TokensAfter, got.PromptBudget)
	}
}

func TestGroupUnitsKeepsToolCallWithResults(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "run"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "a", Name: "bash", Arguments: []byte(`{}`)},
			{ID: "b", Name: "read", Arguments: []byte(`{}`)},
		}},
		{Role: llm.RoleTool, ToolCallID: "a", Content: "one"},
		{Role: llm.RoleTool, ToolCallID: "b", Content: "two"},
		{Role: llm.RoleAssistant, Content: "done"},
	}
	units := groupUnits(messages)
	if len(units) != 3 {
		t.Fatalf("units = %d, want 3", len(units))
	}
	if len(units[1].msgs) != 3 {
		t.Fatalf("tool-call unit has %d messages, want 3", len(units[1].msgs))
	}
	assertNoOrphanToolResult(t, flattenUnits(units))
}

func TestBuildKeepsLatestUserEvenWhenRecentBudgetIsSmall(t *testing.T) {
	opts := testOptions()
	opts.KeepRecentMessages = 1
	opts.KeepRecentTokens = 50
	m := NewManager(opts, nil, &recordingSummarizer{}, nil)
	input := copyInput(conversation(8, 700))
	input.Messages = append(input.Messages, llm.Message{Role: llm.RoleUser, Content: "must survive"})

	got, err := m.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !containsContent(got.Messages, "must survive") {
		t.Fatal("the latest user message was compacted away")
	}
}

func TestBuildInvariantsAcrossShapes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for caseNo := 0; caseNo < 20; caseNo++ {
		var messages []llm.Message
		latestUser := ""
		for unit := 0; unit < 3+rng.Intn(12); unit++ {
			switch rng.Intn(4) {
			case 0:
				latestUser = fmt.Sprintf("user-%d-%d", caseNo, unit)
				text := latestUser + " " + largeText(20+rng.Intn(900))
				messages = append(messages, llm.Message{Role: llm.RoleUser, Content: text})
			case 1:
				messages = append(messages, llm.Message{
					Role: llm.RoleAssistant, Content: largeText(20 + rng.Intn(900)),
				})
			default:
				callID := fmt.Sprintf("call-%d-%d", caseNo, unit)
				messages = append(messages, llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{
						ID: callID, Name: "read", Arguments: []byte(`{"path":"x"}`),
					}},
				})
				for result := 0; result < 1+rng.Intn(2); result++ {
					messages = append(messages, llm.Message{
						Role:       llm.RoleTool,
						ToolCallID: callID,
						Content:    largeText(20 + rng.Intn(1200)),
					})
				}
			}
		}

		opts := testOptions()
		opts.ContextWindow = 2500 + rng.Intn(5000)
		opts.KeepRecentMessages = 1 + rng.Intn(4)
		opts.KeepRecentTokens = 200 + rng.Intn(1000)
		opts.ArtifactThreshold = 400
		summarizer := &recordingSummarizer{}
		m := NewManager(opts, nil, summarizer, NewFileArtifactStore(t.TempDir()))
		input := copyInput(messages)
		input.ContextWindow = opts.ContextWindow
		input.MaxOutputTokens = opts.MaxOutputTokens
		got, err := m.Build(context.Background(), input)
		if err != nil {
			t.Fatalf("case %d: %v", caseNo, err)
		}
		if latestUser != "" && !containsContent(got.Messages, latestUser) &&
			!strings.Contains(strings.Join(got.Warnings, "\n"), "still exceeds budget") {
			t.Fatalf("case %d: dropped latest user message", caseNo)
		}
		assertNoOrphanToolResult(t, got.Messages)
		if got.TokensAfter > got.PromptBudget &&
			!strings.Contains(strings.Join(got.Warnings, "\n"), "still exceeds budget") {
			t.Fatalf("case %d: over budget without warning: %d > %d",
				caseNo, got.TokensAfter, got.PromptBudget)
		}
		if got.Compacted && (len(got.NewHistory) == 0 || !isSummaryMessage(got.NewHistory[0])) {
			t.Fatalf("case %d: compacted history has no capsule", caseNo)
		}
	}
}

func containsContent(messages []llm.Message, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func assertNoOrphanToolResult(t *testing.T, messages []llm.Message) {
	t.Helper()
	open := map[string]bool{}
	for _, message := range messages {
		if message.Role == llm.RoleAssistant {
			for _, call := range message.ToolCalls {
				open[call.ID] = true
			}
		}
		if message.Role == llm.RoleTool {
			if !open[message.ToolCallID] {
				t.Fatalf("tool result %q has no tool call in the same unit", message.ToolCallID)
			}
		}
	}
}

func TestBuildDegradesOldImagesDuringCompactionAndKeepsRecentImages(t *testing.T) {
	summarizer := &recordingSummarizer{}
	opts := testOptions()
	opts.ContextWindow = 4000
	opts.MaxOutputTokens = 400
	opts.CompactAt = 0.5
	opts.KeepRecentMessages = 2
	opts.KeepRecentTokens = 2500
	m := NewManager(opts, nil, summarizer, nil)

	oldImg := llm.Image{MediaType: "image/png", Width: 1200, Height: 900, SHA256: "abcdef0123456789"}
	recentImg := llm.Image{MediaType: "image/jpeg", Width: 400, Height: 300, SHA256: "1122334455667788"}

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "old screenshot " + largeText(1200), Images: []llm.Image{oldImg}},
		{Role: llm.RoleAssistant, Content: "old reply " + largeText(1200)},
		{Role: llm.RoleUser, Content: "mid " + largeText(1200)},
		{Role: llm.RoleAssistant, Content: "mid reply " + largeText(1200)},
		{Role: llm.RoleUser, Content: "recent screenshot", Images: []llm.Image{recentImg}},
	}
	got, err := m.Build(context.Background(), Input{
		SystemPrompt:    "system",
		Messages:        msgs,
		ContextWindow:   opts.ContextWindow,
		MaxOutputTokens: opts.MaxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compacted {
		t.Fatal("expected compaction to trigger")
	}
	if len(summarizer.calls) == 0 {
		t.Fatal("expected summarizer to be called")
	}
	// Summarizer must receive placeholders instead of raw images.
	sawPlaceholder := false
	for _, call := range summarizer.calls {
		for _, msg := range call {
			if len(msg.Images) > 0 {
				t.Fatalf("summarizer received raw images: %+v", msg.Images)
			}
			if strings.Contains(msg.Content, "[image: image/png 1200x900 sha256:abcdef01]") {
				sawPlaceholder = true
			}
		}
	}
	if !sawPlaceholder {
		t.Fatal("summarizer did not receive degraded image placeholder")
	}
	// Recent retained user message still has its original image.
	last := got.Messages[len(got.Messages)-1]
	if len(last.Images) != 1 || last.Images[0].SHA256 != recentImg.SHA256 {
		t.Fatalf("recent image lost during compaction: %+v", last)
	}
}
