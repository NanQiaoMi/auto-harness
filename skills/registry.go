package skills

import (
	"fmt"
	"strings"
)

// Registry holds the skills of a run.
//
// The skills are looked up by name, and the order is stable, so the
// prompt and the command list do not shuffle between runs.
type Registry struct {
	skills map[string]*Skill
	order  []string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{skills: map[string]*Skill{}}
}

// Add registers a skill.
//
// A name that is taken keeps the skill that got there first and
// reports the collision, so a project cannot silently shadow a user's
// own skill.
func (r *Registry) Add(skill *Skill) error {
	if skill == nil {
		return fmt.Errorf("skills: cannot add a nil skill")
	}
	if existing, taken := r.skills[skill.Name]; taken {
		return fmt.Errorf("skills: the name %q is already taken by %s", skill.Name, existing.Path)
	}
	r.skills[skill.Name] = skill
	r.order = append(r.order, skill.Name)
	return nil
}

// Get returns a skill by name.
func (r *Registry) Get(name string) (*Skill, bool) {
	skill, ok := r.skills[name]
	return skill, ok
}

// Remove drops a skill.
func (r *Registry) Remove(name string) bool {
	if _, ok := r.skills[name]; !ok {
		return false
	}
	delete(r.skills, name)
	for i, existing := range r.order {
		if existing == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return true
}

// Names returns the skills' names in order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Skills returns the skills in order.
func (r *Registry) Skills() []*Skill {
	out := make([]*Skill, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.skills[name])
	}
	return out
}

// Len returns the number of skills.
func (r *Registry) Len() int {
	return len(r.order)
}

// CommandPrefix is the prefix of a skill's slash command.
const CommandPrefix = "skill:"

// CommandName returns the slash command that invokes a skill.
func CommandName(skillName string) string {
	return CommandPrefix + skillName
}

// splitCommand splits a command line into the skill's name and the
// arguments that follow it.
//
// The line is the one the user typed, so it carries the leading slash.
func splitCommand(line string) (string, string) {
	trimmed := strings.TrimPrefix(line, "/")
	if !strings.HasPrefix(trimmed, CommandPrefix) {
		return "", ""
	}
	rest := trimmed[len(CommandPrefix):]
	if i := strings.IndexAny(rest, " \t\n"); i >= 0 {
		return rest[:i], strings.TrimSpace(rest[i+1:])
	}
	return rest, ""
}

// Expand turns a skill command into the block the model reads.
//
// The skill's instructions become the message, and the arguments the
// user typed follow as the request the instructions are applied to.
//
// A command naming a skill that does not exist is returned unchanged:
// the model then sees the text the user typed, which is a better
// outcome than a block that silently says nothing.
func (r *Registry) Expand(line string) (string, bool) {
	name, args := splitCommand(line)
	if name == "" {
		return line, false
	}
	skill, ok := r.Get(name)
	if !ok {
		return line, false
	}
	block, err := skill.Block()
	if err != nil {
		return line, false
	}
	if args == "" {
		return block, true
	}
	return block + "\n\n" + args, true
}
