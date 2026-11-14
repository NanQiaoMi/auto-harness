package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/session"
)

// treeSession is the session the /tree commands operate on.
type treeSession struct {
	// sess is the active session.
	sess *session.Session
	// ag is the agent whose history follows the session branch.
	ag *agent.Agent
	// dir is where sessions are stored.
	dir string
}

// registerTreeCommands wires the session-tree commands: /tree lists and
// navigates branches, /fork and /clone start new sessions.
//
// They follow pi's split: /tree moves inside the current session file
// (related alternatives stay together), while /fork and /clone create
// a separate session file (the alternative becomes separate work).
func registerTreeCommands(commands *ext.CommandRegistry, ts *treeSession) {
	commands.Register("tree", &ext.Command{
		Description: "show the session tree: /tree [entry-id]",
		Handler: func(ctx context.Context, args string) error {
			return runTree(ts, args)
		},
	})
	commands.Register("fork", &ext.Command{
		Description: "fork a new session from an earlier point: /fork [entry-id]",
		Handler: func(ctx context.Context, args string) error {
			return runFork(ts, args, false)
		},
	})
	commands.Register("clone", &ext.Command{
		Description: "copy the current branch into a new session: /clone [name]",
		Handler: func(ctx context.Context, args string) error {
			return runFork(ts, args, true)
		},
	})
}

// runTree lists the session tree, or moves to an entry when one is
// given.
//
// Entries are listed with a short index so they can be referenced
// without copying the raw ID.
func runTree(ts *treeSession, args string) error {
	if ts.sess == nil {
		return fmt.Errorf("no session")
	}
	tree := ts.sess.Tree()

	if id := strings.TrimSpace(args); id != "" {
		return switchBranch(ts, resolveEntry(tree, id))
	}

	// The entries the listing shows, in display order: the current
	// branch first, then the alternatives.
	ordered := treeEntries(tree)
	index := map[string]int{}
	for i, e := range ordered {
		index[e.ID] = i + 1
	}
	current := map[string]bool{}
	for _, e := range tree.CurrentPath() {
		current[e.ID] = true
	}

	fmt.Printf("session %s — %d messages, %d branch(es)\n",
		ts.sess.Meta.ID, len(tree.Messages()), len(tree.Branches()))
	fmt.Println()

	for _, e := range tree.CurrentPath() {
		marker := "  "
		if e.ID == tree.LeafID() {
			marker = "→ "
		}
		fmt.Printf("%s[%d] %s  %s\n", marker, index[e.ID], e.ID, session.DescribeEntry(e))
	}

	// The alternatives: entries that branch off the current path.
	var alts []*session.Entry
	for _, e := range ordered {
		if !current[e.ID] {
			alts = append(alts, e)
		}
	}
	if len(alts) > 0 {
		fmt.Println("\nalternative branches:")
		for _, e := range alts {
			depth := len(tree.Ancestors(e.ID))
			fmt.Printf("  [%d] %s  %s%s\n", index[e.ID], e.ID,
				strings.Repeat("  ", depth), session.DescribeEntry(e))
		}
		fmt.Println("\nuse /tree <index> to switch to a branch")
	}
	return nil
}

// treeEntries returns the entries the tree listing shows, in display
// order: the current branch first, then the alternative branches.
// Metadata entries are excluded.
func treeEntries(tree *session.Tree) []*session.Entry {
	current := map[string]bool{}
	for _, e := range tree.CurrentPath() {
		current[e.ID] = true
	}
	var ordered []*session.Entry
	for _, e := range tree.CurrentPath() {
		if !isMetadata(e) {
			ordered = append(ordered, e)
		}
	}
	for _, e := range tree.Entries() {
		if current[e.ID] || isMetadata(e) {
			continue
		}
		ordered = append(ordered, e)
	}
	return ordered
}

// isMetadata reports whether an entry is session metadata rather than
// conversation content.
func isMetadata(e *session.Entry) bool {
	switch e.Type {
	case session.EntryModelChange, session.EntryLeafPointer:
		return true
	}
	return false
}

// resolveEntry turns a user-supplied reference into an entry ID. It
// accepts the index shown by /tree or a full entry ID.
func resolveEntry(tree *session.Tree, ref string) string {
	// A full entry ID.
	if _, ok := tree.Entry(ref); ok {
		return ref
	}
	// An index into the listing.
	n, err := strconv.Atoi(ref)
	if err != nil || n < 1 {
		return ref
	}
	ordered := treeEntries(tree)
	if n <= len(ordered) {
		return ordered[n-1].ID
	}
	return ref
}

// switchBranch moves the session to another branch and reloads the
// agent's history from it.
func switchBranch(ts *treeSession, id string) error {
	if err := ts.sess.SetLeaf(id); err != nil {
		return err
	}
	ts.ag.ReloadHistory()
	e, _ := ts.sess.Tree().Entry(id)
	fmt.Printf("switched to %s — %s (%d messages in context)\n",
		id, session.DescribeEntry(e), len(ts.ag.History()))
	return nil
}

// runFork creates a new session from the current one.
//
// With clone, the whole current branch is copied. Otherwise the fork
// point is the given entry, or the last user message, which is the
// point pi's /tree offers for branching.
func runFork(ts *treeSession, args string, clone bool) error {
	if ts.sess == nil {
		return fmt.Errorf("no session")
	}
	tree := ts.sess.Tree()

	args = strings.TrimSpace(args)
	forkAt := ""
	name := ""
	if clone {
		name = args
	} else {
		forkAt = args
		if forkAt == "" {
			// Default: the last user message, which is where a new
			// alternative answer usually branches from.
			forkAt = lastUserEntry(tree)
			if forkAt == "" {
				return fmt.Errorf("no user message to fork from")
			}
		} else {
			forkAt = resolveEntry(tree, forkAt)
		}
	}

	dst, err := session.Fork(ts.sess, session.ForkOptions{
		Dir:       ts.dir,
		AtEntryID: forkAt,
		Name:      name,
	})
	if err != nil {
		return err
	}
	defer dst.Close()

	fmt.Printf("forked session %s (%d messages)\n", dst.Meta.ID, len(dst.Messages))
	if dst.Meta.ParentSession != "" {
		fmt.Printf("  from %s\n", dst.Meta.ParentSession)
	}
	fmt.Printf("  resume it with: harness -session %s\n", dst.Meta.ID)
	return nil
}

// lastUserEntry returns the ID of the last user message on the current
// branch, or "" when there is none.
func lastUserEntry(tree *session.Tree) string {
	path := tree.CurrentPath()
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		if e.Type == session.EntryMessage && e.Message != nil && e.Message.Role == "user" {
			return e.ID
		}
	}
	return ""
}
