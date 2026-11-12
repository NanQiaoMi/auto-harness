package perm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConfigFile is the on-disk format of a permission configuration.
//
// It matches the shape other agent tools use: a list of rules plus a
// default action for calls no rule matches.
type ConfigFile struct {
	// Rules are evaluated in order; the first match decides.
	Rules []Rule `json:"rules"`
	// Default is the action for calls no rule matches.
	Default Action `json:"default,omitempty"`
}

// LoadFile reads a permission configuration file.
func LoadFile(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f ConfigFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("perm: %s: %w", path, err)
	}
	set := &Set{Rules: f.Rules, Default: f.Default}
	if set.Default == "" {
		set.Default = ActionAsk
	}
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("perm: %s: %w", path, err)
	}
	return set, nil
}

// SaveFile writes a permission configuration file.
//
// The builtin rules are skipped: they ship with the harness and must
// stay in force even when the user file does not mention them.
func SaveFile(path string, s *Set) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rules := make([]Rule, 0, len(s.Rules))
	for _, r := range s.Rules {
		if r.Builtin {
			continue
		}
		rules = append(rules, r)
	}
	f := ConfigFile{Rules: rules, Default: s.Default}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// DefaultUserPath returns the user-level permission file path.
func DefaultUserPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".harness/permissions.json"
	}
	return filepath.Join(home, ".harness", "permissions.json")
}

// DefaultProjectPath returns the project-level permission file path.
func DefaultProjectPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".harness", "permissions.json")
}

// Builtin returns the default rule set.
//
// The defaults are deliberately conservative: reading is allowed, writes
// are asked about, and the clearly destructive operations are denied.
// A user who wants a different balance writes a permission file.
//
// The deny rules are marked Builtin: they are the safety floor and a
// user's own allow rule cannot lift them. The rest of the defaults are
// ordinary rules, so a user file that comes first overrides them.
func Builtin() *Set {
	set := &Set{
		Default: ActionAsk,
		Rules: []Rule{
			{
				Name:    "read-only tools",
				Tools:   []string{"read"},
				Action:  ActionAllow,
				Reason:  "reading files is safe",
				Builtin: true,
			},
			{
				Name:    "current time",
				Tools:   []string{"current_time"},
				Action:  ActionAllow,
				Builtin: true,
			},
			{
				Name:    "destructive shell commands",
				Tools:   []string{"bash"},
				Args:    map[string]string{"command": `(?i)\brm\s+(-[a-z]*[rf]|--recursive)`},
				Action:  ActionDeny,
				Reason:  "recursive removal is destructive",
				Builtin: true,
			},
			{
				Name:    "disk operations",
				Tools:   []string{"bash"},
				Args:    map[string]string{"command": `(?i)\b(mkfs|fdisk|diskutil\s+erase)`},
				Action:  ActionDeny,
				Reason:  "disk operations are destructive",
				Builtin: true,
			},
			{
				Name:    "privilege escalation",
				Tools:   []string{"bash"},
				Args:    map[string]string{"command": `(?i)\bsudo\b|\bsu\s+-\b|\bdoas\b`},
				Action:  ActionDeny,
				Reason:  "privilege escalation is not allowed",
				Builtin: true,
			},
			{
				Name:    "writes to protected paths",
				Tools:   []string{"write", "edit"},
				Paths:   []string{`(^|/)\.git/`, `(^|/)\.env$`, `\.ssh/`, `\.aws/credentials`},
				Action:  ActionDeny,
				Reason:  "the path is protected",
				Builtin: true,
			},
			{
				Name:    "file writes",
				Tools:   []string{"write", "edit"},
				Action:  ActionAsk,
				Reason:  "writing to the filesystem changes the project",
				Builtin: true,
			},
			{
				Name:    "shell commands",
				Tools:   []string{"bash"},
				Action:  ActionAsk,
				Reason:  "shell commands can change anything the user can",
				Builtin: true,
			},
			{
				Name:    "mcp tools",
				Tools:   []string{"mcp__*"},
				Action:  ActionAsk,
				Reason:  "MCP tools act on external systems",
				Builtin: true,
			},
		},
	}
	return set
}

// Floor returns the deny rules that no configuration may lift.
//
// It is the set of rules that protect the machine, not the project.
func Floor() *Set {
	set := Builtin()
	var floor []Rule
	for _, r := range set.Rules {
		if r.Builtin && r.Action == ActionDeny {
			floor = append(floor, r)
		}
	}
	return &Set{Default: ActionDeny, Rules: floor}
}

// Merge combines the rule sets.
//
// The order is: the user's own rules first, then the project's, then
// the built-in defaults. The most specific source therefore wins, and a
// user rule can relax a built-in default (but never a builtin deny —
// see Floor).
//
// The project file's default action wins when it sets one.
func Merge(user, project *Set) *Set {
	out := NewSet(ActionAsk)
	if user != nil {
		out.Rules = append(out.Rules, user.Rules...)
		out.Default = user.Default
	}
	if project != nil {
		out.Rules = append(out.Rules, project.Rules...)
		if project.Default != "" {
			out.Default = project.Default
		}
	}
	if out.Default == "" {
		out.Default = ActionAsk
	}
	return out
}

// LoadConfig reads the permission configuration.
//
// The user-level file is read first, then the project-level one when
// the project is trusted. The built-in deny rules are always in force;
// a configuration file can relax the other defaults but not those.
func LoadConfig(userPath, projectPath string, trustProject bool) (*Set, error) {
	var user, project *Set
	var err error
	if userPath != "" {
		user, err = LoadFile(userPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if trustProject && projectPath != "" {
		project, err = LoadFile(projectPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	merged := Merge(user, project)
	// The built-in rules go last, so the user's rules come first, and
	// the builtin denies stay in force.
	merged.Rules = append(merged.Rules, Builtin().Rules...)
	return merged, nil
}
