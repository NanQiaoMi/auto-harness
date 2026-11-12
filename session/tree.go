package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mimi/auto-harness/llm"
)

// Session file format versions.
const (
	// VersionLinear is the legacy format: a flat sequence of messages
	// with no tree structure. It is migrated on load.
	VersionLinear = 1
	// VersionTree is the current format: entries link to their parent,
	// forming a tree that supports branching inside one file.
	VersionTree = 2
)

// EntryType identifies the kind of a session entry.
type EntryType string

const (
	// EntryMessage is a conversation message.
	EntryMessage EntryType = "message"
	// EntryModelChange records a model switch.
	EntryModelChange EntryType = "model_change"
	// EntryCompaction records a context compaction.
	EntryCompaction EntryType = "compaction"
	// EntryBranchSummary records the summary of an abandoned branch.
	EntryBranchSummary EntryType = "branch_summary"
	// EntryLabel is a user-defined bookmark on an entry.
	EntryLabel EntryType = "label"
	// EntrySessionInfo carries session metadata (e.g. a display name).
	EntrySessionInfo EntryType = "session_info"
	// EntryLeafPointer records the current position in the tree. It is
	// written when the leaf moves, so a reloaded session resumes on the
	// branch the user was working on.
	EntryLeafPointer EntryType = "leaf_pointer"
)

// Entry is one node in the session tree.
//
// Every entry except the header has an ID and a ParentID. The ParentID
// is empty for a root entry. Branching means adding a new child to an
// existing entry, so leaving a branch never erases it.
type Entry struct {
	Type EntryType `json:"type"`
	// ID identifies the entry within the session.
	ID string `json:"id"`
	// ParentID is the entry this one continues from. Empty for a root.
	ParentID string `json:"parentId"`
	// Timestamp is when the entry was created.
	Timestamp time.Time `json:"timestamp"`

	// Message is set on EntryMessage entries.
	Message *llm.Message `json:"message,omitempty"`

	// Model and Provider are set on EntryModelChange entries.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`

	// Summary and FirstKeptEntryID are set on EntryCompaction entries.
	Summary string `json:"summary,omitempty"`
	// FirstKeptEntryID is the first entry retained from before the
	// compaction. Entries before it are replaced by Summary.
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	// TokensBefore is the context size before compaction.
	TokensBefore int `json:"tokensBefore,omitempty"`

	// FromID is the abandoned leaf summarized by an EntryBranchSummary.
	FromID string `json:"fromId,omitempty"`

	// TargetID is the entry a label points at.
	TargetID string `json:"targetId,omitempty"`
	// Label is the bookmark text.
	Label string `json:"label,omitempty"`
	// LeafID is the entry the leaf moved to (EntryLeafPointer).
	LeafID string `json:"leafId,omitempty"`

	// Name is the session display name (EntrySessionInfo).
	Name string `json:"name,omitempty"`
}

// Header is the first line of a session file.
type Header struct {
	Type      string    `json:"type"` // always "session"
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	// CWD is the working directory the session was created in.
	CWD string `json:"cwd,omitempty"`
	// ParentSession is the path of the session this one was forked from.
	ParentSession string `json:"parentSession,omitempty"`
}

// Tree is a session's entry tree.
//
// The tree supports branching inside a single file: moving the leaf to
// an earlier entry and adding a new child starts a new branch while
// keeping the old one.
type Tree struct {
	header Header
	// entries holds every entry by ID.
	entries map[string]*Entry
	// order preserves the append order of entry IDs.
	order []string
	// leafID is the current position in the tree.
	leafID string
	// children maps a parent ID to its child entry IDs, in append order.
	children map[string][]string
}

// NewTree creates an empty tree with a header.
func NewTree(h Header) *Tree {
	return &Tree{
		header:   h,
		entries:  map[string]*Entry{},
		children: map[string][]string{},
	}
}

// Header returns the session header.
func (t *Tree) Header() Header { return t.header }

// LeafID returns the current leaf entry ID.
func (t *Tree) LeafID() string { return t.leafID }

// Entry returns an entry by ID.
func (t *Tree) Entry(id string) (*Entry, bool) {
	e, ok := t.entries[id]
	return e, ok
}

// Entries returns every entry in append order.
func (t *Tree) Entries() []*Entry {
	out := make([]*Entry, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, t.entries[id])
	}
	return out
}

// Append adds an entry to the tree, linking it to the current leaf.
// It returns the new entry's ID.
//
// Two entry kinds are metadata rather than conversation content: a
// leaf pointer moves the current position, and a model change records
// the active model. Neither joins the branch, so the branch stays a
// clean chain of messages.
func (t *Tree) Append(e *Entry) string {
	if e.ID == "" {
		e.ID = newEntryID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	switch e.Type {
	case EntryLeafPointer:
		// The leaf moved: record the pointer without adding it to the
		// branch.
		t.entries[e.ID] = e
		t.order = append(t.order, e.ID)
		if e.LeafID != "" {
			if _, ok := t.entries[e.LeafID]; ok {
				t.leafID = e.LeafID
			}
		}
		return e.ID
	case EntryModelChange:
		// The model changed: record it without touching the branch.
		t.entries[e.ID] = e
		t.order = append(t.order, e.ID)
		return e.ID
	}
	if e.ParentID == "" {
		e.ParentID = t.leafID
	}
	t.entries[e.ID] = e
	t.order = append(t.order, e.ID)
	if e.ParentID != "" {
		t.children[e.ParentID] = append(t.children[e.ParentID], e.ID)
	}
	t.leafID = e.ID
	return e.ID
}

// SetLeafPointer moves the leaf without branching. It is the persisted
// form of SetLeaf, used when the position must survive a reload.
func (t *Tree) SetLeafPointer(id string) {
	if _, ok := t.entries[id]; ok {
		t.leafID = id
	}
}

// AppendMessage adds a message entry to the current branch.
func (t *Tree) AppendMessage(msg llm.Message) string {
	m := msg
	return t.Append(&Entry{Type: EntryMessage, Message: &m})
}

// SetLeaf moves the current position to an entry. The next appended
// entry continues from there, which is how a new branch is created.
//
// Moving to an entry that is not on the current branch is allowed:
// the tree keeps every branch.
func (t *Tree) SetLeaf(id string) error {
	if _, ok := t.entries[id]; !ok {
		return fmt.Errorf("session: no such entry %q", id)
	}
	t.leafID = id
	return nil
}

// Children returns the child entry IDs of an entry, in append order.
func (t *Tree) Children(id string) []string {
	return t.children[id]
}

// Path returns the entries from the root down to the given entry,
// inclusive. It is the branch that entry belongs to.
func (t *Tree) Path(id string) []*Entry {
	var path []*Entry
	seen := map[string]bool{}
	for id != "" {
		if seen[id] {
			break // defensive: a cycle would otherwise hang
		}
		seen[id] = true
		e, ok := t.entries[id]
		if !ok {
			break
		}
		path = append(path, e)
		id = e.ParentID
	}
	// Reverse into root-first order.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// CurrentPath returns the branch from the root to the current leaf.
func (t *Tree) CurrentPath() []*Entry {
	return t.Path(t.leafID)
}

// Branch returns the entries on the current branch that produce model
// context, honoring compaction.
//
// It follows pi's buildContextEntries: walk from the leaf to the root,
// and if a compaction entry is on the path, the latest one wins — its
// summary replaces everything before the first kept entry.
func (t *Tree) Branch() []*Entry {
	path := t.CurrentPath()

	// Find the latest compaction on the path.
	compactIdx := -1
	for i, e := range path {
		if e.Type == EntryCompaction {
			compactIdx = i
		}
	}
	if compactIdx < 0 {
		return path
	}

	compaction := path[compactIdx]
	out := []*Entry{compaction}

	// Keep the entries from firstKeptEntryID up to (but not
	// including) the compaction entry.
	keptStart := -1
	if compaction.FirstKeptEntryID != "" {
		for i, e := range path {
			if e.ID == compaction.FirstKeptEntryID {
				keptStart = i
				break
			}
		}
	}
	if keptStart >= 0 {
		for i := keptStart; i < compactIdx; i++ {
			out = append(out, path[i])
		}
	}
	// Everything after the compaction entry is kept as-is.
	out = append(out, path[compactIdx+1:]...)
	return out
}

// Messages returns the model-visible messages of the current branch.
// Non-message entries (labels, model changes) contribute no message.
func (t *Tree) Messages() []llm.Message {
	var out []llm.Message
	for _, e := range t.Branch() {
		switch e.Type {
		case EntryMessage:
			if e.Message != nil {
				out = append(out, *e.Message)
			}
		case EntryCompaction:
			// The summary stands in for the compacted messages.
			out = append(out, llm.Message{
				Role:    llm.RoleUser,
				Content: "【会话压缩摘要 — 此前对话已压缩，请据此继续协助】\n" + e.Summary,
			})
		}
	}
	return out
}

// Branches returns the leaf entries of the tree: the entry IDs that
// have no children. They are the tips of every branch. Metadata
// entries are not branch tips.
func (t *Tree) Branches() []string {
	var out []string
	for _, id := range t.order {
		e := t.entries[id]
		if e.Type == EntryModelChange || e.Type == EntryLeafPointer {
			continue
		}
		if len(t.children[id]) == 0 {
			out = append(out, id)
		}
	}
	return out
}

// Siblings returns the child entry IDs of the given entry's parent,
// i.e. the alternative branches available from that point.
func (t *Tree) Siblings(id string) []string {
	e, ok := t.entries[id]
	if !ok {
		return nil
	}
	if e.ParentID == "" {
		return []string{id}
	}
	return t.children[e.ParentID]
}

// Ancestors returns the entry IDs from the root down to (but not
// including) the given entry.
func (t *Tree) Ancestors(id string) []string {
	var out []string
	for cur := id; cur != ""; {
		e, ok := t.entries[cur]
		if !ok {
			break
		}
		cur = e.ParentID
		if cur != "" {
			out = append(out, cur)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// entryCounter makes entry IDs unique within a process; the random part
// keeps them unique across processes.
var entryCounter struct {
	n uint32
}

// newEntryID returns a short unique entry ID.
func newEntryID() string {
	entryCounter.n++
	return fmt.Sprintf("%08x%04x", entryCounter.n, uint32(time.Now().UnixNano()&0xffff))
}

// DescribeEntry returns a short human-readable label for an entry, used
// by the /tree listing.
func DescribeEntry(e *Entry) string {
	switch e.Type {
	case EntryMessage:
		if e.Message == nil {
			return "(empty)"
		}
		text := strings.TrimSpace(e.Message.Content)
		if text == "" && len(e.Message.ToolCalls) > 0 {
			names := make([]string, 0, len(e.Message.ToolCalls))
			for _, c := range e.Message.ToolCalls {
				names = append(names, c.Name)
			}
			text = "tool call: " + strings.Join(names, ", ")
		}
		if text == "" {
			return "(empty)"
		}
		return firstLine(text)
	case EntryCompaction:
		return "compaction"
	case EntryBranchSummary:
		return "branch summary"
	case EntryModelChange:
		return "model: " + e.Model
	case EntryLabel:
		return "label: " + e.Label
	case EntrySessionInfo:
		return "name: " + e.Name
	}
	return string(e.Type)
}

// firstLine returns the first line of text, truncated.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 70
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// marshalEntry encodes an entry for the JSONL file.
func marshalEntry(e *Entry) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// sortEntryIDs sorts entry IDs by their timestamp, used when merging
// imported transcripts.
func sortEntryIDs(t *Tree, ids []string) {
	sort.SliceStable(ids, func(i, j int) bool {
		a, okA := t.entries[ids[i]]
		b, okB := t.entries[ids[j]]
		if !okA || !okB {
			return ids[i] < ids[j]
		}
		return a.Timestamp.Before(b.Timestamp)
	})
}
