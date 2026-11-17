// Package plugins loads process-extension plugin descriptors.
//
// A plugin directory contains plugin.json plus its program and bundled
// files. plugin.json is a launch and identity descriptor, not a second
// tool registry: after launch, the extension's runtime manifest remains
// the authority for its tools and consult hooks.
package plugins

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

	"github.com/NanQiaoMi/auto-harness/envsubst"
	"github.com/NanQiaoMi/auto-harness/ext"
)

// ManifestFile is the descriptor every directory plugin carries.
const ManifestFile = "plugin.json"

// SchemaVersion is the descriptor schema this binary accepts.
const SchemaVersion = 1

// Manifest describes how to launch one plugin.
type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Description   string            `json:"description,omitempty"`
	Command       []string          `json:"command"`
	CWD           string            `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Capabilities  []string          `json:"capabilities,omitempty"`
	Consults      []string          `json:"consults,omitempty"`
}

// Candidate is one discovered plugin: either a manifest plugin or a
// legacy executable that speaks the extension protocol directly.
type Candidate struct {
	Dir      string
	Path     string
	Manifest Manifest
	Legacy   bool
}

// Name is the plugin's stable selection name.
func (c Candidate) Name() string {
	if c.Legacy {
		return filepath.Base(c.Path)
	}
	return c.Manifest.Name
}

// Version is empty for a legacy executable.
func (c Candidate) Version() string {
	return c.Manifest.Version
}

// Load reads a directory plugin's manifest and validates its path
// fields.
func Load(dir string) (Candidate, error) {
	return LoadWithEnv(dir, nil)
}

// LoadWithEnv is Load with profile-level variables available during
// manifest expansion.
func LoadWithEnv(dir string, inherited map[string]string) (Candidate, error) {
	candidate, err := readCandidate(dir)
	if err != nil {
		return Candidate{}, err
	}
	if err := validateLaunch(candidate.Dir, candidate.Manifest, inherited); err != nil {
		return Candidate{}, fmt.Errorf("plugin: %s: %w", candidate.Path, err)
	}
	return candidate, nil
}

// readCandidate reads and validates the manifest schema without
// expanding its launch fields. Discovery needs the stable name to apply
// a profile allowlist; an unselected plugin's environment must not be
// required just to skip it.
func readCandidate(dir string) (Candidate, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Candidate{}, fmt.Errorf("plugin: %s: %w", dir, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	path := filepath.Join(abs, ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return Candidate{}, fmt.Errorf("plugin: %s: %w", path, err)
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return Candidate{}, fmt.Errorf("plugin: %s: %w", path, err)
	}
	return Candidate{Dir: abs, Path: path, Manifest: manifest}, nil
}

// ParseManifest validates one plugin descriptor. Paths are checked
// later against the plugin directory.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Manifest{}, fmt.Errorf("the file contains more than one JSON value")
		}
		return Manifest{}, err
	}
	if m.SchemaVersion != SchemaVersion {
		return Manifest{}, fmt.Errorf("unsupported schema_version %d (want %d)", m.SchemaVersion, SchemaVersion)
	}
	if !pluginNameRE.MatchString(m.Name) {
		return Manifest{}, fmt.Errorf("name %q must match %s", m.Name, pluginNameRE)
	}
	if strings.TrimSpace(m.Version) == "" {
		return Manifest{}, fmt.Errorf("version is required")
	}
	if len(m.Command) == 0 || strings.TrimSpace(m.Command[0]) == "" {
		return Manifest{}, fmt.Errorf("command must be a non-empty array")
	}
	for i, arg := range m.Command {
		if strings.ContainsRune(arg, 0) {
			return Manifest{}, fmt.Errorf("command[%d] contains a NUL byte", i)
		}
	}
	if strings.ContainsRune(m.CWD, 0) {
		return Manifest{}, fmt.Errorf("cwd contains a NUL byte")
	}
	seenCapabilities := map[string]bool{}
	for i, capability := range m.Capabilities {
		switch capability {
		case ext.CapabilityTools, ext.CapabilityHooks, ext.CapabilityEvents:
		default:
			return Manifest{}, fmt.Errorf("capabilities[%d] %q is not supported", i, capability)
		}
		if seenCapabilities[capability] {
			return Manifest{}, fmt.Errorf("capabilities contains %q more than once", capability)
		}
		seenCapabilities[capability] = true
	}
	seenConsults := map[string]bool{}
	for i, consult := range m.Consults {
		if !consultRE.MatchString(consult) {
			return Manifest{}, fmt.Errorf("consults[%d] %q is invalid", i, consult)
		}
		if seenConsults[consult] {
			return Manifest{}, fmt.Errorf("consults contains %q more than once", consult)
		}
		seenConsults[consult] = true
	}
	for key := range m.Env {
		if !envNameRE.MatchString(key) {
			return Manifest{}, fmt.Errorf("env key %q is invalid", key)
		}
	}
	return m, nil
}

var (
	pluginNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	consultRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// validateLaunch checks cwd and a path-like executable against the
// plugin root. A bare command (python3, node) remains a PATH lookup.
func validateLaunch(root string, m Manifest, inherited map[string]string) error {
	lookup := lookupFor(root, m, inherited)
	cwdValue, err := envsubst.Expand(m.CWD, lookup)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	if strings.TrimSpace(cwdValue) == "" {
		cwdValue = root
	}
	cwd, err := resolveInside(root, cwdValue)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd %s is not a directory", cwd)
	}
	command, err := envsubst.Expand(m.Command[0], lookup)
	if err != nil {
		return fmt.Errorf("command[0]: %w", err)
	}
	if pathLike(command) {
		if !filepath.IsAbs(command) {
			command = filepath.Join(cwd, command)
		}
		if _, err := resolveInside(root, command); err != nil {
			return fmt.Errorf("command[0]: %w", err)
		}
	}
	for i := 1; i < len(m.Command); i++ {
		if _, err := envsubst.Expand(m.Command[i], lookup); err != nil {
			return fmt.Errorf("command[%d]: %w", i, err)
		}
	}
	for key, value := range m.Env {
		if _, err := envsubst.Expand(value, lookup); err != nil {
			return fmt.Errorf("env %s: %w", key, err)
		}
	}
	return nil
}

// Extension turns a manifest plugin into a process extension.
//
// inherited contains profile-level environment. Manifest-level entries
// override it, and the reserved HARNESS_PLUGIN_* variables override
// both.
func (c Candidate) Extension(inherited map[string]string) (*ext.ProcessExtension, error) {
	if c.Legacy {
		env := sortedEnv(inherited)
		return &ext.ProcessExtension{Command: c.Path, Env: env}, nil
	}
	root := c.Dir
	lookup := lookupFor(root, c.Manifest, inherited)
	cwdValue, err := envsubst.Expand(c.Manifest.CWD, lookup)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: cwd: %w", c.Manifest.Name, err)
	}
	if strings.TrimSpace(cwdValue) == "" {
		cwdValue = root
	}
	cwd, err := resolveInside(root, cwdValue)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: cwd: %w", c.Manifest.Name, err)
	}
	args := make([]string, len(c.Manifest.Command))
	for i, value := range c.Manifest.Command {
		args[i], err = envsubst.Expand(value, lookup)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: command[%d]: %w", c.Manifest.Name, i, err)
		}
	}
	command := args[0]
	args = args[1:]
	if pathLike(command) {
		if !filepath.IsAbs(command) {
			command = filepath.Join(cwd, command)
		}
		command, err = resolveInside(root, command)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: command: %w", c.Manifest.Name, err)
		}
	}
	env := map[string]string{}
	for key, value := range inherited {
		env[key] = value
	}
	for key, value := range c.Manifest.Env {
		resolved, err := envsubst.Expand(value, lookup)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: env %s: %w", c.Manifest.Name, key, err)
		}
		env[key] = resolved
	}
	env["HARNESS_PLUGIN_DIR"] = root
	env["HARNESS_PLUGIN_NAME"] = c.Manifest.Name
	env["HARNESS_PLUGIN_VERSION"] = c.Manifest.Version
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("plugin %s: command is empty", c.Manifest.Name)
	}
	return &ext.ProcessExtension{
		Command:      command,
		Args:         args,
		Dir:          cwd,
		Env:          sortedEnv(env),
		Capabilities: c.Manifest.Capabilities,
	}, nil
}

func lookupFor(root string, m Manifest, inherited map[string]string) envsubst.Lookup {
	return func(name string) (string, bool) {
		switch name {
		case "PLUGIN_DIR", "pluginDir":
			return root, true
		case "PLUGIN_NAME":
			return m.Name, true
		case "PLUGIN_VERSION":
			return m.Version, true
		}
		if value, ok := inherited[name]; ok {
			return value, true
		}
		return os.LookupEnv(name)
	}
}

func resolveInside(root, value string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(realRoot, path)
	}
	path = filepath.Clean(path)
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside the plugin directory", value)
	}
	return realPath, nil
}

func pathLike(command string) bool {
	return filepath.IsAbs(command) ||
		strings.HasPrefix(command, ".") ||
		strings.ContainsRune(command, filepath.Separator) ||
		strings.Contains(command, "/")
}

func sortedEnv(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

// Discover finds manifest plugins and legacy executable plugins.
//
// Each plugin directory with plugin.json is one candidate, even when it
// also contains executable helper files. A directory without a manifest
// keeps the original convention: executable files directly inside it
// are legacy candidates. Executable files directly in dir are
// candidates too.
//
// Earlier directories win a name collision. That keeps the user's
// plugins ahead of a project's replacement with the same name.
func Discover(dirs []string) ([]Candidate, []error) {
	return DiscoverWithEnv(dirs, nil)
}

// DiscoverWithEnv discovers identities and validates manifest schema.
// Launch fields are deliberately expanded only after a profile selects
// the candidate, through Candidate.Extension. The inherited argument is
// kept for API compatibility.
func DiscoverWithEnv(dirs []string, _ map[string]string) ([]Candidate, []error) {
	var found []Candidate
	var errs []error
	seenNames := map[string]string{}
	seenPaths := map[string]bool{}

	add := func(candidate Candidate, source string) {
		if seenPaths[candidate.Path] {
			return
		}
		seenPaths[candidate.Path] = true
		if previous, exists := seenNames[candidate.Name()]; exists {
			errs = append(errs, fmt.Errorf("plugin: %s: duplicate name %q (already provided by %s)",
				source, candidate.Name(), previous))
			return
		}
		seenNames[candidate.Name()] = source
		found = append(found, candidate)
	}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("plugin: %s: %w", dir, err))
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			path := filepath.Join(dir, name)
			if entry.IsDir() {
				if manifestExists(path) {
					candidate, err := readCandidate(path)
					if err != nil {
						errs = append(errs, err)
						continue
					}
					add(candidate, candidate.Path)
					continue
				}
				for _, executable := range executablesIn(path) {
					add(Candidate{Path: executable, Dir: filepath.Dir(executable), Legacy: true}, executable)
				}
				continue
			}
			if isExecutable(path) {
				add(Candidate{Path: path, Dir: filepath.Dir(path), Legacy: true}, path)
			}
		}
	}
	return found, errs
}

func manifestExists(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ManifestFile))
	return err == nil && !info.IsDir()
}

func executablesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if isExecutable(path) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
