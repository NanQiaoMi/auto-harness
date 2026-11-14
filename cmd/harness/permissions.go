package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/perm"
	"github.com/NanQiaoMi/auto-harness/sandbox"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// permSetup holds the permission layer's state.
type permSetup struct {
	// guard enforces the rules.
	guard *perm.Guard
	// set is the rule set in effect.
	set *perm.Set
	// userPath is the user-level permission file.
	userPath string
	// projectPath is the project-level permission file.
	projectPath string
	// sandboxProfile is the sandbox the shell commands run in, when
	// the sandbox is enabled.
	sandboxProfile sandbox.Profile
	// sandboxed reports whether the sandbox is active.
	sandboxed bool
}

// loadPermissions builds the permission layer.
//
// The rule set comes from the built-in defaults, the user-level file,
// the project-level file and the config file, in that order of
// precedence. The config file's rules are appended last.
//
// interactive says whether a user is present to approve a call. When it
// is false the calls that need confirmation are blocked: pi's rule is
// to fail safe when there is no UI to confirm.
func loadPermissions(ctx context.Context, cfg *config.Config, projectRoot string, registry *tools.Registry, interactive bool) *permSetup {
	setup := &permSetup{
		userPath:    perm.DefaultUserPath(),
		projectPath: perm.DefaultProjectPath(projectRoot),
	}

	if cfg.NoPermissions {
		setup.set = perm.NewSet(perm.ActionAllow)
		return setup
	}

	set, err := perm.LoadConfig(setup.userPath, setup.projectPath, cfg.TrustProject)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] permissions: %v\n", err)
		set = perm.Builtin()
	}
	// The config file's rules come first: they are the most explicit
	// statement of intent, so they take precedence over the files and
	// the built-in defaults.
	if cfg.Permissions != nil {
		set.Rules = append(append([]perm.Rule{}, cfg.Permissions.Rules...), set.Rules...)
		if cfg.Permissions.Default != "" {
			set.Default = cfg.Permissions.Default
		}
	}
	setup.set = set

	guard := perm.NewGuard(set, projectRoot)
	guard.Logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[perm] "+format+"\n", args...)
	}
	if interactive {
		// In an interactive session the user can approve a call.
		guard.Approver = &perm.PromptApprover{Workdir: projectRoot}
	} else {
		// In a one-shot run there is nobody to ask, so the call is
		// blocked instead of silently allowed.
		guard.Approver = perm.DenyApprover{Reason: "no interactive session to confirm the call"}
	}
	setup.guard = guard

	// The sandbox.
	if cfg.Sandbox {
		profile := sandbox.DefaultProfile(projectRoot)
		if len(cfg.SandboxWritePaths) > 0 {
			profile.WritePaths = cfg.SandboxWritePaths
		}
		profile.AllowNetwork = cfg.SandboxAllowNetwork
		setup.sandboxProfile = profile
		setup.sandboxed = sandbox.Available()
	}
	return setup
}

// setLineReader shares the REPL's input with the permission prompt, so
// both read from one stream.
func (s *permSetup) setLineReader(lines perm.LineReader) {
	if s.guard == nil {
		return
	}
	if pa, ok := s.guard.Approver.(*perm.PromptApprover); ok {
		pa.Lines = lines
	}
}

// setPromptOutput redirects the permission prompt, so the TUI can show
// it in the transcript instead of interleaving it with the screen
// updates.
func (s *permSetup) setPromptOutput(w io.Writer) {
	if s.guard == nil {
		return
	}
	if pa, ok := s.guard.Approver.(*perm.PromptApprover); ok {
		pa.Out = w
	}
}

// sandboxRunner returns the runner for the bash tool, or nil when the
// sandbox is off.
func (s *permSetup) sandboxRunner() tools.SandboxRunner {
	if !s.sandboxed {
		return nil
	}
	profile := s.sandboxProfile
	return func(ctx context.Context, command, workdir string) (string, bool, error) {
		return sandbox.Exec(ctx, command, profile, workdir)
	}
}

// report prints the permission rules in effect.
func (s *permSetup) report() {
	fmt.Printf("user config:    %s\n", s.userPath)
	fmt.Printf("project config: %s\n", s.projectPath)
	fmt.Printf("default:        %s\n", s.set.Default)
	fmt.Println("\nrules (builtin denies first, then first match wins):")
	for _, line := range s.set.List() {
		fmt.Println(" ", line)
	}
	if s.sandboxed {
		fmt.Printf("\nsandbox: %s\n", s.sandboxProfile.Describe())
	} else {
		fmt.Println("\nsandbox: off")
	}
	allowed, blocked, asked := s.guard.Stats()
	fmt.Printf("\nthis session: %d allowed, %d asked, %d blocked\n", allowed, asked, blocked)
}

// saveSet writes a rule set to the user-level permission file.
func (s *permSetup) saveSet(set *perm.Set) error {
	return perm.SaveFile(s.userPath, set)
}

// allowPath adds an allow rule for a path and saves the rules.
func (s *permSetup) allowPath(path string) error {
	abs := perm.NormalizePath(path, ".")
	rule := perm.Rule{
		Name:   "allowed by user",
		Tools:  []string{"write", "edit"},
		Paths:  []string{abs},
		Action: perm.ActionAllow,
	}
	// Only the user's own rules are written back: the built-in rules
	// stay in the binary, where they can be updated.
	userRules := make([]perm.Rule, 0, len(s.set.Rules))
	for _, r := range s.set.Rules {
		if r.Builtin {
			continue
		}
		userRules = append(userRules, r)
	}
	next := perm.NewSet(s.set.Default)
	// The new rule goes first, so it takes precedence over the
	// defaults it relaxes.
	next.Rules = append([]perm.Rule{rule}, userRules...)
	if err := s.saveSet(next); err != nil {
		return err
	}
	s.set = next
	fmt.Printf("allowed writes to %s\n", abs)
	return nil
}

// expandHomePath expands a leading ~/ in a path.
func expandHomePath(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
