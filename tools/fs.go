package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fileReadLimit caps Read output (lines).
const (
	defaultReadLimit = 2000
	maxReadLimit     = 10000
	maxToolOutput    = 30000 // chars
)

// truncate caps oversized tool output, marking the cut point.
func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + fmt.Sprintf("\n... [truncated, %d chars total]", len(s))
}

func decodeArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return fmt.Errorf("missing arguments")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// NewReadTool returns the file-reading tool.
func NewReadTool() *Tool {
	return &Tool{
		Name:          "read",
		Label:         "Read File",
		Description:   "Read a UTF-8 text file. Returns the content with line numbers. Reads up to 2000 lines by default.",
		PromptSnippet: "read: read a text file (optional offset/limit for large files)",
		PromptGuidelines: []string{
			"Use read to inspect files before editing them.",
			"For large files, read only the range you need with offset/limit.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute or workspace-relative path of the file to read",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "1-based line number to start reading from (optional)",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to read (optional, default 2000)",
				},
			},
			"required": []string{"path"},
		},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error) {
			var params struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decodeArgs(args, &params); err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			data, err := os.ReadFile(params.Path)
			if err != nil {
				return Result{Content: fmt.Sprintf("read %s: %v", params.Path, err), IsError: true}, nil
			}
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			offset := params.Offset
			if offset < 1 {
				offset = 1
			}
			limit := params.Limit
			if limit <= 0 {
				limit = defaultReadLimit
			}
			if limit > maxReadLimit {
				limit = maxReadLimit
			}
			start := offset - 1
			if start > len(lines) {
				start = len(lines)
			}
			end := start + limit
			if end > len(lines) {
				end = len(lines)
			}
			var b strings.Builder
			for i := start; i < end; i++ {
				fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
			}
			return Result{Content: truncate(b.String())}, nil
		},
	}
}

// NewWriteTool returns the file-writing tool.
func NewWriteTool() *Tool {
	return &Tool{
		Name:          "write",
		Label:         "Write File",
		Description:   "Write a UTF-8 text file, creating parent directories as needed. Overwrites existing content.",
		PromptSnippet: "write: create or overwrite a text file",
		PromptGuidelines: []string{
			"Use write to create new files or fully replace file content.",
			"For partial changes prefer the edit tool.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute or workspace-relative path of the file to write",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full file content to write",
				},
			},
			"required": []string{"path", "content"},
		},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error) {
			var params struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decodeArgs(args, &params); err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			// The write takes the file's queue, so it cannot interleave
			// with an edit of the same file.
			var writeErr error
			err := WithFileMutation(params.Path, func() error {
				if dir := filepath.Dir(params.Path); dir != "" {
					if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
						writeErr = fmt.Errorf("write %s: %w", params.Path, mkErr)
						return nil
					}
				}
				if wErr := os.WriteFile(params.Path, []byte(params.Content), 0o644); wErr != nil {
					writeErr = fmt.Errorf("write %s: %w", params.Path, wErr)
				}
				return nil
			})
			if writeErr != nil {
				return Result{Content: writeErr.Error(), IsError: true}, nil
			}
			if err != nil {
				return Result{Content: fmt.Sprintf("write %s: %v", params.Path, err), IsError: true}, nil
			}
			return Result{Content: fmt.Sprintf("Wrote %d bytes to %s", len(params.Content), params.Path)}, nil
		},
	}
}

// NewEditTool returns the exact string replacement tool.
func NewEditTool() *Tool {
	return &Tool{
		Name:          "edit",
		Label:         "Edit File",
		Description:   "Replace an exact string in a file. Fails if old_string is not found or is not unique unless replace_all is true.",
		PromptSnippet: "edit: exact string replacement in an existing file",
		PromptGuidelines: []string{
			"old_string must match the file exactly, including whitespace.",
			"Include enough surrounding context to make old_string unique.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute or workspace-relative path of the file to edit",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "Exact text to replace (must appear exactly once unless replace_all)",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "Replacement text",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence of old_string (default false)",
				},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(Update)) (Result, error) {
			var params struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decodeArgs(args, &params); err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			// The edit takes the file's queue: it reads the file and
			// writes it back, and a write of the same file must not
			// slip in between.
			var editErr error
			var replaced int
			err := WithFileMutation(params.Path, func() error {
				data, readErr := os.ReadFile(params.Path)
				if readErr != nil {
					editErr = fmt.Errorf("edit %s: %w", params.Path, readErr)
					return nil
				}
				content := string(data)
				count := strings.Count(content, params.OldString)
				switch {
				case count == 0:
					editErr = fmt.Errorf("edit %s: old_string not found", params.Path)
					return nil
				case count > 1 && !params.ReplaceAll:
					editErr = fmt.Errorf("edit %s: old_string occurs %d times; add context to make it unique or set replace_all", params.Path, count)
					return nil
				}
				var updated string
				if params.ReplaceAll {
					updated = strings.ReplaceAll(content, params.OldString, params.NewString)
					replaced = count
				} else {
					updated = strings.Replace(content, params.OldString, params.NewString, 1)
					replaced = 1
				}
				if wErr := os.WriteFile(params.Path, []byte(updated), 0o644); wErr != nil {
					editErr = fmt.Errorf("edit %s: %w", params.Path, wErr)
				}
				return nil
			})
			if editErr != nil {
				return Result{Content: editErr.Error(), IsError: true}, nil
			}
			if err != nil {
				return Result{Content: fmt.Sprintf("edit %s: %v", params.Path, err), IsError: true}, nil
			}
			return Result{Content: fmt.Sprintf("Replaced %d occurrence(s) in %s", replaced, params.Path)}, nil
		},
	}
}
