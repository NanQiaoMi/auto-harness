// Package orchestrator runs delegated agents as durable background
// tasks. Each task has its own session and context manager, while the
// parent sees only a bounded report through the agent tool.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// State is a delegated task's lifecycle state.
type State string

const (
	StateQueued      State = "queued"
	StateRunning     State = "running"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCancelled   State = "cancelled"
	StateInterrupted State = "interrupted"
)

// Isolation describes where a delegated task may edit.
type Isolation string

const (
	// IsolationAuto uses a linked Git worktree when the parent is in a
	// repository and falls back to the shared workspace otherwise.
	IsolationAuto Isolation = "auto"
	// IsolationWorktree requires a linked Git worktree.
	IsolationWorktree Isolation = "worktree"
	// IsolationShared deliberately uses the parent's working tree.
	IsolationShared Isolation = "shared"
)

// Outcome is the bounded result a delegated worker returns.
type Outcome struct {
	Summary string
	Turns   int
	Usage   llm.Usage
}

// Worker is one stateful delegated conversation. Run may be called more
// than once for follow-up prompts; the worker's own session carries the
// conversation between calls.
type Worker interface {
	Run(ctx context.Context, prompt string) (Outcome, error)
	Close() error
}

// Prepared is what a Factory creates for one task.
type Prepared struct {
	Worker      Worker
	SessionID   string
	SessionPath string
	Workspace   string
	Isolation   Isolation
}

// Factory builds the isolated runtime for a task.
type Factory func(ctx context.Context, task Task) (Prepared, error)

// Config bounds the orchestrator. Zero values use safe defaults.
type Config struct {
	MaxConcurrent int
	MaxDepth      int
	Timeout       time.Duration
	ResultLimit   int
	// StoreDir is where task records are persisted. Empty keeps task
	// records in memory only.
	StoreDir string
	Factory  Factory
}

const (
	DefaultMaxConcurrent = 4
	DefaultMaxDepth      = 3
	DefaultTimeout       = 30 * time.Minute
	DefaultResultLimit   = 6000
	maxPromptBytes       = 512 * 1024
	maxNameRunes         = 120
)

func (c Config) withDefaults() Config {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.MaxDepth <= 0 {
		c.MaxDepth = DefaultMaxDepth
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.ResultLimit <= 0 {
		c.ResultLimit = DefaultResultLimit
	}
	return c
}

// Task is the durable, externally visible record of one delegated run.
type Task struct {
	ID             string    `json:"id"`
	ParentID       string    `json:"parentId,omitempty"`
	Name           string    `json:"name,omitempty"`
	Prompt         string    `json:"prompt,omitempty"`
	State          State     `json:"state"`
	Depth          int       `json:"depth"`
	Model          string    `json:"model,omitempty"`
	Isolation      Isolation `json:"isolation,omitempty"`
	Workspace      string    `json:"workspace,omitempty"`
	SessionID      string    `json:"sessionId,omitempty"`
	SessionPath    string    `json:"sessionPath,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Error          string    `json:"error,omitempty"`
	Turns          int       `json:"turns,omitempty"`
	Runs           int       `json:"runs,omitempty"`
	Queued         int       `json:"queued,omitempty"`
	InputTokens    int       `json:"inputTokens,omitempty"`
	OutputTokens   int       `json:"outputTokens,omitempty"`
	SummaryCut     bool      `json:"summaryTruncated,omitempty"`
	TimeoutSeconds int       `json:"timeoutSeconds,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	StartedAt      time.Time `json:"startedAt,omitempty"`
	EndedAt        time.Time `json:"endedAt,omitempty"`
}

// Terminal reports whether a task has stopped processing prompts.
func (t Task) Terminal() bool {
	switch t.State {
	case StateCompleted, StateFailed, StateCancelled, StateInterrupted:
		return true
	default:
		return false
	}
}

// SpawnRequest is one delegation request.
type SpawnRequest struct {
	Prompt    string
	Name      string
	Model     string
	Isolation Isolation
	Timeout   time.Duration
}

type taskState struct {
	Task
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	change   chan struct{}
	prompts  []string
	worker   Worker
	prepared bool
	// live reports whether this process owns a task loop for the
	// record. Tasks loaded from disk are history-only until they are
	// explicitly resumed by a future runtime.
	live bool
}

// Manager owns delegated tasks and their concurrency limit.
type Manager struct {
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	tasks  map[string]*taskState
	order  []string
	closed bool
	// workerErrs records close failures from background loops. Close
	// returns them after the loops have stopped.
	workerErrs []error

	closeOnce sync.Once
}

// New creates a manager. The parent context bounds every background
// task: closing the harness cancels the work instead of orphaning it.
func New(parent context.Context, cfg Config) (*Manager, error) {
	if parent == nil {
		parent = context.Background()
	}
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
		tasks:  map[string]*taskState{},
	}
	if cfg.StoreDir != "" {
		if err := m.load(); err != nil {
			cancel()
			return nil, err
		}
	}
	return m, nil
}

type taskContextKey struct{}

// WithTaskID marks a context as belonging to a delegated task. The
// agent tool uses it to enforce parent/child visibility and recursion
// depth without trusting model-supplied parent IDs.
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, taskContextKey{}, id)
}

// TaskID returns the delegated task that owns ctx, or an empty string
// for the primary agent.
func TaskID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(taskContextKey{}).(string)
	return id
}

// Spawn queues a delegated task and returns immediately.
func (m *Manager) Spawn(ctx context.Context, req SpawnRequest) (Task, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Task{}, fmt.Errorf("orchestrator: prompt must not be empty")
	}
	if len(prompt) > maxPromptBytes {
		return Task{}, fmt.Errorf("orchestrator: prompt exceeds %d bytes", maxPromptBytes)
	}
	isolation := req.Isolation
	if isolation == "" {
		isolation = IsolationAuto
	}
	switch isolation {
	case IsolationAuto, IsolationWorktree, IsolationShared:
	default:
		return Task{}, fmt.Errorf("orchestrator: unknown isolation %q", isolation)
	}
	timeout := m.cfg.Timeout
	if req.Timeout > 0 {
		timeout = req.Timeout
	}

	parentID := TaskID(ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("orchestrator: manager is closed")
	}
	depth := 0
	if parentID != "" {
		parent, ok := m.tasks[parentID]
		if !ok {
			m.mu.Unlock()
			return Task{}, fmt.Errorf("orchestrator: parent task %q not found", parentID)
		}
		if parent.Terminal() {
			m.mu.Unlock()
			return Task{}, fmt.Errorf("orchestrator: parent task %s is already %s", parentID, parent.State)
		}
		depth = parent.Depth + 1
	}
	if depth > m.cfg.MaxDepth {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("orchestrator: delegation depth %d exceeds the limit %d",
			depth, m.cfg.MaxDepth)
	}
	id, err := newTaskID()
	if err != nil {
		m.mu.Unlock()
		return Task{}, err
	}
	now := time.Now().UTC()
	taskCtx, cancel := context.WithCancel(m.ctx)
	ts := &taskState{
		Task: Task{
			ID:             id,
			ParentID:       parentID,
			Name:           cleanName(req.Name, prompt),
			Prompt:         prompt,
			State:          StateQueued,
			Depth:          depth,
			Model:          strings.TrimSpace(req.Model),
			Isolation:      isolation,
			TimeoutSeconds: int(timeout / time.Second),
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		ctx:     taskCtx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		change:  make(chan struct{}),
		prompts: []string{prompt},
		live:    true,
	}
	ts.Queued = len(ts.prompts)
	m.tasks[id] = ts
	m.order = append(m.order, id)
	if err := m.persistLocked(ts); err != nil {
		delete(m.tasks, id)
		m.order = removeString(m.order, id)
		cancel()
		m.mu.Unlock()
		return Task{}, err
	}
	snapshot := ts.Task
	// Add while the manager lock is held: Close uses the same lock to
	// establish the closed boundary, so it cannot return before this
	// task loop is enrolled in the wait group.
	m.wg.Add(1)
	m.mu.Unlock()

	go m.taskLoop(ts)
	signal(ts.wake)
	return snapshot, nil
}

// Send appends a follow-up prompt. A completed task wakes up again; a
// running task receives the prompt when its current run finishes.
func (m *Manager) Send(ctx context.Context, id, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("orchestrator: prompt must not be empty")
	}
	if len(prompt) > maxPromptBytes {
		return fmt.Errorf("orchestrator: prompt exceeds %d bytes", maxPromptBytes)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("orchestrator: manager is closed")
	}
	ts, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("orchestrator: task %q not found", id)
	}
	if !ts.live {
		m.mu.Unlock()
		return fmt.Errorf(
			"orchestrator: task %s was restored from disk and cannot accept new prompts; spawn a new task",
			id)
	}
	switch ts.State {
	case StateCancelled, StateInterrupted:
		m.mu.Unlock()
		return fmt.Errorf("orchestrator: task %s is %s", id, ts.State)
	case StateFailed:
		m.mu.Unlock()
		return fmt.Errorf("orchestrator: task %s failed; spawn a new task instead", id)
	}
	ts.prompts = append(ts.prompts, prompt)
	ts.Queued = len(ts.prompts)
	if ts.State == StateCompleted {
		ts.State = StateQueued
		ts.EndedAt = time.Time{}
	}
	ts.UpdatedAt = time.Now().UTC()
	if err := m.persistLocked(ts); err != nil {
		m.mu.Unlock()
		return err
	}
	m.notifyLocked(ts)
	m.mu.Unlock()
	signal(ts.wake)
	return nil
}

// Get returns a task record.
func (m *Manager) Get(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ts, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return snapshotLocked(ts), true
}

// CanAccess reports whether scope may inspect or control id. The
// primary agent (empty scope) can access everything. A delegated task
// can access itself and its descendants, never siblings or ancestors.
func (m *Manager) CanAccess(scope, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return canAccessLocked(m.tasks, scope, id)
}

// List returns tasks in newest-first order. An empty scope lists every
// task. A non-empty scope lists descendants; recursive includes the
// full subtree, otherwise only direct children.
func (m *Manager) List(scope string, recursive bool) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0)
	for _, id := range m.order {
		ts := m.tasks[id]
		if ts == nil || ts.ID == scope {
			continue
		}
		if scope != "" {
			if !canAccessLocked(m.tasks, scope, ts.ID) {
				continue
			}
			if !recursive && ts.ParentID != scope {
				continue
			}
		} else if !recursive && ts.ParentID != "" {
			continue
		}
		out = append(out, snapshotLocked(ts))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Wait blocks until the task is terminal or timeout elapses. A timeout
// returns the current snapshot with done=false and no error, so a
// caller can poll without treating slow work as a failure.
func (m *Manager) Wait(ctx context.Context, id string, timeout time.Duration) (Task, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		m.mu.Lock()
		ts, ok := m.tasks[id]
		if !ok {
			m.mu.Unlock()
			return Task{}, false, fmt.Errorf("orchestrator: task %q not found", id)
		}
		snapshot := snapshotLocked(ts)
		change := ts.change
		m.mu.Unlock()
		if snapshot.Terminal() {
			return snapshot, true, nil
		}
		select {
		case <-ctx.Done():
			return snapshot, false, ctx.Err()
		case <-timer.C:
			return snapshot, false, nil
		case <-change:
		}
	}
}

// Cancel stops a task, its current run and every descendant. The
// operation is idempotent for already-terminal tasks.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.Lock()
	ts, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("orchestrator: task %q not found", id)
	}
	targets := m.descendantsLocked(id)
	if !ts.Terminal() {
		targets = append([]string{id}, targets...)
	}
	now := time.Now().UTC()
	for _, targetID := range targets {
		target := m.tasks[targetID]
		if target == nil || target.Terminal() {
			continue
		}
		target.State = StateCancelled
		target.UpdatedAt = now
		target.EndedAt = now
		target.Error = "cancelled"
		target.Queued = 0
		target.cancel()
		_ = m.persistLocked(target)
		m.notifyLocked(target)
	}
	m.mu.Unlock()
	return nil
}

// Close cancels workers and marks unfinished tasks as interrupted.
func (m *Manager) Close() error {
	var errs []error
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		now := time.Now().UTC()
		for _, id := range m.order {
			ts := m.tasks[id]
			if ts == nil || ts.Terminal() {
				continue
			}
			ts.State = StateInterrupted
			ts.UpdatedAt = now
			ts.EndedAt = now
			ts.Error = "harness stopped before the task completed"
			ts.Queued = 0
			if err := m.persistLocked(ts); err != nil {
				errs = append(errs, err)
			}
			m.notifyLocked(ts)
		}
		m.mu.Unlock()
		m.cancel()
		m.wg.Wait()

		m.mu.Lock()
		workers := make([]Worker, 0, len(m.tasks))
		for _, ts := range m.tasks {
			if ts.worker != nil {
				workers = append(workers, ts.worker)
				ts.worker = nil
				ts.prepared = false
			}
		}
		errs = append(errs, m.workerErrs...)
		m.workerErrs = nil
		m.mu.Unlock()
		for _, worker := range workers {
			if err := worker.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	})
	return errors.Join(errs...)
}

func (m *Manager) taskLoop(ts *taskState) {
	defer m.wg.Done()
	defer m.closeWorker(ts)

	for {
		select {
		case <-ts.ctx.Done():
			m.markInterrupted(ts, "harness stopped before the task completed")
			return
		case <-ts.wake:
		}

		select {
		case m.sem <- struct{}{}:
		case <-ts.ctx.Done():
			m.markInterrupted(ts, "harness stopped before the task completed")
			return
		}

		if !ts.prepared {
			if err := m.prepare(ts); err != nil {
				<-m.sem
				return
			}
		}

		for {
			prompt, ok := m.nextPrompt(ts)
			if !ok {
				m.markCompletedIfIdle(ts)
				break
			}
			if !m.runPrompt(ts, prompt) {
				<-m.sem
				return
			}
		}
		<-m.sem
	}
}

// closeWorker detaches and closes a task's worker exactly once. The
// task loop and Manager.Close can both reach the end of a task, so the
// pointer is cleared while holding the manager lock before Close runs.
func (m *Manager) closeWorker(ts *taskState) {
	m.mu.Lock()
	worker := ts.worker
	ts.worker = nil
	ts.prepared = false
	m.mu.Unlock()
	if worker == nil {
		return
	}
	if err := worker.Close(); err != nil {
		m.mu.Lock()
		m.workerErrs = append(m.workerErrs, err)
		m.mu.Unlock()
	}
}

func (m *Manager) prepare(ts *taskState) error {
	m.mu.Lock()
	if ts.State != StateCancelled && ts.State != StateInterrupted {
		ts.State = StateRunning
		if ts.StartedAt.IsZero() {
			ts.StartedAt = time.Now().UTC()
		}
		ts.UpdatedAt = time.Now().UTC()
		if err := m.persistLocked(ts); err != nil {
			m.mu.Unlock()
			return err
		}
		m.notifyLocked(ts)
	}
	taskCopy := ts.Task
	m.mu.Unlock()

	if m.cfg.Factory == nil {
		m.fail(ts, fmt.Errorf("orchestrator: no worker factory configured"))
		return fmt.Errorf("orchestrator: no worker factory configured")
	}
	prepared, err := m.cfg.Factory(WithTaskID(ts.ctx, ts.ID), taskCopy)
	if err != nil {
		m.fail(ts, fmt.Errorf("orchestrator: prepare %s: %w", ts.ID, err))
		return err
	}
	if prepared.Worker == nil {
		err := fmt.Errorf("orchestrator: factory returned no worker for %s", ts.ID)
		m.fail(ts, err)
		return err
	}

	m.mu.Lock()
	ts.worker = prepared.Worker
	ts.prepared = true
	ts.SessionID = prepared.SessionID
	ts.SessionPath = prepared.SessionPath
	ts.Workspace = prepared.Workspace
	if prepared.Isolation != "" {
		ts.Isolation = prepared.Isolation
	}
	ts.UpdatedAt = time.Now().UTC()
	if err := m.persistLocked(ts); err != nil {
		ts.worker = nil
		ts.prepared = false
		m.mu.Unlock()
		_ = prepared.Worker.Close()
		m.fail(ts, fmt.Errorf("orchestrator: persist prepared task: %w", err))
		return err
	}
	m.notifyLocked(ts)
	m.mu.Unlock()
	return nil
}

func (m *Manager) nextPrompt(ts *taskState) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts.State == StateCancelled || ts.State == StateInterrupted || ts.State == StateFailed {
		return "", false
	}
	if len(ts.prompts) == 0 {
		return "", false
	}
	prompt := ts.prompts[0]
	ts.prompts = ts.prompts[1:]
	ts.Queued = len(ts.prompts)
	ts.State = StateRunning
	ts.UpdatedAt = time.Now().UTC()
	_ = m.persistLocked(ts)
	m.notifyLocked(ts)
	return prompt, true
}

// runPrompt returns false when the task loop must stop.
func (m *Manager) runPrompt(ts *taskState, prompt string) bool {
	m.mu.Lock()
	worker := ts.worker
	timeout := m.cfg.Timeout
	if ts.TimeoutSeconds > 0 {
		timeout = time.Duration(ts.TimeoutSeconds) * time.Second
	}
	m.mu.Unlock()
	if worker == nil {
		m.fail(ts, fmt.Errorf("orchestrator: worker for %s is not available", ts.ID))
		return false
	}

	runCtx := ts.ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ts.ctx, timeout)
	}
	defer cancel()
	runCtx = WithTaskID(runCtx, ts.ID)
	outcome, err := worker.Run(runCtx, prompt)
	if err != nil {
		if ts.ctx.Err() != nil || m.ctx.Err() != nil {
			m.markInterrupted(ts, "harness stopped before the task completed")
			return false
		}
		if m.taskState(ts.ID) == StateCancelled {
			return false
		}
		if errors.Is(err, context.DeadlineExceeded) {
			m.fail(ts, fmt.Errorf("timed out after %s", timeout))
		} else {
			m.fail(ts, err)
		}
		return false
	}

	m.mu.Lock()
	if ts.State == StateCancelled || ts.State == StateInterrupted {
		m.mu.Unlock()
		return false
	}
	summary, cut := limitText(outcome.Summary, m.cfg.ResultLimit)
	ts.Summary = summary
	ts.SummaryCut = cut
	ts.Turns += outcome.Turns
	ts.InputTokens += outcome.Usage.InputTokens
	ts.OutputTokens += outcome.Usage.OutputTokens
	ts.Runs++
	ts.Error = ""
	ts.UpdatedAt = time.Now().UTC()
	_ = m.persistLocked(ts)
	m.notifyLocked(ts)
	m.mu.Unlock()
	return true
}

func (m *Manager) markCompletedIfIdle(ts *taskState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts.State != StateRunning || len(ts.prompts) > 0 {
		return
	}
	ts.State = StateCompleted
	ts.UpdatedAt = time.Now().UTC()
	ts.EndedAt = ts.UpdatedAt
	ts.Queued = 0
	_ = m.persistLocked(ts)
	m.notifyLocked(ts)
}

func (m *Manager) markInterrupted(ts *taskState, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts.Terminal() {
		return
	}
	now := time.Now().UTC()
	ts.State = StateInterrupted
	ts.UpdatedAt = now
	ts.EndedAt = now
	ts.Error = reason
	ts.Queued = 0
	_ = m.persistLocked(ts)
	m.notifyLocked(ts)
}

func (m *Manager) fail(ts *taskState, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts.State == StateCancelled || ts.State == StateInterrupted {
		return
	}
	now := time.Now().UTC()
	ts.State = StateFailed
	ts.UpdatedAt = now
	ts.EndedAt = now
	ts.Error = err.Error()
	ts.Queued = 0
	_ = m.persistLocked(ts)
	m.notifyLocked(ts)
	m.cancelDescendantsLocked(ts.ID, "parent task failed")
}

func (m *Manager) cancelDescendantsLocked(id, reason string) {
	now := time.Now().UTC()
	for _, targetID := range m.descendantsLocked(id) {
		target := m.tasks[targetID]
		if target == nil || target.Terminal() {
			continue
		}
		target.State = StateCancelled
		target.UpdatedAt = now
		target.EndedAt = now
		target.Error = reason
		target.Queued = 0
		target.cancel()
		_ = m.persistLocked(target)
		m.notifyLocked(target)
	}
}

func (m *Manager) taskState(id string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts := m.tasks[id]; ts != nil {
		return ts.State
	}
	return ""
}

func (m *Manager) descendantsLocked(id string) []string {
	var out []string
	for _, candidate := range m.order {
		ts := m.tasks[candidate]
		if ts == nil {
			continue
		}
		for parent := ts.ParentID; parent != ""; {
			if parent == id {
				out = append(out, ts.ID)
				break
			}
			parentTask := m.tasks[parent]
			if parentTask == nil {
				break
			}
			parent = parentTask.ParentID
		}
	}
	return out
}

func canAccessLocked(tasks map[string]*taskState, scope, id string) bool {
	if scope == "" {
		return true
	}
	if scope == id {
		return true
	}
	ts := tasks[id]
	for ts != nil && ts.ParentID != "" {
		if ts.ParentID == scope {
			return true
		}
		ts = tasks[ts.ParentID]
	}
	return false
}

func snapshotLocked(ts *taskState) Task {
	out := ts.Task
	out.Queued = len(ts.prompts)
	return out
}

func (m *Manager) notifyLocked(ts *taskState) {
	close(ts.change)
	ts.change = make(chan struct{})
}

func (m *Manager) load() error {
	dir := filepath.Join(m.cfg.StoreDir, "tasks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("orchestrator: read store: %w", err)
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return fmt.Errorf("orchestrator: read task record: %w", err)
		}
		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			return fmt.Errorf("orchestrator: parse %s: %w", entry.Name(), err)
		}
		if task.ID == "" {
			continue
		}
		if !task.Terminal() {
			task.State = StateInterrupted
			task.Error = "interrupted by a harness restart"
			task.UpdatedAt = now
			task.EndedAt = now
			task.Queued = 0
		}
		ts := &taskState{Task: task, change: make(chan struct{})}
		m.tasks[task.ID] = ts
		m.order = append(m.order, task.ID)
		if !task.Terminal() || task.State == StateInterrupted {
			if err := m.persistLocked(ts); err != nil {
				return err
			}
		}
	}
	sort.Slice(m.order, func(i, j int) bool {
		a, b := m.tasks[m.order[i]], m.tasks[m.order[j]]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	return nil
}

func (m *Manager) persistLocked(ts *taskState) error {
	if m.cfg.StoreDir == "" {
		return nil
	}
	dir := filepath.Join(m.cfg.StoreDir, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("orchestrator: create store: %w", err)
	}
	data, err := json.MarshalIndent(ts.Task, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ts.ID+".json")
	temp, err := os.CreateTemp(dir, "."+ts.ID+"-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func newTaskID() (string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "sub-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:]), nil
}

func cleanName(name, prompt string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(prompt)
		if i := strings.IndexByte(name, '\n'); i >= 0 {
			name = name[:i]
		}
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "delegated task"
	}
	runes := []rune(name)
	if len(runes) > maxNameRunes {
		name = string(runes[:maxNameRunes]) + "…"
	}
	return name
}

func limitText(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	used := 0
	end := 0
	for i, r := range runes {
		size := utf8.RuneLen(r)
		if used+size > limit {
			end = i
			break
		}
		used += size
		end = i + 1
	}
	if end <= 0 {
		return "", true
	}
	return string(runes[:end]) + fmt.Sprintf(
		"\n[delegated result truncated at %d bytes; full transcript: see the task session]", limit), true
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}
