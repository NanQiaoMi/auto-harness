package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NanQiaoMi/auto-harness/sandbox"
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
// treated as one that cannot vouch for the operation.
const consultTimeout = 5 * time.Second

// Plugin capability names. A process extension with a nil Capabilities
// slice keeps the legacy behavior and may use every surface.
const (
	CapabilityTools  = "tools"
	CapabilityHooks  = "hooks"
	CapabilityEvents = "events"
	// CapabilityCommands lets the extension offer slash commands. It is
	// separate from tools because a command is run by the user, not by
	// the model, and it is the user's own typing that a command sees.
	CapabilityCommands = "commands"
)

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
	// Capabilities limits the surfaces the runtime manifest may use.
	// nil means unrestricted (legacy plugins); a non-nil list is an
	// explicit grant.
	Capabilities []string
	// RestartPolicy optionally supervises the child process after it
	// exits. The zero value preserves the historical no-restart
	// behavior.
	RestartPolicy RestartPolicy
	// Sandbox confines the child process with the operating-system
	// sandbox. nil runs the child with the harness's own access.
	//
	// This is confinement, not a capability check: a capability decides
	// which surfaces the extension may use inside the protocol, while
	// the sandbox decides what the process can touch once it runs. An
	// extension that is otherwise well behaved can still read the
	// user's SSH keys, and only the kernel can stop that.
	Sandbox *sandbox.Profile
	// RequireSandbox turns "this platform has no sandbox" into a load
	// error instead of running the child unconfined. It is the honest
	// setting for anything the user asked to be confined.
	RequireSandbox bool

	mu     sync.Mutex
	run    *processRun
	ctx    context.Context
	cancel context.CancelFunc
	// manifest describes the extension after the handshake.
	manifest Manifest
	// stopped marks the extension as shut down.
	stopped        bool
	supervisorDone chan struct{}
	restartCount   int
	restarting     bool
	lastExit       string
	lastError      string
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
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return fmt.Errorf("ext: %s: process was already stopped", e.Command)
	}
	if e.run != nil {
		e.mu.Unlock()
		return fmt.Errorf("ext: %s: process is already running", e.Command)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	e.cancel = cancel
	e.manifest = Manifest{}
	e.supervisorDone = nil
	e.restartCount = 0
	e.restarting = false
	e.lastExit = ""
	e.lastError = ""
	e.mu.Unlock()

	setupComplete := false
	defer func() {
		if !setupComplete {
			_ = e.Stop()
		}
	}()

	run, manifest, err := e.startProcess(api)
	if err != nil {
		return err
	}
	runAccepted := false
	defer func() {
		if !runAccepted {
			terminateRun(run)
		}
	}()
	if len(manifest.Tools) > 0 && !e.allows(CapabilityTools) {
		return fmt.Errorf("ext: %s: manifest provides tools without the %q capability", e.Command, CapabilityTools)
	}
	if len(manifest.Consults) > 0 && !e.allows(CapabilityHooks) {
		return fmt.Errorf("ext: %s: manifest consults events without the %q capability", e.Command, CapabilityHooks)
	}
	if len(manifest.Commands) > 0 && !e.allows(CapabilityCommands) {
		return fmt.Errorf("ext: %s: manifest provides commands without the %q capability", e.Command, CapabilityCommands)
	}
	e.mu.Lock()
	e.manifest = manifest
	e.run = run
	e.mu.Unlock()

	// Register the remote tools as local tools.
	for _, mt := range manifest.Tools {
		tool := e.remoteTool(mt)
		if err := api.RegisterTool(tool); err != nil {
			return fmt.Errorf("ext: %s: register tool %q: %w", e.Command, mt.Name, err)
		}
	}

	// Register the remote commands as local slash commands.
	for _, mc := range manifest.Commands {
		if mc.Name == "" {
			return fmt.Errorf("ext: %s: a command needs a name", e.Command)
		}
		cmd := e.remoteCommand(mc)
		api.RegisterCommand(mc.Name, cmd)
	}

	// The extension may veto the events it is consulted about. The
	// handler sits on the bus like any other, so the veto travels the
	// same path an in-process extension's would.
	//
	// These are the events the harness asks about: a tool call, a
	// shell command, the prompt a run is about to send, and the
	// session operations that replace or move the conversation. They
	// are the places where the decision belongs to the extension — one
	// can only say no, and the prompt can also be rewritten.
	api.On(EventToolCall, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventUserBash, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventInput, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventSessionBeforeCompact, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventSessionBeforeSwitch, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventSessionBeforeFork, func(ev *Event) error {
		e.consult(ev)
		return nil
	})
	api.On(EventSessionBeforeTree, func(ev *Event) error {
		e.consult(ev)
		return nil
	})

	done := make(chan struct{})
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return fmt.Errorf("ext: %s: stopped during setup", e.Command)
	}
	e.supervisorDone = done
	e.mu.Unlock()
	run.accept()
	go e.supervise(api, manifest)
	runAccepted = true
	setupComplete = true
	return nil
}

// consult asks the extension about an event and applies its answer.
//
// The question goes out as a request, so the extension can answer
// before the call runs.
//
// The policy is fail-closed, and it is the same one pi documents for
// its own gate handlers: an extension that cannot make a safety
// decision has not made one, so the operation does not happen. An
// extension that does not answer, answers with an error, or times out
// blocks the operation. A broken policy extension must not become an
// open door.
//
// input is the one exception, and it is not a gate: a rewrite that
// cannot be computed has a safe default, the line the user typed. So
// silence leaves the prompt alone instead of swallowing it. Failing
// closed there would turn a broken extension into a harness that eats
// every message.
//
// Two things keep this from being as sharp as pi's. The wait is
// bounded (pi's in-process handler can hang the harness indefinitely),
// and the extension must have opted in: an extension whose manifest
// does not name the event is never asked about it, so a policy
// extension that predates this channel is not treated as failing.
func (e *ProcessExtension) consult(ev *Event) {
	if !e.consults(ev.Type) {
		return
	}
	conn := e.connection()
	if conn == nil {
		// input has a safe default; every gate fails closed while the
		// process is down or being restarted.
		if ev.Type == EventInput {
			return
		}
		e.refuse(ev, "the policy extension process is not running")
		return
	}
	params := ConsultParams{
		Event:     string(ev.Type),
		Text:      ev.Text,
		Reason:    ev.Reason,
		SessionID: ev.SessionID,
		EntryID:   ev.EntryID,
		Data:      ev.Data,
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
		// input is a rewrite, not a gate: the safe default is the
		// line the user typed, so a silent extension changes nothing.
		if ev.Type == EventInput {
			return
		}
		// Otherwise the extension could not vouch for the operation,
		// so the operation does not happen.
		e.refuse(ev, "the policy extension did not answer: "+err.Error())
		return
	}
	if out.Text != nil {
		ev.Text = *out.Text
	}
	if out.Block {
		e.refuse(ev, out.Reason)
		return
	}
	if out.Cancel {
		ev.Cancel = true
		// Keep the extension's explanation: the caller reports it to
		// the user in place of the generic "cancelled by an extension".
		if out.Reason != "" && ev.BlockReason == "" {
			ev.BlockReason = out.Reason
		}
	}
}

// consults reports whether the extension asked to be consulted about
// the given event.
func (e *ProcessExtension) consults(t EventType) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range e.manifest.Consults {
		if name == string(t) {
			return true
		}
	}
	return false
}

// refuse stops the operation the event guards.
//
// The veto mechanisms are not the same: a tool call and a shell command
// are blocked (the reason is reported back to the model), while
// compaction, the prompt and the session operations are cancelled (in
// none of them is a model waiting for a reason). The reason is kept on
// the event either way.
func (e *ProcessExtension) refuse(ev *Event, reason string) {
	switch ev.Type {
	case EventSessionBeforeCompact, EventInput,
		EventSessionBeforeSwitch, EventSessionBeforeFork, EventSessionBeforeTree:
		ev.Cancel = true
		ev.BlockReason = reason
		return
	}
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
		Retryable:   mt.Retryable,
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			conn := e.connection()
			if conn == nil {
				// A transport failure is an error, not an answer: the
				// extension never saw the call, so a retryable tool may
				// be tried again once the process is back.
				return tools.Result{}, fmt.Errorf("extension process is not running")
			}
			var out ToolResult
			err := conn.call(MethodToolExecute, ExecParams{
				ToolCallID: callID,
				Name:       mt.Name,
				Arguments:  args,
			}, &out)
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Content: out.Content, IsError: out.IsError}, nil
		},
	}
}

// remoteCommand wraps a remote slash command so running it proxies to
// the child.
//
// The command is the user's own action, not the model's: it runs with
// the arguments the user typed, and whatever the extension returns is
// printed for them. It never touches the conversation, which is what
// distinguishes a command from a tool.
func (e *ProcessExtension) remoteCommand(mc ManifestCommand) *Command {
	return &Command{
		Description: mc.Description,
		Handler: func(ctx context.Context, args string) error {
			conn := e.connection()
			if conn == nil {
				return fmt.Errorf("extension process is not running")
			}
			var out struct {
				Text string `json:"text"`
			}
			if err := conn.call(MethodCommandExecute, CommandParams{Name: mc.Name, Args: args}, &out); err != nil {
				return err
			}
			if out.Text != "" {
				fmt.Print(out.Text)
				if !strings.HasSuffix(out.Text, "\n") {
					fmt.Println()
				}
			}
			return nil
		},
	}
}

// handleNotification forwards an extension event to the host bus.
func (e *ProcessExtension) handleNotification(api *API, msg Protocol) {
	if msg.Method != MethodEvent {
		return
	}
	if !e.allows(CapabilityEvents) {
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

func (e *ProcessExtension) allows(capability string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Capabilities == nil {
		return true
	}
	for _, allowed := range e.Capabilities {
		if allowed == capability {
			return true
		}
	}
	return false
}

// Sandboxed reports whether this extension asked to be confined.
//
// It says what was configured, not whether the platform can honour it;
// startProcess reports the second.
func (e *ProcessExtension) Sandboxed() bool {
	return e.Sandbox != nil
}

// SandboxProfile returns the profile the child is confined with, with
// the defaults filled in, or nil when it runs unconfined.
func (e *ProcessExtension) SandboxProfile() *sandbox.Profile {
	return e.sandboxProfile()
}

// sandboxProfile returns the profile to confine the child with, or nil
// when it runs unconfined.
//
// A profile with no write path would deny every write, which is not what
// "confine this extension" should mean: the child needs its own
// directory to be writable, or it cannot even load its own code. So a
// configured profile that names no writable directory gets the plugin
// directory, and one that names none at all gets the default credential
// denials.
func (e *ProcessExtension) sandboxProfile() *sandbox.Profile {
	if e.Sandbox == nil {
		return nil
	}
	p := *e.Sandbox
	if len(p.DenyReadPaths) == 0 {
		p.DenyReadPaths = sandbox.DefaultDenyReadPaths()
	}
	if len(p.WritePaths) == 0 && e.Dir != "" {
		p.WritePaths = []string{e.Dir}
	}
	return &p
}

// connection returns the live connection, if any.
func (e *ProcessExtension) connection() *rpcConn {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || e.run == nil {
		return nil
	}
	return e.run.conn
}

// Stop shuts the extension process down: it asks the child to exit and
// kills it if it does not.
func (e *ProcessExtension) Stop() error {
	e.mu.Lock()
	run := e.run
	e.run = nil
	cancel := e.cancel
	supervisorDone := e.supervisorDone
	e.restarting = false
	if e.stopped {
		e.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		waitSupervisor(supervisorDone)
		return nil
	}
	e.stopped = true
	e.mu.Unlock()

	if run != nil {
		terminateRun(run)
	}
	if cancel != nil {
		cancel()
	}
	waitSupervisor(supervisorDone)
	return nil
}

func waitSupervisor(done chan struct{}) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// Teardown implements the Teardown interface, so the manager releases
// the child process when the extension is unloaded by a reload.
func (e *ProcessExtension) Teardown() error {
	return e.Stop()
}
