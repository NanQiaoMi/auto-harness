package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execTool(t *testing.T, tool *Tool, args string) Result {
	t.Helper()
	res, err := tool.Execute(context.Background(), "c1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

func TestReadWriteEditRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "a.txt")

	res := execTool(t, NewWriteTool(), `{"path":"`+path+`","content":"line1\nline2\nline3\n"}`)
	if res.IsError {
		t.Fatalf("write: %s", res.Content)
	}

	res = execTool(t, NewReadTool(), `{"path":"`+path+`"}`)
	if res.IsError {
		t.Fatalf("read: %s", res.Content)
	}
	if !strings.Contains(res.Content, "line2") || !strings.Contains(res.Content, "2\tline2") {
		t.Errorf("read content = %q", res.Content)
	}

	// offset/limit
	res = execTool(t, NewReadTool(), `{"path":"`+path+`","offset":2,"limit":1}`)
	if strings.Contains(res.Content, "line1") || !strings.Contains(res.Content, "line2") {
		t.Errorf("offset/limit content = %q", res.Content)
	}

	// unique edit
	res = execTool(t, NewEditTool(), `{"path":"`+path+`","old_string":"line2","new_string":"LINE2"}`)
	if res.IsError {
		t.Fatalf("edit: %s", res.Content)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "line1\nLINE2\nline3\n" {
		t.Errorf("after edit = %q", data)
	}

	// non-unique old_string fails
	execTool(t, NewWriteTool(), `{"path":"`+path+`","content":"aa aa aa"}`)
	res = execTool(t, NewEditTool(), `{"path":"`+path+`","old_string":"aa","new_string":"b"}`)
	if !res.IsError || !strings.Contains(res.Content, "3 times") {
		t.Errorf("want uniqueness error, got %q", res.Content)
	}

	// replace_all works
	res = execTool(t, NewEditTool(), `{"path":"`+path+`","old_string":"aa","new_string":"b","replace_all":true}`)
	if res.IsError {
		t.Fatalf("replace_all: %s", res.Content)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "b b b" {
		t.Errorf("after replace_all = %q", data)
	}

	// missing old_string fails
	res = execTool(t, NewEditTool(), `{"path":"`+path+`","old_string":"zzz","new_string":"x"}`)
	if !res.IsError {
		t.Error("want not-found error")
	}
}

func TestReadMissingFileIsError(t *testing.T) {
	res := execTool(t, NewReadTool(), `{"path":"/nonexistent/nope.txt"}`)
	if !res.IsError {
		t.Error("want error result for missing file")
	}
}

func TestBashTool(t *testing.T) {
	res := execTool(t, NewBashTool(), `{"command":"echo hi && echo err >&2"}`)
	if res.IsError {
		t.Fatalf("bash: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hi") || !strings.Contains(res.Content, "err") {
		t.Errorf("output = %q", res.Content)
	}

	// non-zero exit is an error result but still returns output
	res = execTool(t, NewBashTool(), `{"command":"echo out; exit 3"}`)
	if !res.IsError || !strings.Contains(res.Content, "out") {
		t.Errorf("want error with output, got %+v", res)
	}

	// timeout
	res = execTool(t, NewBashTool(), `{"command":"sleep 5","timeout_ms":100}`)
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Errorf("want timeout error, got %+v", res)
	}

	// bad args
	res = execTool(t, NewBashTool(), `{`)
	if !res.IsError {
		t.Error("want argument error")
	}
}

func TestTruncate(t *testing.T) {
	big := strings.Repeat("x", maxToolOutput+100)
	out := truncate(big)
	if len(out) >= len(big) {
		t.Error("output not truncated")
	}
	if !strings.Contains(out, "truncated") {
		t.Error("missing truncation marker")
	}
}
