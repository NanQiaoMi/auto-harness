package rpc

import (
	"context"
	"io"
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
