package prompts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mimi/auto-harness/frontmatter"
)

// Template is one prompt template.
type Template struct {
	// Name is the command's name, taken from the file's name.
	Name string
	// Description is the one-line summary shown in the command list.
	// It comes from the frontmatter, or from the body's first
	// non-empty line.
	Description string
	// ArgumentHint shows how the command takes its arguments, e.g.
	// "[focus]". It comes from the frontmatter.
	ArgumentHint string
	// Body is the text that enters the conversation.
	Body string
	// Path is where the template came from.
	Path string
}

// Parse reads a template from a file's contents.
//
// The format is markdown with optional YAML frontmatter:
//
//	---
//	description: Review the staged changes
//	argument-hint: "[focus]"
//	---
//	Review the staged changes. Focus on ${1:-correctness}.
//
// The frontmatter is parsed by the shared frontmatter package: the
// harness ships no external dependency, and the fields are a flat set
// of scalars. A frontmatter block that is not a mapping, or a field
// whose value is not a scalar, is an error rather than a guess.
func Parse(name, path, content string) (*Template, error) {
	template := &Template{
		Name: name,
		Path: path,
	}

	fields, body, err := frontmatter.Parse(content)
	if err != nil {
		return nil, fmt.Errorf("prompts: %s: %w", path, err)
	}
	for _, field := range fields {
		switch field.Key {
		case "description":
			template.Description = field.Value
		case "argument-hint", "argument_hint":
			template.ArgumentHint = field.Value
		default:
			// An unknown field is ignored: a template written for
			// another tool may carry fields this one does not
			// know, and refusing to load it would be worse.
		}
	}

	// The body is trimmed: the text that enters the conversation
	// should not start with the blank line that follows the
	// frontmatter.
	template.Body = strings.TrimSpace(body)
	if template.Description == "" {
		template.Description = firstLine(template.Body)
	}
	return template, nil
}

// firstLine returns the first non-empty line of a text.
func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// A markdown heading is a description like any other, but
		// without the hashes.
		return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
	}
	return ""
}

// Load reads one template file.
func Load(path string) (*Template, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("prompts: read %s: %w", path, err)
	}
	name := templateName(filepath.Base(path))
	if name == "" {
		return nil, fmt.Errorf("prompts: %s: the file name is not a command name", path)
	}
	return Parse(name, path, string(content))
}

// templateName turns a file's name into a command's name.
//
// The extension is dropped, and what is left must be usable as a
// command's name: letters, digits, dashes and underscores.
func templateName(file string) string {
	name := strings.TrimSuffix(file, filepath.Ext(file))
	if name == "" {
		return ""
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return ""
		}
	}
	return name
}

// LoadDir reads every template in a directory.
//
// Only the directory's own markdown files are read: a nested directory
// is not a template, because a nested file's name would not be the
// command's name.
//
// The templates are returned in name order, so the command list is
// stable.
func LoadDir(dir string) ([]*Template, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("prompts: read %s: %w", dir, err)
	}
	var out []*Template
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		template, err := Load(filepath.Join(dir, entry.Name()))
		if err != nil {
			// A file that is not a template is skipped, not fatal: one
			// broken file should not take the others down.
			continue
		}
		out = append(out, template)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Registry holds the templates of a run.
//
// The templates are looked up by name, and the order is stable, so the
// command list does not shuffle between runs.
type Registry struct {
	templates map[string]*Template
	order     []string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{templates: map[string]*Template{}}
}

// Add registers a template.
//
// A name that is taken gets a numeric suffix ("review" -> "review:1"),
// so two templates with the same name both stay reachable.
func (r *Registry) Add(t *Template) string {
	final := t.Name
	for i := 1; ; i++ {
		if _, exists := r.templates[final]; !exists {
			break
		}
		final = fmt.Sprintf("%s:%d", t.Name, i)
	}
	r.templates[final] = t
	r.order = append(r.order, final)
	return final
}

// Get returns a template by name.
func (r *Registry) Get(name string) (*Template, bool) {
	t, ok := r.templates[name]
	return t, ok
}

// Remove drops a template.
func (r *Registry) Remove(name string) bool {
	if _, ok := r.templates[name]; !ok {
		return false
	}
	delete(r.templates, name)
	for i, existing := range r.order {
		if existing == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return true
}

// List returns the templates' names in order.
func (r *Registry) List() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Templates returns the templates in order.
func (r *Registry) Templates() []*Template {
	out := make([]*Template, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.templates[name])
	}
	return out
}

// Len returns the number of templates.
func (r *Registry) Len() int {
	return len(r.order)
}
