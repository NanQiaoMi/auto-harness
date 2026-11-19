package perm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestEvaluateFirstMatchWins verifies the first matching rule decides.
func TestEvaluateFirstMatchWins(t *testing.T) {
	set := NewSet(ActionDeny)
	set.Rules = []Rule{
		{Name: "allow reads", Tools: []string{"read"}, Action: ActionAllow},
		{Name: "deny writes", Tools: []string{"write"}, Action: ActionDeny},
	}

	decision, rule := set.Evaluate(Call{Tool: "read"})
	if decision != DecisionAllow {
		t.Errorf("decision = %s, want allow", decision)
	}
	if rule == nil || rule.Name != "allow reads" {
		t.Errorf("rule = %+v, want the read rule", rule)
	}

	decision, rule = set.Evaluate(Call{Tool: "write"})
	if decision != DecisionDeny {
		t.Errorf("decision = %s, want deny", decision)
	}
	if rule == nil || rule.Name != "deny writes" {
		t.Errorf("rule = %+v, want the write rule", rule)
	}

	// An unmatched call falls back to the default.
	decision, rule = set.Evaluate(Call{Tool: "bash"})
	if decision != DecisionDeny {
		t.Errorf("decision = %s, want deny (the default)", decision)
	}
	if rule != nil {
		t.Errorf("rule = %+v, want none for the default", rule)
	}
}

// TestBuiltinDenyIsAFloor verifies the built-in deny rules cannot be
// lifted by an allow rule elsewhere in the set.
func TestBuiltinDenyIsAFloor(t *testing.T) {
	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "allow everything", Tools: []string{"bash"}, Action: ActionAllow},
	}
	set.Rules = append(set.Rules, Builtin().Rules...)

	d, rule := set.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "rm -rf /"}})
	if d != DecisionDeny {
		t.Fatalf("decision = %s, want deny", d)
	}
	if rule == nil || rule.Name != "destructive shell commands" {
		t.Errorf("rule = %+v, want the builtin deny rule", rule)
	}

	// A harmless command still hits the user's allow rule.
	d, _ = set.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "ls"}})
	if d != DecisionAllow {
		t.Errorf("decision = %s, want allow", d)
	}
}

// TestUserRuleOverridesBuiltinDefault verifies the ordering of the
// sources: the user's rules come first, the built-in defaults last.
func TestUserRuleOverridesBuiltinDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")

	user := &Set{Default: ActionAsk, Rules: []Rule{
		{Name: "allow my shell", Tools: []string{"bash"}, Action: ActionAllow},
	}}
	if err := SaveFile(path, user); err != nil {
		t.Fatalf("save: %v", err)
	}
	merged, err := LoadConfig(path, "", false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d, rule := merged.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "ls"}})
	if d != DecisionAllow {
		t.Fatalf("decision = %s, want allow", d)
	}
	if rule == nil || rule.Name != "allow my shell" {
		t.Errorf("rule = %+v, want the user's rule", rule)
	}
}

// TestToolPatternMatching verifies the tool name patterns.
func TestToolPatternMatching(t *testing.T) {
	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "mcp", Tools: []string{"mcp__*"}, Action: ActionDeny},
	}
	if d, _ := set.Evaluate(Call{Tool: "mcp__docs__search"}); d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
	if d, _ := set.Evaluate(Call{Tool: "read"}); d != DecisionAllow {
		t.Errorf("decision = %s, want allow", d)
	}
}

// TestArgumentPatternMatching verifies the argument patterns.
func TestArgumentPatternMatching(t *testing.T) {
	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "rm -rf", Tools: []string{"bash"},
			Args:   map[string]string{"command": `(?i)\brm\s+-[rf]`},
			Action: ActionDeny},
	}
	if d, _ := set.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "rm -rf /"}}); d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
	if d, _ := set.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "ls -la"}}); d != DecisionAllow {
		t.Errorf("decision = %s, want allow", d)
	}
	// A rule with an argument pattern needs that argument to be present.
	if d, _ := set.Evaluate(Call{Tool: "bash"}); d != DecisionAllow {
		t.Errorf("decision = %s, want allow (no command argument)", d)
	}
}

// TestPathPatternMatching verifies the path patterns.
func TestPathPatternMatching(t *testing.T) {
	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "protected", Tools: []string{"write", "edit"},
			Paths:  []string{`(^|/)\.git/`},
			Action: ActionDeny},
	}
	if d, _ := set.Evaluate(Call{Tool: "write", Path: "/repo/.git/config"}); d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
	if d, _ := set.Evaluate(Call{Tool: "write", Path: "/repo/src/main.go"}); d != DecisionAllow {
		t.Errorf("decision = %s, want allow", d)
	}
	// A rule with path patterns needs a path.
	if d, _ := set.Evaluate(Call{Tool: "write"}); d != DecisionAllow {
		t.Errorf("decision = %s, want allow (no path)", d)
	}
}

// TestGuardBlocksDeniedCall verifies the guard blocks a denied call
// through the tool_call hook.
func TestGuardBlocksDeniedCall(t *testing.T) {
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	api := ext.NewAPI(bus, reg, ext.NewCommandRegistry())

	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "deny rm", Tools: []string{"bash"},
			Args:   map[string]string{"command": "rm"},
			Action: ActionDeny, Reason: "removal is not allowed"},
	}

	guard := NewGuard(set, t.TempDir())
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ev := &ext.Event{
		Type:     ext.EventToolCall,
		ToolCall: &llm.ToolCall{Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf /"}`)},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("the call was not blocked")
	}
	if ev.BlockReason != "removal is not allowed" {
		t.Errorf("reason = %q", ev.BlockReason)
	}

	allowed, blocked, asked := guard.Stats()
	if allowed != 0 || blocked != 1 || asked != 0 {
		t.Errorf("stats = %d/%d/%d, want 0/1/0", allowed, blocked, asked)
	}
}

// TestGuardAsksAndBlocksWithoutApprover verifies the fail-safe
// behaviour: a call needing confirmation is blocked when there is
// nobody to ask.
func TestGuardAsksAndBlocksWithoutApprover(t *testing.T) {
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())

	set := NewSet(ActionAsk)
	guard := NewGuard(set, t.TempDir())
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ev := &ext.Event{
		Type:     ext.EventToolCall,
		ToolCall: &llm.ToolCall{Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("the call was not blocked without an approver")
	}
	_, blocked, asked := guard.Stats()
	if blocked != 1 || asked != 1 {
		t.Errorf("stats: blocked = %d, asked = %d, want 1/1", blocked, asked)
	}
}

// TestGuardApprovesWithApprover verifies an approving approver lets the
// call through.
func TestGuardApprovesWithApprover(t *testing.T) {
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())

	guard := NewGuard(NewSet(ActionAsk), t.TempDir())
	guard.Approver = ApproverFunc(func(ctx context.Context, call Call, rule *Rule) (bool, error) {
		return true, nil
	})
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ev := &ext.Event{
		Type:     ext.EventToolCall,
		ToolCall: &llm.ToolCall{Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ev.Block {
		t.Error("the call was blocked despite approval")
	}
}

// TestGuardAsksOneCallAtATime checks that parallel read-only calls do
// not ask two questions at once.
//
// They share one terminal, so a second question would read the answer
// meant for the first. The approver here counts the calls in flight and
// fails if it ever sees two.
func TestGuardAsksOneCallAtATime(t *testing.T) {
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())

	var inFlight, peak int
	var mu sync.Mutex
	guard := NewGuard(NewSet(ActionAsk), t.TempDir())
	guard.Approver = ApproverFunc(func(ctx context.Context, call Call, rule *Rule) (bool, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return true, nil
	})
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := &ext.Event{
				Type:     ext.EventToolCall,
				ToolCall: &llm.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)},
			}
			_ = bus.Emit(ev)
			if ev.Block {
				t.Error("the call was blocked despite approval")
			}
		}()
	}
	wg.Wait()
	if peak > 1 {
		t.Errorf("%d questions were asked at once, want 1", peak)
	}
}

// TestGuardNormalizesPaths verifies the target path is resolved before
// the rules see it.
func TestGuardNormalizesPaths(t *testing.T) {
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())

	workdir := t.TempDir()
	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "deny outside", Tools: []string{"write"},
			Paths:  []string{`/etc/`},
			Action: ActionDeny},
	}
	guard := NewGuard(set, workdir)
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ev := &ext.Event{
		Type:     ext.EventToolCall,
		ToolCall: &llm.ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"../../etc/passwd"}`)},
	}
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Error("a write outside the working directory was not blocked")
	}
}

// TestGuardResolvesDelegatedPathsAgainstWorkspace verifies a
// subagent's relative path is checked in its own worktree, not the
// parent process working directory.
func TestGuardResolvesDelegatedPathsAgainstWorkspace(t *testing.T) {
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())

	parentWorkdir := t.TempDir()
	workspace := t.TempDir()
	set := NewSet(ActionAllow)
	set.Rules = []Rule{
		{Name: "deny subagent secret", Tools: []string{"write"},
			Paths:  []string{filepath.Join(workspace, "secret") + "*"},
			Action: ActionDeny},
	}
	guard := NewGuard(set, parentWorkdir)
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ev := &ext.Event{
		Type:     ext.EventToolCall,
		ToolCall: &llm.ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"secret/report.txt"}`)},
	}
	ext.SetDelegation(ev, "sub-1", "", workspace)
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("relative delegated path was not resolved against its workspace")
	}
}

// TestGuardDelegatedAskFailsClosed verifies that a background agent
// cannot borrow the primary terminal approver.
func TestGuardDelegatedAskFailsClosed(t *testing.T) {
	bus := ext.NewBus()
	api := ext.NewAPI(bus, tools.NewRegistry(), ext.NewCommandRegistry())

	approverCalled := false
	guard := NewGuard(NewSet(ActionAsk), t.TempDir())
	guard.Approver = ApproverFunc(func(context.Context, Call, *Rule) (bool, error) {
		approverCalled = true
		return true, nil
	})
	if err := guard.Setup(api); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ev := &ext.Event{
		Type:     ext.EventToolCall,
		ToolCall: &llm.ToolCall{Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
	}
	ext.SetDelegation(ev, "sub-1", "", t.TempDir())
	if err := bus.Emit(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !ev.Block {
		t.Fatal("delegated call that needs confirmation was not blocked")
	}
	if approverCalled {
		t.Fatal("delegated call invoked the primary terminal approver")
	}
	if !strings.Contains(ev.BlockReason, "pre-approved") {
		t.Fatalf("block reason = %q", ev.BlockReason)
	}
}

// TestBuiltinRulesAreConservative verifies the default rules deny the
// clearly destructive operations and allow reading.
func TestBuiltinRulesAreConservative(t *testing.T) {
	set := Builtin()

	cases := []struct {
		call Call
		want Decision
	}{
		{Call{Tool: "read", Args: map[string]any{"path": "/tmp/x"}}, DecisionAllow},
		{Call{Tool: "bash", Args: map[string]any{"command": "rm -rf /"}}, DecisionDeny},
		{Call{Tool: "bash", Args: map[string]any{"command": "sudo apt install x"}}, DecisionDeny},
		{Call{Tool: "write", Path: "/repo/.git/config"}, DecisionDeny},
		{Call{Tool: "write", Path: "/repo/main.go"}, DecisionAsk},
		{Call{Tool: "bash", Args: map[string]any{"command": "ls"}}, DecisionAsk},
		{Call{Tool: "git", Args: map[string]any{"args": []any{"status"}}}, DecisionAsk},
		{Call{Tool: "checkpoint", Args: map[string]any{"action": "list"}}, DecisionAllow},
		{Call{Tool: "checkpoint", Args: map[string]any{"action": "restore"}}, DecisionAsk},
	}
	for _, c := range cases {
		got, _ := set.Evaluate(c.call)
		if got != c.want {
			t.Errorf("%s: decision = %s, want %s", c.call.Tool, got, c.want)
		}
	}
}

// TestConfigRoundTrip verifies a rule set survives a save and load.
func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")

	set := NewSet(ActionDeny)
	set.Rules = []Rule{
		{Name: "allow read", Tools: []string{"read"}, Action: ActionAllow},
		{Name: "deny rm", Tools: []string{"bash"},
			Args:   map[string]string{"command": "rm"},
			Action: ActionDeny, Reason: "no removal"},
	}
	if err := SaveFile(path, set); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(loaded.Rules))
	}
	if loaded.Default != ActionDeny {
		t.Errorf("default = %s, want deny", loaded.Default)
	}
	d, rule := loaded.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "rm x"}})
	if d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
	if rule.Reason != "no removal" {
		t.Errorf("reason = %q", rule.Reason)
	}
}

// TestMergePrefersUserRules verifies the project file adds rules without
// overriding the user's first match.
func TestMergePrefersUserRules(t *testing.T) {
	user := &Set{Default: ActionAsk, Rules: []Rule{
		{Name: "allow read", Tools: []string{"read"}, Action: ActionAllow},
	}}
	project := &Set{Default: ActionDeny, Rules: []Rule{
		{Name: "allow read too", Tools: []string{"read"}, Action: ActionAllow},
	}}
	merged := Merge(user, project)

	d, rule := merged.Evaluate(Call{Tool: "read"})
	if d != DecisionAllow {
		t.Errorf("decision = %s, want allow", d)
	}
	if rule.Name != "allow read" {
		t.Errorf("rule = %q, want the user's rule", rule.Name)
	}
	if merged.Default != ActionDeny {
		t.Errorf("default = %s, want the project's deny", merged.Default)
	}
}

// TestFloorHoldsTheBuiltinDenies verifies the floor is only the deny
// rules.
func TestFloorHoldsTheBuiltinDenies(t *testing.T) {
	floor := Floor()
	if len(floor.Rules) == 0 {
		t.Fatal("the floor is empty")
	}
	for _, r := range floor.Rules {
		if r.Action != ActionDeny {
			t.Errorf("the floor holds a %s rule: %s", r.Action, r.Name)
		}
	}
	// The floor denies the destructive command.
	d, _ := floor.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "rm -rf /"}})
	if d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
}

// TestLoadConfigFallsBackToBuiltin verifies a missing configuration
// gives the built-in rules.
func TestLoadConfigFallsBackToBuiltin(t *testing.T) {
	set, err := LoadConfig(filepath.Join(t.TempDir(), "none.json"), "", false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(set.Rules) == 0 {
		t.Error("no built-in rules")
	}
	// The builtin deny rules are in force.
	d, _ := set.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "rm -rf /"}})
	if d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
}

// TestValidateRejectsBadRules verifies the configuration checks.
func TestValidateRejectsBadRules(t *testing.T) {
	set := &Set{Rules: []Rule{{Name: "bad", Action: "maybe"}}}
	if err := set.Validate(); err == nil {
		t.Error("expected an error for an unknown action")
	}
	set = &Set{Rules: []Rule{{Name: "bad", Action: ActionAllow, Args: map[string]string{"c": "([unclosed"}}}}
	if err := set.Validate(); err == nil {
		t.Error("expected an error for a bad pattern")
	}
	set = &Set{Rules: []Rule{{Name: "no action"}}}
	if err := set.Validate(); err == nil {
		t.Error("expected an error for a missing action")
	}
}

// TestPromptApproverReadsAnswer verifies the interactive approver.
func TestPromptApproverReadsAnswer(t *testing.T) {
	var out strings.Builder
	p := &PromptApprover{
		In:      strings.NewReader("y\n"),
		Out:     &out,
		Workdir: t.TempDir(),
	}
	allowed, err := p.Approve(context.Background(), Call{Tool: "bash", Args: map[string]any{"command": "ls"}}, nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !allowed {
		t.Error("the call was not approved")
	}
	if !strings.Contains(out.String(), "permission required") {
		t.Errorf("output = %q", out.String())
	}

	// A different answer declines.
	p = &PromptApprover{In: strings.NewReader("n\n"), Out: &out}
	allowed, _ = p.Approve(context.Background(), Call{Tool: "bash"}, nil)
	if allowed {
		t.Error("the call was approved despite the answer")
	}
}

// TestNormalizePath verifies path normalization.
func TestNormalizePath(t *testing.T) {
	got := NormalizePath("src/main.go", "/repo")
	if got != "/repo/src/main.go" {
		t.Errorf("path = %q", got)
	}
	got = NormalizePath("../etc/passwd", "/repo/sub")
	if got != "/repo/etc/passwd" {
		t.Errorf("path = %q", got)
	}
	got = NormalizePath("/abs/path", "/repo")
	if got != "/abs/path" {
		t.Errorf("path = %q", got)
	}
}

// TestSaveAndLoadUserConfig verifies the on-disk format is valid JSON.
func TestSaveAndLoadUserConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	if err := SaveFile(path, Builtin()); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Error("the saved file is not valid JSON")
	}
	// The builtin rules stay in the binary; the file holds only the
	// user's own rules.
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Rules) != 0 {
		t.Errorf("the file holds %d rules, want none", len(loaded.Rules))
	}
	// Re-loading it must not lose the builtin denies.
	merged, err := LoadConfig(path, "", false)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	d, _ := merged.Evaluate(Call{Tool: "bash", Args: map[string]any{"command": "rm -rf /"}})
	if d != DecisionDeny {
		t.Errorf("decision = %s, want deny", d)
	}
}
