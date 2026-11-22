package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NanQiaoMi/auto-harness/llm"
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
	return newReadTool("")
}

// NewReadToolAt returns a read tool rooted at a workspace. Relative
// paths resolve from root and a path outside it is rejected.
func NewReadToolAt(root string) *Tool {
	return newReadTool(root)
}

func newReadTool(root string) *Tool {
	return &Tool{
		Name:          "read",
		Label:         "Read File",
		ReadOnly:      true,
		Description:   "Read a UTF-8 text file or supported image (PNG, JPEG, WebP, GIF). Text files return line-numbered content (up to 2000 lines by default); image files are attached for visual inspection.",
		PromptSnippet: "read: read a text file or image (optional offset/limit for text)",
		PromptGuidelines: []string{
			"Use read to inspect files (or images) before editing or referencing them.",
			"For large text files, read only the range you need with offset/limit.",
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
			path, err := resolveWorkspacePath(root, params.Path)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			if imgRes, ok := readImageIfSupported(path); ok {
				return imgRes, nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return Result{Content: fmt.Sprintf("read %s: %v", path, err), IsError: true}, nil
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

func readImageIfSupported(path string) (Result, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return Result{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return Result{}, false
	}
	var header [16]byte
	n, err := io.ReadFull(f, header[:])
	_ = f.Close()
	if err != nil && err != io.ErrUnexpectedEOF {
		return Result{}, false
	}
	mime := llm.DetectImageMIME(header[:n])
	if mime == "" {
		return Result{}, false
	}
	if info.Size() > llm.MaxImageBytes {
		return Result{
			Content: fmt.Sprintf("read %s: image exceeds %d bytes (%d bytes)", path, llm.MaxImageBytes, info.Size()),
			IsError: true,
		}, true
	}
	img, err := llm.LoadImageFile(path)
	if err != nil {
		return Result{
			Content: fmt.Sprintf("read %s: %v", path, err),
			IsError: true,
		}, true
	}
	return Result{
		Content: fmt.Sprintf("Read image %s (%s, %dx%d, %d bytes)", filepath.Base(path), img.MediaType, img.Width, img.Height, img.ByteSize),
		Images:  []llm.Image{img},
	}, true
}

// NewWriteTool returns the file-writing tool.
func NewWriteTool() *Tool {
	return newWriteTool("")
}

// NewWriteToolAt returns a write tool rooted at a workspace.
func NewWriteToolAt(root string) *Tool {
	return newWriteTool(root)
}

func newWriteTool(root string) *Tool {
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
			path, err := resolveWorkspacePath(root, params.Path)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			// The write takes the file's queue, so it cannot interleave
			// with an edit of the same file.
			var writeErr error
			err = WithFileMutation(path, func() error {
				if dir := filepath.Dir(path); dir != "" {
					if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
						writeErr = fmt.Errorf("write %s: %w", path, mkErr)
						return nil
					}
				}
				if wErr := os.WriteFile(path, []byte(params.Content), 0o644); wErr != nil {
					writeErr = fmt.Errorf("write %s: %w", path, wErr)
				}
				return nil
			})
			if writeErr != nil {
				return Result{Content: writeErr.Error(), IsError: true}, nil
			}
			if err != nil {
				return Result{Content: fmt.Sprintf("write %s: %v", path, err), IsError: true}, nil
			}
			return Result{Content: fmt.Sprintf("Wrote %d bytes to %s", len(params.Content), path)}, nil
		},
	}
}

// NewEditTool returns the exact string replacement tool.
func NewEditTool() *Tool {
	return newEditTool("")
}

// NewEditToolAt returns an edit tool rooted at a workspace.
func NewEditToolAt(root string) *Tool {
	return newEditTool(root)
}

func newEditTool(root string) *Tool {
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
			path, err := resolveWorkspacePath(root, params.Path)
			if err != nil {
				return Result{Content: err.Error(), IsError: true}, nil
			}
			// The edit takes the file's queue: it reads the file and
			// writes it back, and a write of the same file must not
			// slip in between.
			var editErr error
			var replaced int
			err = WithFileMutation(path, func() error {
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					editErr = fmt.Errorf("edit %s: %w", path, readErr)
					return nil
				}
				content := string(data)
				count := strings.Count(content, params.OldString)
				switch {
				case count == 0:
					editErr = fmt.Errorf("edit %s: old_string not found", path)
					return nil
				case count > 1 && !params.ReplaceAll:
					editErr = fmt.Errorf("edit %s: old_string occurs %d times; add context to make it unique or set replace_all", path, count)
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
				if wErr := os.WriteFile(path, []byte(updated), 0o644); wErr != nil {
					editErr = fmt.Errorf("edit %s: %w", path, wErr)
				}
				return nil
			})
			if editErr != nil {
				return Result{Content: editErr.Error(), IsError: true}, nil
			}
			if err != nil {
				return Result{Content: fmt.Sprintf("edit %s: %v", path, err), IsError: true}, nil
			}
			return Result{Content: fmt.Sprintf("Replaced %d occurrence(s) in %s", replaced, path)}, nil
		},
	}
}

// resolveWorkspacePath resolves path against root and rejects escapes.
// With an empty root, the path is returned unchanged for the primary
// agent's historical behavior.
func resolveWorkspacePath(root, path string) (string, error) {
	if root == "" {
		return path, nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, filepath.FromSlash(candidate))
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %s: %w", absRoot, err)
	}
	realPath, err := resolveSymlinkPath(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", path, err)
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside the delegated workspace %s", path, absRoot)
	}
	return absPath, nil
}

// resolveSymlinkPath resolves the existing prefix of path and appends
// the not-yet-created suffix. A dangling symlink is an error rather than
// a missing file: writing through it could create a file outside the
// workspace after the containment check.
func resolveSymlinkPath(path string) (string, error) {
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if _, statErr := os.Lstat(current); statErr == nil {
			return "", err
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
