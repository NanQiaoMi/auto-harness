package rpc

import (
	"context"
	"io"
	"testing"

	"github.com/NanQiaoMi/auto-harness/orchestrator"
)

func TestDelegatedAgentRPCCommands(t *testing.T) {
	task := orchestrator.Task{
		ID:    "sub-1",
		Name:  "review",
		State: orchestrator.StateRunning,
	}
	server := NewServer(nil, nil, nil, io.Discard)
	server.Agents = func() []orchestrator.Task {
		return []orchestrator.Task{task}
	}
	server.AgentTask = func(id string) (orchestrator.Task, error) {
		if id != task.ID {
			t.Fatalf("id = %q", id)
		}
		return task, nil
	}
	cancelled := ""
	server.CancelAgent = func(ctx context.Context, id string) error {
		cancelled = id
		return nil
	}

	response := dispatchRPC(t, server, `{"id":"1","type":"get_agents"}`)
	if !response.Success {
		t.Fatalf("get_agents = %#v", response)
	}
	payload, ok := response.Data.(AgentsPayload)
	if !ok || len(payload.Tasks) != 1 || payload.Tasks[0].ID != task.ID {
		t.Fatalf("agents payload = %#v", response.Data)
	}

	response = dispatchRPC(t, server, `{"id":"2","type":"get_agent","agentId":"sub-1"}`)
	if !response.Success {
		t.Fatalf("get_agent = %#v", response)
	}
	one, ok := response.Data.(AgentPayload)
	if !ok || one.Task.ID != task.ID {
		t.Fatalf("agent payload = %#v", response.Data)
	}

	response = dispatchRPC(t, server, `{"id":"3","type":"cancel_agent","agentId":"sub-1"}`)
	if !response.Success || cancelled != task.ID {
		t.Fatalf("cancel_agent = %#v cancelled=%q", response, cancelled)
	}
}

func TestDelegatedAgentRPCRequiresConfiguration(t *testing.T) {
	server := NewServer(nil, nil, nil, io.Discard)
	response := dispatchRPC(t, server, `{"id":"1","type":"get_agents"}`)
	if !response.Success {
		t.Fatalf("unconfigured list = %#v", response)
	}
	response = dispatchRPC(t, server, `{"id":"2","type":"get_agent","agentId":"sub-1"}`)
	if response.Success {
		t.Fatalf("unconfigured get = %#v", response)
	}
}
