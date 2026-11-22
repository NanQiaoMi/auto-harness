package contextmgr

import (
	"testing"

	"github.com/NanQiaoMi/auto-harness/llm"
)

func TestHeuristicEstimatorIsConservative(t *testing.T) {
	e := HeuristicEstimator{}

	if got := e.EstimateText(""); got != 0 {
		t.Fatalf("empty text = %d, want 0", got)
	}
	if got := e.EstimateText(string(make([]byte, 400))); got != 111 {
		t.Fatalf("400 ASCII bytes = %d, want the conservative 111", got)
	}
	if got := e.EstimateText("你好世界"); got != 5 {
		t.Fatalf("four non-ASCII runes = %d, want 5", got)
	}
}

func TestHeuristicEstimatorIncludesToolCallsAndSchemas(t *testing.T) {
	e := HeuristicEstimator{}
	plain := llm.Message{Role: llm.RoleAssistant, Content: "run it"}
	called := plain
	called.ToolCalls = []llm.ToolCall{{
		ID:        "call-1",
		Name:      "bash",
		Arguments: []byte(`{"command":"echo hello"}`),
	}}

	if e.EstimateMessage(called) <= e.EstimateMessage(plain) {
		t.Fatal("a tool call must add to the message estimate")
	}
	if e.EstimateTools(nil) != 0 {
		t.Fatal("no tools must cost no prompt tokens")
	}
	tools := []llm.ToolDefinition{{
		Name:        "bash",
		Description: "run a command",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
		},
	}}
	if e.EstimateTools(tools) <= 0 {
		t.Fatal("a tool schema must have a positive estimate")
	}
}

func TestHeuristicEstimatorIncludesImages(t *testing.T) {
	e := HeuristicEstimator{}
	plain := llm.Message{Role: llm.RoleUser, Content: "see"}
	smallImg := plain
	smallImg.Images = []llm.Image{{MediaType: "image/png", Width: 100, Height: 100}}
	bigImg := plain
	bigImg.Images = []llm.Image{{MediaType: "image/png", Width: 3000, Height: 3000}}
	noDims := plain
	noDims.Images = []llm.Image{{MediaType: "image/png"}}

	if got, base := e.EstimateMessage(smallImg), e.EstimateMessage(plain); got <= base {
		t.Fatalf("small image estimate %d did not exceed plain %d", got, base)
	}
	if e.EstimateMessage(bigImg) <= e.EstimateMessage(smallImg) {
		t.Fatalf("big image %d should cost more than small image %d",
			e.EstimateMessage(bigImg), e.EstimateMessage(smallImg))
	}
	if e.EstimateMessage(noDims) <= e.EstimateMessage(plain) {
		t.Fatalf("dimensionless image %d should add fallback tokens over %d",
			e.EstimateMessage(noDims), e.EstimateMessage(plain))
	}
}
