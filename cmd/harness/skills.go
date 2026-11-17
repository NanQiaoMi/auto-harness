package main

import (
	"context"
	"fmt"
	"os"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/skills"
)

// skillSetup wires the skills into a run.
//
// A skill is a directory holding a SKILL.md file. At startup only the
// name, the description and the location reach the system prompt; the
// instructions are read when the model asks for them, or when the user
// types /skill:name.
//
// The setup is optional: a run without skills behaves exactly as it did
// before.
type skillSetup struct {
	// registry holds the skills.
	registry *skills.Registry
	// diagnostics are the problems found while loading.
	diagnostics []skills.Diagnostic
	// dirs are the directories the skills came from.
	dirs []string
	// out is where the command output goes. The TUI replaces it with
	// a writer into its transcript, because the screen owns the
	// terminal.
	out func(string)
	// missing are profile-selected names that were not found.
	missing []string
}

// loadSkills reads the skills from the project's and the user's
// directories.
//
// The project comes first, so a project's skill is the one the model
// sees for a name the project defines; the user's own directory is the
// fallback for the names the project does not use. That is the same
// precedence the permission layer gives the user's rules.
func loadSkills(cfg *config.Config, workdir string, trusted bool, customDirs []string, selected []string) *skillSetup {
	setup := &skillSetup{
		registry: skills.NewRegistry(),
		out: func(line string) {
			fmt.Println(line)
		},
	}

	// The project's skills are instructions the model would follow, so
	// they wait for the trust decision like the project's code does.
	skipped := map[string]bool{}
	if !trusted {
		for _, dir := range projectSkillDirs(workdir) {
			skipped[dir.Path] = true
		}
	}

	dirs := profileSkillDirs(customDirs)
	dirs = append(dirs, skills.Dirs(workdir, config.Dir())...)
	dirs = uniqueSkillDirs(dirs)
	selectedSet := map[string]bool{}
	if selected != nil {
		for _, name := range selected {
			selectedSet[name] = true
		}
	}
	for _, dir := range dirs {
		if skipped[dir.Path] {
			continue
		}
		found, diags := skills.LoadFromDirs([]skills.Dir{dir})
		setup.diagnostics = append(setup.diagnostics, diags...)
		if len(found) == 0 {
			continue
		}
		setup.dirs = append(setup.dirs, dir.Path)
		for _, skill := range found {
			if selected != nil && !selectedSet[skill.Name] {
				continue
			}
			delete(selectedSet, skill.Name)
			if err := setup.registry.Add(skill); err != nil {
				setup.diagnostics = append(setup.diagnostics, skills.Collision(skill.Path, err.Error()))
			}
		}
	}
	if selected != nil {
		for _, name := range selected {
			if selectedSet[name] {
				setup.missing = append(setup.missing, name)
			}
		}
	}

	// The warnings are printed where the user can see them: a skill
	// that did not load is otherwise invisible.
	for _, diagnostic := range setup.diagnostics {
		if diagnostic.Type == "collision" {
			fmt.Fprintf(os.Stderr, "[warn] skills: %s: %s\n", diagnostic.Path, diagnostic.Message)
		}
	}
	return setup
}

// profileSkillDirs turns a profile's directories into discovery
// entries. They use the harness's flexible mode, so both SKILL.md
// bundles and a directory of markdown skills work.
func profileSkillDirs(dirs []string) []skills.Dir {
	out := make([]skills.Dir, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, skills.Dir{Path: dir, Mode: skills.ModeHarness})
	}
	return out
}

func uniqueSkillDirs(dirs []skills.Dir) []skills.Dir {
	seen := map[string]bool{}
	out := make([]skills.Dir, 0, len(dirs))
	for _, dir := range dirs {
		if dir.Path == "" || seen[dir.Path] {
			continue
		}
		seen[dir.Path] = true
		out = append(out, dir)
	}
	return out
}

// systemPrompt folds the skills into the system prompt.
//
// Only the name, the description and the location go in. The full
// instructions stay on disk until the model reads the file, which is
// what keeps a large skill library cheap.
func (s *skillSetup) systemPrompt(base string) string {
	if s == nil || s.registry == nil {
		return base
	}
	return base + skills.FormatForPrompt(s.registry.Skills())
}

// install registers the skill commands.
//
// The commands are registered on the same registry the builtins and the
// extensions use, so a skill and an extension's command live side by
// side.
func (s *skillSetup) install(commands *ext.CommandRegistry, ag *agent.Agent) {
	if s == nil || s.registry == nil {
		return
	}
	for _, skill := range s.registry.Skills() {
		skill := skill
		commands.Register(skills.CommandName(skill.Name), &ext.Command{
			Description: skill.Description,
			Handler: func(ctx context.Context, args string) error {
				return s.run(ctx, ag, skill, args)
			},
		})
	}
}

// run sends a skill's instructions to the agent.
//
// The arguments the user typed follow the instructions, so the model
// knows what to apply them to.
func (s *skillSetup) run(ctx context.Context, ag *agent.Agent, skill *skills.Skill, args string) error {
	block, err := skill.Block()
	if err != nil {
		return err
	}
	prompt := block
	if args != "" {
		prompt = block + "\n\n" + args
	}
	out := s.out
	if out != nil {
		out(prompt)
	}
	res, err := ag.Run(ctx, prompt)
	if err != nil {
		return err
	}
	if res.Final != "" && out != nil {
		out(res.Final)
	}
	return nil
}

// expand turns a skill command into the block the model reads.
//
// It is the same expansion the /skill:name command performs, but it
// runs on the input line: a user who types /skill:name by hand gets
// the skill's instructions, and the model never sees the raw command.
func (s *skillSetup) expand(line string) (string, bool) {
	if s == nil || s.registry == nil {
		return line, false
	}
	return s.registry.Expand(line)
}

// report prints the skills and the warnings.
//
// The command shows where each skill came from: a skill that does not
// do what its description says is usually a skill from the wrong
// directory.
func (s *skillSetup) report(out func(string)) {
	if out == nil {
		out = func(string) {}
	}
	if s == nil || s.registry == nil {
		return
	}
	if s.registry.Len() == 0 {
		out("no skills")
	} else {
		out("skills:")
		for _, skill := range s.registry.Skills() {
			out(fmt.Sprintf("  /%-20s %s", skills.CommandName(skill.Name), skill.Description))
			out(fmt.Sprintf("  %-22s %s", "", skill.Path))
		}
	}
	for _, diagnostic := range s.diagnostics {
		out(fmt.Sprintf("  [%s] %s: %s", diagnostic.Type, diagnostic.Path, diagnostic.Message))
	}
	for _, name := range s.missing {
		out(fmt.Sprintf("  [missing] profile selected skill %q", name))
	}
}
