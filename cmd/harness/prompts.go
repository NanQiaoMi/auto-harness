package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/projcontext"
	"github.com/NanQiaoMi/auto-harness/prompts"
)

// promptSetup wires the prompt templates and the project context files
// into a run.
//
// The templates are markdown files that become slash commands, and the
// context files are the markdown a project keeps to tell the agent how
// to work in it.
//
// Both are optional: a run without either behaves exactly as it did
// before.
type promptSetup struct {
	// registry holds the templates.
	registry *prompts.Registry
	// files are the project's context files.
	files *projcontext.Instructions
	// dirs are the directories the templates came from.
	dirs []string
	// out is where a template's expanded text goes. The TUI replaces
	// it with a writer into its transcript, because the screen owns
	// the terminal.
	out func(string)
}

// loadPrompts reads the templates and the context files.
//
// The templates are read from the project directory and from the
// harness's own directory, so a project can add commands without
// touching the user's own ones.
//
// The context files come from the working directory: they describe the
// project the agent is working in.
func loadPrompts(cfg *config.Config, workdir string, trusted bool) *promptSetup {
	setup := &promptSetup{
		registry: prompts.NewRegistry(),
		files:    &projcontext.Instructions{},
		out: func(line string) {
			fmt.Println(line)
		},
	}

	// The project directory: the templates that belong to this
	// project. They are the project's data, so they wait for the trust
	// decision — unless this is also the user's own directory.
	projectDir := filepath.Join(workdir, ".harness", "prompts")
	// The user's own directory: the templates that follow them
	// everywhere.
	userDir := filepath.Join(config.Dir(), "prompts")
	if projectDir == userDir {
		trusted = true
	}
	// seen keeps a directory from being read twice.
	seen := map[string]bool{}
	for _, dir := range []string{projectDir, userDir} {
		if !trusted && dir == projectDir {
			continue
		}
		// The two directories can be the same one: a project that
		// lives in the home directory is its own user directory. The
		// templates must not be loaded twice.
		if seen[dir] {
			continue
		}
		seen[dir] = true

		templates, err := prompts.LoadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] prompts: %v\n", err)
			continue
		}
		if len(templates) == 0 {
			continue
		}
		setup.dirs = append(setup.dirs, dir)
		for _, template := range templates {
			setup.registry.Add(template)
		}
	}

	// The context files: the project's instructions.
	files, err := projcontext.Load(workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] context: %v\n", err)
	} else {
		setup.files = files
	}

	return setup
}

// install registers the templates as slash commands.
//
// The commands are registered on the same registry the builtins and the
// extensions use, so a template and an extension's command live
// side by side.
//
// The handler expands the template and sends the result to the agent
// as if the user had typed it.
func (p *promptSetup) install(commands *ext.CommandRegistry, ag *agent.Agent) {
	if p == nil || p.registry == nil {
		return
	}
	for _, template := range p.registry.Templates() {
		template := template
		commands.Register(template.Name, &ext.Command{
			Description: template.Description,
			Handler: func(ctx context.Context, args string) error {
				return p.run(ctx, ag, template, args)
			},
		})
	}
}

// run expands a template and sends it to the agent.
//
// The arguments are split with shell quoting rules, so a quoted
// argument stays one argument.
func (p *promptSetup) run(ctx context.Context, ag *agent.Agent, template *prompts.Template, args string) error {
	parsed, err := prompts.SplitArgs(args)
	if err != nil {
		return err
	}
	prompt := prompts.Expand(template.Body, parsed)
	if prompt == "" {
		return fmt.Errorf("the template is empty")
	}
	out := p.out
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

// systemPrompt folds the context files into the system prompt.
func (p *promptSetup) systemPrompt(base string) string {
	if p == nil {
		return base
	}
	return projcontext.Apply(base, p.files)
}

// report prints the templates and the context files.
//
// The command shows where each template came from: a template that
// does not do what it says is usually a template from the wrong
// directory.
func (p *promptSetup) report(out func(string)) {
	if p == nil || p.registry == nil {
		return
	}
	if out == nil {
		out = func(string) {}
	}
	if p.registry.Len() == 0 {
		out("no prompt templates")
	} else {
		out("prompt templates:")
		for _, template := range p.registry.Templates() {
			out(fmt.Sprintf("  /%-14s %s", template.Name, template.Description))
			if template.ArgumentHint != "" {
				out(fmt.Sprintf("  %-16s arguments: %s", "", template.ArgumentHint))
			}
			out(fmt.Sprintf("  %-16s %s", "", template.Path))
		}
	}
	if p.files != nil && p.files.Path != "" {
		out(fmt.Sprintf("context: %s", p.files.Path))
	}
	if p.files != nil && p.files.System != "" {
		out(fmt.Sprintf("system prompt: %s", filepath.Join(filepath.Dir(p.files.Path), projcontext.SystemFile)))
	}
}
