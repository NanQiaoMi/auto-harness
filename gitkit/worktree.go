package gitkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GitDirs returns the repository and Git storage directories a command
// may need to write. A linked worktree's objects and refs live in the
// common Git directory outside the checkout, so a write sandbox must
// include it for commits and checkpoints to work.
func GitDirs(ctx context.Context, start string) ([]string, error) {
	root, err := Repository(ctx, start)
	if err != nil {
		return nil, err
	}
	out, code, err := runGit(ctx, root, nil, nil,
		"rev-parse", "--git-common-dir", "--git-dir")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root,
			[]string{"rev-parse", "--git-common-dir", "--git-dir"}, out, code)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	seen := map[string]bool{}
	dirs := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(root, line)
		}
		line, err = filepath.Abs(line)
		if err != nil {
			return nil, err
		}
		line = filepath.Clean(line)
		if !seen[line] {
			seen[line] = true
			dirs = append(dirs, line)
		}
	}
	return dirs, nil
}

// AddDetachedWorktree creates a linked Git worktree at path, checked out
// from HEAD in detached-HEAD state. It returns the absolute worktree
// path. The parent directory is created when needed.
//
// A delegated agent uses this to edit a project without touching the
// user's working tree. The worktree is not removed automatically: it
// holds the delegated agent's uncommitted result until a caller decides
// to keep or discard it.
func AddDetachedWorktree(ctx context.Context, start, path string) (string, error) {
	root, err := Repository(ctx, start)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("gitkit: worktree path must not be empty")
	}
	checkPath := path
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		checkPath = filepath.Join(parent, filepath.Base(path))
	}
	rel, relErr := filepath.Rel(canonicalPath(root), checkPath)
	if relErr == nil && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gitkit: worktree path %s must be outside the repository", path)
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("gitkit: worktree path %s already exists", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	out, code, err := runGit(ctx, root, nil, nil,
		"worktree", "add", "--detach", path, "HEAD")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", gitCommandError(root,
			[]string{"worktree", "add", "--detach", path, "HEAD"}, out, code)
	}
	return path, nil
}

// RemoveWorktree removes a linked worktree. force is required when it
// contains modified or untracked files. A missing checkout is pruned
// from Git's worktree metadata instead of being reported as a failure.
func RemoveWorktree(ctx context.Context, start, path string, force bool) error {
	root, err := Repository(ctx, start)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	out, code, err := runGit(ctx, root, nil, nil, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			out, code, err = runGit(ctx, root, nil, nil, "worktree", "prune")
			if err != nil {
				return err
			}
			if code == 0 {
				return nil
			}
		}
		return gitCommandError(root, args, out, code)
	}
	return nil
}
