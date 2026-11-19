package ext

const (
	delegationAgentIDKey = "delegation.agentId"
	delegationParentKey  = "delegation.parentAgentId"
	delegationSpaceKey   = "delegation.workspace"
)

// SetDelegation attaches delegated-agent metadata to an event. It
// leaves values that are already present untouched, so an extension
// may annotate or override an event before the agent fills its own
// identity.
//
// Nothing is attached for the primary agent: its events must keep
// whatever Data they came with. Allocating an empty map for every
// event would make "Data == nil" useless as a test — a text delta
// would look like it carried a payload, and a consumer that tells an
// answer from reasoning by that test would misfile every answer.
func SetDelegation(e *Event, agentID, parentAgentID, workspace string) {
	if e == nil {
		return
	}
	if agentID == "" && parentAgentID == "" && workspace == "" {
		return
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if agentID != "" {
		if _, exists := e.Data[delegationAgentIDKey]; !exists {
			e.Data[delegationAgentIDKey] = agentID
		}
	}
	if parentAgentID != "" {
		if _, exists := e.Data[delegationParentKey]; !exists {
			e.Data[delegationParentKey] = parentAgentID
		}
	}
	if workspace != "" {
		if _, exists := e.Data[delegationSpaceKey]; !exists {
			e.Data[delegationSpaceKey] = workspace
		}
	}
}

// AgentID returns the delegated task that emitted e, or an empty string
// for the primary agent.
func AgentID(e *Event) string {
	return delegationString(e, delegationAgentIDKey)
}

// ParentAgentID returns the delegated task that started e's agent, or
// an empty string for a top-level agent.
func ParentAgentID(e *Event) string {
	return delegationString(e, delegationParentKey)
}

// Workspace returns the working tree a delegated agent treats as its
// root. The primary agent has no delegated workspace and returns "".
func Workspace(e *Event) string {
	return delegationString(e, delegationSpaceKey)
}

func delegationString(e *Event, key string) string {
	if e == nil || e.Data == nil {
		return ""
	}
	value, _ := e.Data[key].(string)
	return value
}
