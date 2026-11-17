package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/NanQiaoMi/auto-harness/contextmgr"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

type captureContextBuilder struct {
	mu     sync.Mutex
	inputs []contextmgr.Input
}

func (b *captureContextBuilder) Build(_ context.Context, in contextmgr.Input) (contextmgr.Result, error) {
	b.mu.Lock()
	b.inputs = append(b.inputs, in)
	b.mu.Unlock()
	return contextmgr.Result{
		Messages:     in.Messages,
		TokensBefore: 1,
		TokensAfter:  1,
		PromptBudget: 1000,
	}, nil
}

func (b *captureContextBuilder) Status(in contextmgr.Input) contextmgr.Status {
	return contextmgr.Status{
		Model:         in.Model,
		ContextWindow: in.ContextWindow,
		PromptBudget:  1000,
	}
}

func TestContextBudgetFollowsModelSwitch(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{Content: "one"}, {Content: "two"},
	}}
	builder := &captureContextBuilder{}
	bus := ext.NewBus()
	ag := New(Config{
		Provider: provider,
		Model:    "model-a",
		MaxTurns: 2,
		ContextBudget: func(model string) (int, int) {
			switch model {
			case "model-a":
				return 4000, 600
			case "model-b":
				return 9000, 1200
			default:
				return 0, 0
			}
		},
	}, bus, tools.NewRegistry())
	ag.SetContextManager(builder)

	if _, err := ag.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	ag.SetModel("model-b")
	if _, err := ag.Run(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	builder.mu.Lock()
	defer builder.mu.Unlock()
	if len(builder.inputs) != 2 {
		t.Fatalf("context builds = %d, want 2", len(builder.inputs))
	}
	if builder.inputs[0].ContextWindow != 4000 || builder.inputs[0].MaxOutputTokens != 600 {
		t.Fatalf("model-a budget = %+v", builder.inputs[0])
	}
	if builder.inputs[1].ContextWindow != 9000 || builder.inputs[1].MaxOutputTokens != 1200 {
		t.Fatalf("model-b budget = %+v", builder.inputs[1])
	}

	status, ok := ag.ContextStatus()
	if !ok {
		t.Fatal("the context inspector was not found")
	}
	if status.Model != "model-b" || status.ContextWindow != 9000 {
		t.Fatalf("status = %+v", status)
	}
}

func TestExplicitContextWindowWinsOverCatalog(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{{Content: "ok"}}}
	builder := &captureContextBuilder{}
	ag := New(Config{
		Provider:      provider,
		Model:         "model-a",
		MaxTurns:      1,
		ContextWindow: 7000,
		MaxTokens:     450,
		ContextBudget: func(string) (int, int) {
			return 4000, 600
		},
	}, ext.NewBus(), tools.NewRegistry())
	ag.SetContextManager(builder)

	if _, err := ag.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	builder.mu.Lock()
	defer builder.mu.Unlock()
	if len(builder.inputs) != 1 {
		t.Fatalf("context builds = %d", len(builder.inputs))
	}
	if builder.inputs[0].ContextWindow != 7000 {
		t.Fatalf("window = %d, want the explicit 7000", builder.inputs[0].ContextWindow)
	}
	if builder.inputs[0].MaxOutputTokens != 450 {
		t.Fatalf("output reservation = %d, want the explicit 450", builder.inputs[0].MaxOutputTokens)
	}
}

func TestAutomaticCompactionPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	sess, err := session.New(dir, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	fillSession(t, sess, 12)

	provider := &fakeProvider{responses: []llm.Message{{Content: "done"}}}
	bus := ext.NewBus()
	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 2}, bus, tools.NewRegistry())
	ag.Session = sess
	ag.SetHistory(sess.Tree().Messages())

	var compactEvents []*ext.Event
	bus.On(ext.EventSessionCompact, func(event *ext.Event) error {
		copy := *event
		compactEvents = append(compactEvents, &copy)
		return nil
	})

	mgr := contextmgr.NewManager(contextmgr.Options{
		ContextWindow:      2600,
		MaxOutputTokens:    400,
		ReserveTokens:      100,
		CompactAt:          0.5,
		KeepRecentMessages: 2,
		KeepRecentTokens:   500,
		SummaryMaxTokens:   200,
	}, nil, contextmgr.SummarizerFunc(func(context.Context, []llm.Message, int) (string, error) {
		return "CAPSULE", nil
	}), nil)
	ag.SetContextManager(mgr)

	if _, err := ag.Run(context.Background(), "LATEST QUESTION"); err != nil {
		t.Fatal(err)
	}
	history := ag.History()
	if len(history) == 0 || !strings.Contains(history[0].Content, "CAPSULE") {
		t.Fatalf("history does not start with the capsule: %#v", history)
	}
	if !historyContains(history, "LATEST QUESTION") || !historyContains(history, "done") {
		t.Fatalf("history lost the latest turn: %#v", history)
	}
	if historyContains(history, "user-0 ") {
		t.Fatal("the oldest exchange survived outside the capsule")
	}
	if len(compactEvents) != 1 {
		t.Fatalf("session_compact events = %d, want 1", len(compactEvents))
	}
	if compactEvents[0].Data["automatic"] != true {
		t.Fatalf("session_compact data = %#v", compactEvents[0].Data)
	}
	if compactEvents[0].Data["summary"] != "CAPSULE" {
		t.Fatalf("session_compact summary = %#v", compactEvents[0].Data["summary"])
	}

	var compactions []*session.Entry
	for _, entry := range sess.Tree().Entries() {
		if entry.Type == session.EntryCompaction {
			compactions = append(compactions, entry)
		}
	}
	if len(compactions) != 1 {
		t.Fatalf("persisted compactions = %d, want 1", len(compactions))
	}
	if compactions[0].Summary != "CAPSULE" {
		t.Fatalf("persisted summary = %q", compactions[0].Summary)
	}
	firstKept, ok := sess.Tree().Entry(compactions[0].FirstKeptEntryID)
	if !ok || firstKept.Message == nil {
		t.Fatalf("first kept entry %q is not a message", compactions[0].FirstKeptEntryID)
	}
	if !sameMessage(*firstKept.Message, history[1]) {
		t.Fatalf("first kept entry = %#v, want the first retained message %#v",
			*firstKept.Message, history[1])
	}

	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := session.Load(dir, sess.Meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if len(reloaded.Messages) == 0 || !strings.Contains(reloaded.Messages[0].Content, "CAPSULE") {
		t.Fatalf("reloaded messages = %#v", reloaded.Messages)
	}
	if !historyContains(reloaded.Messages, "LATEST QUESTION") ||
		!historyContains(reloaded.Messages, "done") {
		t.Fatalf("reloaded context lost retained messages: %#v", reloaded.Messages)
	}
	if historyContains(reloaded.Messages, "user-0 ") {
		t.Fatal("reloaded context retained compacted history")
	}
}

func TestAutomaticCompactionDoesNotPersistWhenContextHookRewrites(t *testing.T) {
	dir := t.TempDir()
	sess, err := session.New(dir, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	fillSession(t, sess, 12)

	provider := &fakeProvider{responses: []llm.Message{{Content: "done"}}}
	bus := ext.NewBus()
	var compactEvents int
	bus.On(ext.EventContext, func(event *ext.Event) error {
		event.Messages = append(event.Messages, llm.Message{
			Role: llm.RoleUser, Content: "TRANSIENT CONTEXT",
		})
		return nil
	})
	bus.On(ext.EventSessionCompact, func(event *ext.Event) error {
		compactEvents++
		return nil
	})

	ag := New(Config{Provider: provider, Model: "test-model", MaxTurns: 2}, bus, tools.NewRegistry())
	ag.Session = sess
	ag.SetHistory(sess.Tree().Messages())
	ag.SetContextManager(contextmgr.NewManager(contextmgr.Options{
		ContextWindow:      2600,
		MaxOutputTokens:    400,
		ReserveTokens:      100,
		CompactAt:          0.5,
		KeepRecentMessages: 2,
		KeepRecentTokens:   500,
	}, nil, contextmgr.SummarizerFunc(func(context.Context, []llm.Message, int) (string, error) {
		return "CAPSULE", nil
	}), nil))

	if _, err := ag.Run(context.Background(), "LATEST QUESTION"); err != nil {
		t.Fatal(err)
	}
	if historyContains(ag.History(), "CAPSULE") {
		t.Fatal("the transient request compaction replaced persistent history")
	}
	if compactEvents != 0 {
		t.Fatalf("session_compact events = %d, want 0", compactEvents)
	}
	for _, entry := range sess.Tree().Entries() {
		if entry.Type == session.EntryCompaction {
			t.Fatal("a transient request wrote a persistent compaction entry")
		}
	}
	if !historyContains(ag.History(), "user-0 ") {
		t.Fatal("the persistent history was modified")
	}
}

func TestFindKeptStartUsesCurrentBranch(t *testing.T) {
	sess, err := session.New(t.TempDir(), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.Append(
		llm.Message{Role: llm.RoleUser, Content: "root"},
		llm.Message{Role: llm.RoleAssistant, Content: "root answer"},
		llm.Message{Role: llm.RoleUser, Content: "repeat"},
		llm.Message{Role: llm.RoleAssistant, Content: "done"},
	); err != nil {
		t.Fatal(err)
	}
	messages := messageEntries(sess.Tree().Entries())
	if len(messages) != 4 {
		t.Fatalf("message entries = %d", len(messages))
	}
	rootID := messages[0].ID
	mainRepeatID := messages[2].ID
	mainLeafID := messages[3].ID

	if err := sess.SetLeaf(rootID); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(
		llm.Message{Role: llm.RoleUser, Content: "repeat"},
		llm.Message{Role: llm.RoleAssistant, Content: "done"},
	); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetLeaf(mainLeafID); err != nil {
		t.Fatal(err)
	}

	got := findKeptStart(sess.Tree().Branch(), []llm.Message{
		{Role: llm.RoleUser, Content: "repeat"},
		{Role: llm.RoleAssistant, Content: "done"},
	})
	if got != mainRepeatID {
		t.Fatalf("kept start = %q, want the current branch's %q", got, mainRepeatID)
	}
}

func fillSession(t *testing.T, sess *session.Session, pairs int) {
	t.Helper()
	for i := 0; i < pairs; i++ {
		if err := sess.Append(
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("user-%d %s", i, strings.Repeat("x", 600))},
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("assistant-%d %s", i, strings.Repeat("x", 600))},
		); err != nil {
			t.Fatal(err)
		}
	}
}

func messageEntries(entries []*session.Entry) []*session.Entry {
	var out []*session.Entry
	for _, entry := range entries {
		if entry.Type == session.EntryMessage && entry.Message != nil {
			out = append(out, entry)
		}
	}
	return out
}

func historyContains(messages []llm.Message, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}
