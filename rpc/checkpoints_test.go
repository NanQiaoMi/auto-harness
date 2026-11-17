package rpc

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/gitkit"
)

func TestCheckpointRPCCommands(t *testing.T) {
	now := time.Now().UTC()
	items := []gitkit.Checkpoint{
		{ID: "newest", Tree: "t2", IndexTree: "i2", Created: now, Kind: gitkit.CheckpointAuto},
		{ID: "older", Tree: "t1", IndexTree: "i1", Created: now.Add(-time.Minute), Kind: gitkit.CheckpointManual},
	}
	server := NewServer(nil, nil, nil, io.Discard)
	server.Checkpoints = func(ctx context.Context) ([]gitkit.Checkpoint, error) {
		return items, nil
	}
	server.CreateCheckpoint = func(ctx context.Context, label string) (gitkit.Checkpoint, bool, error) {
		if label != "manual label" {
			t.Fatalf("label = %q", label)
		}
		return items[0], true, nil
	}
	var rewound string
	var reset bool
	server.RewindCheckpoint = func(ctx context.Context, selector string, resetHead bool) (gitkit.RewindResult, error) {
		rewound = selector
		reset = resetHead
		return gitkit.RewindResult{Restored: items[0]}, nil
	}

	response := dispatchRPC(t, server, `{"id":"1","type":"get_checkpoints"}`)
	if !response.Success {
		t.Fatalf("get checkpoints = %#v", response)
	}
	payload, ok := response.Data.(CheckpointsPayload)
	if !ok || len(payload.Checkpoints) != 2 {
		t.Fatalf("checkpoints payload = %#v", response.Data)
	}

	response = dispatchRPC(t, server, `{"id":"2","type":"create_checkpoint","label":"manual label"}`)
	if !response.Success {
		t.Fatalf("create checkpoint = %#v", response)
	}
	created, ok := response.Data.(CheckpointPayload)
	if !ok || !created.Created || created.Checkpoint.ID != "newest" {
		t.Fatalf("create payload = %#v", response.Data)
	}

	response = dispatchRPC(t, server,
		`{"id":"3","type":"rewind_checkpoint","checkpoint":"1","resetHead":false}`)
	if !response.Success {
		t.Fatalf("rewind checkpoint = %#v", response)
	}
	if rewound != "newest" || reset {
		t.Fatalf("rewind args = %q, %v", rewound, reset)
	}
}

func TestCheckpointRPCRequiresConfiguration(t *testing.T) {
	server := NewServer(nil, nil, nil, io.Discard)
	response := dispatchRPC(t, server, `{"id":"1","type":"get_checkpoints"}`)
	if response.Success || response.Error == "" {
		t.Fatalf("unconfigured response = %#v", response)
	}
}

func TestCheckpointRPCRestoreContext(t *testing.T) {
	items := []gitkit.Checkpoint{
		{ID: "newest", Tree: "t2", IndexTree: "i2", Kind: gitkit.CheckpointAuto},
		{ID: "older", Tree: "t1", IndexTree: "i1", Kind: gitkit.CheckpointManual},
	}
	server := NewServer(nil, nil, nil, io.Discard)
	server.Checkpoints = func(ctx context.Context) ([]gitkit.Checkpoint, error) {
		return items, nil
	}
	var selector string
	server.UndoTurn = func(ctx context.Context, got string) (gitkit.TurnRewindResult, error) {
		selector = got
		return gitkit.TurnRewindResult{
			RewindResult:         gitkit.RewindResult{Restored: items[0]},
			SessionAnchor:        "turn-1",
			ConversationRestored: true,
		}, nil
	}
	rewindCalled := false
	server.RewindCheckpoint = func(ctx context.Context, selector string, resetHead bool) (gitkit.RewindResult, error) {
		rewindCalled = true
		return gitkit.RewindResult{}, nil
	}

	response := dispatchRPC(t, server,
		`{"id":"1","type":"rewind_checkpoint","checkpoint":"1","restoreContext":true}`)
	if !response.Success {
		t.Fatalf("restore context = %#v", response)
	}
	if selector != "newest" {
		t.Fatalf("undo selector = %q", selector)
	}
	if rewindCalled {
		t.Fatal("workspace-only rewind ran for restoreContext")
	}
	result, ok := response.Data.(gitkit.TurnRewindResult)
	if !ok || !result.ConversationRestored || result.SessionAnchor != "turn-1" {
		t.Fatalf("restore context payload = %#v", response.Data)
	}

	selector = ""
	response = dispatchRPC(t, server,
		`{"id":"2","type":"rewind_checkpoint","checkpoint":"1","restoreContext":true,"resetHead":false}`)
	if response.Success {
		t.Fatalf("restoreContext with resetHead=false = %#v", response)
	}
	if selector != "" {
		t.Fatalf("undo ran despite invalid flags: selector = %q", selector)
	}
}

func TestCheckpointRPCRestoreContextRequiresUndo(t *testing.T) {
	server := NewServer(nil, nil, nil, io.Discard)
	server.Checkpoints = func(ctx context.Context) ([]gitkit.Checkpoint, error) {
		return []gitkit.Checkpoint{{ID: "newest"}}, nil
	}
	server.RewindCheckpoint = func(ctx context.Context, selector string, resetHead bool) (gitkit.RewindResult, error) {
		return gitkit.RewindResult{}, nil
	}
	response := dispatchRPC(t, server,
		`{"id":"1","type":"rewind_checkpoint","checkpoint":"1","restoreContext":true}`)
	if response.Success || response.Error != "turn undo is not configured" {
		t.Fatalf("unconfigured undo = %#v", response)
	}
}

func TestCheckpointRPCRejectsRestoreWhileStreaming(t *testing.T) {
	server := NewServer(nil, nil, nil, io.Discard)
	server.Checkpoints = func(ctx context.Context) ([]gitkit.Checkpoint, error) {
		return []gitkit.Checkpoint{{ID: "newest"}}, nil
	}
	server.RewindCheckpoint = func(ctx context.Context, selector string, resetHead bool) (gitkit.RewindResult, error) {
		return gitkit.RewindResult{}, nil
	}
	server.setStreaming(true)
	response := dispatchRPC(t, server,
		`{"id":"1","type":"rewind_checkpoint","checkpoint":"1","restoreContext":true}`)
	if response.Success || !strings.Contains(response.Error, "run is in progress") {
		t.Fatalf("streaming restore = %#v", response)
	}
}

func dispatchRPC(t *testing.T, server *Server, raw string) *Response {
	t.Helper()
	command, err := parseCommand([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	response := server.dispatch(context.Background(), command)
	if response == nil {
		t.Fatal("dispatch returned no response")
	}
	return response
}
