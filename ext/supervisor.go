package ext

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"time"

	"github.com/NanQiaoMi/auto-harness/sandbox"
)

// RestartMode controls whether a process extension is restarted after
// its child process exits.
type RestartMode string

const (
	// RestartNever is the default and preserves the historical
	// one-process lifetime.
	RestartNever RestartMode = "never"
	// RestartOnFailure restarts only when the child exits non-zero.
	RestartOnFailure RestartMode = "on_failure"
	// RestartAlways restarts after either a clean or failed exit.
	RestartAlways RestartMode = "always"
)

// RestartPolicy bounds automatic process recovery. MaxRestarts counts
// successful restarts for the lifetime of this extension instance;
// zero starts no replacement when Mode enables supervision.
type RestartPolicy struct {
	Mode        RestartMode
	MaxRestarts int
	Backoff     time.Duration
	MaxBackoff  time.Duration
}

// SupervisionStatus is a point-in-time health snapshot for an
// extension process.
type SupervisionStatus struct {
	Mode        RestartMode
	Restarts    int
	MaxRestarts int
	Healthy     bool
	Restarting  bool
	LastExit    string
	LastError   string
}

type processRun struct {
	cmd   *exec.Cmd
	conn  *rpcConn
	done  chan struct{}
	ready chan struct{}
	err   error
}

// accept allows notifications from a child whose manifest has been
// approved. Events emitted during the handshake are dropped: the
// process has not yet established which authority it is claiming.
func (r *processRun) accept() {
	close(r.ready)
}

func (p RestartPolicy) normalized() RestartPolicy {
	if p.Mode == "" {
		p.Mode = RestartNever
	}
	if p.Mode != RestartOnFailure && p.Mode != RestartAlways {
		p.Mode = RestartNever
	}
	if p.Mode == RestartNever {
		return p
	}
	if p.MaxRestarts < 0 {
		p.MaxRestarts = 0
	}
	if p.MaxRestarts > 100 {
		p.MaxRestarts = 100
	}
	if p.Backoff <= 0 {
		p.Backoff = 250 * time.Millisecond
	}
	if p.Backoff < 10*time.Millisecond {
		p.Backoff = 10 * time.Millisecond
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = 30 * time.Second
	}
	if p.MaxBackoff < p.Backoff {
		p.MaxBackoff = p.Backoff
	}
	if p.MaxBackoff > time.Minute {
		p.MaxBackoff = time.Minute
	}
	return p
}

// SupervisionStatus returns the current health of this extension. A
// process that is deliberately stopped or has exhausted its policy is
// unhealthy; a process in a restart backoff is unhealthy but marked
// Restarting.
func (e *ProcessExtension) SupervisionStatus() SupervisionStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	policy := e.RestartPolicy.normalized()
	return SupervisionStatus{
		Mode:        policy.Mode,
		Restarts:    e.restartCount,
		MaxRestarts: policy.MaxRestarts,
		Healthy:     !e.stopped && e.run != nil && !e.restarting,
		Restarting:  e.restarting,
		LastExit:    e.lastExit,
		LastError:   e.lastError,
	}
}

func (e *ProcessExtension) currentCommand() *exec.Cmd {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return nil
	}
	return e.run.cmd
}

// startProcess launches one child, installs its read loop and completes
// the manifest handshake. It does not publish the run into e.run; the
// caller decides whether the run is accepted.
func (e *ProcessExtension) startProcess(api *API) (*processRun, Manifest, error) {
	e.mu.Lock()
	ctx := e.ctx
	stopped := e.stopped
	e.mu.Unlock()
	if stopped {
		return nil, Manifest{}, fmt.Errorf("ext: %s: process is stopped", e.Command)
	}
	if ctx == nil {
		return nil, Manifest{}, fmt.Errorf("ext: %s: process context is not initialized", e.Command)
	}
	cmd, sandboxed, err := sandbox.Command(ctx, e.Command, e.Args, e.sandboxProfile(), e.Dir)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("ext: %s: sandbox: %w", e.Command, err)
	}
	if e.Sandbox != nil && !sandboxed {
		if e.RequireSandbox {
			return nil, Manifest{}, fmt.Errorf(
				"ext: %s: a sandbox was required but this platform has none", e.Command)
		}
		fmt.Fprintf(os.Stderr,
			"[warn] ext: %s: no sandbox on this platform; the extension runs unconfined\n", e.Command)
	}
	if len(e.Env) > 0 {
		cmd.Env = append(cmd.Environ(), e.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("ext: %s: stdin pipe: %w", e.Command, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("ext: %s: stdout pipe: %w", e.Command, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, Manifest{}, fmt.Errorf("ext: %s: start: %w", e.Command, err)
	}
	run := &processRun{
		cmd:   cmd,
		conn:  newRPCConn(stdout, stdin, stdin),
		done:  make(chan struct{}),
		ready: make(chan struct{}),
	}
	go func() {
		run.err = cmd.Wait()
		close(run.done)
	}()
	go run.conn.readLoop(func(msg Protocol) {
		select {
		case <-run.ready:
			e.handleNotification(api, msg)
		default:
		}
	})

	var manifest Manifest
	if err := run.conn.callTimeout(MethodManifest, nil, &manifest, handshakeTimeout); err != nil {
		terminateRun(run)
		return nil, Manifest{}, fmt.Errorf("ext: %s: manifest: %w", e.Command, err)
	}
	if manifest.Name == "" {
		manifest.Name = e.Command
	}
	return run, manifest, nil
}

// supervise watches one run at a time. Every replacement must have the
// same runtime manifest as the process approved during Setup; a plugin
// cannot silently change its tools or consult authority after a crash.
func (e *ProcessExtension) supervise(api *API, expected Manifest) {
	e.mu.Lock()
	run := e.run
	policy := e.RestartPolicy.normalized()
	done := e.supervisorDone
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.restarting = false
		e.mu.Unlock()
		close(done)
	}()

	for run != nil {
		<-run.done
		e.mu.Lock()
		if e.stopped || e.run != run {
			e.mu.Unlock()
			return
		}
		exitErr := run.err
		exit := exitDescription(exitErr)
		e.run = nil
		e.lastExit = exit
		if exitErr != nil {
			e.lastError = exitErr.Error()
		} else {
			e.lastError = ""
		}
		e.mu.Unlock()

		if policy.Mode == RestartNever ||
			(policy.Mode == RestartOnFailure && exitErr == nil) {
			return
		}
		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			return
		}
		if e.restartCount >= policy.MaxRestarts {
			e.restarting = false
			e.lastError = restartLimitError(policy.MaxRestarts, exit)
			e.mu.Unlock()
			return
		}
		e.restarting = true
		e.mu.Unlock()

		var next *processRun
		for starts := 0; starts < policy.MaxRestarts; starts++ {
			e.mu.Lock()
			if e.stopped || e.restartCount >= policy.MaxRestarts {
				e.restarting = false
				e.mu.Unlock()
				return
			}
			attempt := e.restartCount + 1
			e.mu.Unlock()

			if !e.waitBackoff(restartDelay(policy, attempt)) {
				return
			}
			candidate, manifest, err := e.startProcess(api)
			if err != nil {
				e.mu.Lock()
				e.lastError = err.Error()
				e.mu.Unlock()
				continue
			}
			if !reflect.DeepEqual(manifest, expected) {
				terminateRun(candidate)
				e.mu.Lock()
				e.restarting = false
				e.lastError = "restarted process exposed a different manifest"
				e.mu.Unlock()
				return
			}
			e.mu.Lock()
			if e.stopped {
				e.restarting = false
				e.mu.Unlock()
				terminateRun(candidate)
				return
			}
			if e.restartCount >= policy.MaxRestarts {
				e.restarting = false
				e.mu.Unlock()
				terminateRun(candidate)
				return
			}
			next = candidate
			e.run = candidate
			e.restartCount++
			e.restarting = false
			e.lastError = ""
			e.mu.Unlock()
			candidate.accept()
			break
		}
		if next == nil {
			e.mu.Lock()
			e.restarting = false
			if e.lastError == "" {
				e.lastError = fmt.Sprintf(
					"restart attempts exhausted after %d tries", policy.MaxRestarts)
			}
			e.mu.Unlock()
			return
		}
		run = next
	}
}

func restartLimitError(limit int, exit string) string {
	return fmt.Sprintf("restart limit %d reached after %s", limit, exit)
}

func restartDelay(policy RestartPolicy, attempt int) time.Duration {
	delay := policy.Backoff
	for i := 1; i < attempt; i++ {
		if delay >= policy.MaxBackoff/2 {
			return policy.MaxBackoff
		}
		delay *= 2
	}
	if delay > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return delay
}

func (e *ProcessExtension) waitBackoff(delay time.Duration) bool {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// terminateRun gives one child a bounded chance to exit gracefully,
// then kills it. cmd.Wait is owned by the process-run waiter goroutine;
// this function only waits for that goroutine.
func terminateRun(run *processRun) {
	if run == nil {
		return
	}
	if run.conn != nil {
		_ = run.conn.notify(MethodShutdown, nil)
		run.conn.close()
	}
	select {
	case <-run.done:
		return
	case <-time.After(2 * time.Second):
		if run.cmd != nil && run.cmd.Process != nil {
			_ = run.cmd.Process.Kill()
		}
		<-run.done
	}
}
