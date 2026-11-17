// Package agentprofile defines an agent environment profile: one
// declarative file that selects a model, prompt, tool surface, plugins,
// MCP servers, skills, prompts and context policy.
//
// A profile is data, not code. Loading one only changes what the
// harness assembles for this run; every plugin and MCP process it names
// still goes through the normal trust and launch paths.
package agentprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/envsubst"
	"github.com/NanQiaoMi/auto-harness/llm"
)

// SchemaVersion is the only profile schema this binary accepts.
const SchemaVersion = 1

// Scope says where a profile was found.
type Scope string

const (
	ScopeUser     Scope = "user"
	ScopeProject  Scope = "project"
	ScopeExplicit Scope = "explicit"
)

// Profile is the on-disk agent environment description.
//
// Nil and empty selections are intentionally different. A nil tools,
// plugins, MCP servers, skills or prompts field means "use the normal
// discovery result"; an empty array means "expose none". That is what
// lets one profile describe a shell-only agent and another a
// deliberately tool-free narrator.
type Profile struct {
	SchemaVersion    int                   `json:"schema_version"`
	Name             string                `json:"name"`
	Description      string                `json:"description,omitempty"`
	Provider         string                `json:"provider,omitempty"`
	Model            string                `json:"model,omitempty"`
	BaseURL          string                `json:"base_url,omitempty"`
	SystemPrompt     string                `json:"system_prompt,omitempty"`
	SystemPromptFile string                `json:"system_prompt_file,omitempty"`
	MaxTurns         int                   `json:"max_turns,omitempty"`
	MaxTokens        int                   `json:"max_tokens,omitempty"`
	Tools            []string              `json:"tools,omitempty"`
	Plugins          []string              `json:"plugins,omitempty"`
	PluginDirs       []string              `json:"plugin_dirs,omitempty"`
	MCPServers       []string              `json:"mcp_servers,omitempty"`
	Skills           []string              `json:"skills,omitempty"`
	SkillDirs        []string              `json:"skill_dirs,omitempty"`
	Prompts          []string              `json:"prompts,omitempty"`
	PromptDirs       []string              `json:"prompt_dirs,omitempty"`
	Context          *config.ContextConfig `json:"context,omitempty"`
	// Env is inherited by plugin and MCP child processes. It is never
	// written back into the harness process environment.
	Env map[string]string `json:"env,omitempty"`
}

// Resolved is a profile plus where and how it was loaded.
type Resolved struct {
	Profile Profile
	Path    string
	Dir     string
	Scope   Scope
}

// Dir returns the user's profile directory, ~/.harness/profiles.
func Dir() string {
	return filepath.Join(config.Dir(), "profiles")
}

// ProjectDir returns a project's profile directory.
func ProjectDir(workdir string) string {
	return filepath.Join(workdir, ".harness", "profiles")
}

// Resolve loads a profile selector.
//
// A selector that looks like a path is loaded directly. Otherwise it is
// a profile name: a trusted project's profile shadows the user's one,
// because it describes that project's environment. An untrusted project
// profile is never loaded; if no user profile exists, Resolve reports
// that the profile is unavailable rather than silently falling back.
//
// An explicitly selected file is the user's own choice and does not
// depend on project trust.
func Resolve(selector, workdir string, trusted bool) (*Resolved, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, fmt.Errorf("agent profile: empty selector")
	}
	expanded, err := expandPath(selector)
	if err != nil {
		return nil, err
	}
	if looksLikePath(expanded) {
		resolved, err := Load(expanded, ScopeExplicit)
		if err != nil {
			return nil, err
		}
		return resolved, nil
	}

	projectPath := filepath.Join(ProjectDir(workdir), selector+".json")
	userPath := filepath.Join(Dir(), selector+".json")
	if trusted {
		if fileExists(projectPath) {
			return Load(projectPath, ScopeProject)
		}
	}
	if fileExists(userPath) {
		return Load(userPath, ScopeUser)
	}
	if !trusted && fileExists(projectPath) {
		return nil, fmt.Errorf("agent profile %q exists in the project but the project is not trusted", selector)
	}
	return nil, fmt.Errorf("agent profile %q not found (looked in %s and %s)", selector, ProjectDir(workdir), Dir())
}

// Load reads and validates one profile file.
func Load(path string, scope Scope) (*Resolved, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return nil, fmt.Errorf("agent profile: %s: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("agent profile: %s: %w", abs, err)
	}
	profile, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("agent profile: %s: %w", abs, err)
	}
	return &Resolved{
		Profile: profile,
		Path:    abs,
		Dir:     filepath.Dir(abs),
		Scope:   scope,
	}, nil
}

// Parse validates one profile document. Relative paths are resolved
// against baseDir, which is normally the profile file's directory.
func Parse(data []byte, baseDir string) (Profile, error) {
	var p Profile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Profile{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Profile{}, fmt.Errorf("the file contains more than one JSON value")
		}
		return Profile{}, err
	}
	if err := validate(&p, baseDir); err != nil {
		return Profile{}, err
	}
	return p, nil
}

var (
	profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	itemNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	toolNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validate(p *Profile, baseDir string) error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", p.SchemaVersion, SchemaVersion)
	}
	if !profileNameRE.MatchString(p.Name) {
		return fmt.Errorf("name %q must match %s", p.Name, profileNameRE)
	}
	if strings.TrimSpace(p.SystemPrompt) != "" && strings.TrimSpace(p.SystemPromptFile) != "" {
		return fmt.Errorf("set either system_prompt or system_prompt_file, not both")
	}
	if p.MaxTurns < 0 || p.MaxTokens < 0 {
		return fmt.Errorf("max_turns and max_tokens must not be negative")
	}
	for _, item := range []struct {
		field  string
		values []string
		re     *regexp.Regexp
	}{
		{"tools", p.Tools, toolNameRE},
		{"plugins", p.Plugins, itemNameRE},
		{"mcp_servers", p.MCPServers, itemNameRE},
		{"skills", p.Skills, itemNameRE},
		{"prompts", p.Prompts, itemNameRE},
	} {
		if err := validateNames(item.field, item.values, item.re); err != nil {
			return err
		}
	}

	var err error
	if p.SystemPromptFile, err = resolvePathField(p.SystemPromptFile, baseDir); err != nil {
		return fmt.Errorf("system_prompt_file: %w", err)
	}
	if p.SystemPromptFile != "" {
		content, readErr := os.ReadFile(p.SystemPromptFile)
		if readErr != nil {
			return fmt.Errorf("system_prompt_file: %w", readErr)
		}
		p.SystemPrompt = strings.TrimSpace(string(content))
	}
	for field, values := range map[string][]string{
		"plugin_dirs": p.PluginDirs,
		"skill_dirs":  p.SkillDirs,
		"prompt_dirs": p.PromptDirs,
	} {
		for i, value := range values {
			resolved, resolveErr := resolvePathField(value, baseDir)
			if resolveErr != nil {
				return fmt.Errorf("%s[%d]: %w", field, i, resolveErr)
			}
			if resolved == "" {
				return fmt.Errorf("%s[%d] must not be empty", field, i)
			}
			info, statErr := os.Stat(resolved)
			if statErr != nil {
				return fmt.Errorf("%s[%d]: %w", field, i, statErr)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s[%d] %s is not a directory", field, i, resolved)
			}
			values[i] = resolved
		}
	}
	if p.Context != nil {
		if err := validateContext(p.Context, baseDir); err != nil {
			return err
		}
	}
	if p.Env != nil {
		expanded := make(map[string]string, len(p.Env))
		for key, value := range p.Env {
			if !envNameRE.MatchString(key) {
				return fmt.Errorf("env key %q is invalid", key)
			}
			resolved, expandErr := envsubst.Expand(value, envsubst.OS())
			if expandErr != nil {
				return fmt.Errorf("env %s: %w", key, expandErr)
			}
			expanded[key] = resolved
		}
		p.Env = expanded
	}
	if p.BaseURL != "" {
		resolved, expandErr := envsubst.Expand(p.BaseURL, envsubst.OS())
		if expandErr != nil {
			return fmt.Errorf("base_url: %w", expandErr)
		}
		p.BaseURL = resolved
	}
	return nil
}

func validateNames(field string, values []string, re *regexp.Regexp) error {
	seen := map[string]bool{}
	for i, value := range values {
		if !re.MatchString(value) {
			return fmt.Errorf("%s[%d] %q is invalid", field, i, value)
		}
		if seen[value] {
			return fmt.Errorf("%s contains %q more than once", field, value)
		}
		seen[value] = true
	}
	return nil
}

func validateContext(c *config.ContextConfig, baseDir string) error {
	if c.Window < 0 || c.ReserveTokens < 0 || c.KeepRecentTokens < 0 ||
		c.KeepRecentMessages < 0 || c.SummaryMaxTokens < 0 ||
		c.SummaryChunkTokens < 0 || c.ArtifactThreshold < 0 || c.ArtifactPreview < 0 {
		return fmt.Errorf("context values must not be negative")
	}
	if c.CompactAt < 0 || c.CompactAt > 1 {
		return fmt.Errorf("context.compact_at must be between 0 and 1")
	}
	if c.ArtifactDir != "" {
		resolved, err := resolvePathField(c.ArtifactDir, baseDir)
		if err != nil {
			return fmt.Errorf("context.artifact_dir: %w", err)
		}
		c.ArtifactDir = resolved
	}
	return nil
}

func resolvePathField(value, baseDir string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	expanded, err := envsubst.Expand(value, envsubst.OS())
	if err != nil {
		return "", err
	}
	expanded, err = expandPath(expanded)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(baseDir, expanded)
	}
	return filepath.Clean(expanded), nil
}

func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func looksLikePath(selector string) bool {
	return filepath.IsAbs(selector) ||
		strings.HasPrefix(selector, ".") ||
		strings.ContainsRune(selector, filepath.Separator) ||
		strings.Contains(selector, "/") ||
		strings.HasSuffix(strings.ToLower(selector), ".json")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ApplyToConfig overlays the profile on an effective configuration.
//
// The profile sits above config-file and environment defaults. CLI
// flags are applied again by the caller afterwards, so an explicit flag
// remains the strongest instruction.
func (r *Resolved) ApplyToConfig(cfg *config.Config) {
	if r == nil || cfg == nil {
		return
	}
	p := r.Profile
	if p.Provider != "" {
		cfg.Provider = p.Provider
	}
	if p.Model != "" {
		cfg.Model = p.Model
		if p.Provider == "" {
			cfg.Provider = llm.GuessProvider(p.Model)
		}
	}
	if p.BaseURL != "" {
		cfg.BaseURL = p.BaseURL
	}
	if p.SystemPrompt != "" {
		cfg.SystemPrompt = p.SystemPrompt
	}
	if p.MaxTurns > 0 {
		cfg.MaxTurns = p.MaxTurns
	}
	if p.MaxTokens > 0 {
		cfg.MaxTokens = p.MaxTokens
	}
	if p.Context != nil {
		next := config.ContextConfig{}
		if cfg.Context != nil {
			next = *cfg.Context
		}
		overlayContext(&next, p.Context)
		cfg.Context = &next
	}
}

func overlayContext(dst *config.ContextConfig, src *config.ContextConfig) {
	if src.Window > 0 {
		dst.Window = src.Window
	}
	if src.ReserveTokens > 0 {
		dst.ReserveTokens = src.ReserveTokens
	}
	if src.CompactAt > 0 {
		dst.CompactAt = src.CompactAt
	}
	if src.KeepRecentTokens > 0 {
		dst.KeepRecentTokens = src.KeepRecentTokens
	}
	if src.KeepRecentMessages > 0 {
		dst.KeepRecentMessages = src.KeepRecentMessages
	}
	if src.SummaryMaxTokens > 0 {
		dst.SummaryMaxTokens = src.SummaryMaxTokens
	}
	if src.SummaryChunkTokens > 0 {
		dst.SummaryChunkTokens = src.SummaryChunkTokens
	}
	if src.ArtifactThreshold > 0 {
		dst.ArtifactThreshold = src.ArtifactThreshold
	}
	if src.ArtifactPreview > 0 {
		dst.ArtifactPreview = src.ArtifactPreview
	}
	if src.ArtifactDir != "" {
		dst.ArtifactDir = src.ArtifactDir
	}
}

// Info is a location that can list available profiles.
type Info struct {
	Name  string `json:"name"`
	Scope Scope  `json:"scope"`
	Path  string `json:"path"`
}

// List returns the profile files available from the user and, when
// trusted, the project.
func List(workdir string, trusted bool) ([]Info, error) {
	byName := map[string]Info{}
	dirs := []struct {
		path  string
		scope Scope
	}{
		{Dir(), ScopeUser},
	}
	if trusted {
		dirs = append([]struct {
			path  string
			scope Scope
		}{{ProjectDir(workdir), ScopeProject}}, dirs...)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("agent profile: %s: %w", dir.path, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".json")
			if _, exists := byName[name]; exists {
				continue
			}
			byName[name] = Info{Name: name, Scope: dir.scope, Path: filepath.Join(dir.path, entry.Name())}
		}
	}
	out := make([]Info, 0, len(byName))
	for _, info := range byName {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
