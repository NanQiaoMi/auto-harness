package schemas

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/jsonout"
	"github.com/NanQiaoMi/auto-harness/rpc"
)

func loadSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return doc
}

func toStringSet(v any) map[string]bool {
	out := map[string]bool{}
	slice, _ := v.([]any)
	for _, item := range slice {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

func TestSchemasMatchWireConstants(t *testing.T) {
	// 1. RPC schema
	rpcDoc := loadSchema(t, "rpc.v1.schema.json")
	if rpcDoc["x-schema-version"] != rpc.SchemaVersion {
		t.Errorf("rpc x-schema-version = %v, want %q", rpcDoc["x-schema-version"], rpc.SchemaVersion)
	}
	rpcDefs := rpcDoc["$defs"].(map[string]any)
	cmdEnum := toStringSet(rpcDefs["Command"].(map[string]any)["properties"].(map[string]any)["type"].(map[string]any)["enum"])
	for _, cmd := range []string{
		rpc.CmdPrompt, rpc.CmdAbort, rpc.CmdNewSession, rpc.CmdGetState,
		rpc.CmdGetMessages, rpc.CmdSetModel, rpc.CmdGetAvailableModels,
		rpc.CmdSetThinking, rpc.CmdCompact, rpc.CmdBash,
		rpc.CmdGetSessionStats, rpc.CmdGetContext, rpc.CmdGetEntries,
		rpc.CmdGetTree, rpc.CmdGetLastAssistantText, rpc.CmdSetSessionName,
		rpc.CmdGetCommands, rpc.CmdGetProfile, rpc.CmdGetPlugins,
		rpc.CmdFork, rpc.CmdClone, rpc.CmdGetCheckpoints,
		rpc.CmdCreateCheckpoint, rpc.CmdRewindCheckpoint,
		rpc.CmdGetAgents, rpc.CmdGetAgent, rpc.CmdCancelAgent,
	} {
		if !cmdEnum[cmd] {
			t.Errorf("rpc schema missing command %q", cmd)
		}
	}
	rpcEventEnum := toStringSet(rpcDefs["Event"].(map[string]any)["properties"].(map[string]any)["type"].(map[string]any)["enum"])
	for _, ev := range []string{
		rpc.EventAgentStart, rpc.EventAgentEnd, string(ext.EventAgentBeforeSettle),
		rpc.EventAgentSettled, string(ext.EventRetry), rpc.EventTurnStart,
		rpc.EventTurnEnd, rpc.EventMessageStart, rpc.EventMessageUpdate,
		rpc.EventMessageEnd, rpc.EventToolExecutionStart, rpc.EventToolExecutionEnd,
		rpc.EventRequestStart, rpc.EventSessionBeforeCompact, rpc.EventSessionCompact,
	} {
		if !rpcEventEnum[ev] {
			t.Errorf("rpc schema missing event %q", ev)
		}
	}

	// 2. JSON output schema
	jsonDoc := loadSchema(t, "jsonout.v1.schema.json")
	if jsonDoc["x-schema-version"] != jsonout.SchemaVersion {
		t.Errorf("jsonout x-schema-version = %v, want %q", jsonDoc["x-schema-version"], jsonout.SchemaVersion)
	}
	jsonDefs := jsonDoc["$defs"].(map[string]any)
	jsonEvEnum := toStringSet(jsonDefs["Record"].(map[string]any)["properties"].(map[string]any)["type"].(map[string]any)["enum"])
	for _, ev := range []string{
		jsonout.EventAgentStart, jsonout.EventAgentEnd, jsonout.EventAgentBeforeSettle,
		jsonout.EventAgentSettled, jsonout.EventRetry, jsonout.EventTurnStart,
		jsonout.EventTurnEnd, jsonout.EventRequestStart, jsonout.EventMessageStart,
		jsonout.EventMessageUpdate, jsonout.EventMessageEnd,
		jsonout.EventToolExecutionStart, jsonout.EventToolExecutionEnd,
		jsonout.EventSessionBeforeCompact, jsonout.EventSessionCompact,
	} {
		if !jsonEvEnum[ev] {
			t.Errorf("jsonout schema missing event %q", ev)
		}
	}

	// 3. Extension process protocol schema
	extDoc := loadSchema(t, "extension.v1.schema.json")
	if extDoc["x-schema-version"] != ext.SchemaVersion {
		t.Errorf("ext x-schema-version = %v, want %q", extDoc["x-schema-version"], ext.SchemaVersion)
	}
	extMethods := toStringSet(extDoc["properties"].(map[string]any)["method"].(map[string]any)["enum"])
	for _, m := range []string{
		ext.MethodManifest, ext.MethodToolExecute, ext.MethodCommandExecute,
		ext.MethodShutdown, ext.MethodEvent, ext.MethodConsult,
	} {
		if !extMethods[m] {
			t.Errorf("extension schema missing method %q", m)
		}
	}
}
