package tools

import (
	"path/filepath"
	"sync"
)

// MutationQueue serializes the file mutations that target the same
// file.
//
// It mirrors pi's file-mutation-queue: the edit and write tools take
// the queue for the path they are about to change, so two mutations
// of the same file cannot interleave. Mutations of different files
// still run in parallel.
//
// The queue is keyed by the file's real path, so two names for the
// same file ("a/b.txt" and "./a/b.txt") share one queue.
type MutationQueue struct {
	mu sync.Mutex
	// queues holds the tail of each file's queue.
	queues map[string]chan struct{}
}

// NewMutationQueue returns an empty queue.
func NewMutationQueue() *MutationQueue {
	return &MutationQueue{queues: map[string]chan struct{}{}}
}

// DefaultMutationQueue is the queue the built-in tools use.
//
// A package-level queue is what pi does: the tools are separate
// objects, and the queue that ties them together has to be shared.
var DefaultMutationQueue = NewMutationQueue()

// WithMutation runs fn while holding the queue for the file.
//
// The queue is released even when fn panics.
func (q *MutationQueue) WithMutation(path string, fn func() error) error {
	key := q.key(path)

	// The registration is serialized: two goroutines must not compute
	// the same key and then both take the same slot.
	q.mu.Lock()
	prev := q.queues[key]
	slot := make(chan struct{})
	q.queues[key] = slot
	q.mu.Unlock()

	// Wait for the previous holder of this file.
	if prev != nil {
		<-prev
	}
	defer func() {
		close(slot)
		q.mu.Lock()
		// Only clear the slot when no one queued behind it.
		if q.queues[key] == slot {
			delete(q.queues, key)
		}
		q.mu.Unlock()
	}()

	return fn()
}

// key returns the queue key of a path: its real path when it exists,
// its resolved path otherwise.
//
// A file that does not exist yet has no real path, so the resolved
// path is used — two names for the same new file still share a
// queue.
func (q *MutationQueue) key(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// WithFileMutation runs fn while holding the queue for the file, using
// the default queue.
func WithFileMutation(path string, fn func() error) error {
	return DefaultMutationQueue.WithMutation(path, fn)
}
