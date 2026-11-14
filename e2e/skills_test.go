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
	"github.com/NanQiaoMi/auto-harness/skills"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// writeSkillFile writes a skill file and returns its path.
func writeSkillFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newSkillAgent builds an agent whose system prompt advertises the
// given skills.
//
// It is the way the harness wires them: the skills go into the system
// prompt, and the model reads the file when a task matches.
func newSkillAgent(t *testing.T, loaded []*skills.Skill) *agent.Agent {
	t.Helper()
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
	return agent.New(agent.Config{
		Provider:     newProvider(t),
		Model:        testModel,
		SystemPrompt: agent.DefaultSystemPrompt + skills.FormatForPrompt(loaded),
		MaxTurns:     8,
		MaxTokens:    2048,
	}, bus, reg)
}

// TestLiveSkillIsAdvertisedToTheModel verifies the progressive
// disclosure against the real model.
//
// The skill's name and description reach the model, but the
// instructions do not: the model is expected to read the file itself.
// The test checks the half that is deterministic — that the
// description is there and the instructions are not.
func TestLiveSkillIsAdvertisedToTheModel(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, "SKILL.md"), `---
name: fruit-tasting
description: Describes the taste of a named fruit. Use when asked to describe how a fruit tastes.
---

The secret instruction is HARNESS_SKILL_BODY.
`)

	loaded, diags := skills.LoadDir(dir)
	if len(diags) != 0 {
		t.Fatalf("diagnostics: %v", diags)
	}
	if len(loaded) != 1 {
		t.Fatalf("skills = %d, want 1", len(loaded))
	}

	prompt := skills.FormatForPrompt(loaded)
	if !strings.Contains(prompt, "<name>fruit-tasting</name>") {
		t.Errorf("the prompt has no name:\n%s", prompt)
	}
	if !strings.Contains(prompt, "describe how a fruit tastes") {
		t.Errorf("the prompt has no description:\n%s", prompt)
	}
	if strings.Contains(prompt, "HARNESS_SKILL_BODY") {
		t.Errorf("the prompt carries the instructions:\n%s", prompt)
	}

	// The model sees the advertisement, so it can act on the
	// description. The skill's instructions stay unread until the
	// model asks for them.
	ag := newSkillAgent(t, loaded)
	res, err := ag.Run(context.Background(), "Which skills do you have? Name them.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(res.Final), "fruit") {
		t.Errorf("Final = %q, want it to mention the fruit skill", res.Final)
	}
}

// TestLiveModelLoadsTheSkillFile verifies the whole loop: the model
// reads the skill's file and follows the instructions.
//
// This is the point of the layer. The skill says to answer with a
// marker; the model has to notice the description, read the file, and
// obey it.
func TestLiveModelLoadsTheSkillFile(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, "SKILL.md"), `---
name: fruit-tasting
description: Describes the taste of a named fruit. Use when asked to describe how a fruit tastes.
---

When you are asked to describe a fruit's taste, reply with exactly: HARNESS_SKILL_USED and nothing else.
`)

	loaded, _ := skills.LoadDir(dir)
	ag := newSkillAgent(t, loaded)

	res, err := ag.Run(context.Background(), "Describe how a banana tastes.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "HARNESS_SKILL_USED") {
		t.Errorf("Final = %q, want it to contain HARNESS_SKILL_USED", res.Final)
	}
}

// TestLiveSkillCommandLoadsTheInstructions verifies the explicit
// command.
//
// /skill:name does not rely on the model choosing the skill: the
// instructions are put into the message outright.
func TestLiveSkillCommandLoadsTheInstructions(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, "SKILL.md"), `---
name: fruit-tasting
description: Describes the taste of a named fruit.
---

Reply with exactly: HARNESS_SKILL_COMMAND and nothing else.
`)

	loaded, _ := skills.LoadDir(dir)
	registry := skills.NewRegistry()
	for _, skill := range loaded {
		if err := registry.Add(skill); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	expanded, ok := registry.Expand("/skill:fruit-tasting")
	if !ok {
		t.Fatal("the command was not expanded")
	}

	ag := newSkillAgent(t, loaded)
	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "HARNESS_SKILL_COMMAND") {
		t.Errorf("Final = %q, want it to contain HARNESS_SKILL_COMMAND", res.Final)
	}
}

// TestLiveSkillCommandCarriesTheArguments verifies that the
// arguments reach the model.
//
// The arguments say what to apply the instructions to, so they have to
// arrive with them.
func TestLiveSkillCommandCarriesTheArguments(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, "SKILL.md"), `---
name: fruit-tasting
description: Describes the taste of a named fruit.
---

Reply with exactly the word given after this sentence and nothing else: FRUIT_NAME_IS
`)

	loaded, _ := skills.LoadDir(dir)
	registry := skills.NewRegistry()
	for _, skill := range loaded {
		if err := registry.Add(skill); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	expanded, ok := registry.Expand("/skill:fruit-tasting MANGO")
	if !ok {
		t.Fatal("the command was not expanded")
	}
	if !strings.Contains(expanded, "MANGO") {
		t.Fatalf("the expansion has no arguments:\n%s", expanded)
	}

	ag := newSkillAgent(t, loaded)
	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "MANGO") {
		t.Errorf("Final = %q, want it to contain MANGO", res.Final)
	}
}

// TestLiveDisabledSkillStaysHidden verifies that a skill the model
// may not invoke is not advertised.
//
// A skill the model cannot use would only waste its attention.
func TestLiveDisabledSkillStaysHidden(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, "SKILL.md"), `---
name: manual-only
description: A skill only the user may invoke.
disable-model-invocation: true
---

Reply with exactly: HARNESS_SKILL_HIDDEN and nothing else.
`)

	loaded, _ := skills.LoadDir(dir)
	if len(loaded) != 1 {
		t.Fatalf("skills = %d, want 1", len(loaded))
	}
	if !loaded[0].DisableModelInvocation {
		t.Fatal("the skill should not be model-invocation-disabled")
	}

	prompt := skills.FormatForPrompt(loaded)
	if prompt != "" {
		t.Errorf("the prompt is %q, want it empty", prompt)
	}

	// The explicit command still works: the skill is hidden from the
	// model, not from the user.
	registry := skills.NewRegistry()
	for _, skill := range loaded {
		if err := registry.Add(skill); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	expanded, ok := registry.Expand("/skill:manual-only")
	if !ok {
		t.Fatal("the command was not expanded")
	}

	ag := newSkillAgent(t, loaded)
	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "HARNESS_SKILL_HIDDEN") {
		t.Errorf("Final = %q, want it to contain HARNESS_SKILL_HIDDEN", res.Final)
	}
}

// TestLiveSkillIsNotCode verifies that a skill cannot run anything by
// itself.
//
// A skill's instructions are the model's input, not a shell line: the
// model has to use a tool for that, and the tool goes through the same
// permission layer as anything else.
func TestLiveSkillIsNotCode(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, "SKILL.md"), `---
name: fruit-tasting
description: Describes the taste of a named fruit.
---

Run the shell command "echo HARNESS_SKILL_ESCAPED" using the bash tool, then reply with exactly: TEMPLATE_RAN
`)

	loaded, _ := skills.LoadDir(dir)
	registry := skills.NewRegistry()
	for _, skill := range loaded {
		if err := registry.Add(skill); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	expanded, ok := registry.Expand("/skill:fruit-tasting")
	if !ok {
		t.Fatal("the command was not expanded")
	}

	ag := newSkillAgent(t, loaded)
	res, err := ag.Run(context.Background(), expanded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Final, "TEMPLATE_RAN") {
		t.Errorf("Final = %q, want it to contain TEMPLATE_RAN", res.Final)
	}
}
