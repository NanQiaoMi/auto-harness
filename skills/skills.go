// Package skills implements pi's skills layer: the Agent Skills
// standard, where a skill is a directory holding a SKILL.md file with
// instructions the model loads only when a task matches.
//
// The design is progressive disclosure. At startup the harness puts
// each skill's name, description and location into the system prompt,
// so the model knows what exists. The full instructions stay on disk
// until the model reads the file — which keeps a large skill library
// cheap.
//
// A skill is data, not code: it cannot run anything by itself. The
// instructions it carries may tell the model to run scripts, but those
// scripts go through the same tools and the same permission layer as
// anything else the model does.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NanQiaoMi/auto-harness/frontmatter"
)

// FileName is the file that holds a skill's instructions.
const FileName = "SKILL.md"

// The limits the Agent Skills specification puts on the frontmatter.
const (
	// MaxNameLength is the longest a skill's name may be.
	MaxNameLength = 64
	// MaxDescriptionLength is the longest a skill's description may be.
	MaxDescriptionLength = 1024
)

// Skill is one loaded skill.
type Skill struct {
	// Name is the skill's name: the frontmatter's name field, or the
	// name of the directory holding it.
	Name string
	// Description is the routing signal. The model reads it to decide
	// whether the skill applies, so it says both what the skill does
	// and when it applies.
	Description string
	// Path is the SKILL.md file.
	Path string
	// Dir is the skill's directory: the parent of SKILL.md. The
	// skill's bundled files are relative to it.
	Dir string
	// DisableModelInvocation hides the skill from the system prompt, so
	// only the explicit /skill:name command reaches it.
	DisableModelInvocation bool
	// Source says where the skill came from: "user", "project" or
	// "agents".
	Source string
}

// Diagnostic is a problem found while loading skills.
//
// A diagnostic is a warning, not a failure: one broken skill must not
// take the others down, and an invalid field is usually a mistake in
// one file rather than a broken installation.
type Diagnostic struct {
	// Type is "warning" or "collision".
	Type string
	// Message explains the problem.
	Message string
	// Path is the file the problem was found in.
	Path string
}

// Warning returns a warning diagnostic.
func Warning(path, message string) Diagnostic {
	return Diagnostic{Type: "warning", Message: message, Path: path}
}

// Collision returns a name-collision diagnostic.
func Collision(path, message string) Diagnostic {
	return Diagnostic{Type: "collision", Message: message, Path: path}
}

// Load reads one skill file.
//
// A skill without a description is not loaded: the description is the
// only thing that makes a skill discoverable, so a skill without one
// would sit in the library and never be chosen.
//
// A skill whose name or description breaks the specification's limits
// is loaded with a warning. The limits are advisory — a skill with a
// long description still works, it is just not portable.
func Load(path string) (*Skill, []Diagnostic) {
	var diagnostics []Diagnostic

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, []Diagnostic{Warning(path, fmt.Sprintf("cannot read the skill: %v", err))}
	}

	fields, _, err := frontmatter.Parse(string(content))
	if err != nil {
		return nil, []Diagnostic{Warning(path, fmt.Sprintf("cannot parse the frontmatter: %v", err))}
	}

	// A declared skill has no frontmatter at all: there is nothing to
	// route on, so it is not a skill.
	raw, _ := frontmatter.Get(fields, "description")
	description := strings.TrimSpace(raw)
	if description == "" {
		return nil, nil
	}

	dir := filepath.Dir(path)
	name, _ := frontmatter.Get(fields, "name")
	name = strings.TrimSpace(name)
	if name == "" {
		// A skill without a declared name takes the name of the
		// directory holding it.
		name = filepath.Base(dir)
	}

	for _, message := range validateName(name) {
		diagnostics = append(diagnostics, Warning(path, message))
	}
	if len(description) > MaxDescriptionLength {
		diagnostics = append(diagnostics, Warning(path, fmt.Sprintf(
			"the description exceeds %d characters (%d)", MaxDescriptionLength, len(description))))
	}

	disable, _ := frontmatter.Get(fields, "disable-model-invocation")
	disableModelInvocation := strings.EqualFold(strings.TrimSpace(disable), "true")

	return &Skill{
		Name:                  name,
		Description:           description,
		Path:                  path,
		Dir:                   dir,
		DisableModelInvocation: disableModelInvocation,
	}, diagnostics
}

// validateName checks a skill's name against the specification.
//
// The rules are the Agent Skills ones: lowercase letters, numbers and
// hyphens, no hyphen at either end and no two in a row, at most 64
// characters.
func validateName(name string) []string {
	var problems []string
	if len(name) > MaxNameLength {
		problems = append(problems, fmt.Sprintf(
			"the name exceeds %d characters (%d)", MaxNameLength, len(name)))
	}
	if !isValidName(name) {
		problems = append(problems,
			"the name contains invalid characters (lowercase letters, numbers and hyphens only)")
	}
	if strings.HasPrefix(name, "-") {
		problems = append(problems, "the name must not start with a hyphen")
	}
	if strings.HasSuffix(name, "-") {
		problems = append(problems, "the name must not end with a hyphen")
	}
	if strings.Contains(name, "--") {
		problems = append(problems, "the name must not contain consecutive hyphens")
	}
	return problems
}

// isValidName reports whether a name uses only the allowed characters.
func isValidName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-':
		default:
			return false
		}
	}
	return true
}

// Mode selects the discovery rules of a skills directory.
//
// The two modes exist because the two directory kinds have different
// histories: the harness's own directory grew out of pi's, where a bare
// markdown file is a skill, while the specification's directory is the
// portable form, where a skill is always a directory holding a
// SKILL.md.
type Mode int

const (
	// ModeHarness is the harness's own directory: a bare markdown file
	// is a skill too.
	ModeHarness Mode = iota
	// ModeAgents is the specification's directory: only a directory
	// holding a SKILL.md is a skill.
	ModeAgents
)

// LoadDir reads the skills in a directory.
//
// The discovery rules follow the specification:
//
//   - a directory holding a SKILL.md is a skill, and the walk stops
//     there: the skill's own subdirectories are its files, not more
//     skills
//   - otherwise the directory's markdown files are skills (in the
//     harness's mode), and its subdirectories are searched for more
//
// The skills are returned in name order, so the prompt is stable
// between runs.
func LoadDir(dir string) ([]*Skill, []Diagnostic) {
	return LoadDirMode(dir, ModeHarness)
}

// LoadDirMode reads the skills in a directory, using the given mode.
func LoadDirMode(dir string, mode Mode) ([]*Skill, []Diagnostic) {
	var skills []*Skill
	var diagnostics []Diagnostic

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []Diagnostic{Warning(dir, fmt.Sprintf("cannot read the directory: %v", err))}
	}

	// A directory with its own SKILL.md is a skill root: the walk stops
	// at it, so the files it bundles are not mistaken for skills.
	for _, entry := range entries {
		if entry.Name() != FileName || entry.IsDir() {
			continue
		}
		skill, diags := Load(filepath.Join(dir, FileName))
		diagnostics = append(diagnostics, diags...)
		if skill != nil {
			skills = append(skills, skill)
		}
		sortSkills(skills)
		return skills, diagnostics
	}

	for _, entry := range entries {
		name := entry.Name()
		// A hidden directory is not part of the skill library, and
		// node_modules is a dependency tree, not instructions.
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		path := filepath.Join(dir, name)
		if entry.IsDir() {
			sub, diags := LoadDirMode(path, mode)
			skills = append(skills, sub...)
			diagnostics = append(diagnostics, diags...)
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			continue
		}
		// The specification's directory holds the portable form only:
		// a bare markdown file there is not a skill.
		if mode == ModeAgents {
			continue
		}
		skill, diags := Load(path)
		diagnostics = append(diagnostics, diags...)
		if skill != nil {
			skills = append(skills, skill)
		}
	}

	sortSkills(skills)
	return skills, diagnostics
}

// sortSkills puts the skills in name order.
func sortSkills(skills []*Skill) {
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Name != skills[j].Name {
			return skills[i].Name < skills[j].Name
		}
		return skills[i].Path < skills[j].Path
	})
}

// LoadAll reads the skills from every given directory.
//
// The directories are read in order, and the first skill to claim a
// name keeps it: a later directory cannot shadow an earlier one. The
// collision is reported, so the user can see which file won.
//
// The same file reached through two directories — a symlink, or a
// project that lives in the home directory — is loaded once.
//
// Every directory uses the harness's discovery rules.
func LoadAll(dirs []string) ([]*Skill, []Diagnostic) {
	var skills []*Skill
	var diagnostics []Diagnostic

	byName := map[string]*Skill{}
	seen := map[string]bool{}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		found, diags := LoadDir(dir)
		diagnostics = append(diagnostics, diags...)
		for _, skill := range found {
			key := realPath(skill.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			if existing, taken := byName[skill.Name]; taken {
				diagnostics = append(diagnostics, Collision(skill.Path, fmt.Sprintf(
					"the name %q is already taken by %s", skill.Name, existing.Path)))
				continue
			}
			byName[skill.Name] = skill
			skills = append(skills, skill)
		}
	}

	sortSkills(skills)
	return skills, diagnostics
}

// realPath resolves a path through its symlinks.
//
// Two paths to the same file are one skill, not two.
func realPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// Body returns the skill's instructions, without the frontmatter.
//
// The body is what the model reads: the frontmatter is the harness's
// business, not the model's.
func (s *Skill) Body() (string, error) {
	content, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("skills: read %s: %w", s.Path, err)
	}
	_, body, err := frontmatter.Parse(string(content))
	if err != nil {
		// A file whose frontmatter is broken still has a body: the
		// text after the frontmatter is usable.
		body = stripFrontmatter(string(content))
	}
	return body, nil
}

// stripFrontmatter removes the frontmatter block from a document.
//
// It is the fallback for a file the parser cannot read: the body is
// the text after the closing delimiter, or the whole document when
// there is none.
func stripFrontmatter(content string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return strings.TrimSpace(normalized)
	}
	lines := strings.Split(normalized, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	return strings.TrimSpace(normalized)
}
