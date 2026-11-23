package extsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
)

func TestServerHandlesManifestToolCommandConsultAndEvents(t *testing.T) {
	srv := NewServer("go-ext", "Go SDK extension")
	srv.RegisterTool(ext.ManifestTool{
		Name:        "greet",
		Description: "say hello",
	}, func(ctx context.Context, callID string, args json.RawMessage) ext.ToolResult {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(args, &p)
		return ext.ToolResult{Content: "hi " + p.Name}
	})
	srv.RegisterCommand(ext.ManifestCommand{
		Name:        "ping",
		Description: "ping command",
	}, func(ctx context.Context, args string) (string, error) {
		return "pong " + args, nil
	})
	srv.OnConsult("tool_call", func(ctx context.Context, params ext.ConsultParams) ext.ConsultResult {
		if params.ToolCall != nil && params.ToolCall.Name == "forbidden" {
			return ext.ConsultResult{Block: true, Reason: "blocked by go-ext"}
		}
		return ext.ConsultResult{}
	})

	requests := strings.Join([]string{
		`{"schema_version":"1.0","id":1,"method":"manifest"}`,
		`{"schema_version":"1.0","id":2,"method":"tool.execute","params":{"tool_call_id":"c1","name":"greet","arguments":{"name":"world"}}}`,
		`{"schema_version":"1.0","id":3,"method":"command.execute","params":{"name":"ping","args":"42"}}`,
		`{"schema_version":"1.0","id":4,"method":"event.consult","params":{"event":"tool_call","toolCall":{"id":"c2","name":"forbidden"}}}`,
		`{"schema_version":"2.0","id":5,"method":"manifest"}`,
		`{"schema_version":"1.0","method":"shutdown"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(requests), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %d, want 5:\n%s", len(lines), out.String())
	}

	var mResp ext.Protocol
	if err := json.Unmarshal([]byte(lines[0]), &mResp); err != nil {
		t.Fatal(err)
	}
	var man ext.Manifest
	if err := json.Unmarshal(mResp.Result, &man); err != nil {
		t.Fatal(err)
	}
	if man.SchemaVersion != ext.SchemaVersion || man.Name != "go-ext" || len(man.Tools) != 1 || len(man.Commands) != 1 || len(man.Consults) != 1 {
		t.Errorf("manifest = %+v", man)
	}

	var tResp ext.Protocol
	_ = json.Unmarshal([]byte(lines[1]), &tResp)
	var tr ext.ToolResult
	_ = json.Unmarshal(tResp.Result, &tr)
	if tr.Content != "hi world" {
		t.Errorf("tool result = %+v", tr)
	}

	var cResp ext.Protocol
	_ = json.Unmarshal([]byte(lines[2]), &cResp)
	if !strings.Contains(string(cResp.Result), "pong 42") {
		t.Errorf("cmd result = %s", cResp.Result)
	}

	var conResp ext.Protocol
	_ = json.Unmarshal([]byte(lines[3]), &conResp)
	var cr ext.ConsultResult
	_ = json.Unmarshal(conResp.Result, &cr)
	if !cr.Block || cr.Reason != "blocked by go-ext" {
		t.Errorf("consult = %+v", cr)
	}

	var badResp ext.Protocol
	_ = json.Unmarshal([]byte(lines[4]), &badResp)
	if badResp.Error == nil || !strings.Contains(badResp.Error.Message, "schema_version") {
		t.Errorf("expected schema_version error, got %+v", badResp)
	}
}
