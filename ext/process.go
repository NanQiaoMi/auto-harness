package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/NanQiaoMi/auto-harness/tools"
)

// handshakeTimeout bounds the manifest handshake with an extension
// process.
const handshakeTimeout = 10 * time.Second

// consultTimeout bounds how long the harness waits for an extension's
// answer to an event it is consulted about.
//
// It is short on purpose: the answer decides whether a tool call runs
// now, and a model is waiting. An extension that is slow to answer is
// treated as one that does not object.
const consultTimeout = 5 * time.Second

// ProcessExtension runs an external extension process and bridges it into
// the harness: the tools it advertises become regular tools, the events
// it pushes are dispatched on the host bus, and the events it is
// consulted about can be blocked by its answer.
//
// This is how extensions are written in any language: speak the JSON-RPC
// protocol over stdin/stdout.
type ProcessExtension struct {
	// Command is the executable to run.
	Command string
	// Args are passed to the executable.
	Args []string
	// Dir is the working directory of the process.
	Dir string
	// Env adds environment variables to the child process.
	Env []string

	mu   sync.Mutex
	conn *rpcConn
	cmd  *exec.Cmd
	// manifest describes the extension after the handshake.
	manifest Manifest
	// stopped marks the extension as shut down.
	stopped bool
}

// NewProcessExtension creates a process extension handle.
func NewProcessExtension(command string, args ...string) *ProcessExtension {
	return &ProcessExtension{Command: command, Args: args}
}

// Name implements Extension.
func (e *ProcessExtension) Name() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.manifest.Name != "" {
		return e.manifest.Name
	}
	return e.Command
}

// Manifest returns the extension's manifest (after the handshake).
func (e *ProcessExtension) Manifest() Manifest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.manifest
}

// Setup implements Extension: it starts the process, performs the
// manifest handshake and registers the advertised tools.
func (e *ProcessExtension) Setup(api *API) error {
	cmd := exec.Command(e.Command, e.Args...)
	if e.Dir != "" {
		cmd.Dir = e.Dir
	}
	if len(e.Env) > 0 {
		cmd.Env = append(cmd.Environ(), e.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ext: %s: stdin pipe: %w", e.Command, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ext: %s: stdout pipe: %w", e.Command, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ext: %s: start: %w", e.Command, err)
	}

	conn := newRPCConn(stdout, stdin, stdin)
	e.mu.Lock()
	e.conn = conn
	e.cmd = cmd
	e.mu.Unlock()

	go conn.readLoop(func(msg Protocol) {
		e.handleNotification(api, msg)
	})

	// Handshake: ask for the manifest. A short timeout keeps a broken
	// extension from hanging the harness.
	var manifest Manifest
	if err := conn.callTimeout(MethodManifest, nil, &manifest, handshakeTimeout); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("ext: %s: manifest: %w", e.Command, err)
	}
	if manifest.Name == "" {
		manifest.Name = e.Command
	}
	e.mu.Lock()
	e.manifest = manifest
	e.mu.Unlock()

	// Register the remote tools as local tools.
	for _, mt := range manifest.Tools {
		tool := e.remoteTool(mt)
		if err := api.RegisterTool(tool); err != nil {
			return fmt.Errorf("ext: %s: register tool %q: %w", e.Command, mt.Name, err)
		}
	}

	// The extension may veto the events it is consulted about. The
	// handler sits on the bus like any other, so the veto travels the
	// same path an in-process extension's would.
	//
	// The three events are the ones the harness asks about: a tool
	// call, a shell command, and the moment before the history is
	// compacted. They are the three the kernel acts on a decision
	// about, so they are the three worth asking.
	api.On(EventToolCall, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventUserBash, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventSessionBeforeCompact, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	return nil
}

// consult asks the extension about an event and applies its answer.
//
// The question goes out as a request, so the extension can answer
// before the call runs. An extension that does not answer, or answers
// with an error, lets the call through: a broken extension must not
// freeze the harness, and blocking everything would be worse than
// letting one call by.
func (e *ProcessExtension) consult(ev *Event) {
	conn := e.connection()
	if conn == nil {
		return
	}
	params := ConsultParams{
		Event: string(ev.Type),
		Text:  ev.Text,
		Data:  ev.Data,
	}
	if ev.ToolCall != nil {
		params.ToolCall = &ConsultToolCall{
			ID:        ev.ToolCall.ID,
			Name:      ev.ToolCall.Name,
			Arguments: ev.ToolCall.Arguments,
		}
	}
	if ev.BashCommand != "" {
		params.BashCommand = ev.BashCommand
	}
	var out ConsultResult
	if err := conn.callTimeout(MethodConsult, params, &out, consultTimeout); err != nil {
		return
	}
	if out.Cancel {
		ev.Cancel = true
	}
	if !out.Block {
		return
	}
	reason := out.Reason
	if reason == "" {
		reason = "blocked by an extension"
	}
	ev.Block = true
	ev.BlockReason = reason
}

// remoteTool wraps a remote tool so calling it proxies to the child.
func (e *ProcessExtension) remoteTool(mt ManifestTool) *tools.Tool {
	return &tools.Tool{
		Name:        mt.Name,
		Label:       mt.Label,
		Description: mt.Description,
		Parameters:  mt.Parameters,
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			conn := e.connection()
			if conn == nil {
				return tools.Result{Content: "extension process is not running", IsError: true}, nil
			}
			var out ToolResult
			err := conn.call(MethodToolExecute, ExecParams{
				ToolCallID: callID,
				Name:       mt.Name,
				Arguments:  args,
			}, &out)
			if err != nil {
				return tools.Result{Content: err.Error(), IsError: true}, nil
			}
			return tools.Result{Content: out.Content, IsError: out.IsError}, nil
		},
	}
}

// handleNotification forwards an extension event to the host bus.
func (e *ProcessExtension) handleNotification(api *API, msg Protocol) {
	if msg.Method != MethodEvent {
		return
	}
	var params EventParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}
	api.Bus.Emit(&Event{
		Type: EventType(params.Event),
		Text: params.Text,
		Data: params.Data,
	})
}

// connection returns the live connection, if any.
func (e *ProcessExtension) connection() *rpcConn {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	return e.conn
}

// Stop shuts the extension process down: it asks the child to exit and
// kills it if it does not.
func (e *ProcessExtension) Stop() error {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return nil
	}
	e.stopped = true
	conn := e.conn
	cmd := e.cmd
	e.mu.Unlock()

	if conn != nil {
		// Give the child a chance to clean up; closing the pipe makes the
		// child's stdin hit EOF so it can exit on its own.
		_ = conn.notify(MethodShutdown, nil)
		conn.close()
	}
	if cmd != nil && cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			return nil
		case <-time.After(2 * time.Second):
			// The child ignored the shutdown request; kill it.
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return nil
}

// Teardown implements the Teardown interface, so the manager releases
// the child process when the extension is unloaded by a reload.
func (e *ProcessExtension) Teardown() error {
	return e.Stop()
}
