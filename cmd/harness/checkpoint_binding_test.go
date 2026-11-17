package main

import (
	"testing"

	"github.com/NanQiaoMi/auto-harness/gitkit"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
)

func TestCheckpointSessionBindingUsesTurnForModelTools(t *testing.T) {
	sess, err := session.New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Append(
		llm.Message{Role: llm.RoleUser, Content: "first"},
		llm.Message{Role: llm.RoleAssistant, Content: "answer"},
		llm.Message{Role: llm.RoleUser, Content: "second"},
	); err != nil {
		t.Fatal(err)
	}
	anchor := sess.CheckpointAnchor()
	turn := sess.CurrentTurn()
	leaf := sess.CurrentLeaf()
	if anchor == "" || turn == "" || leaf == "" || anchor == leaf {
		t.Fatalf("setup = anchor %q turn %q leaf %q", anchor, turn, leaf)
	}

	auto := checkpointSessionBinding(sess, gitkit.CheckpointAuto, "bash")
	if auto.ID != sess.Meta.ID || auto.Anchor != anchor || auto.Turn != turn {
		t.Fatalf("auto binding = %#v", auto)
	}
	shell := checkpointSessionBinding(sess, gitkit.CheckpointAuto, "user_bash")
	if shell.ID != sess.Meta.ID || shell.Anchor != leaf || shell.Turn != "" {
		t.Fatalf("user shell binding = %#v", shell)
	}
	manual := checkpointSessionBinding(sess, gitkit.CheckpointManual, "repl")
	if manual.Anchor != leaf || manual.Turn != "" {
		t.Fatalf("manual binding = %#v", manual)
	}
	if zero := checkpointSessionBinding(nil, gitkit.CheckpointAuto, "bash"); zero != (gitkit.SessionBinding{}) {
		t.Fatalf("nil session binding = %#v", zero)
	}
}
