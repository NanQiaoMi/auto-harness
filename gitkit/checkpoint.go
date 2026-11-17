package gitkit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCheckpointLimit bounds the number of retained snapshots.
	DefaultCheckpointLimit = 100
	maxCheckpointLabel     = 200
)

// CheckpointKind describes why a snapshot was taken.
type CheckpointKind string

const (
	CheckpointAuto   CheckpointKind = "auto"
	CheckpointManual CheckpointKind = "manual"
	CheckpointSafety CheckpointKind = "safety"
)

// Checkpoint is one restorable workspace state.
type Checkpoint struct {
	ID           string         `json:"id"`
	Tree         string         `json:"tree"`
	IndexTree    string         `json:"index_tree,omitempty"`
	IndexBlob    string         `json:"index_blob,omitempty"`
	RefsBlob     string         `json:"refs_blob,omitempty"`
	StashLogBlob string         `json:"stash_log_blob,omitempty"`
	Head         string         `json:"head,omitempty"`
	Branch       string         `json:"branch,omitempty"`
	Detached     bool           `json:"detached,omitempty"`
	Created      time.Time      `json:"created"`
	Kind         CheckpointKind `json:"kind"`
	Label        string         `json:"label"`
	Tool         string         `json:"tool,omitempty"`
	Commit       string         `json:"commit"`
	Current      bool           `json:"current"`
	Differs      bool           `json:"differs"`
	ShortHead    string         `json:"shortHead,omitempty"`
	// SessionID and SessionAnchor bind an automatic checkpoint to the
	// conversation turn that produced it. SessionTurn is the user
	// message that started the turn; it disambiguates branches that
	// share the same anchor.
	SessionID     string `json:"sessionId,omitempty"`
	SessionAnchor string `json:"sessionAnchor,omitempty"`
	SessionTurn   string `json:"sessionTurn,omitempty"`
}

// SessionBinding identifies the conversation position captured with a
// checkpoint. Anchor is empty when the turn began before the first
// conversational entry. Turn is set for automatic checkpoints created
// by a model turn and empty for manual, safety and user-shell snapshots.
type SessionBinding struct {
	ID     string
	Anchor string
	Turn   string
}

// RewindResult reports the applied checkpoint and the safety snapshot
// that can undo the restore itself.
type RewindResult struct {
	Restored  Checkpoint `json:"restored"`
	Safety    Checkpoint `json:"safety"`
	HeadReset bool       `json:"headReset"`
	RefsReset bool       `json:"refsReset"`
	Noop      bool       `json:"noop,omitempty"`
	Warning   string     `json:"warning,omitempty"`
}

// Manager takes and restores workspace snapshots. The start directory
// is resolved lazily, so `git init` later in the session starts working
// without rebuilding the harness.
type Manager struct {
	Start string
	Limit int
	// BindSession is called while creating a checkpoint. Auto
	// checkpoints normally bind to the state before the current user
	// turn; safety and manual checkpoints bind to the current leaf.
	BindSession func(kind CheckpointKind, tool string) SessionBinding

	mu sync.Mutex
}

// NewCheckpointManager creates a manager rooted at start.
func NewCheckpointManager(start string) *Manager {
	return &Manager{Start: start, Limit: DefaultCheckpointLimit}
}

// BeforeTool implements the agent's before-mutation hook. Outside a
// repository it is a no-op; inside one, a snapshot failure blocks the
// mutating tool rather than silently removing the rollback guarantee.
func (m *Manager) BeforeTool(ctx context.Context, toolName string, args json.RawMessage) error {
	if toolName == "checkpoint" {
		// list/create need no pre-snapshot, and restore takes its own
		// safety checkpoint before touching anything.
		return nil
	}
	if toolName == "git" && isReadOnlyGitCall(args) {
		return nil
	}
	_, _, err := m.Create(ctx, "before "+toolName, CheckpointAuto, toolName)
	if errors.Is(err, ErrNotRepository) {
		return nil
	}
	return err
}

func isReadOnlyGitCall(raw json.RawMessage) bool {
	var params struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || len(params.Args) == 0 {
		return false
	}
	command, rest := gitSubcommand(params.Args)
	switch command {
	case "annotate", "blame", "cat-file", "check-attr", "check-ignore",
		"check-ref-format", "count-objects", "describe", "diff", "for-each-ref",
		"grep", "help", "log", "ls-files", "ls-remote", "ls-tree", "merge-base",
		"name-rev", "rev-list", "rev-parse", "shortlog", "show", "show-ref",
		"status", "var", "verify-commit", "verify-tag", "version", "whatchanged":
		// These are read-only unless they are told to write a report to
		// a file (for example `git diff --output=review.patch`).
		return !hasGitFileOutput(rest)
	case "branch":
		return branchIsReadOnly(rest)
	case "tag":
		return tagIsReadOnly(rest)
	case "remote":
		if len(rest) == 0 {
			return true
		}
		switch rest[0] {
		case "-v", "--verbose", "show", "get-url":
			return true
		}
		return false
	case "stash":
		// Bare `git stash` is `git stash push`, not a list.
		return len(rest) > 0 && (rest[0] == "list" || rest[0] == "show")
	case "reflog":
		if len(rest) == 0 {
			return true
		}
		return rest[0] != "expire" && rest[0] != "delete"
	case "worktree":
		return len(rest) == 0 || rest[0] == "list"
	case "notes":
		return len(rest) == 0 || rest[0] == "list" || rest[0] == "show"
	case "submodule":
		return len(rest) == 0 || rest[0] == "status" || rest[0] == "summary"
	default:
		return false
	}
}

func branchIsReadOnly(args []string) bool {
	listing := false
	for _, arg := range args {
		switch {
		case arg == "--list" || arg == "-l":
			listing = true
		case arg == "--delete", arg == "--move", arg == "--copy",
			arg == "--force", arg == "--edit-description",
			arg == "--set-upstream-to", arg == "--unset-upstream",
			arg == "--track", arg == "--no-track",
			arg == "-d", arg == "-D", arg == "-m", arg == "-M",
			arg == "-c", arg == "-C", arg == "-f", arg == "-u", arg == "-t":
			return false
		case strings.HasPrefix(arg, "--set-upstream-to="),
			strings.HasPrefix(arg, "--unset-upstream="),
			strings.HasPrefix(arg, "--edit-description="):
			return false
		}
	}
	if listing {
		return true
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			// A positional argument is a branch name being created.
			return false
		}
	}
	return true
}

func tagIsReadOnly(args []string) bool {
	listing := len(args) == 0
	for _, arg := range args {
		switch {
		case arg == "--list" || arg == "-l" || arg == "--verify" || arg == "-v",
			arg == "--contains" || arg == "--no-contains", arg == "--points-at",
			arg == "--merged", arg == "--no-merged", arg == "--format",
			strings.HasPrefix(arg, "--format="), strings.HasPrefix(arg, "-n"):
			listing = true
		case arg == "--delete" || arg == "-d", arg == "--annotate" || arg == "-a",
			arg == "--sign" || arg == "-s", arg == "--force" || arg == "-f",
			arg == "--create-reflog":
			return false
		}
	}
	if listing {
		return true
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			// A positional argument is a tag being created.
			return false
		}
	}
	return true
}

func hasGitFileOutput(args []string) bool {
	for i, arg := range args {
		switch {
		case arg == "--output" || arg == "-o":
			if i+1 < len(args) && args[i+1] != "-" {
				return true
			}
		case strings.HasPrefix(arg, "--output="):
			if strings.TrimPrefix(arg, "--output=") != "-" {
				return true
			}
		case strings.HasPrefix(arg, "-o") && len(arg) > 2 && arg[2] != '-':
			return arg[2:] != "-"
		}
	}
	return false
}

func gitSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
		switch {
		case arg == "-C", arg == "-c", arg == "--git-dir", arg == "--work-tree",
			arg == "--namespace", arg == "--exec-path":
			i++
		case strings.HasPrefix(arg, "--git-dir="),
			strings.HasPrefix(arg, "--work-tree="),
			strings.HasPrefix(arg, "--namespace="),
			strings.HasPrefix(arg, "--exec-path="),
			strings.HasPrefix(arg, "-c"):
			continue
		}
	}
	return "", nil
}

// Create stores a snapshot of the complete non-ignored worktree, the
// exact index, HEAD, and the repository's refs. created is false when
// the newest checkpoint already has the same state. Safety checkpoints
// are never deduplicated: each restore gets a distinct marker that can
// itself be restored.
func (m *Manager) Create(ctx context.Context, label string, kind CheckpointKind, tool string) (Checkpoint, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createLocked(ctx, label, kind, tool)
}

func (m *Manager) createLocked(ctx context.Context, label string, kind CheckpointKind, tool string) (Checkpoint, bool, error) {
	root, err := Repository(ctx, m.Start)
	if err != nil {
		return Checkpoint{}, false, err
	}
	state, err := snapshotState(ctx, root)
	if err != nil {
		return Checkpoint{}, false, err
	}
	var binding SessionBinding
	if m.BindSession != nil {
		binding = m.BindSession(kind, tool)
	}
	if strings.TrimSpace(label) == "" {
		label = string(kind) + " checkpoint"
	}
	if len(label) > maxCheckpointLabel {
		label = label[:maxCheckpointLabel]
	}
	newest, hasNewest, err := m.newestLocked(ctx, root)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if kind != CheckpointSafety && hasNewest {
		if sameSnapshotState(newest, state) &&
			newest.SessionID == binding.ID &&
			newest.SessionAnchor == binding.Anchor &&
			newest.SessionTurn == binding.Turn {
			return newest, false, nil
		}
	}

	now := time.Now().UTC()
	id, err := newCheckpointID(now)
	if err != nil {
		return Checkpoint{}, false, err
	}
	meta := Checkpoint{
		ID:            id,
		Tree:          state.Tree,
		IndexTree:     state.IndexTree,
		IndexBlob:     state.IndexBlob,
		RefsBlob:      state.RefsBlob,
		StashLogBlob:  state.StashLogBlob,
		Head:          state.Head,
		Branch:        state.Branch,
		Detached:      state.Detached,
		Created:       now,
		Kind:          kind,
		Label:         label,
		Tool:          tool,
		SessionID:     binding.ID,
		SessionAnchor: binding.Anchor,
		SessionTurn:   binding.Turn,
	}
	message, err := checkpointMessage(meta)
	if err != nil {
		return Checkpoint{}, false, err
	}
	args := []string{"commit-tree", state.Tree}
	if state.Head != "" {
		args = append(args, "-p", state.Head)
	}
	args = append(args, "-m", message)
	out, code, err := runGit(ctx, root, nil, checkpointEnv(now), args...)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if code != 0 {
		return Checkpoint{}, false, gitCommandError(root, args, out, code)
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return Checkpoint{}, false, fmt.Errorf("gitkit: commit-tree returned an empty object")
	}
	ref := checkpointRefPrefix + id
	out, code, err = runGit(ctx, root, nil, nil,
		"update-ref", ref, commit)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if code != 0 {
		return Checkpoint{}, false, gitCommandError(root,
			[]string{"update-ref", ref, commit}, out, code)
	}
	meta.Commit = commit
	if err := m.pruneLocked(ctx, root); err != nil {
		return Checkpoint{}, false, err
	}
	m.decorate([]*Checkpoint{&meta}, state)
	return meta, true, nil
}

// List returns checkpoints newest first and marks those that match or
// differ from the current workspace.
func (m *Manager) List(ctx context.Context) ([]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := Repository(ctx, m.Start)
	if err != nil {
		return nil, err
	}
	items, err := m.listLocked(ctx, root)
	if err != nil {
		return nil, err
	}
	state, err := snapshotState(ctx, root)
	if err != nil {
		return nil, err
	}
	pointers := make([]*Checkpoint, 0, len(items))
	for i := range items {
		pointers = append(pointers, &items[i])
	}
	m.decorate(pointers, state)
	return items, nil
}

// Rewind restores one checkpoint. An empty selector chooses the next
// older checkpoint than the one matching the workspace, so repeated
// rewinds never jump forward. resetHead also rolls back HEAD, commits,
// branches, tags, remote refs and the stash stack.
func (m *Manager) Rewind(ctx context.Context, selector string, resetHead bool) (RewindResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := Repository(ctx, m.Start)
	if err != nil {
		return RewindResult{}, err
	}
	items, err := m.listLocked(ctx, root)
	if err != nil {
		return RewindResult{}, err
	}
	state, err := snapshotState(ctx, root)
	if err != nil {
		return RewindResult{}, err
	}
	target, err := chooseCheckpoint(items, state, selector)
	if err != nil {
		return RewindResult{}, err
	}
	if checkpointMatchesWorkspace(target, state, resetHead) {
		m.decorate([]*Checkpoint{&target}, state)
		return RewindResult{Restored: target, Noop: true}, nil
	}

	if resetHead {
		if err := ensureBranchAvailable(ctx, root, target); err != nil {
			return RewindResult{}, err
		}
	}

	safety, _, err := m.createLocked(ctx, "before rewind "+target.ID, CheckpointSafety, "rewind")
	if err != nil {
		return RewindResult{}, fmt.Errorf("gitkit: create safety checkpoint: %w", err)
	}
	didReset := resetHead && !sameHeadState(target, state)
	refsReset := resetHead && target.RefsBlob != ""
	if err := applyCheckpoint(ctx, root, target, resetHead); err != nil {
		// A restore is itself undoable: put the exact pre-rewind state
		// back before returning the error.
		if rollbackErr := applyCheckpoint(ctx, root, safety, true); rollbackErr != nil {
			return RewindResult{}, fmt.Errorf(
				"gitkit: restore %s failed: %w; rollback to safety %s also failed: %v",
				target.ID, err, safety.ID, rollbackErr)
		}
		return RewindResult{}, fmt.Errorf(
			"gitkit: restore %s failed and was rolled back to safety %s: %w",
			target.ID, safety.ID, err)
	}
	warning := ""
	if resetHead && target.RefsBlob == "" {
		warning = "checkpoint predates full ref snapshots; only files, index and HEAD were restored"
	}
	finalState, err := snapshotState(ctx, root)
	if err != nil {
		return RewindResult{}, err
	}
	m.decorate([]*Checkpoint{&target, &safety}, finalState)
	return RewindResult{
		Restored:  target,
		Safety:    safety,
		HeadReset: didReset,
		RefsReset: refsReset,
		Warning:   warning,
	}, nil
}

func chooseCheckpoint(items []Checkpoint, current workspaceState, selector string) (Checkpoint, error) {
	selector = strings.TrimSpace(selector)
	if selector != "" {
		var matches []Checkpoint
		for _, item := range items {
			if item.ID == selector || strings.HasPrefix(item.ID, selector) {
				matches = append(matches, item)
			}
		}
		switch len(matches) {
		case 0:
			return Checkpoint{}, fmt.Errorf("gitkit: no checkpoint matches %q", selector)
		case 1:
			return matches[0], nil
		default:
			return Checkpoint{}, fmt.Errorf("gitkit: checkpoint prefix %q is ambiguous", selector)
		}
	}
	start := 0
	for i := range items {
		if sameSnapshotState(items[i], current) {
			start = i + 1
			break
		}
	}
	for i := start; i < len(items); i++ {
		item := items[i]
		if !sameSnapshotState(item, current) {
			return item, nil
		}
	}
	return Checkpoint{}, fmt.Errorf("gitkit: no earlier checkpoint differs from the current workspace")
}

type workspaceState struct {
	Tree         string
	IndexTree    string
	IndexBlob    string
	RefsBlob     string
	StashLogBlob string
	Head         string
	Branch       string
	Detached     bool
}

func snapshotState(ctx context.Context, root string) (workspaceState, error) {
	tempDir, err := os.MkdirTemp("", "harness-git-index-")
	if err != nil {
		return workspaceState{}, err
	}
	defer os.RemoveAll(tempDir)
	indexPath := filepath.Join(tempDir, "index")
	env := []string{"GIT_INDEX_FILE=" + indexPath}
	out, code, err := runGit(ctx, root, nil, env, "add", "-A", "--", ".")
	if err != nil {
		return workspaceState{}, err
	}
	if code != 0 {
		return workspaceState{}, gitCommandError(root, []string{"add", "-A", "--", "."}, out, code)
	}
	out, code, err = runGit(ctx, root, nil, env, "write-tree")
	if err != nil {
		return workspaceState{}, err
	}
	if code != 0 {
		return workspaceState{}, gitCommandError(root, []string{"write-tree"}, out, code)
	}
	state := workspaceState{Tree: strings.TrimSpace(string(out))}

	paths, err := gitPaths(ctx, root, "index", "logs/refs/stash")
	if err != nil {
		return workspaceState{}, err
	}
	realIndexPath, stashLogPath := paths[0], paths[1]
	if info, statErr := os.Stat(realIndexPath); statErr == nil {
		if info.IsDir() {
			return workspaceState{}, fmt.Errorf("gitkit: Git index path %s is a directory", realIndexPath)
		}
		state.IndexBlob, err = hashFile(ctx, root, realIndexPath)
		if err != nil {
			return workspaceState{}, err
		}
	} else if !os.IsNotExist(statErr) {
		return workspaceState{}, statErr
	}

	out, code, err = runGit(ctx, root, nil, nil, "write-tree")
	if err != nil {
		return workspaceState{}, err
	}
	if code != 0 {
		unmerged, unmergedErr := hasUnmergedEntries(ctx, root)
		if unmergedErr != nil {
			return workspaceState{}, unmergedErr
		}
		if !unmerged {
			return workspaceState{}, fmt.Errorf(
				"gitkit: cannot snapshot the index: %w",
				gitCommandError(root, []string{"write-tree"}, out, code))
		}
		// An unmerged index cannot be written as a tree. IndexBlob is
		// the exact stage-1/2/3 state and is restored verbatim.
		state.IndexTree = ""
	} else {
		state.IndexTree = strings.TrimSpace(string(out))
	}

	out, code, err = runGit(ctx, root, nil, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return workspaceState{}, err
	}
	if code == 0 {
		state.Head = strings.TrimSpace(string(out))
	}
	out, code, err = runGit(ctx, root, nil, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return workspaceState{}, err
	}
	if code == 0 {
		state.Branch = strings.TrimSpace(string(out))
	} else {
		state.Detached = true
	}

	refs, err := listRefs(ctx, root)
	if err != nil {
		return workspaceState{}, err
	}
	state.RefsBlob, err = writeJSONBlob(ctx, root, refs)
	if err != nil {
		return workspaceState{}, err
	}
	state.StashLogBlob, err = hashOptionalFile(ctx, root, stashLogPath)
	if err != nil {
		return workspaceState{}, err
	}
	return state, nil
}

func sameSnapshotState(item Checkpoint, state workspaceState) bool {
	return checkpointFilesMatch(item, state) &&
		sameHeadState(item, state) &&
		(item.RefsBlob == "" || item.RefsBlob == state.RefsBlob) &&
		(item.StashLogBlob == "" || item.StashLogBlob == state.StashLogBlob)
}

func checkpointFilesMatch(item Checkpoint, state workspaceState) bool {
	if item.Tree != state.Tree {
		return false
	}
	if item.IndexBlob != "" {
		return item.IndexBlob == state.IndexBlob
	}
	return item.IndexTree == state.IndexTree
}

func sameHeadState(item Checkpoint, state workspaceState) bool {
	return item.Head == state.Head &&
		item.Branch == state.Branch &&
		item.Detached == state.Detached
}

func checkpointMatchesWorkspace(item Checkpoint, state workspaceState, resetHead bool) bool {
	if !checkpointFilesMatch(item, state) {
		return false
	}
	if !resetHead {
		return true
	}
	return sameHeadState(item, state) &&
		(item.RefsBlob == "" || item.RefsBlob == state.RefsBlob) &&
		(item.StashLogBlob == "" || item.StashLogBlob == state.StashLogBlob)
}

func applyCheckpoint(ctx context.Context, root string, target Checkpoint, resetHead bool) error {
	if resetHead {
		if err := setHeadTo(ctx, root, target); err != nil {
			return err
		}
	}
	if err := restoreWorkspace(ctx, root, target); err != nil {
		return err
	}
	if resetHead && target.RefsBlob != "" {
		if err := restoreRefs(ctx, root, target.RefsBlob); err != nil {
			return err
		}
		if err := restoreStashLog(ctx, root, target.StashLogBlob); err != nil {
			return err
		}
	}
	return nil
}

func setHeadTo(ctx context.Context, root string, target Checkpoint) error {
	if target.Detached {
		if target.Head == "" {
			return fmt.Errorf("gitkit: detached checkpoint has no HEAD")
		}
		out, code, err := runGit(ctx, root, nil, nil,
			"update-ref", "--no-deref", "HEAD", target.Head)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root,
				[]string{"update-ref", "--no-deref", "HEAD", target.Head}, out, code)
		}
		return nil
	}
	if target.Branch == "" {
		return fmt.Errorf("gitkit: checkpoint has no branch or detached HEAD")
	}
	out, code, err := runGit(ctx, root, nil, nil,
		"symbolic-ref", "HEAD", target.Branch)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root,
			[]string{"symbolic-ref", "HEAD", target.Branch}, out, code)
	}
	if target.Head == "" {
		out, code, err := runGit(ctx, root, nil, nil,
			"update-ref", "-d", target.Branch)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root,
				[]string{"update-ref", "-d", target.Branch}, out, code)
		}
		return nil
	}
	out, code, err = runGit(ctx, root, nil, nil,
		"update-ref", target.Branch, target.Head)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root,
			[]string{"update-ref", target.Branch, target.Head}, out, code)
	}
	return nil
}

func restoreWorkspace(ctx context.Context, root string, target Checkpoint) error {
	out, code, err := runGit(ctx, root, nil, nil,
		"ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root,
			[]string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"},
			out, code)
	}
	paths := splitNUL(out)
	seen := make(map[string]bool, len(paths))
	removable := make(map[string]bool, len(paths))
	for _, path := range paths {
		removable[path] = true
	}
	if err := validateRestorePaths(ctx, root, target.Tree, removable); err != nil {
		return err
	}
	var directories []string
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		abs, err := safeRepoPath(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.IsDir() {
			// A tracked file may have been replaced by an untracked
			// directory. Its files are removed below; the directory
			// itself is handled after they are gone.
			directories = append(directories, path)
			continue
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, path := range directories {
		abs, err := safeRepoPath(root, path)
		if err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf(
				"gitkit: refusing to overwrite directory %s while restoring (ignored files may be in the way): %w",
				path, err)
		}
	}
	if err := prepareRestorePaths(ctx, root, target.Tree); err != nil {
		return err
	}
	args := []string{"read-tree", "--reset", "-u", target.Tree}
	out, code, err = runGit(ctx, root, nil, nil, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root, args, out, code)
	}
	return restoreIndex(ctx, root, target)
}

func prepareRestorePaths(ctx context.Context, root, tree string) error {
	out, code, err := runGit(ctx, root, nil, nil,
		"ls-tree", "-r", "-z", "--name-only", tree)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root,
			[]string{"ls-tree", "-r", "-z", "--name-only", tree}, out, code)
	}
	var emptyDirectories []string
	for _, rel := range splitNUL(out) {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		current := root
		for i, part := range parts {
			current = filepath.Join(current, filepath.FromSlash(part))
			info, err := os.Lstat(current)
			if err != nil {
				if os.IsNotExist(err) {
					break
				}
				return err
			}
			if i == len(parts)-1 {
				if info.IsDir() {
					emptyDirectories = append(emptyDirectories, rel)
				} else {
					return fmt.Errorf(
						"gitkit: cannot restore file %s: an ignored file or symlink occupies that path",
						rel)
				}
				break
			}
			if !info.IsDir() {
				return fmt.Errorf(
					"gitkit: cannot restore %s: path component %s is not a directory (an ignored file may be in the way)",
					rel, strings.Join(parts[:i+1], "/"))
			}
		}
	}
	// Validate every target path before removing anything. A conflict in
	// one path must not leave unrelated empty directories deleted.
	for _, rel := range emptyDirectories {
		abs, err := safeRepoPath(root, rel)
		if err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf(
				"gitkit: cannot restore file %s: directory is not empty (ignored files may be in the way): %w",
				rel, err)
		}
	}
	return nil
}

// validateRestorePaths checks target paths before any current files are
// removed. Paths that Git listed for cleanup may be replaced; anything
// else already occupying a target path is protected, notably ignored
// files and symlinks. Directories are handled after cleanup, when
// remaining non-empty directories reveal ignored children.
func validateRestorePaths(ctx context.Context, root, tree string, removable map[string]bool) error {
	out, code, err := runGit(ctx, root, nil, nil,
		"ls-tree", "-r", "-z", "--name-only", tree)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root,
			[]string{"ls-tree", "-r", "-z", "--name-only", tree}, out, code)
	}
	for _, rel := range splitNUL(out) {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		current := root
		for i, part := range parts {
			current = filepath.Join(current, filepath.FromSlash(part))
			info, err := os.Lstat(current)
			if err != nil {
				if os.IsNotExist(err) {
					break
				}
				return err
			}
			prefix := strings.Join(parts[:i+1], "/")
			if i == len(parts)-1 {
				if info.IsDir() {
					if err := validateDirectoryReplacement(root, prefix, removable); err != nil {
						return err
					}
					break
				}
				if !removable[prefix] {
					return fmt.Errorf(
						"gitkit: cannot restore file %s: an ignored file or symlink occupies that path",
						rel)
				}
				break
			}
			if !info.IsDir() {
				if !removable[prefix] {
					return fmt.Errorf(
						"gitkit: cannot restore %s: path component %s is not a directory (an ignored file may be in the way)",
						rel, prefix)
				}
				break
			}
		}
	}
	return nil
}

func validateDirectoryReplacement(root, rel string, removable map[string]bool) error {
	abs, err := safeRepoPath(root, rel)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := rel + "/" + entry.Name()
		if entry.IsDir() {
			if err := validateDirectoryReplacement(root, child, removable); err != nil {
				return err
			}
			continue
		}
		if !removable[child] {
			return fmt.Errorf(
				"gitkit: cannot restore file %s: directory contains an ignored file or symlink",
				rel)
		}
	}
	return nil
}

type refState struct {
	Name   string `json:"name"`
	Object string `json:"object"`
	Symref string `json:"symref,omitempty"`
}

func listRefs(ctx context.Context, root string) ([]refState, error) {
	out, code, err := runGit(ctx, root, nil, nil,
		"for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root,
			[]string{"for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)"},
			out, code)
	}
	refs := make([]refState, 0)
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		if strings.HasPrefix(fields[0], checkpointRefPrefix) ||
			strings.HasPrefix(fields[0], checkpointRootRefPrefix) {
			continue
		}
		ref := refState{Name: fields[0], Object: fields[1]}
		if len(fields) == 3 {
			ref.Symref = fields[2]
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

func restoreRefs(ctx context.Context, root, blob string) error {
	targetRefs, err := readRefsBlob(ctx, root, blob)
	if err != nil {
		return err
	}
	currentRefs, err := listRefs(ctx, root)
	if err != nil {
		return err
	}
	protected, err := checkedOutElsewhere(ctx, root)
	if err != nil {
		return err
	}

	target := make(map[string]refState, len(targetRefs))
	for _, ref := range targetRefs {
		target[ref.Name] = ref
	}
	current := make(map[string]refState, len(currentRefs))
	for _, ref := range currentRefs {
		current[ref.Name] = ref
	}

	// Validate every protected ref before changing anything.
	for name, ref := range current {
		wanted, ok := target[name]
		if !ok {
			if path := protected[name]; path != "" {
				return fmt.Errorf(
					"gitkit: refusing to delete %s because it is checked out in %s",
					name, path)
			}
			continue
		}
		if !sameRef(ref, wanted) {
			if path := protected[name]; path != "" {
				return fmt.Errorf(
					"gitkit: refusing to change %s because it is checked out in %s",
					name, path)
			}
		}
	}

	// Delete refs that did not exist at the checkpoint.
	for _, ref := range currentRefs {
		if _, ok := target[ref.Name]; ok {
			continue
		}
		if err := deleteRef(ctx, root, ref); err != nil {
			return err
		}
	}

	// Restore ordinary refs first, so symbolic refs can point at them.
	for _, wanted := range targetRefs {
		if wanted.Symref != "" {
			continue
		}
		currentRef, exists := current[wanted.Name]
		if exists && currentRef.Symref == "" && currentRef.Object == wanted.Object {
			continue
		}
		if exists && currentRef.Symref != "" {
			if err := deleteRef(ctx, root, currentRef); err != nil {
				return err
			}
			exists = false
		}
		args := []string{"update-ref", wanted.Name, wanted.Object}
		if exists {
			args = append(args, currentRef.Object)
		}
		out, code, err := runGit(ctx, root, nil, nil, args...)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root, args, out, code)
		}
	}

	// Then restore symbolic refs (for example refs/remotes/origin/HEAD).
	for _, wanted := range targetRefs {
		if wanted.Symref == "" {
			continue
		}
		currentRef, exists := current[wanted.Name]
		if exists && currentRef.Symref == wanted.Symref {
			continue
		}
		if exists && currentRef.Symref == "" {
			if err := deleteRef(ctx, root, currentRef); err != nil {
				return err
			}
		}
		args := []string{"symbolic-ref", wanted.Name, wanted.Symref}
		out, code, err := runGit(ctx, root, nil, nil, args...)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root, args, out, code)
		}
	}
	return nil
}

func deleteRef(ctx context.Context, root string, ref refState) error {
	var args []string
	if ref.Symref != "" {
		args = []string{"symbolic-ref", "-d", ref.Name}
	} else {
		args = []string{"update-ref", "-d", ref.Name, ref.Object}
	}
	out, code, err := runGit(ctx, root, nil, nil, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root, args, out, code)
	}
	return nil
}

func sameRef(a, b refState) bool {
	if a.Symref != b.Symref {
		return false
	}
	if a.Symref != "" {
		return true
	}
	return a.Object == b.Object
}

func checkedOutElsewhere(ctx context.Context, root string) (map[string]string, error) {
	out, code, err := runGit(ctx, root, nil, nil,
		"worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root,
			[]string{"worktree", "list", "--porcelain"}, out, code)
	}
	currentPath := canonicalPath(root)
	protected := make(map[string]string)
	var path, branch string
	flush := func() {
		if path != "" && branch != "" && canonicalPath(path) != currentPath {
			protected[branch] = path
		}
		path, branch = "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			branch = strings.TrimPrefix(line, "branch ")
		}
	}
	flush()
	return protected, nil
}

func canonicalPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func ensureBranchAvailable(ctx context.Context, root string, target Checkpoint) error {
	if target.Detached || target.Branch == "" {
		return nil
	}
	protected, err := checkedOutElsewhere(ctx, root)
	if err != nil {
		return err
	}
	if path := protected[target.Branch]; path != "" {
		return fmt.Errorf(
			"gitkit: checkpoint branch %s is checked out in %s; remove that worktree or restore files only",
			target.Branch, path)
	}
	return nil
}

func restoreIndex(ctx context.Context, root string, target Checkpoint) error {
	indexPath, err := gitPath(ctx, root, "index")
	if err != nil {
		return err
	}
	if target.IndexBlob != "" {
		data, code, err := runGit(ctx, root, nil, nil,
			"cat-file", "blob", target.IndexBlob)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root,
				[]string{"cat-file", "blob", target.IndexBlob}, data, code)
		}
		mode := os.FileMode(0o644)
		if info, statErr := os.Stat(indexPath); statErr == nil {
			mode = info.Mode().Perm()
		}
		return writeFileAtomic(indexPath, data, mode)
	}
	if target.IndexTree != "" {
		args := []string{"read-tree", target.IndexTree}
		out, code, err := runGit(ctx, root, nil, nil, args...)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root, args, out, code)
		}
		return nil
	}
	if err := os.Remove(indexPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func restoreStashLog(ctx context.Context, root, blob string) error {
	path, err := gitPath(ctx, root, "logs/refs/stash")
	if err != nil {
		return err
	}
	if blob == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, code, err := runGit(ctx, root, nil, nil, "cat-file", "blob", blob)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root, []string{"cat-file", "blob", blob}, data, code)
	}
	return writeFileAtomic(path, data, 0o644)
}

func gitPath(ctx context.Context, root, name string) (string, error) {
	paths, err := gitPaths(ctx, root, name)
	if err != nil {
		return "", err
	}
	return paths[0], nil
}

func gitPaths(ctx context.Context, root string, names ...string) ([]string, error) {
	args := []string{"rev-parse"}
	for _, name := range names {
		args = append(args, "--git-path", name)
	}
	out, code, err := runGit(ctx, root, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root, args, out, code)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("gitkit: Git returned %d paths for %d names", len(lines), len(names))
	}
	paths := make([]string, len(lines))
	for i, line := range lines {
		path := strings.TrimSpace(line)
		if path == "" {
			return nil, fmt.Errorf("gitkit: Git returned an empty path for %s", names[i])
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		paths[i] = filepath.Clean(path)
	}
	return paths, nil
}

func hashFile(ctx context.Context, root, path string) (string, error) {
	out, code, err := runGit(ctx, root, nil, nil,
		"hash-object", "-w", "--", path)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", gitCommandError(root,
			[]string{"hash-object", "-w", "--", path}, out, code)
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "", fmt.Errorf("gitkit: hash-object returned an empty object")
	}
	return hash, nil
}

func hashOptionalFile(ctx context.Context, root, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return writeBlob(ctx, root, data)
}

func writeBlob(ctx context.Context, root string, data []byte) (string, error) {
	out, code, err := runGit(ctx, root, data, nil,
		"hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", gitCommandError(root,
			[]string{"hash-object", "-w", "--stdin"}, out, code)
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "", fmt.Errorf("gitkit: hash-object returned an empty object")
	}
	return hash, nil
}

func writeJSONBlob(ctx context.Context, root string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return writeBlob(ctx, root, data)
}

func readRefsBlob(ctx context.Context, root, blob string) ([]refState, error) {
	data, code, err := runGit(ctx, root, nil, nil, "cat-file", "blob", blob)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root, []string{"cat-file", "blob", blob}, data, code)
	}
	var refs []refState
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, fmt.Errorf("gitkit: checkpoint refs: %w", err)
	}
	return refs, nil
}

func hasUnmergedEntries(ctx context.Context, root string) (bool, error) {
	out, code, err := runGit(ctx, root, nil, nil, "ls-files", "-u", "-z")
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, gitCommandError(root, []string{"ls-files", "-u", "-z"}, out, code)
	}
	return len(out) > 0, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".harness-checkpoint-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func safeRepoPath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("gitkit: unsafe repository path %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gitkit: unsafe repository path %q", name)
	}
	path := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gitkit: unsafe repository path %q", name)
	}
	return path, nil
}

type checkpointRef struct {
	id     string
	commit string
}

func (m *Manager) listLocked(ctx context.Context, root string) ([]Checkpoint, error) {
	out, code, err := runGit(ctx, root, nil, nil,
		"for-each-ref", "--format=%(refname) %(objectname)", checkpointRefPrefix)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root,
			[]string{"for-each-ref", "--format=%(refname) %(objectname)", checkpointRefPrefix},
			out, code)
	}
	var refs []checkpointRef
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ref, commit, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(ref, checkpointRefPrefix) {
			continue
		}
		refs = append(refs, checkpointRef{
			id:     strings.TrimPrefix(ref, checkpointRefPrefix),
			commit: strings.TrimSpace(commit),
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].id > refs[j].id })
	return loadCheckpoints(ctx, root, refs)
}

func (m *Manager) newestLocked(ctx context.Context, root string) (Checkpoint, bool, error) {
	out, code, err := runGit(ctx, root, nil, nil,
		"for-each-ref", "--sort=-refname", "--count=1",
		"--format=%(refname) %(objectname)", checkpointRefPrefix)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if code != 0 {
		return Checkpoint{}, false, gitCommandError(root,
			[]string{"for-each-ref", "--sort=-refname", "--count=1",
				"--format=%(refname) %(objectname)", checkpointRefPrefix},
			out, code)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return Checkpoint{}, false, nil
	}
	ref, commit, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(ref, checkpointRefPrefix) {
		return Checkpoint{}, false, fmt.Errorf("gitkit: malformed checkpoint ref %q", line)
	}
	item, err := loadCheckpoint(ctx, root,
		strings.TrimPrefix(ref, checkpointRefPrefix), strings.TrimSpace(commit))
	if err != nil {
		return Checkpoint{}, false, err
	}
	return item, true, nil
}

func loadCheckpoint(ctx context.Context, root, id, commit string) (Checkpoint, error) {
	out, code, err := runGit(ctx, root, nil, nil,
		"show", "-s", "--format=%B", commit)
	if err != nil {
		return Checkpoint{}, err
	}
	if code != 0 {
		return Checkpoint{}, gitCommandError(root,
			[]string{"show", "-s", "--format=%B", commit}, out, code)
	}
	return parseCheckpointMessage(id, commit, out)
}

func loadCheckpoints(ctx context.Context, root string, refs []checkpointRef) ([]Checkpoint, error) {
	if len(refs) == 0 {
		return []Checkpoint{}, nil
	}
	var input strings.Builder
	for _, ref := range refs {
		input.WriteString(ref.commit)
		input.WriteByte('\n')
	}
	out, code, err := runGit(ctx, root, []byte(input.String()), nil,
		"cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, gitCommandError(root, []string{"cat-file", "--batch"}, out, code)
	}
	items := make([]Checkpoint, 0, len(refs))
	remaining := out
	for _, ref := range refs {
		record, rest, err := nextBatchObject(remaining)
		if err != nil {
			return nil, fmt.Errorf("gitkit: checkpoint %s: %w", ref.id, err)
		}
		remaining = rest
		item, err := parseCheckpointMessage(ref.id, ref.commit, commitMessage(record))
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func commitMessage(content []byte) []byte {
	if i := bytes.Index(content, []byte("\n\n")); i >= 0 {
		return content[i+2:]
	}
	return content
}

func nextBatchObject(data []byte) ([]byte, []byte, error) {
	newline := bytes.IndexByte(data, '\n')
	if newline < 0 {
		return nil, nil, fmt.Errorf("cat-file batch header is incomplete")
	}
	header := strings.TrimSpace(string(data[:newline]))
	data = data[newline+1:]
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[1] == "missing" {
		return nil, nil, fmt.Errorf("object %s is missing", fields[0])
	}
	if len(fields) != 3 {
		return nil, nil, fmt.Errorf("cat-file batch header %q is malformed", header)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 {
		return nil, nil, fmt.Errorf("cat-file batch size %q is invalid", fields[2])
	}
	if len(data) < size {
		return nil, nil, fmt.Errorf("cat-file batch object is truncated")
	}
	object := data[:size]
	data = data[size:]
	if len(data) > 0 && data[0] == '\n' {
		data = data[1:]
	}
	return object, data, nil
}

func parseCheckpointMessage(id, commit string, content []byte) (Checkpoint, error) {
	message := strings.TrimSpace(string(content))
	_, payload, ok := strings.Cut(message, "\n\n")
	if !ok {
		return Checkpoint{}, fmt.Errorf("gitkit: checkpoint %s has no metadata", id)
	}
	var item Checkpoint
	if err := json.Unmarshal([]byte(payload), &item); err != nil {
		return Checkpoint{}, fmt.Errorf("gitkit: checkpoint %s metadata: %w", id, err)
	}
	if item.ID != id {
		return Checkpoint{}, fmt.Errorf("gitkit: checkpoint %s metadata id is %q", id, item.ID)
	}
	item.Commit = commit
	return item, nil
}

func checkpointMessage(item Checkpoint) (string, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	return "harness checkpoint\n\n" + string(data), nil
}

func newCheckpointID(now time.Time) (string, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405.000000000Z") + "-" +
		hex.EncodeToString(random[:]), nil
}

func (m *Manager) pruneLocked(ctx context.Context, root string) error {
	limit := m.Limit
	if limit <= 0 {
		limit = DefaultCheckpointLimit
	}
	out, code, err := runGit(ctx, root, nil, nil,
		"for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	if err != nil {
		return err
	}
	if code != 0 {
		return gitCommandError(root,
			[]string{"for-each-ref", "--format=%(refname)", checkpointRefPrefix},
			out, code)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, checkpointRefPrefix) {
			refs = append(refs, line)
		}
	}
	sort.Strings(refs)
	for len(refs) > limit {
		ref := refs[0]
		refs = refs[1:]
		out, code, err := runGit(ctx, root, nil, nil,
			"update-ref", "-d", ref)
		if err != nil {
			return err
		}
		if code != 0 {
			return gitCommandError(root,
				[]string{"update-ref", "-d", ref}, out, code)
		}
	}
	return nil
}

func (m *Manager) decorate(items []*Checkpoint, current workspaceState) {
	for _, item := range items {
		item.Current = sameSnapshotState(*item, current)
		item.Differs = !item.Current
		item.ShortHead = shortOID(item.Head)
	}
}

func shortOID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func splitNUL(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func formatCheckpointLine(index int, item Checkpoint) string {
	state := item.Label
	if item.Current {
		state += " [current]"
	}
	head := item.ShortHead
	if head == "" {
		head = "unborn"
	}
	branch := item.Branch
	if item.Detached {
		branch = "detached"
	}
	if branch == "" {
		branch = "unknown"
	}
	return fmt.Sprintf("%3d  %-36s  %-6s  %-12s  %-20s  %s",
		index, item.ID, item.Kind, head, branch, state)
}

// FormatCheckpoints formats a list for the REPL.
func FormatCheckpoints(items []Checkpoint) string {
	if len(items) == 0 {
		return "no workspace checkpoints"
	}
	var b strings.Builder
	for i, item := range items {
		fmt.Fprintln(&b, formatCheckpointLine(i+1, item))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ParseCheckpointOrdinal resolves the 1-based number shown by
// FormatCheckpoints. IDs and ID prefixes are accepted unchanged.
func ParseCheckpointOrdinal(items []Checkpoint, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", nil
	}
	n, err := strconv.Atoi(selector)
	if err != nil {
		return selector, nil
	}
	if n < 1 || n > len(items) {
		return "", fmt.Errorf("checkpoint number %d is outside 1..%d", n, len(items))
	}
	return items[n-1].ID, nil
}
