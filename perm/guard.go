package perm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
)

// Approver decides whether a call that needs confirmation may run.
//
// It returns true to allow the call. Implementations that cannot ask a
// user (a non-interactive run) must return false, so the call is
// blocked rather than silently allowed.
type Approver interface {
	Approve(ctx context.Context, call Call, rule *Rule) (bool, error)
}

// ApproverFunc adapts a function into an Approver.
type ApproverFunc func(ctx context.Context, call Call, rule *Rule) (bool, error)

func (f ApproverFunc) Approve(ctx context.Context, call Call, rule *Rule) (bool, error) {
	return f(ctx, call, rule)
}

// DenyApprover blocks every call that needs confirmation. It is the
// right default when there is no user to ask.
type DenyApprover struct {
	// Reason explains the denial.
	Reason string
}

// Approve always denies.
func (d DenyApprover) Approve(ctx context.Context, call Call, rule *Rule) (bool, error) {
	reason := d.Reason
	if reason == "" {
		reason = "no interactive session to confirm the call"
	}
	return false, nil
}

// PromptApprover asks the user on the terminal.
type PromptApprover struct {
	// In is where the answer is read from.
	In io.Reader
	// Out is where the question is written.
	Out io.Writer
	// Workdir is the directory paths are resolved against.
	Workdir string
	// Lines reads the answer when the caller shares its input stream
	// with the REPL. It takes precedence over In.
	Lines LineReader
}

// Approve asks the user to confirm the call.
func (p *PromptApprover) Approve(ctx context.Context, call Call, rule *Rule) (bool, error) {
	out := p.Out
	if out == nil {
		out = os.Stderr
	}
	if p.Workdir == "" {
		p.Workdir = "."
	}

	fmt.Fprintf(out, "\n⚠️  permission required: %s\n", call.Tool)
	if call.Path != "" {
		fmt.Fprintf(out, "    path: %s\n", call.Path)
	}
	if cmd, ok := call.Args["command"].(string); ok {
		fmt.Fprintf(out, "    command: %s\n", cmd)
	}
	if gitArgs, ok := call.Args["args"]; ok {
		if encoded, err := json.Marshal(gitArgs); err == nil {
			fmt.Fprintf(out, "    git args: %s\n", encoded)
		}
	}
	if action, ok := call.Args["action"].(string); ok {
		fmt.Fprintf(out, "    action: %s\n", action)
	}
	if rule != nil && rule.Reason != "" {
		fmt.Fprintf(out, "    reason: %s\n", rule.Reason)
	}
	fmt.Fprint(out, "    allow? [y/N] ")

	answer, ok := p.readAnswer()
	if !ok {
		return false, nil
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// readAnswer reads one line of input.
func (p *PromptApprover) readAnswer() (string, bool) {
	if p.Lines != nil {
		return p.Lines.ReadLine()
	}
	in := p.In
	if in == nil {
		in = os.Stdin
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		return "", false
	}
	return scanner.Text(), true
}

// Guard enforces a rule set on tool calls.
//
// It installs itself as a tool_call hook: the kernel has no permission
// system of its own (pi's design), so the guard is an extension that
// observes and blocks.
type Guard struct {
	// Set holds the rules.
	Set *Set
	// Approver resolves calls that need confirmation.
	Approver Approver
	// Workdir is the directory paths are resolved against.
	Workdir string
	// Logf receives a line for every decision.
	Logf func(format string, args ...any)

	mu sync.Mutex
	// allowed counts the calls that ran.
	allowed int
	// blocked counts the calls that were stopped.
	blocked int
	// asked counts the calls that needed confirmation.
	asked int

	// askMu serializes the questions.
	//
	// Read-only tool calls run in parallel, so two of them can reach
	// the tool_call hook at once. A question asks the user on the one
	// terminal they share, and both would read from the same line
	// reader: the second would take the answer meant for the first.
	askMu sync.Mutex
}

// NewGuard creates a guard over a rule set.
func NewGuard(set *Set, workdir string) *Guard {
	return &Guard{
		Set:      set,
		Workdir:  workdir,
		Approver: DenyApprover{},
	}
}

// Setup installs the guard's tool_call hook.
func (g *Guard) Setup(api *ext.API) error {
	api.On(ext.EventToolCall, func(e *ext.Event) error {
		if e.ToolCall == nil {
			return nil
		}
		// A call another extension already blocked needs no decision
		// here: asking the user about a call that is not going to run
		// is noise.
		if e.Block {
			return nil
		}
		g.check(e)
		return nil
	})
	return nil
}

// Name implements ext.Extension.
func (g *Guard) Name() string { return "permissions" }

// check evaluates the call and blocks it when the rules say so.
func (g *Guard) check(e *ext.Event) {
	call := Call{
		Tool: e.ToolCall.Name,
		Args: decodeArgs(e.ToolCall.Arguments),
	}
	workdir := g.Workdir
	if workspace := ext.Workspace(e); workspace != "" {
		workdir = workspace
	}
	// The target path, when the tool has one.
	if raw, ok := call.Args["path"].(string); ok {
		call.Path = NormalizePath(raw, workdir)
	}

	decision, rule := g.Set.Evaluate(call)

	switch decision {
	case DecisionAllow:
		g.count(&g.allowed)
		g.log("allowed %s", call.Tool)
		return
	case DecisionDeny:
		g.count(&g.blocked)
		reason := "blocked by permission rules"
		if rule != nil && rule.Reason != "" {
			reason = rule.Reason
		}
		e.Block = true
		e.BlockReason = reason
		g.log("denied %s: %s", call.Tool, reason)
		return
	}

	// The call needs confirmation.
	g.count(&g.asked)
	approver := g.Approver
	if ext.AgentID(e) != "" {
		// A delegated agent runs in the background. Prompting on the
		// primary terminal would interleave with the REPL, so a call
		// that was not pre-approved fails closed.
		approver = DenyApprover{
			Reason: "delegated agents require pre-approved permission rules",
		}
	}
	if approver == nil {
		approver = DenyApprover{}
	}
	// One question at a time: the approver reads the terminal, and a
	// second question would take the first one's answer.
	g.askMu.Lock()
	allowed, err := approver.Approve(context.Background(), call, rule)
	g.askMu.Unlock()
	if err != nil {
		e.Block = true
		e.BlockReason = "permission check failed: " + err.Error()
		g.count(&g.blocked)
		return
	}
	if !allowed {
		e.Block = true
		e.BlockReason = approvalDenialReason(approver)
		g.count(&g.blocked)
		g.log("declined %s", call.Tool)
		return
	}
	g.count(&g.allowed)
	g.log("approved %s", call.Tool)
}

func approvalDenialReason(approver Approver) string {
	switch deny := approver.(type) {
	case DenyApprover:
		if deny.Reason != "" {
			return deny.Reason
		}
	case *DenyApprover:
		if deny != nil && deny.Reason != "" {
			return deny.Reason
		}
	}
	return "the user did not approve the call"
}

// count increments a counter.
func (g *Guard) count(counter *int) {
	g.mu.Lock()
	*counter++
	g.mu.Unlock()
}

// log writes a decision line.
func (g *Guard) log(format string, args ...any) {
	if g.Logf == nil {
		return
	}
	g.Logf(format, args...)
}

// Stats reports how many calls ran, were blocked and needed
// confirmation.
func (g *Guard) Stats() (allowed, blocked, asked int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allowed, g.blocked, g.asked
}

// decodeArgs parses a tool's raw arguments.
func decodeArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return map[string]any{}
	}
	return args
}

// SetPathArg overrides the argument that carries the target path. It
// lets a custom tool report its path to the permission rules.
func SetPathArg(args map[string]any, path string) {
	if args == nil {
		return
	}
	args["path"] = path
}

// PathOf returns the target path of a tool call, if it has one.
func PathOf(call *llm.ToolCall) string {
	if call == nil {
		return ""
	}
	args := decodeArgs(call.Arguments)
	if p, ok := args["path"].(string); ok {
		return p
	}
	return ""
}

// DefaultPathArgs lists the argument names that carry a target path in
// the built-in tools.
var DefaultPathArgs = []string{"path", "file", "filename"}

// NormalizeWorkdir returns the working directory to resolve paths
// against.
func NormalizeWorkdir(dir string) string {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	return filepath.Clean(dir)
}
