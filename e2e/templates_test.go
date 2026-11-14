//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/projcontext"
	"github.com/NanQiaoMi/auto-harness/prompts"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// TestLivePromptTemplateFeedsTheModel verifies that a prompt template
// reaches the model with its arguments substituted.
//
// The template is a markdown file that becomes a slash command. What
// the model sees is the template's body with the arguments expanded,
// so the substituted value has to arrive.
func TestLivePromptTemplateFeedsTheModel(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "greet.md")
	body := "Say hello to ${1:-nobody}. Reply with exactly: HELLO_" + "${1:-nobody}"
	content := "---\ndescription: greet someone\nargument-hint: \"[name]\"\n---\n" + body
	if err := os.WriteFile(templatePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write the template: %v", err)
	}

	loaded, err := prompts.LoadDir(dir)
	if err != nil {
		t.Fatalf("load the templates: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("templates = %d, want 1", len(loaded))
	}

	registry := prompts.NewRegistry()
	registry.Add(loaded[0])

	// The agent runs the expanded template, the way the REPL does.
	isolate(t)
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	ag := agent.New(agent.Config{
		Provider:  newProvider(t),
		Model:     testModel,
		MaxTurns:  6,
		MaxTokens: 1024,
	}, bus, reg)

	template := loaded[0]
	args, err := prompts.SplitArgs("WIDGET")
	if err != nil {
		t.Fatalf("split the arguments: %v", err)
	}
	expanded := prompts.Expand(template.Body, args)
	if !strings.Contains(expanded, "HELLO_WIDGET") {
		t.Fatalf("the expanded template is %q, want it to contain HELLO_WIDGET", expanded)
	}

	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "HELLO_WIDGET") {
		t.Errorf("Final = %q, want it to contain HELLO_WIDGET", res.Final)
	}
}

// TestLivePromptTemplateDefaultArgument verifies the default form
// against the real model.
//
// A template that says ${1:-nobody} must send "nobody" when no
// argument is given: the default is what the model sees.
func TestLivePromptTemplateDefaultArgument(t *testing.T) {
	ag, _, _ := newAgent(t)

	expanded := prompts.Expand("Reply with exactly: HELLO_${1:-nobody}", nil)
	if expanded != "Reply with exactly: HELLO_nobody" {
		t.Fatalf("the expanded template is %q", expanded)
	}

	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "HELLO_nobody") {
		t.Errorf("Final = %q, want it to contain HELLO_nobody", res.Final)
	}
}

// TestLiveContextFileReachesTheModel verifies that the project's
// instructions reach the model.
//
// The context file is folded into the system prompt, so the model
// answers the way it was told to.
func TestLiveContextFileReachesTheModel(t *testing.T) {
	dir := t.TempDir()
	instructions := filepath.Join(dir, projcontext.InstructionsFile)
	content := "Reply to every question with exactly the word BANANA and nothing else."
	if err := os.WriteFile(instructions, []byte(content), 0o644); err != nil {
		t.Fatalf("write the instructions: %v", err)
	}

	files, err := projcontext.Load(dir)
	if err != nil {
		t.Fatalf("load the context: %v", err)
	}
	if files.Body == "" {
		t.Fatal("the instructions were not loaded")
	}

	systemPrompt := projcontext.Apply(agent.DefaultSystemPrompt, files)
	if !strings.Contains(systemPrompt, "BANANA") {
		t.Fatalf("the system prompt does not carry the instructions: %q", systemPrompt)
	}

	isolate(t)
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	ag := agent.New(agent.Config{
		Provider:     newProvider(t),
		Model:        testModel,
		SystemPrompt: systemPrompt,
		MaxTurns:     6,
		MaxTokens:    1024,
	}, bus, reg)

	res, err := ag.Run(context.Background(), "What is the weather today?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res.Final), "BANANA") {
		t.Errorf("Final = %q, want it to answer BANANA", res.Final)
	}
}

// TestLiveSystemFileReplacesThePrompt verifies that the system file
// replaces the prompt outright.
//
// The system file is the whole system prompt here, so the model has
// nothing but this instruction to go on. The instruction says so
// explicitly: a bare "reply with X" leaves the model free to look
// around the working directory first, and then it answers with
// whatever it found instead of the marker.
func TestLiveSystemFileReplacesThePrompt(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, projcontext.SystemFile)
	content := "You are a test fixture. Do not use any tools. " +
		"Reply to the user's message with exactly: SYSTEM_FILE_USED and nothing else."
	if err := os.WriteFile(system, []byte(content), 0o644); err != nil {
		t.Fatalf("write the system file: %v", err)
	}

	files, err := projcontext.Load(dir)
	if err != nil {
		t.Fatalf("load the context: %v", err)
	}
	systemPrompt := projcontext.Apply(agent.DefaultSystemPrompt, files)
	if systemPrompt != content {
		t.Fatalf("the system prompt is %q, want %q", systemPrompt, content)
	}

	isolate(t)
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	for _, tool := range []*tools.Tool{
		tools.NewBashTool(), tools.NewReadTool(), tools.NewWriteTool(), tools.NewEditTool(),
	} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	ag := agent.New(agent.Config{
		Provider:     newProvider(t),
		Model:        testModel,
		SystemPrompt: systemPrompt,
		MaxTurns:     6,
		MaxTokens:    1024,
	}, bus, reg)

	// The turn does not ask for anything else: the system file
	// replaced the prompt outright, so the model has only the
	// file's instruction to go on.
	res, err := ag.Run(context.Background(), "Go ahead.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "SYSTEM_FILE_USED") {
		t.Errorf("Final = %q, want it to contain SYSTEM_FILE_USED", res.Final)
	}
}

// TestLiveTemplateIsNotCode checks that a template cannot run anything.
//
// The templates are data: a template that looks like a command is just
// text the model reads.
func TestLiveTemplateIsNotCode(t *testing.T) {
	ag, _, _ := newAgent(t)

	// The template's body asks for a command to be run. The harness
	// does not run it: the body is the model's input, not a shell
	// line, so the model has to use the tool for that.
	body := "Run the shell command `echo HARNESS_TEMPLATE_ESCAPED` using the bash tool, " +
		"then reply with exactly: TEMPLATE_RAN"
	expanded := prompts.Expand(body, nil)
	if expanded != body {
		t.Fatalf("the expansion changed the body: %q", expanded)
	}

	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "TEMPLATE_RAN") {
		t.Errorf("Final = %q, want it to contain TEMPLATE_RAN", res.Final)
	}
}

// TestLiveQuotedArgumentStaysOneArgument checks the shell-style
// splitting against the real model.
//
// A quoted argument is one argument, so the substitution puts the
// whole phrase in the prompt.
func TestLiveQuotedArgumentStaysOneArgument(t *testing.T) {
	ag, _, _ := newAgent(t)

	args, err := prompts.SplitArgs(`"API compatibility"`)
	if err != nil {
		t.Fatalf("split the arguments: %v", err)
	}
	if len(args) != 1 || args[0] != "API compatibility" {
		t.Fatalf("the arguments are %v, want one argument", args)
	}

	expanded := prompts.Expand("Reply with exactly: TOPIC_${1}", args)
	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "TOPIC_API compatibility") {
		t.Errorf("Final = %q, want it to contain TOPIC_API compatibility", res.Final)
	}
}
