package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/tools"
)

// exampleExtensions returns the extensions bundled with the harness
// binary. This one demonstrates the full extension surface: hooks, a
// custom tool and a custom command. User extensions follow the same
// shape (see ext.Extension / ext.Named).
func exampleExtensions() []ext.Extension {
	return []ext.Extension{statsExtension()}
}

func statsExtension() ext.Extension {
	return ext.Named("stats", func(api *ext.API) error {
		var (
			toolCalls  int
			toolErrors int
			userMsgs   int
			turns      int
			usage      = struct{ in, out int }{}
		)

		api.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
			toolCalls++
			if e.ToolResult != nil && e.ToolResult.IsError {
				toolErrors++
			}
			return nil
		})
		api.On(ext.EventAgentStart, func(e *ext.Event) error {
			userMsgs++
			return nil
		})
		api.On(ext.EventTurnStart, func(e *ext.Event) error {
			turns++
			return nil
		})
		api.On(ext.EventAgentEnd, func(e *ext.Event) error {
			usage.in += e.Usage.InputTokens
			usage.out += e.Usage.OutputTokens
			return nil
		})

		api.RegisterCommand("stats", &ext.Command{
			Description: "show session statistics (from the stats extension)",
			Handler: func(ctx context.Context, args string) error {
				fmt.Printf("user inputs: %d, turns: %d, tool calls: %d (%d errors), tokens: %d in / %d out\n",
					userMsgs, turns, toolCalls, toolErrors, usage.in, usage.out)
				return nil
			},
		})

		// A minimal custom tool, registered exactly like built-ins.
		return api.RegisterTool(&tools.Tool{
			Name:          "current_time",
			Label:         "Current Time",
			Description:   "Return the current local date and time in RFC3339 format.",
			PromptSnippet: "current_time: get the current date/time",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
				return tools.Result{Content: time.Now().Format(time.RFC3339)}, nil
			},
		})
	})
}
