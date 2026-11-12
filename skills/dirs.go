package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The directories the harness looks in.
//
// The names follow the conventions of the tools the harness speaks to:
// the harness's own directories are named after it, and the Agent
// Skills directories are the ones the specification names.
const (
	// ConfigDirName is the harness's own per-project directory.
	ConfigDirName = ".harness"
	// SkillsDirName is the directory holding the skills.
	SkillsDirName = "skills"
	// AgentsDirName is the specification's own per-project directory.
	AgentsDirName = ".agents"
)

// Dir is one skills directory and the mode it is read in.
type Dir struct {
	// Path is the directory.
	Path string
	// Mode selects the discovery rules.
	Mode Mode
}

// Dirs lists the directories the skills are read from, in the order
// they are read.
//
// The order decides a name collision: the first directory to claim a
// name keeps it. The project's own skills come before the user's, so a
// project can add skills without being able to replace the ones the
// user relies on — the same precedence the permission layer uses.
//
// The user directory comes last: a skill the user wrote is the
// fallback, not the override.
func Dirs(workdir, agentDir string) []Dir {
	var dirs []Dir
	seen := map[string]bool{}
	add := func(path string, mode Mode) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		dirs = append(dirs, Dir{Path: path, Mode: mode})
	}

	// The project the agent is working in.
	if workdir != "" {
		add(filepath.Join(workdir, ConfigDirName, SkillsDirName), ModeHarness)
		// The specification's project directory, and the ones above it:
		// a repository's skills apply to every directory under it.
		for _, path := range ancestorAgentsDirs(workdir) {
			add(path, ModeAgents)
		}
	}

	// The user's own skills.
	if agentDir != "" {
		add(filepath.Join(agentDir, SkillsDirName), ModeHarness)
		home, err := os.UserHomeDir()
		if err == nil {
			add(filepath.Join(home, AgentsDirName, SkillsDirName), ModeAgents)
		}
	}
	return dirs
}

// ancestorAgentsDirs returns the specification's project skill
// directories from dir upwards.
//
// The walk stops at the repository's root, where the .git directory
// marks the boundary: the skills above a repository belong to whatever
// encloses it, not to the repository.
func ancestorAgentsDirs(dir string) []string {
	current, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	var dirs []string
	for {
		dirs = append(dirs, filepath.Join(current, AgentsDirName, SkillsDirName))
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return dirs
		}
		parent := filepath.Dir(current)
		if parent == current {
			return dirs
		}
		current = parent
	}
}

// LoadFromDirs reads the skills from the given directories.
//
// The directories are read in order, and the first skill to claim a
// name keeps it. The collisions are reported as diagnostics.
func LoadFromDirs(dirs []Dir) ([]*Skill, []Diagnostic) {
	var skills []*Skill
	var diagnostics []Diagnostic

	byName := map[string]*Skill{}
	seen := map[string]bool{}

	for _, dir := range dirs {
		if dir.Path == "" {
			continue
		}
		found, diags := LoadDirMode(dir.Path, dir.Mode)
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

	SortByName(skills)
	return skills, diagnostics
}

// SortByName sorts the skills by name.
//
// The sort is in place, and the order is the same one the prompt uses:
// name order, then path.
func SortByName(skills []*Skill) {
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Name != skills[j].Name {
			return skills[i].Name < skills[j].Name
		}
		return skills[i].Path < skills[j].Path
	})
}
