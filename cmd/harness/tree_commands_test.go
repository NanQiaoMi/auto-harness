package main

import (
	"testing"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// newTreeTest builds a session with one exchange and the command set
// that operates on it.
func newTreeTest(t *testing.T) (*treeSession, *ext.Bus, *session.Session) {
	t.Helper()
	dir := t.TempDir()
	sess, err := session.New(dir, "test-model")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	if err := sess.Append(
		llm.Message{Role: llm.RoleUser, Content: "first"},
		llm.Message{Role: llm.RoleAssistant, Content: "answer"},
	); err != nil {
		t.Fatalf("append: %v", err)
	}

	bus := ext.NewBus()
	ag := agent.New(agent.Config{Model: "test-model"}, bus, tools.NewRegistry())
	ag.Session = sess
	ag.SetHistory(sess.Messages)

	return &treeSession{sess: sess, ag: ag, dir: dir, bus: bus}, bus, sess
}

// firstMessage returns the ID of the first message entry on the current
// branch.
func firstMessage(t *testing.T, tree *session.Tree) string {
	t.Helper()
	for _, e := range tree.CurrentPath() {
		if e.Type == session.EntryMessage {
			return e.ID
		}
	}
	t.Fatal("the session has no message entry")
	return ""
}

// countSessions returns how many session files the directory holds.
func countSessions(t *testing.T, dir string) int {
	t.Helper()
	list, err := session.List(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(list)
}

func TestForkCanBeCancelled(t *testing.T) {
	ts, bus, sess := newTreeTest(t)
	before := countSessions(t, ts.dir)

	bus.On(ext.EventSessionBeforeFork, func(e *ext.Event) error {
		if e.Reason != "fork" {
			t.Errorf("reason = %q, want fork", e.Reason)
		}
		if e.EntryID == "" {
			t.Error("the event carries no entry to fork from")
		}
		if e.EntryID != firstMessage(t, sess.Tree()) {
			t.Errorf("entry = %q, want the last user message", e.EntryID)
		}
		e.Cancel = true
		e.BlockReason = "no forks today"
		return nil
	})

	if err := runFork(ts, "", false); err != nil {
		t.Fatalf("runFork: %v", err)
	}
	if after := countSessions(t, ts.dir); after != before {
		t.Errorf("a cancelled fork wrote a session: %d -> %d files", before, after)
	}
}

func TestCloneIsApprovedAndWritten(t *testing.T) {
	ts, _, _ := newTreeTest(t)
	before := countSessions(t, ts.dir)

	// A handler that does not cancel must not block the clone.
	ts.bus.On(ext.EventSessionBeforeFork, func(e *ext.Event) error {
		if e.Reason != "clone" {
			t.Errorf("reason = %q, want clone", e.Reason)
		}
		return nil
	})
	if err := runFork(ts, "", true); err != nil {
		t.Fatalf("runFork: %v", err)
	}
	if after := countSessions(t, ts.dir); after != before+1 {
		t.Errorf("the approved clone wrote %d files, want %d", after, before+1)
	}
}

func TestTreeSwitchCanBeCancelled(t *testing.T) {
	ts, bus, sess := newTreeTest(t)
	leaf := sess.Tree().LeafID()
	target := firstMessage(t, sess.Tree())

	bus.On(ext.EventSessionBeforeTree, func(e *ext.Event) error {
		if e.EntryID != target {
			t.Errorf("target = %q, want %q", e.EntryID, target)
		}
		if e.Data["from"] != leaf {
			t.Errorf("from = %v, want %q", e.Data["from"], leaf)
		}
		e.Cancel = true
		return nil
	})

	if err := switchBranch(ts, target); err != nil {
		t.Fatalf("switchBranch: %v", err)
	}
	if got := sess.Tree().LeafID(); got != leaf {
		t.Errorf("the leaf moved to %q, want it to stay at %q", got, leaf)
	}
}

func TestTreeSwitchMovesTheBranch(t *testing.T) {
	ts, _, sess := newTreeTest(t)
	target := firstMessage(t, sess.Tree())

	if err := switchBranch(ts, target); err != nil {
		t.Fatalf("switchBranch: %v", err)
	}
	if got := sess.Tree().LeafID(); got != target {
		t.Errorf("leaf = %q, want %q", got, target)
	}
	if got := len(ts.ag.History()); got != 1 {
		t.Errorf("context has %d messages, want the 1 before the switch", got)
	}
}
