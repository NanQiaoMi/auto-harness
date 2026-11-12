//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mimi/auto-harness/agent"
	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/perm"
	"github.com/mimi/auto-harness/tools"
)

// TestLivePermissionDenied verifies the permission layer blocks a
// destructive tool call with the real model, and reports the block back
// to the model so it can react.
func TestLivePermissionDenied(t *testing.T) {
	apiKey(t) // require a live key
	isolate(t)

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{tools.NewBashTool(), tools.NewReadTool()} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}

	// The deny rule blocks a recursive removal. Nothing else is denied,
	// so the model may still call tools that do not match.
	set := perm.NewSet(perm.ActionAllow)
	set.Rules = []perm.Rule{
		{
			Name:   "destructive shell commands",
			Tools:  []string{"bash"},
			Args:   map[string]string{"command": `(?i)\brm\s+(-[a-z]*[rf]|--recursive)`},
			Action: perm.ActionDeny,
			Reason: "recursive removal is destructive",
		},
	}

	guard := perm.NewGuard(set, t.TempDir())
	guard.Approver = perm.DenyApprover{Reason: "no interactive session"}
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())
	if err := guard.Setup(api); err != nil {
		t.Fatalf("guard setup: %v", err)
	}

	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  6,
		MaxTokens: 2048,
	}, bus, reg)

	// The target of the blocked call: it must not exist afterwards.
	target := filepath.Join(t.TempDir(), "must-not-exist")

	var blockReason string
	var sawBlockedCall bool
	bus.On(ext.EventToolCall, func(e *ext.Event) error {
		if e.Block {
			blockReason = e.BlockReason
			sawBlockedCall = true
		}
		return nil
	})
	var toolError string
	bus.On(ext.EventToolResult, func(e *ext.Event) error {
		if e.ToolResult != nil && e.ToolResult.IsError {
			toolError = e.ToolResult.Content
		}
		return nil
	})

	res, err := ag.Run(context.Background(),
		"Use the bash tool to run exactly: rm -rf "+target+". "+
			"Then reply with one sentence about what happened.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawBlockedCall {
		t.Fatal("the destructive tool call was not blocked")
	}
	if !strings.Contains(blockReason, "destructive") {
		t.Errorf("block reason = %q, want it to mention the destructive command", blockReason)
	}
	// The blocked call is reported to the model as an error result.
	if !strings.Contains(toolError, "blocked") {
		t.Errorf("tool error = %q, want it to mention the block", toolError)
	}
	// The command never ran.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("the blocked command ran anyway")
	}
	// The model saw the block and answered.
	if strings.TrimSpace(res.Final) == "" {
		t.Errorf("expected a final answer, got %q", res.Final)
	}
	if !strings.Contains(strings.ToLower(res.Final), "block") &&
		!strings.Contains(strings.ToLower(res.Final), "denied") &&
		!strings.Contains(strings.ToLower(res.Final), "not allowed") &&
		!strings.Contains(strings.ToLower(res.Final), "permission") {
		t.Errorf("final answer = %q, want it to mention the permission block", res.Final)
	}
}

// TestLivePermissionAllowed verifies a permitted tool call runs with the
// real model.
func TestLivePermissionAllowed(t *testing.T) {
	apiKey(t) // require a live key
	isolate(t)

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{tools.NewBashTool()} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}

	set := perm.NewSet(perm.ActionAllow)
	guard := perm.NewGuard(set, t.TempDir())
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())
	if err := guard.Setup(api); err != nil {
		t.Fatalf("guard setup: %v", err)
	}

	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  6,
		MaxTokens: 2048,
	}, bus, reg)

	const marker = "harness-perm-allowed-8b2e"
	var toolResult string
	bus.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
		if e.ToolResult != nil {
			toolResult = e.ToolResult.Content
		}
		return nil
	})

	res, err := ag.Run(context.Background(),
		"Use the bash tool to run exactly: echo "+marker+". Then reply with the output you saw.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(toolResult, marker) {
		t.Errorf("tool result = %q, want it to contain %q", toolResult, marker)
	}
	if !strings.Contains(res.Final, marker) {
		t.Errorf("final answer = %q, want it to mention %q", res.Final, marker)
	}
}
