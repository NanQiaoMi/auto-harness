package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/mcp"
	"github.com/NanQiaoMi/auto-harness/perm"
	"github.com/NanQiaoMi/auto-harness/skills"
)

// The stored trust decisions.
const (
	trustYes = "yes"
	trustNo  = "no"
)

// trustStore remembers project trust decisions between runs.
//
// It is a file of its own, not part of config.json: the config file is
// hand-written policy, while this file is written by the harness when
// the user answers.
type trustStore struct {
	// Path is where the decisions live.
	Path string `json:"-"`
	// Projects maps an absolute directory to "yes" or "no".
	Projects map[string]string `json:"projects"`
}

// trustStorePath returns ~/.harness/trust.json.
func trustStorePath() string {
	return filepath.Join(config.Dir(), "trust.json")
}

// loadTrustStore reads the decisions.
//
// A missing or malformed file means no decisions: a truncated trust
// file must not keep the harness from starting, and the fallback (ask
// the user, or deny when there is nobody to ask) is safe.
func loadTrustStore(path string) *trustStore {
	store := &trustStore{Path: path, Projects: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return store
	}
	var file struct {
		Projects map[string]string `json:"projects"`
	}
	if err := json.Unmarshal(data, &file); err == nil && file.Projects != nil {
		store.Projects = file.Projects
	}
	return store
}

// decision returns the stored decision for a directory ("" when none).
func (s *trustStore) decision(dir string) string {
	return s.Projects[dir]
}

// set records a decision and writes the file. An empty decision
// forgets the directory, so the next start asks again.
func (s *trustStore) set(dir, decision string) error {
	if decision == "" {
		delete(s.Projects, dir)
	} else {
		s.Projects[dir] = decision
	}
	return s.save()
}

func (s *trustStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Projects map[string]string `json:"projects"`
	}{s.Projects}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.Path, append(data, '\n'), 0o644)
}

// trustResource is one kind of project-local resource a trust decision
// covers.
type trustResource struct {
	// Label names the kind: "extensions", "MCP servers", ...
	Label string
	// Path is where the resource lives.
	Path string
	// Count is how many items were found, when counting makes sense.
	Count int
}

// projectResources lists the project-local resources that would load
// with the project trusted.
//
// A path that is also the user's own (the working directory is the
// home directory) is not a project resource: it loads either way, and
// asking about it would be asking the user to trust themselves.
func projectResources(workdir string) []trustResource {
	root := absolutePath(workdir)
	home, _ := os.UserHomeDir()

	var out []trustResource

	// Process extensions.
	projectExt := filepath.Join(root, ext.ProjectExtensionsDir)
	userExt := ""
	if home != "" {
		userExt = filepath.Join(home, ext.GlobalExtensionsDir)
	}
	if projectExt != userExt {
		if n := len(ext.DiscoverIn([]string{projectExt})); n > 0 {
			out = append(out, trustResource{Label: "extensions", Path: projectExt, Count: n})
		}
	}

	// MCP servers.
	if path := mcp.DefaultProjectPath(root); path != mcp.DefaultUserPath() && fileExists(path) {
		out = append(out, trustResource{Label: "MCP servers", Path: path})
	}

	// Permission rules.
	if path := perm.DefaultProjectPath(root); path != perm.DefaultUserPath() && fileExists(path) {
		out = append(out, trustResource{Label: "permission rules", Path: path})
	}

	// Prompt templates.
	projectPrompts := filepath.Join(root, skills.ConfigDirName, "prompts")
	userPrompts := filepath.Join(config.Dir(), "prompts")
	if projectPrompts != userPrompts {
		if n := countFiles(projectPrompts, ".md"); n > 0 {
			out = append(out, trustResource{Label: "prompt templates", Path: projectPrompts, Count: n})
		}
	}

	// Skills.
	for _, dir := range projectSkillDirs(root) {
		found, _ := skills.LoadFromDirs([]skills.Dir{dir})
		if len(found) > 0 {
			out = append(out, trustResource{Label: "skills", Path: dir.Path, Count: len(found)})
		}
	}
	return out
}

// projectSkillDirs returns the skill directories the project owns: the
// working directory's own, and the specification's ancestor
// directories. The user's own directories are left out, even when the
// working directory is the home directory.
func projectSkillDirs(workdir string) []skills.Dir {
	user := map[string]bool{
		filepath.Join(config.Dir(), skills.SkillsDirName): true,
	}
	if home, err := os.UserHomeDir(); err == nil {
		user[filepath.Join(home, skills.AgentsDirName, skills.SkillsDirName)] = true
	}
	var out []skills.Dir
	for _, dir := range skills.Dirs(workdir, "") {
		if user[dir.Path] {
			continue
		}
		out = append(out, dir)
	}
	return out
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// countFiles counts the files with the given extension directly inside
// dir.
func countFiles(dir, ext string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ext) {
			n++
		}
	}
	return n
}

// absolutePath returns the absolute, symlink-resolved form of dir.
//
// Two paths that reach the same directory must produce the same trust
// key, or a project trusted through one of them would be asked about
// again through the other.
func absolutePath(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// trustDecision is the outcome of the project trust check.
type trustDecision struct {
	// Trusted reports whether the project's resources may load.
	Trusted bool
	// Source says who made the decision: "extension", "saved",
	// "config", "prompt", "not asked" or "no resources".
	Source string
	// Resources are what the decision covers.
	Resources []trustResource
	// Dir is the project directory the decision is about.
	Dir string
}

// trustSetup is the resolved decision plus the store, for the /trust
// command.
type trustSetup struct {
	decision trustDecision
	store    *trustStore
}

// resolveProjectTrust decides whether the project's own resources may
// load, and records the decision when the extension or the user asked
// for it to be remembered.
//
// The order is pi's: an extension from the trusted scope decides first
// (it can suppress the prompt), then the saved decision, then the
// config file's trust_project, then the user. With nobody to ask the
// answer is no: a project whose resources nobody vouched for must not
// run code.
func resolveProjectTrust(cfg *config.Config, workdir string, bus *ext.Bus, interactive bool, ask func(string) bool) *trustSetup {
	dir := absolutePath(workdir)
	setup := &trustSetup{
		store: loadTrustStore(trustStorePath()),
		decision: trustDecision{
			Dir:       dir,
			Resources: projectResources(dir),
		},
	}
	if len(setup.decision.Resources) == 0 {
		setup.decision.Source = "no resources"
		return setup
	}

	// The trusted scope's extensions decide first. The project's own
	// have not loaded yet, which is the point: they must not run before
	// the decision.
	if bus != nil {
		ev := &ext.Event{Type: ext.EventProjectTrust, Cwd: dir}
		if err := bus.Emit(ev); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] project_trust: %v\n", err)
		}
		if ev.Trusted == trustYes || ev.Trusted == trustNo {
			setup.decision.Trusted = ev.Trusted == trustYes
			setup.decision.Source = "extension"
			if ev.Remember {
				if err := setup.store.set(dir, ev.Trusted); err != nil {
					fmt.Fprintf(os.Stderr, "[warn] trust: %v\n", err)
				}
			}
			return setup
		}
	}

	// A saved answer outlives the process.
	if saved := setup.store.decision(dir); saved == trustYes || saved == trustNo {
		setup.decision.Trusted = saved == trustYes
		setup.decision.Source = "saved"
		return setup
	}

	// The config file's trust_project is the user saying "always".
	if cfg.TrustProject {
		setup.decision.Trusted = true
		setup.decision.Source = "config"
		return setup
	}

	if !interactive || ask == nil {
		setup.decision.Source = "not asked"
		fmt.Fprintf(os.Stderr, "[warn] untrusted project: skipped project %s (%s); trust it interactively or set \"trust_project\": true\n",
			trustLabels(setup.decision.Resources), dir)
		return setup
	}

	setup.decision.Source = "prompt"
	setup.decision.Trusted = ask(trustQuestion(setup.decision))
	answer := trustNo
	if setup.decision.Trusted {
		answer = trustYes
	}
	// The answer is remembered: the same project is not asked again
	// unless the user runs /trust reset.
	if err := setup.store.set(dir, answer); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] trust: %v\n", err)
	}
	return setup
}

// trustQuestion is the prompt shown for an undecided project.
func trustQuestion(d trustDecision) string {
	var b strings.Builder
	b.WriteString("This project ships resources that load or run automatically:\n")
	for _, r := range d.Resources {
		if r.Count > 0 {
			fmt.Fprintf(&b, "  - %s (%d): %s\n", r.Label, r.Count, r.Path)
		} else {
			fmt.Fprintf(&b, "  - %s: %s\n", r.Label, r.Path)
		}
	}
	b.WriteString("Trust this project?")
	return b.String()
}

// trustLabels joins the resource labels for a notice line.
func trustLabels(resources []trustResource) string {
	seen := map[string]bool{}
	var labels []string
	for _, r := range resources {
		if seen[r.Label] {
			continue
		}
		seen[r.Label] = true
		labels = append(labels, r.Label)
	}
	return strings.Join(labels, ", ")
}

// trustPrompter turns the shared line reader into the trust prompt.
//
// The reader is the same one the REPL and the permission prompt use:
// several readers over one stdin would steal each other's input.
func trustPrompter(lines perm.LineReader) func(string) bool {
	if lines == nil {
		return nil
	}
	return func(question string) bool {
		fmt.Println(question)
		fmt.Print("[y/N] ")
		scanner := perm.NewScanner(lines)
		if !scanner.Scan() {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "y", "yes":
			return true
		}
		return false
	}
}

// sourceNote describes where the decision came from.
func (d trustDecision) sourceNote() string {
	switch d.Source {
	case "extension":
		return "decided by an extension"
	case "saved":
		return "saved decision"
	case "config":
		return "trust_project in config.json"
	case "prompt":
		return "answered now"
	case "not asked":
		return "nobody to ask; project resources are skipped"
	case "no resources":
		return "no project resources"
	}
	return d.Source
}

// report prints the current decision.
func (t *trustSetup) report(out func(string)) {
	if out == nil {
		out = func(string) {}
	}
	out("project: " + t.decision.Dir)
	state := "not trusted"
	if t.decision.Trusted {
		state = "trusted"
	}
	out("trust: " + state + " (" + t.decision.sourceNote() + ")")
	if len(t.decision.Resources) > 0 {
		out("resources:")
		for _, r := range t.decision.Resources {
			if r.Count > 0 {
				out(fmt.Sprintf("  - %s (%d): %s", r.Label, r.Count, r.Path))
			} else {
				out(fmt.Sprintf("  - %s: %s", r.Label, r.Path))
			}
		}
	}
	out("trust file: " + t.store.Path)
}

// apply handles "yes", "no" and "reset".
//
// The decision changes what loads at the next start: extensions, MCP
// servers, permission rules, skills and prompt templates are wired
// during startup, so a running session keeps what it has.
func (t *trustSetup) apply(args string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "yes":
		if err := t.store.set(t.decision.Dir, trustYes); err != nil {
			return "", err
		}
		return "saved: this project's resources load on the next start", nil
	case "no":
		if err := t.store.set(t.decision.Dir, trustNo); err != nil {
			return "", err
		}
		return "saved: this project's resources stay unloaded", nil
	case "reset":
		if err := t.store.set(t.decision.Dir, ""); err != nil {
			return "", err
		}
		return "forgotten: the next start asks again", nil
	}
	return "", fmt.Errorf("usage: /trust [yes|no|reset]")
}
