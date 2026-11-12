package session

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mimi/auto-harness/llm"
)

// ForkOptions controls how a session is forked.
type ForkOptions struct {
	// Dir is where the new session file is written.
	Dir string
	// Model is recorded in the new session's meta.
	Model string
	// Name is the display name of the new session.
	Name string
	// AtEntryID is the entry the fork branches from. When empty, the
	// current branch of the source session is copied.
	AtEntryID string
	// IncludeSummary appends a summary of the abandoned branch to the
	// forked session, so the context of the branch left behind is not
	// lost.
	IncludeSummary bool
}

// Fork creates a new session that continues the source session from
// the given entry.
//
// The new session is a separate file: the original stays untouched, so
// the forked work can diverge freely. This mirrors pi's /fork.
//
// When AtEntryID is empty the whole current branch is copied (pi's
// /clone).
func Fork(src *Session, opts ForkOptions) (*Session, error) {
	if src == nil {
		return nil, fmt.Errorf("session: cannot fork a nil session")
	}
	if opts.Dir == "" {
		opts.Dir = DefaultDir()
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}

	src.mu.Lock()
	tree := src.tree
	leaf := src.tree.leafID
	model := src.Meta.Model
	src.mu.Unlock()

	// The fork point: the given entry, or the current leaf.
	forkAt := opts.AtEntryID
	if forkAt == "" {
		forkAt = leaf
	}
	if _, ok := tree.Entry(forkAt); !ok {
		return nil, fmt.Errorf("session: cannot fork at unknown entry %q", forkAt)
	}

	if opts.Model == "" {
		opts.Model = model
	}

	id := time.Now().UTC().Format("20060102-150405.000000000")
	path := filepath.Join(opts.Dir, id+".jsonl")
	newTree := NewTree(Header{
		Type:          "session",
		Version:       VersionTree,
		ID:            id,
		Timestamp:     time.Now().UTC(),
		ParentSession: src.Path,
	})

	// Copy the branch up to the fork point. Clearing the parent ID
	// before appending makes Append chain the copies into the same
	// shape as the source branch.
	//
	// Only entries that carry conversation context are copied; the
	// metadata entries (model change, leaf pointer) belong to the
	// source session.
	for _, e := range tree.Path(forkAt) {
		switch e.Type {
		case EntryMessage, EntryCompaction, EntryBranchSummary:
		default:
			continue
		}
		cp := *e
		cp.ParentID = ""
		newTree.Append(&cp)
	}

	dst := &Session{
		Meta: Meta{
			ID:            id,
			CreatedAt:     newTree.Header().Timestamp,
			Model:         opts.Model,
			ParentSession: src.Path,
			Name:          opts.Name,
		},
		Path: path,
		tree: newTree,
	}
	if err := dst.open(true); err != nil {
		return nil, err
	}
	// Persist the copied entries.
	for _, e := range newTree.Entries() {
		if err := dst.writeRecord(e); err != nil {
			dst.Close()
			return nil, err
		}
	}
	if opts.Name != "" {
		if err := dst.SetName(opts.Name); err != nil {
			dst.Close()
			return nil, err
		}
	}
	dst.Messages = newTree.Messages()
	return dst, nil
}

// Clone copies the current branch of a session into a new session
// (pi's /clone).
func Clone(src *Session, opts ForkOptions) (*Session, error) {
	opts.AtEntryID = ""
	return Fork(src, opts)
}

// BranchSummary builds a summary of an abandoned branch, given the
// entries of that branch. The summarizer is supplied by the caller so
// the session package stays free of LLM dependencies.
type BranchSummary struct {
	// FromID is the leaf that was left behind.
	FromID string
	// Summary is the generated text.
	Summary string
}

// BranchMessages returns the messages of the branch ending at the given
// entry, excluding the common ancestor with the current branch.
//
// It is the input a branch summarizer needs: what the abandoned path
// did that the current path does not.
func (t *Tree) BranchMessages(fromID string) []llm.Message {
	if _, ok := t.Entry(fromID); !ok {
		return nil
	}
	// The common ancestor: walk up from the abandoned leaf until the
	// path intersects the current branch.
	current := map[string]bool{}
	for _, e := range t.CurrentPath() {
		current[e.ID] = true
	}
	common := ""
	for cur := fromID; cur != ""; {
		if current[cur] {
			common = cur
			break
		}
		e, ok := t.Entry(cur)
		if !ok {
			break
		}
		cur = e.ParentID
	}
	// Everything on the abandoned path below the common ancestor. The
	// path is root-first, so the abandoned entries are the ones after
	// the common ancestor.
	path := t.Path(fromID)
	start := 0
	if common != "" {
		for i, e := range path {
			if e.ID == common {
				start = i + 1
				break
			}
		}
	}
	var out []llm.Message
	for _, e := range path[start:] {
		if e.Type == EntryMessage && e.Message != nil {
			out = append(out, *e.Message)
		}
	}
	return out
}

// AddBranchSummary appends a summary of the abandoned branch to the
// current branch, so the context of the left-behind path reaches the
// model (pi's BranchSummaryEntry).
func (s *Session) AddBranchSummary(fromID, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &Entry{Type: EntryBranchSummary, FromID: fromID, Summary: summary}
	s.tree.Append(e)
	if err := s.writeRecord(e); err != nil {
		return err
	}
	return nil
}

// SetLeaf moves the session's current position to an entry, starting a
// new branch on the next append. The messages of the branch become the
// session's active transcript.
//
// The move is persisted as a leaf-pointer entry, so a reloaded session
// resumes on the branch the user was working on.
func (s *Session) SetLeaf(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.tree.SetLeaf(id); err != nil {
		return err
	}
	e := &Entry{Type: EntryLeafPointer, LeafID: id, ParentID: id}
	s.tree.Append(e)
	if err := s.writeRecord(e); err != nil {
		return err
	}
	s.Messages = s.tree.Messages()
	return nil
}

// Branch returns the entries of the current branch.
func (s *Session) Branch() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tree.Branch()
}

// Compaction records a context compaction in the tree: a summary that
// stands in for the messages before firstKeptEntryID.
//
// The compaction entry joins the branch, so the model context after a
// reload is the summary plus the retained messages — the original
// messages stay in the file.
func (s *Session) Compaction(summary, firstKeptEntryID string, tokensBefore int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &Entry{
		Type:             EntryCompaction,
		Summary:          summary,
		FirstKeptEntryID: firstKeptEntryID,
		TokensBefore:     tokensBefore,
	}
	s.tree.Append(e)
	if err := s.writeRecord(e); err != nil {
		return err
	}
	return nil
}
