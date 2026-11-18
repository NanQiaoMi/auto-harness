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
