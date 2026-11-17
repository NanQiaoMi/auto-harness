package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func testTool(name string) *Tool {
	return &Tool{
		Name:        name,
		Description: name,
		Execute: func(context.Context, string, json.RawMessage, func(Update)) (Result, error) {
			return Result{}, nil
		},
	}
}

func TestSetActiveToolsExactCanExposeNone(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("bash")); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetActiveToolsExact(nil); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.GetActiveTools()); got != 0 {
		t.Fatalf("active tools = %d, want 0", got)
	}
	if err := registry.SetActiveToolsExact([]string{"bash"}); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.GetActiveTools()); got != 1 {
		t.Fatalf("active tools = %d, want 1", got)
	}
	if err := registry.SetActiveToolsExact([]string{"missing"}); err == nil {
		t.Fatal("unknown tool was accepted")
	}
}

func TestSetActiveToolsLegacyEmptyStillMeansAll(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("bash")); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetActiveTools(nil); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.GetActiveTools()); got != 1 {
		t.Fatalf("active tools = %d, want all", got)
	}
}
