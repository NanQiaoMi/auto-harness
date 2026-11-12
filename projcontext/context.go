// Package projcontext implements pi's project context files: the markdown
// files a project keeps to tell the agent how to work in it.
//
// Two files are recognised:
//
//	AGENTS.md   project instructions, appended to the system prompt
//	SYSTEM.md   the system prompt itself, replacing the default
//
// The instructions are searched from the working directory upwards, so
// a repository's AGENTS.md applies to every directory under it. The
// nearest file wins for the same name: a directory's own AGENTS.md
// shadows the one above it.
//
// The files are data, not code: a context file cannot run anything. It
// can only tell the model what to do, which is why it is safe to have
// one in a repository that is not yours.
package projcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The file names the context loader recognises.
const (
	// InstructionsFile holds the project's instructions.
	InstructionsFile = "AGENTS.md"
	// SystemFile replaces the system prompt.
	SystemFile = "SYSTEM.md"
)

// Instructions is the context files of a directory.
type Instructions struct {
	// Path is where the instructions file came from.
	Path string
	// Body is the instructions file's contents.
	Body string
	// System is the system file's contents, when there is one.
	System string
}

// Load reads the context files for a directory.
//
// The instructions are searched from the directory upwards: the
// nearest AGENTS.md is the one that applies. The system file is only
// read from the directory itself, since a system prompt inherited
// from a parent directory would be surprising.
//
// The files are optional: a directory without them is not an error.
func Load(dir string) (*Instructions, error) {
	out := &Instructions{}

	instructions, err := findInstructions(dir)
	if err != nil {
		return nil, err
	}
	out = instructions

	system, err := readFile(filepath.Join(dir, SystemFile))
	if err != nil {
		return nil, err
	}
	out.System = system
	return out, nil
}

// findInstructions walks up from dir looking for the instructions file.
//
// The search stops at the filesystem root. A directory named .git ends
// the walk: the repository's root is where the instructions live, and
// going past it would pick up the home directory's file.
func findInstructions(dir string) (*Instructions, error) {
	current, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("context: resolve %s: %w", dir, err)
	}
	for {
		body, err := readFile(filepath.Join(current, InstructionsFile))
		if err != nil {
			return nil, err
		}
		if body != "" {
			return &Instructions{
				Path: filepath.Join(current, InstructionsFile),
				Body: body,
			}, nil
		}
		// The repository's root ends the walk.
		if filepath.Base(current) == ".git" {
			return &Instructions{}, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return &Instructions{}, nil
		}
		current = parent
	}
}

// readFile reads a context file.
//
// A file that is not there is not an error: the context files are
// optional.
func readFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("context: read %s: %w", path, err)
	}
	return strings.TrimSpace(string(content)), nil
}

// Apply folds the context files into a system prompt.
//
// The instructions are appended to the prompt, so they add to what the
// default prompt says rather than replacing it. The system file, when
// there is one, replaces the prompt outright: a file that exists to
// be the system prompt should be the system prompt.
//
// The result is trimmed, so an empty context file changes nothing.
func Apply(systemPrompt string, files *Instructions) string {
	if files == nil {
		return systemPrompt
	}
	if files.System != "" {
		return files.System
	}
	if files.Body == "" {
		return systemPrompt
	}
	return strings.TrimSpace(systemPrompt) + "\n\n" + files.Body
}
