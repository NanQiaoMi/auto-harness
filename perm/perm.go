// Package perm implements a permission layer for tool calls.
//
// The design follows pi: the kernel has no built-in permission system,
// and permissions live in the extension layer, where a tool_call hook
// can block a call. This package provides the machinery — rules, matchers
// and decisions — and the harness wires it in as an extension.
//
// The guiding principle is fail-safe: a rule that matches and cannot be
// resolved (no user to ask, no matching allow rule) blocks the call
// rather than letting it through.
package perm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Decision is the outcome of evaluating a tool call against the rules.
type Decision string

const (
	// DecisionAllow lets the call run without asking.
	DecisionAllow Decision = "allow"
	// DecisionAsk needs the user to confirm before the call runs.
	DecisionAsk Decision = "ask"
	// DecisionDeny blocks the call.
	DecisionDeny Decision = "deny"
)

// Action is a rule's action: one of the decisions.
type Action string

const (
	ActionAllow Action = "allow"
	ActionAsk   Action = "ask"
	ActionDeny  Action = "deny"
)

// Rule is one permission rule.
//
// A rule matches a tool call when every populated field matches:
// the tool name, the argument patterns and the path patterns. Empty
// fields match anything.
type Rule struct {
	// Name identifies the rule in listings and in denial messages.
	Name string `json:"name,omitempty"`
	// Tools lists the tool names this rule applies to. A name may end
	// with "*" to match a prefix. Empty means every tool.
	Tools []string `json:"tools,omitempty"`
	// Args maps an argument name to a pattern the argument's value must
	// match. The pattern is a regular expression.
	Args map[string]string `json:"args,omitempty"`
	// Paths lists path patterns the call's target path must match.
	Paths []string `json:"paths,omitempty"`
	// Action is what to do when the rule matches.
	Action Action `json:"action"`
	// Reason explains the decision, shown to the user and the model.
	Reason string `json:"reason,omitempty"`
	// Builtin marks a rule shipped with the harness. A builtin deny
	// rule is a safety floor: no other rule can allow what it denies.
	// Builtin rules are also skipped when a configuration file is
	// written, so the user file holds only the user's own rules.
	Builtin bool `json:"-"`

	// compiled holds the compiled argument patterns.
	compiled map[string]*regexp.Regexp
}

// Call describes the tool call being evaluated.
type Call struct {
	// Tool is the tool's name.
	Tool string
	// Args are the tool's arguments.
	Args map[string]any
	// Path is the file the call targets, if any.
	Path string
}

// argString renders an argument as a string for pattern matching.
func argString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

// Matches reports whether the rule applies to the call.
func (r *Rule) Matches(call Call) bool {
	// The tool name.
	if len(r.Tools) > 0 && !matchAnyPattern(r.Tools, call.Tool) {
		return false
	}

	// The argument patterns.
	for name, pattern := range r.Args {
		value, ok := call.Args[name]
		if !ok {
			return false
		}
		re, err := r.compiledPattern(name, pattern)
		if err != nil || !re.MatchString(argString(value)) {
			return false
		}
	}

	// The path patterns.
	if len(r.Paths) > 0 {
		if call.Path == "" {
			return false
		}
		if !matchAnyPattern(r.Paths, call.Path) {
			return false
		}
	}
	return true
}

// compiledPattern compiles an argument pattern, caching the result.
func (r *Rule) compiledPattern(name, pattern string) (*regexp.Regexp, error) {
	if r.compiled == nil {
		r.compiled = map[string]*regexp.Regexp{}
	}
	if re, ok := r.compiled[name]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	r.compiled[name] = re
	return re, nil
}

// matchAnyPattern reports whether the value matches any of the
// patterns. A pattern ending in "*" matches by prefix; otherwise the
// pattern is a regular expression.
func matchAnyPattern(patterns []string, value string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(value, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if re, err := regexp.Compile(p); err == nil && re.MatchString(value) {
			return true
		}
		if p == value {
			return true
		}
	}
	return false
}

// Set is an ordered list of rules.
type Set struct {
	// Rules are evaluated in order; the first match decides.
	Rules []Rule `json:"rules"`
	// Default is the action for calls no rule matches.
	Default Action `json:"default,omitempty"`
}

// NewSet creates a set with the given default action.
func NewSet(defaultAction Action) *Set {
	if defaultAction == "" {
		defaultAction = ActionAsk
	}
	return &Set{Default: defaultAction}
}

// Evaluate returns the decision for a call and the rule that produced
// it.
//
// The rules come in two tiers:
//
//   - A matching deny rule always wins. The deny rules are the safety
//     floor; an allow rule elsewhere in the set cannot lift them.
//   - Otherwise the first matching rule decides. The set is ordered
//     from the most specific source (the user's own file) to the
//     built-in defaults, so a user rule overrides a default.
//
// When no rule matches, the set's default decides.
func (s *Set) Evaluate(call Call) (Decision, *Rule) {
	// The builtin deny rules first: they are the safety floor, and no
	// other rule can lift them.
	for i := range s.Rules {
		if s.Rules[i].Action != ActionDeny || !s.Rules[i].Builtin {
			continue
		}
		if s.Rules[i].Matches(call) {
			return DecisionDeny, &s.Rules[i]
		}
	}
	// Then the first matching rule of the other kinds.
	for i := range s.Rules {
		if !s.Rules[i].Matches(call) {
			continue
		}
		switch s.Rules[i].Action {
		case ActionAllow:
			return DecisionAllow, &s.Rules[i]
		case ActionDeny:
			return DecisionDeny, &s.Rules[i]
		case ActionAsk:
			return DecisionAsk, &s.Rules[i]
		}
	}
	switch s.Default {
	case ActionAllow:
		return DecisionAllow, nil
	case ActionDeny:
		return DecisionDeny, nil
	default:
		return DecisionAsk, nil
	}
}

// Validate checks the rules for problems.
func (s *Set) Validate() error {
	if s.Default != "" && s.Default != ActionAllow && s.Default != ActionAsk && s.Default != ActionDeny {
		return fmt.Errorf("perm: unknown default action %q", s.Default)
	}
	for i, r := range s.Rules {
		switch r.Action {
		case ActionAllow, ActionAsk, ActionDeny:
		case "":
			return fmt.Errorf("perm: rule %d (%s) has no action", i, r.Name)
		default:
			return fmt.Errorf("perm: rule %d (%s) has unknown action %q", i, r.Name, r.Action)
		}
		for name, pattern := range r.Args {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("perm: rule %d (%s): bad pattern for %q: %w", i, r.Name, name, err)
			}
		}
	}
	return nil
}

// NormalizePath expands a path to an absolute, cleaned form so rules
// match regardless of how the path was written.
//
// Relative paths are resolved against the working directory; a path
// that escapes it (via "..") is kept as an absolute path.
func NormalizePath(path, workdir string) string {
	if path == "" {
		return ""
	}
	expanded := path
	if strings.HasPrefix(expanded, "~") {
		if home := homeDir(); home != "" {
			expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~"))
		}
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(workdir, expanded)
	}
	return filepath.Clean(expanded)
}

// List returns the rule descriptions, for the /permissions command.
//
// A builtin deny rule is marked, because no other rule can lift it.
func (s *Set) List() []string {
	out := make([]string, 0, len(s.Rules))
	for _, r := range s.Rules {
		desc := r.Name
		if desc == "" {
			desc = "(unnamed)"
		}
		if r.Builtin && r.Action == ActionDeny {
			desc += " (builtin)"
		}
		var parts []string
		if len(r.Tools) > 0 {
			parts = append(parts, "tools="+strings.Join(r.Tools, ","))
		}
		if len(r.Args) > 0 {
			keys := make([]string, 0, len(r.Args))
			for k := range r.Args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s~%s", k, r.Args[k]))
			}
		}
		if len(r.Paths) > 0 {
			parts = append(parts, "paths="+strings.Join(r.Paths, ","))
		}
		out = append(out, fmt.Sprintf("%-8s %-16s %s", r.Action, desc, strings.Join(parts, " ")))
	}
	return out
}

// homeDir returns the user's home directory.
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
