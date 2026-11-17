package gitkit

import (
	"context"
	"fmt"
	"strings"
)

// Conversation is the durable side of a turn undo. gitkit intentionally
// depends on this small interface rather than the session package so the
// workspace and conversation layers remain replaceable independently.
type Conversation interface {
	SessionID() string
	CurrentLeaf() string
	CurrentTurn() string
	CheckpointAnchor() string
	CanSetLeaf(string) bool
	SetLeaf(string) error
	ReloadHistory()
}

// TurnRewindResult is a workspace rewind plus the conversation move that
// removes the turn which produced it.
type TurnRewindResult struct {
	RewindResult
	SessionID            string `json:"sessionId,omitempty"`
	SessionAnchor        string `json:"sessionAnchor,omitempty"`
	SessionTurn          string `json:"sessionTurn,omitempty"`
	ConversationRestored bool   `json:"conversationRestored"`
	WorkspaceRestored    bool   `json:"workspaceRestored"`
}

// UndoTurn restores the state before the current conversation turn and
// moves the conversation to that turn's anchor. A turn may contain
// several tool calls, so the oldest checkpoint bound to the turn wins:
// it is the snapshot taken before the first mutation. Unlike Rewind,
// it never leaves an assistant tool call without its tool result in the
// active transcript.
func (m *Manager) UndoTurn(ctx context.Context, convo Conversation, selector string) (TurnRewindResult, error) {
	if convo == nil {
		return TurnRewindResult{}, fmt.Errorf("gitkit: undo turn requires a conversation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	root, err := Repository(ctx, m.Start)
	if err != nil {
		return TurnRewindResult{}, err
	}
	items, err := m.listLocked(ctx, root)
	if err != nil {
		return TurnRewindResult{}, err
	}
	state, err := snapshotState(ctx, root)
	if err != nil {
		return TurnRewindResult{}, err
	}
	sessionID := convo.SessionID()
	turnID := convo.CurrentTurn()
	anchor := convo.CheckpointAnchor()
	target, found, err := chooseConversationCheckpoint(items, state, selector, sessionID, turnID)
	if err != nil {
		return TurnRewindResult{}, err
	}
	currentLeaf := convo.CurrentLeaf()
	if !found {
		return m.undoConversationOnlyLocked(ctx, root, convo, currentLeaf, sessionID, turnID, anchor)
	}
	if strings.TrimSpace(selector) == "" && target.SessionAnchor != anchor {
		return TurnRewindResult{}, fmt.Errorf(
			"gitkit: checkpoint %s is anchored at %q, but the current turn starts at %q",
			target.ID, target.SessionAnchor, anchor)
	}
	if !convo.CanSetLeaf(target.SessionAnchor) {
		return TurnRewindResult{}, fmt.Errorf(
			"gitkit: checkpoint %s points at missing conversation entry %q",
			target.ID, target.SessionAnchor)
	}

	if checkpointMatchesWorkspace(target, state, true) &&
		target.SessionAnchor == currentLeaf {
		m.decorate([]*Checkpoint{&target}, state)
		return TurnRewindResult{
			RewindResult:  RewindResult{Restored: target, Noop: true},
			SessionID:     sessionID,
			SessionAnchor: target.SessionAnchor,
			SessionTurn:   target.SessionTurn,
		}, nil
	}

	if err := ensureBranchAvailable(ctx, root, target); err != nil {
		return TurnRewindResult{}, err
	}
	safety, _, err := m.createLocked(ctx, "before undo "+target.ID, CheckpointSafety, "undo")
	if err != nil {
		return TurnRewindResult{}, fmt.Errorf("gitkit: create safety checkpoint: %w", err)
	}

	didReset := !sameHeadState(target, state)
	refsReset := target.RefsBlob != ""
	if err := applyCheckpoint(ctx, root, target, true); err != nil {
		if rollbackErr := applyCheckpoint(ctx, root, safety, true); rollbackErr != nil {
			return TurnRewindResult{}, fmt.Errorf(
				"gitkit: undo %s failed: %w; rollback to safety %s also failed: %v",
				target.ID, err, safety.ID, rollbackErr)
		}
		return TurnRewindResult{}, fmt.Errorf(
			"gitkit: undo %s failed and was rolled back to safety %s: %w",
			target.ID, safety.ID, err)
	}

	if err := convo.SetLeaf(target.SessionAnchor); err != nil {
		// Keep the two layers in step. The workspace is restored first
		// because it is the part with an independent safety snapshot.
		if rollbackErr := applyCheckpoint(ctx, root, safety, true); rollbackErr != nil {
			return TurnRewindResult{}, fmt.Errorf(
				"gitkit: restore conversation after checkpoint %s failed: %w; workspace rollback to safety %s also failed: %v",
				target.ID, err, safety.ID, rollbackErr)
		}
		if leafErr := convo.SetLeaf(currentLeaf); leafErr != nil {
			return TurnRewindResult{}, fmt.Errorf(
				"gitkit: restore conversation after checkpoint %s failed: %w; workspace rolled back, but conversation rollback also failed: %v",
				target.ID, err, leafErr)
		}
		convo.ReloadHistory()
		return TurnRewindResult{}, fmt.Errorf(
			"gitkit: restore conversation after checkpoint %s failed and was rolled back: %w",
			target.ID, err)
	}
	convo.ReloadHistory()

	finalState, err := snapshotState(ctx, root)
	if err != nil {
		return TurnRewindResult{}, err
	}
	m.decorate([]*Checkpoint{&target, &safety}, finalState)
	warning := ""
	if target.RefsBlob == "" {
		warning = "checkpoint predates full ref snapshots; only files, index and HEAD were restored"
	}
	return TurnRewindResult{
		RewindResult: RewindResult{
			Restored:  target,
			Safety:    safety,
			HeadReset: didReset,
			RefsReset: refsReset,
			Warning:   warning,
		},
		SessionID:            sessionID,
		SessionAnchor:        target.SessionAnchor,
		SessionTurn:          target.SessionTurn,
		ConversationRestored: true,
		WorkspaceRestored:    true,
	}, nil
}

// undoConversationOnlyLocked handles a turn that has no workspace
// checkpoint, either because it was read-only or because it has just
// been typed. The conversation still moves, and the safety checkpoint
// makes the move reversible, but files are deliberately left alone.
func (m *Manager) undoConversationOnlyLocked(ctx context.Context, root string, convo Conversation, currentLeaf, sessionID, turnID, anchor string) (TurnRewindResult, error) {
	if !convo.CanSetLeaf(anchor) {
		return TurnRewindResult{}, fmt.Errorf(
			"gitkit: conversation entry %q is missing", anchor)
	}
	if anchor == currentLeaf {
		return TurnRewindResult{
			RewindResult:  RewindResult{Noop: true},
			SessionID:     sessionID,
			SessionAnchor: anchor,
			SessionTurn:   turnID,
		}, nil
	}
	safety, _, err := m.createLocked(ctx, "before undo "+turnID, CheckpointSafety, "undo")
	if err != nil {
		return TurnRewindResult{}, fmt.Errorf("gitkit: create safety checkpoint: %w", err)
	}
	if err := convo.SetLeaf(anchor); err != nil {
		if leafErr := convo.SetLeaf(currentLeaf); leafErr != nil {
			return TurnRewindResult{}, fmt.Errorf(
				"gitkit: restore conversation to %q failed: %w; conversation rollback to %q also failed: %v",
				anchor, err, currentLeaf, leafErr)
		}
		convo.ReloadHistory()
		return TurnRewindResult{}, fmt.Errorf(
			"gitkit: restore conversation to %q failed and was rolled back: %w",
			anchor, err)
	}
	convo.ReloadHistory()

	finalState, err := snapshotState(ctx, root)
	if err != nil {
		return TurnRewindResult{}, err
	}
	m.decorate([]*Checkpoint{&safety}, finalState)
	return TurnRewindResult{
		RewindResult: RewindResult{
			Safety:  safety,
			Warning: "no workspace checkpoint belongs to this turn; the conversation was restored without changing files",
		},
		SessionID:            sessionID,
		SessionAnchor:        anchor,
		SessionTurn:          turnID,
		ConversationRestored: true,
	}, nil
}

// chooseConversationCheckpoint selects the checkpoint to undo. An
// explicit selector is an ID or unique prefix. With no selector, the
// oldest checkpoint bound to the current turn is chosen, because the
// newest one only protects the last tool call of that turn.
func chooseConversationCheckpoint(items []Checkpoint, current workspaceState, selector, sessionID, turnID string) (Checkpoint, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector != "" {
		target, err := chooseCheckpoint(items, current, selector)
		if err != nil {
			return Checkpoint{}, false, err
		}
		if target.SessionID != sessionID {
			return Checkpoint{}, false, fmt.Errorf(
				"gitkit: checkpoint %s belongs to another session; use /rewind for workspace-only restore",
				target.ID)
		}
		return target, true, nil
	}
	if turnID == "" {
		return Checkpoint{}, false, fmt.Errorf("gitkit: no current turn to undo")
	}
	var target Checkpoint
	found := false
	for _, item := range items {
		if item.SessionID == sessionID && item.SessionTurn == turnID {
			// Items are newest first; keeping overwriting leaves the
			// oldest snapshot of this turn.
			target = item
			found = true
		}
	}
	return target, found, nil
}

// FormatTurnRewindResult formats a workspace-and-conversation undo.
func FormatTurnRewindResult(result TurnRewindResult) string {
	var text string
	switch {
	case result.Restored.ID != "":
		text = FormatRewindResult(result.RewindResult)
	case result.Noop:
		text = "workspace and conversation already match this turn"
	default:
		var b strings.Builder
		b.WriteString("workspace unchanged")
		if result.Safety.ID != "" {
			fmt.Fprintf(&b, "\nsafety checkpoint: %s", result.Safety.ID)
		}
		if result.Warning != "" {
			fmt.Fprintf(&b, "\nwarning: %s", result.Warning)
		}
		text = b.String()
	}
	if result.ConversationRestored {
		if result.SessionAnchor == "" {
			text += "\nconversation reset to before its first turn"
		} else {
			text += fmt.Sprintf("\nconversation reset to entry %s", result.SessionAnchor)
		}
	}
	return text
}
