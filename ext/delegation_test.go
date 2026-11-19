package ext

import "testing"

func TestDelegationMetadataRoundTripsThroughData(t *testing.T) {
	e := &Event{Data: map[string]any{"thinking": true}}
	SetDelegation(e, "sub-1", "sub-0", "/tmp/work")

	if got := AgentID(e); got != "sub-1" {
		t.Fatalf("AgentID = %q", got)
	}
	if got := ParentAgentID(e); got != "sub-0" {
		t.Fatalf("ParentAgentID = %q", got)
	}
	if got := Workspace(e); got != "/tmp/work" {
		t.Fatalf("Workspace = %q", got)
	}
	if e.Data["thinking"] != true {
		t.Fatal("SetDelegation replaced unrelated event data")
	}
}

func TestDelegationMetadataDoesNotOverwriteExistingValues(t *testing.T) {
	e := &Event{}
	SetDelegation(e, "sub-1", "", "/tmp/one")
	SetDelegation(e, "sub-2", "sub-0", "/tmp/two")

	if got := AgentID(e); got != "sub-1" {
		t.Fatalf("AgentID = %q, want existing value", got)
	}
	if got := ParentAgentID(e); got != "sub-0" {
		t.Fatalf("ParentAgentID = %q", got)
	}
	if got := Workspace(e); got != "/tmp/one" {
		t.Fatalf("Workspace = %q, want existing value", got)
	}
}

func TestDelegationHelpersHandleNilAndMissingData(t *testing.T) {
	if AgentID(nil) != "" || ParentAgentID(nil) != "" || Workspace(nil) != "" {
		t.Fatal("nil event returned delegation metadata")
	}
	e := &Event{}
	if AgentID(e) != "" || ParentAgentID(e) != "" || Workspace(e) != "" {
		t.Fatal("empty event returned delegation metadata")
	}
}

// TestPrimaryAgentEventsKeepTheirData checks that a primary agent's
// event is not given an empty payload map.
//
// The distinction matters: a consumer tells an answer from reasoning
// by testing Data for nil (the thinking delta is the one that carries a
// flag). An empty map on every event would make every answer look like
// reasoning.
func TestPrimaryAgentEventsKeepTheirData(t *testing.T) {
	e := &Event{Type: EventMessageUpdate, Text: "answer"}
	SetDelegation(e, "", "", "")
	if e.Data != nil {
		t.Errorf("Data = %v, want nil for a primary-agent event", e.Data)
	}

	// A delegated agent's event does carry identity.
	e = &Event{Type: EventMessageUpdate, Text: "answer"}
	SetDelegation(e, "agent-1", "parent-0", "/tmp/ws")
	if AgentID(e) != "agent-1" || ParentAgentID(e) != "parent-0" || Workspace(e) != "/tmp/ws" {
		t.Errorf("delegation metadata = %v", e.Data)
	}
}
