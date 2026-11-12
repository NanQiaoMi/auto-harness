// Package session provides durable conversation state, modeled after
// pi-durable: JSONL transcripts whose entries form a tree, so a
// conversation can branch inside one file or be forked into a new one.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mimi/auto-harness/llm"
)

// DefaultDir is the per-user session store location.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".harness/sessions"
	}
	return filepath.Join(home, ".harness", "sessions")
}

// Meta describes a session file.
type Meta struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Model     string    `json:"model,omitempty"`
	// ParentSession is the path of the session this one was forked from.
	ParentSession string `json:"parent_session,omitempty"`
	// Name is the user-assigned display name.
	Name string `json:"name,omitempty"`
}

// Session is a persisted conversation transcript.
//
// Messages are appended to the current branch of the tree. Moving the
// leaf (Tree.SetLeaf) and appending again starts a new branch; the
// abandoned branch stays in the file.
type Session struct {
	mu   sync.Mutex
	Meta Meta
	// Messages is the in-memory transcript of the current branch.
	Messages []llm.Message
	// Path is the JSONL file backing this session.
	Path string

	// tree holds the entry tree. It is nil for sessions opened in
	// legacy (v1) mode, which are migrated on load.
	tree *Tree
	// legacy marks a session that was loaded from the v1 linear format
	// and migrated in memory.
	legacy bool

	file *os.File
}

// New creates a session with a fresh ID (timestamp-based, sortable).
func New(dir, model string) (*Session, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id := time.Now().UTC().Format("20060102-150405.000000000")
	s := &Session{
		Meta: Meta{ID: id, CreatedAt: time.Now().UTC(), Model: model},
		Path: filepath.Join(dir, id+".jsonl"),
	}
	s.tree = NewTree(Header{
		Type:      "session",
		Version:   VersionTree,
		ID:        id,
		Timestamp: s.Meta.CreatedAt,
	})
	if err := s.open(true); err != nil {
		return nil, err
	}
	// Record the starting model as an entry so a resumed session
	// knows which model it was using.
	if model != "" {
		e := &Entry{Type: EntryModelChange, Model: model}
		s.tree.Append(e)
		if err := s.writeRecord(e); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Load opens an existing session by ID.
//
// Both the current tree format (v2) and the legacy linear format (v1)
// are accepted; a v1 file is migrated in memory and rewritten on the
// next append.
func Load(dir, id string) (*Session, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	defer f.Close()

	s := &Session{Path: path}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var legacy []llm.Message
	legacyHeader := Header{}
	legacyFirst := true
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Peek at the type to tell the formats apart.
		var probe struct {
			Type  string `json:"type"`
			Kind  string `json:"kind"`
			Entry *Entry `json:"-"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			return nil, fmt.Errorf("session %s: line %d: bad record: %w", id, lineNo, err)
		}
		switch {
		case probe.Type == "session":
			var h Header
			if err := json.Unmarshal([]byte(line), &h); err != nil {
				return nil, fmt.Errorf("session %s: line %d: bad header: %w", id, lineNo, err)
			}
			s.tree = NewTree(h)
			s.Meta.ID = h.ID
			s.Meta.CreatedAt = h.Timestamp
			s.Meta.ParentSession = h.ParentSession
			legacyHeader = h
		case probe.Type != "":
			// A tree entry.
			if s.tree == nil {
				return nil, fmt.Errorf("session %s: line %d: entry before header", id, lineNo)
			}
			var e Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				return nil, fmt.Errorf("session %s: line %d: bad entry: %w", id, lineNo, err)
			}
			s.tree.Append(&e)
			if e.Type == EntrySessionInfo && e.Name != "" {
				s.Meta.Name = e.Name
			}
		case probe.Kind == "meta":
			// Legacy v1 meta record.
			var rec struct {
				Meta *Meta `json:"meta"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return nil, fmt.Errorf("session %s: line %d: bad meta: %w", id, lineNo, err)
			}
			if rec.Meta != nil {
				s.Meta = *rec.Meta
			}
			legacyFirst = false
		case probe.Kind == "message":
			var rec struct {
				Message *llm.Message `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return nil, fmt.Errorf("session %s: line %d: bad message: %w", id, lineNo, err)
			}
			if rec.Message != nil {
				legacy = append(legacy, *rec.Message)
			}
			legacyFirst = false
		default:
			return nil, fmt.Errorf("session %s: line %d: unknown record", id, lineNo)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}

	if s.tree == nil {
		// A legacy linear session: migrate it into a tree.
		if legacyFirst && len(legacy) == 0 {
			return nil, fmt.Errorf("session %s: empty transcript", id)
		}
		h := Header{
			Type:      "session",
			Version:   VersionTree,
			ID:        s.Meta.ID,
			Timestamp: s.Meta.CreatedAt,
		}
		if legacyHeader.ID != "" {
			h.ID = legacyHeader.ID
			h.Timestamp = legacyHeader.Timestamp
		}
		s.tree = NewTree(h)
		for _, m := range legacy {
			s.tree.AppendMessage(m)
		}
		s.legacy = true
	}

	// The model comes from the session info entry when present.
	s.Messages = s.tree.Messages()
	if s.Meta.Model == "" {
		s.Meta.Model = s.currentModel()
	}
	if err := s.open(false); err != nil {
		return nil, err
	}
	return s, nil
}

// open opens the backing file for appending. When create is set, the
// file is created exclusively and the header written.
func (s *Session) open(create bool) error {
	flags := os.O_APPEND | os.O_WRONLY
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(s.Path, flags, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	if create {
		h := s.tree.Header()
		data, err := json.Marshal(h)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	return nil
}

// writeRecord writes one JSONL line.
func (s *Session) writeRecord(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.file.Write(append(data, '\n'))
	return err
}

// Append adds messages to the current branch and persists them.
func (s *Session) Append(msgs ...llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		m := m
		e := &Entry{Type: EntryMessage, Message: &m}
		s.tree.Append(e)
		if err := s.writeRecord(e); err != nil {
			return err
		}
		s.Messages = append(s.Messages, m)
	}
	return nil
}

// Tree returns the session's entry tree.
func (s *Session) Tree() *Tree {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tree
}

// SetModel records the model in the session going forward. The change
// is persisted as a model-change entry, so a resumed session knows
// which model it was using (pi's ModelChangeEntry).
func (s *Session) SetModel(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Meta.Model == model {
		return
	}
	s.Meta.Model = model
	e := &Entry{Type: EntryModelChange, Model: model}
	s.tree.Append(e)
	if err := s.writeRecord(e); err != nil {
		return
	}
}

// SetName assigns a display name to the session, recorded as an entry
// so it survives reloads.
func (s *Session) SetName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &Entry{Type: EntrySessionInfo, Name: name}
	s.tree.Append(e)
	if err := s.writeRecord(e); err != nil {
		return err
	}
	s.Meta.Name = name
	return nil
}

// currentModel returns the model named by the latest model-change entry.
func (s *Session) currentModel() string {
	var model string
	for _, e := range s.tree.Entries() {
		if e.Type == EntryModelChange && e.Model != "" {
			model = e.Model
		}
	}
	return model
}

// Close flushes and closes the underlying file.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Info summarizes a session file for listings.
type Info struct {
	ID        string
	CreatedAt time.Time
	Model     string
	Path      string
	Messages  int
	// Branches is the number of branch tips in the session.
	Branches int
	// Name is the user-assigned display name.
	Name string
	// ParentSession is the source of a forked session.
	ParentSession string
}

// List returns all sessions in dir, newest first.
func List(dir string) ([]Info, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		info := inspectFile(filepath.Join(dir, ent.Name()))
		if info.ID != "" {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// inspectFile reads a session file's summary without loading it fully.
func inspectFile(path string) Info {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	info := Info{ID: id, Path: path}
	f, err := os.Open(path)
	if err != nil {
		return Info{}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var parents []string
	childCount := map[string]int{}
	for scanner.Scan() {
		var probe struct {
			Type      string `json:"type"`
			Kind      string `json:"kind"`
			Version   int    `json:"version"`
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			At        string `json:"at"`
			ParentID  string `json:"parentId"`
			Entry     *Entry `json:"-"`
		}
		if json.Unmarshal(scanner.Bytes(), &probe) != nil {
			continue
		}
		if probe.Type == "session" {
			if probe.ID != "" {
				info.ID = probe.ID
			}
			ts := probe.Timestamp
			if ts == "" {
				ts = probe.At
			}
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				info.CreatedAt = t
			}
			continue
		}
		if probe.Kind == "meta" {
			var rec struct {
				Meta *Meta `json:"meta"`
			}
			if json.Unmarshal(scanner.Bytes(), &rec) == nil && rec.Meta != nil {
				info.CreatedAt = rec.Meta.CreatedAt
				info.Model = rec.Meta.Model
				info.Name = rec.Meta.Name
				info.ParentSession = rec.Meta.ParentSession
			}
			continue
		}
		if probe.Kind == "message" {
			info.Messages++
			continue
		}
		// A tree entry: track parents to count branch tips.
		var e Entry
		if json.Unmarshal(scanner.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		if e.Type == EntryMessage {
			info.Messages++
		}
		if e.Type == EntryModelChange && e.Model != "" {
			info.Model = e.Model
		}
		if e.Type == EntrySessionInfo && e.Name != "" {
			info.Name = e.Name
		}
		if e.ParentID != "" {
			childCount[e.ParentID]++
		}
		parents = append(parents, e.ID)
	}
	// The branch tips are the entries that are nobody's parent.
	info.Branches = 0
	for _, id := range parents {
		if childCount[id] == 0 {
			info.Branches++
		}
	}
	if info.Branches == 0 && len(parents) > 0 {
		info.Branches = 1
	}
	return info
}
