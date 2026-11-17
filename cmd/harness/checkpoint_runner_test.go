package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/tools"
)

type recordingBefore struct {
	names []string
	err   error
}

func (r *recordingBefore) BeforeTool(ctx context.Context, toolName string, args json.RawMessage) error {
	r.names = append(r.names, toolName)
	return r.err
}

type recordingShell struct {
	commands []string
	result   tools.Result
}

func (r *recordingShell) Run(ctx context.Context, command string) (tools.Result, error) {
	r.commands = append(r.commands, command)
	return r.result, nil
}

func TestWorkspaceCheckpointRunnerSnapshotsBeforeShell(t *testing.T) {
	before := &recordingBefore{}
	shell := &recordingShell{result: tools.Result{Content: "ok\n"}}
	runner := &workspaceCheckpointRunner{checkpointer: before, runner: shell}

	result, err := runner.Run(context.Background(), "echo ok")
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "ok\n" {
		t.Fatalf("result = %+v", result)
	}
	if len(before.names) != 1 || before.names[0] != "user_bash" {
		t.Fatalf("checkpoint calls = %v", before.names)
	}
	if len(shell.commands) != 1 || shell.commands[0] != "echo ok" {
		t.Fatalf("shell calls = %v", shell.commands)
	}
}

func TestWorkspaceCheckpointRunnerFailsClosed(t *testing.T) {
	before := &recordingBefore{err: errors.New("snapshot failed")}
	shell := &recordingShell{result: tools.Result{Content: "should not run"}}
	runner := &workspaceCheckpointRunner{checkpointer: before, runner: shell}

	result, err := runner.Run(context.Background(), "rm something")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.ExitCode != 1 ||
		!strings.Contains(result.Content, "snapshot failed") {
		t.Fatalf("blocked result = %+v", result)
	}
	if len(shell.commands) != 0 {
		t.Fatalf("shell ran after checkpoint failure: %v", shell.commands)
	}
}
