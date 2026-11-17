package main

import (
	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/session"
)

// checkpointSessionBinding describes the conversation position captured
// with a workspace checkpoint. Model tool calls bind to the current
// turn; user-shell, manual and safety snapshots bind to the current
// leaf.
func checkpointSessionBinding(current *session.Session, kind gitkit.CheckpointKind, tool string) gitkit.SessionBinding {
	if current == nil {
		return gitkit.SessionBinding{}
	}
	if kind == gitkit.CheckpointAuto && tool != "user_bash" {
		anchor, turn := current.TurnBinding()
		return gitkit.SessionBinding{ID: current.Meta.ID, Anchor: anchor, Turn: turn}
	}
	return gitkit.SessionBinding{ID: current.Meta.ID, Anchor: current.CurrentLeaf()}
}
