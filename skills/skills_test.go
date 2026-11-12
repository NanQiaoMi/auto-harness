package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill writes a skill file and returns its path.
func writeSkill(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestLoadReadsTheFrontmatter checks the fields of a valid skill.
func TestLoadReadsTheFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", `---
name: pdf-tools
description: Extract text and tables from PDF files.
---

# PDF tools

Read references/formats.md first.
`)

	skill, diags := Load(path)
	if skill == nil {
		t.Fatal("the skill was not loaded")
	}
	if skill.Name != "pdf-tools" {
		t.Errorf("the name is %q", skill.Name)
	}
	if skill.Description != "Extract text and tables from PDF files." {
		t.Errorf("the description is %q", skill.Description)
	}
	if skill.Dir != dir {
		t.Errorf("the directory is %q, want %q", skill.Dir, dir)
	}
	if skill.DisableModelInvocation {
		t.Error("the skill should not be model-invocation-disabled")
	}
	if len(diags) != 0 {
		t.Errorf("diagnostics = %v, want none", diags)
	}
}

// TestLoadTakesTheNameFromTheDirectory checks the fallback when the
// frontmatter declares no name.
func TestLoadTakesTheNameFromTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-skill")
	path := writeSkill(t, dir, "SKILL.md", `---
description: A skill without a declared name.
---

Body.
`)

	skill, _ := Load(path)
	if skill == nil {
		t.Fatal("the skill was not loaded")
	}
	if skill.Name != "my-skill" {
		t.Errorf("the name is %q, want %q", skill.Name, "my-skill")
	}
}

// TestLoadSkipsASkillWithoutADescription checks that a skill with no
// routing signal is not loaded.
//
// The description is the only thing the model sees before it decides
// to read the file, so a skill without one would never be chosen.
func TestLoadSkipsASkillWithoutADescription(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", `---
name: no-description
---

Body.
`)

	skill, _ := Load(path)
	if skill != nil {
		t.Errorf("the skill was loaded: %+v", skill)
	}
}

// TestLoadSkipsAFileWithoutFrontmatter checks that a plain markdown
// file is not a skill.
func TestLoadSkipsAFileWithoutFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", "# Just a document\n\nNo frontmatter here.\n")

	skill, _ := Load(path)
	if skill != nil {
		t.Errorf("the skill was loaded: %+v", skill)
	}
}

// TestLoadWarnsAboutABrokenFrontmatter checks that a malformed
// frontmatter is a warning, not a crash.
func TestLoadWarnsAboutABrokenFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", `---
name: broken
description: [unclosed
---

Body.
`)

	skill, diags := Load(path)
	if skill != nil {
		t.Errorf("the skill was loaded: %+v", skill)
	}
	if len(diags) == 0 {
		t.Error("no diagnostic was reported")
	}
}

// TestLoadWarnsAboutAnInvalidName checks the name rules.
//
// A skill with a bad name is still loaded: the limit is advisory, and
// refusing to load it would lose work the user has done.
func TestLoadWarnsAboutAnInvalidName(t *testing.T) {
	cases := []struct {
		name  string
		want  string
	}{
		{"UPPER", "invalid characters"},
		{"under_score", "invalid characters"},
		{"-leading", "must not start"},
		{"trailing-", "must not end"},
		{"double--hyphen", "consecutive hyphens"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		path := writeSkill(t, dir, "SKILL.md", "---\nname: "+tc.name+"\ndescription: A skill.\n---\n\nBody.\n")

		skill, diags := Load(path)
		if skill == nil {
			t.Errorf("%s: the skill was not loaded", tc.name)
			continue
		}
		found := false
		for _, diagnostic := range diags {
			if strings.Contains(diagnostic.Message, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the diagnostics %v do not mention %q", tc.name, diags, tc.want)
		}
	}
}

// TestLoadWarnsAboutALongName checks the name length limit.
func TestLoadWarnsAboutALongName(t *testing.T) {
	long := strings.Repeat("a", MaxNameLength+1)
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", "---\nname: "+long+"\ndescription: A skill.\n---\n\nBody.\n")

	_, diags := Load(path)
	found := false
	for _, diagnostic := range diags {
		if strings.Contains(diagnostic.Message, "exceeds") {
			found = true
		}
	}
	if !found {
		t.Errorf("the diagnostics %v do not mention the length", diags)
	}
}

// TestLoadReadsTheModelInvocationFlag checks the flag that keeps a
// skill out of the model's reach.
func TestLoadReadsTheModelInvocationFlag(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", `---
name: manual-only
description: A skill only the user can invoke.
disable-model-invocation: true
---

Body.
`)

	skill, _ := Load(path)
	if skill == nil {
		t.Fatal("the skill was not loaded")
	}
	if !skill.DisableModelInvocation {
		t.Error("the skill should not be model-invocation-disabled")
	}
}

// TestBodyStripsTheFrontmatter checks that the model reads the
// instructions, not the harness's metadata.
func TestBodyStripsTheFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", `---
name: pdf-tools
description: A skill.
---

# PDF tools

Do the thing.
`)

	skill, _ := Load(path)
	body, err := skill.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.Contains(body, "description:") {
		t.Errorf("the body still carries the frontmatter: %q", body)
	}
	if !strings.Contains(body, "Do the thing.") {
		t.Errorf("the body is %q", body)
	}
}

// TestLoadDirStopsAtASkillRoot checks the discovery rule.
//
// A directory holding a SKILL.md is a skill, and its subdirectories
// are the skill's own files, not more skills.
func TestLoadDirStopsAtASkillRoot(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "SKILL.md", "---\nname: root-skill\ndescription: The root.\n---\n\nBody.\n")
	writeSkill(t, filepath.Join(dir, "nested"), "SKILL.md", "---\nname: nested-skill\ndescription: Nested.\n---\n\nBody.\n")

	found, _ := LoadDir(dir)
	if len(found) != 1 {
		t.Fatalf("skills = %d, want 1", len(found))
	}
	if found[0].Name != "root-skill" {
		t.Errorf("the skill is %q", found[0].Name)
	}
}

// TestLoadDirFindsNestedSkills checks the discovery rule for a
// directory without a SKILL.md of its own.
func TestLoadDirFindsNestedSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "alpha"), "SKILL.md", "---\nname: alpha\ndescription: The first.\n---\n\nBody.\n")
	writeSkill(t, filepath.Join(dir, "beta"), "SKILL.md", "---\nname: beta\ndescription: The second.\n---\n\nBody.\n")

	found, _ := LoadDir(dir)
	if len(found) != 2 {
		t.Fatalf("skills = %d, want 2", len(found))
	}
	if found[0].Name != "alpha" || found[1].Name != "beta" {
		t.Errorf("the skills are %q and %q", found[0].Name, found[1].Name)
	}
}

// TestLoadDirReadsStandaloneMarkdown checks that a directory of plain
// markdown files is a set of skills.
func TestLoadDirReadsStandaloneMarkdown(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "one.md", "---\ndescription: The first.\n---\n\nBody.\n")
	writeSkill(t, dir, "two.md", "---\ndescription: The second.\n---\n\nBody.\n")

	found, _ := LoadDir(dir)
	if len(found) != 2 {
		t.Fatalf("skills = %d, want 2", len(found))
	}
}

// TestLoadDirSkipsHiddenDirectories checks that a hidden directory is
// not part of the library.
func TestLoadDirSkipsHiddenDirectories(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, ".hidden"), "SKILL.md", "---\nname: hidden\ndescription: Hidden.\n---\n\nBody.\n")
	writeSkill(t, dir, "visible.md", "---\nname: visible\ndescription: Visible.\n---\n\nBody.\n")

	found, _ := LoadDir(dir)
	if len(found) != 1 {
		t.Fatalf("skills = %d, want 1", len(found))
	}
	if found[0].Name != "visible" {
		t.Errorf("the skill is %q", found[0].Name)
	}
}

// TestLoadDirOnAMissingDirectory checks that a directory that is not
// there is not an error.
func TestLoadDirOnAMissingDirectory(t *testing.T) {
	found, diags := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if len(found) != 0 {
		t.Errorf("skills = %d, want 0", len(found))
	}
	if len(diags) != 0 {
		t.Errorf("diagnostics = %v, want none", diags)
	}
}

// TestLoadAllKeepsTheFirstName checks the collision rule.
//
// The first directory to claim a name keeps it, so a project cannot
// silently replace the user's own skill.
func TestLoadAllKeepsTheFirstName(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeSkill(t, first, "SKILL.md", "---\nname: shared\ndescription: The first one.\n---\n\nBody.\n")
	writeSkill(t, second, "SKILL.md", "---\nname: shared\ndescription: The second one.\n---\n\nBody.\n")

	found, diags := LoadAll([]string{first, second})
	if len(found) != 1 {
		t.Fatalf("skills = %d, want 1", len(found))
	}
	if found[0].Description != "The first one." {
		t.Errorf("the description is %q", found[0].Description)
	}
	if len(diags) != 1 || diags[0].Type != "collision" {
		t.Errorf("diagnostics = %v, want one collision", diags)
	}
}

// TestLoadAllSkipsTheSameFileTwice checks that one file reached
// through two directories is loaded once.
func TestLoadAllSkipsTheSameFileTwice(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "SKILL.md", "---\nname: once\ndescription: Once.\n---\n\nBody.\n")

	found, _ := LoadAll([]string{dir, dir})
	if len(found) != 1 {
		t.Fatalf("skills = %d, want 1", len(found))
	}
}

// TestFormatForPromptAdvertisesTheSkills checks the progressive
// disclosure: the prompt holds the name, the description and the
// location, and nothing else.
func TestFormatForPromptAdvertisesTheSkills(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", "---\nname: pdf-tools\ndescription: Extract text from PDFs.\n---\n\nSecret instructions.\n")

	skill, _ := Load(path)
	prompt := FormatForPrompt([]*Skill{skill})

	if !strings.Contains(prompt, "<available_skills>") {
		t.Errorf("the prompt has no skills block:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<name>pdf-tools</name>") {
		t.Errorf("the prompt has no name:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<description>Extract text from PDFs.</description>") {
		t.Errorf("the prompt has no description:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<location>"+path+"</location>") {
		t.Errorf("the prompt has no location:\n%s", prompt)
	}
	if strings.Contains(prompt, "Secret instructions") {
		t.Errorf("the prompt carries the full instructions:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Use the read tool") {
		t.Errorf("the prompt does not tell the model how to load a skill:\n%s", prompt)
	}
}

// TestFormatForPromptWithoutSkills checks that a run without skills
// has no skills section.
func TestFormatForPromptWithoutSkills(t *testing.T) {
	if prompt := FormatForPrompt(nil); prompt != "" {
		t.Errorf("the prompt is %q, want it empty", prompt)
	}
}

// TestFormatForPromptHidesTheDisabledSkills checks that a skill the
// model may not invoke is not advertised.
func TestFormatForPromptHidesTheDisabledSkills(t *testing.T) {
	skill := &Skill{
		Name:                  "manual-only",
		Description:           "Manual.",
		Path:                  "/tmp/manual-only/SKILL.md",
		DisableModelInvocation: true,
	}
	if prompt := FormatForPrompt([]*Skill{skill}); prompt != "" {
		t.Errorf("the prompt is %q, want it empty", prompt)
	}
}

// TestFormatForPromptEscapesTheXML checks that a description with
// markup does not break the block.
func TestFormatForPromptEscapesTheXML(t *testing.T) {
	skill := &Skill{
		Name:        "tricky",
		Description: `A skill with <special> & "characters".`,
		Path:        "/tmp/tricky/SKILL.md",
	}

	prompt := FormatForPrompt([]*Skill{skill})
	if strings.Contains(prompt, "<special>") {
		t.Errorf("the prompt has unescaped markup:\n%s", prompt)
	}
	if !strings.Contains(prompt, "&lt;special&gt;") {
		t.Errorf("the prompt has no escaped markup:\n%s", prompt)
	}
	if !strings.Contains(prompt, "&amp;") {
		t.Errorf("the prompt has no escaped ampersand:\n%s", prompt)
	}
}

// TestBlockHoldsTheInstructions checks the block the /skill:name
// command sends.
func TestBlockHoldsTheInstructions(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", "---\nname: pdf-tools\ndescription: Extract text.\n---\n\nRead references/formats.md first.\n")

	skill, _ := Load(path)
	block, err := skill.Block()
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.Contains(block, `name="pdf-tools"`) {
		t.Errorf("the block has no name:\n%s", block)
	}
	if !strings.Contains(block, "Read references/formats.md first.") {
		t.Errorf("the block has no instructions:\n%s", block)
	}
	if !strings.Contains(block, "References are relative to "+dir) {
		t.Errorf("the block does not say where the references live:\n%s", block)
	}
}

// TestRegistryExpand checks the command expansion.
func TestRegistryExpand(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", "---\nname: pdf-tools\ndescription: Extract text.\n---\n\nDo the work.\n")

	skill, _ := Load(path)
	registry := NewRegistry()
	if err := registry.Add(skill); err != nil {
		t.Fatalf("Add: %v", err)
	}

	expanded, ok := registry.Expand("/skill:pdf-tools extract report.pdf")
	if !ok {
		t.Fatal("the command was not expanded")
	}
	if !strings.Contains(expanded, "Do the work.") {
		t.Errorf("the expansion has no instructions:\n%s", expanded)
	}
	if !strings.Contains(expanded, "extract report.pdf") {
		t.Errorf("the expansion has no arguments:\n%s", expanded)
	}
}

// TestRegistryExpandWithoutArguments checks that the arguments are
// optional.
func TestRegistryExpandWithoutArguments(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "SKILL.md", "---\nname: pdf-tools\ndescription: Extract text.\n---\n\nDo the work.\n")

	skill, _ := Load(path)
	registry := NewRegistry()
	_ = registry.Add(skill)

	expanded, ok := registry.Expand("/skill:pdf-tools")
	if !ok {
		t.Fatal("the command was not expanded")
	}
	if !strings.Contains(expanded, "Do the work.") {
		t.Errorf("the expansion has no instructions:\n%s", expanded)
	}
}

// TestRegistryExpandUnknownSkill checks that a command naming no skill
// is left alone.
//
// The model then sees what the user typed, which is better than a
// block that says nothing.
func TestRegistryExpandUnknownSkill(t *testing.T) {
	registry := NewRegistry()
	line := "/skill:nope do something"

	expanded, ok := registry.Expand(line)
	if ok {
		t.Error("the command was expanded")
	}
	if expanded != line {
		t.Errorf("the line is %q", expanded)
	}
}

// TestRegistryAddRejectsADuplicateName checks the registry's collision
// rule.
func TestRegistryAddRejectsADuplicateName(t *testing.T) {
	registry := NewRegistry()
	first := &Skill{Name: "one", Path: "/a/SKILL.md"}
	second := &Skill{Name: "one", Path: "/b/SKILL.md"}

	if err := registry.Add(first); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := registry.Add(second); err == nil {
		t.Error("the duplicate name was accepted")
	}
	if registry.Len() != 1 {
		t.Errorf("the registry holds %d skills, want 1", registry.Len())
	}
}

// TestDirsFindsTheStandardLocations checks the discovery locations.
func TestDirsFindsTheStandardLocations(t *testing.T) {
	workdir := t.TempDir()
	agentDir := t.TempDir()

	dirs := Dirs(workdir, agentDir)
	want := []string{
		filepath.Join(workdir, ".harness", "skills"),
		filepath.Join(agentDir, "skills"),
	}
	for _, path := range want {
		found := false
		for _, dir := range dirs {
			if dir.Path == path {
				found = true
			}
		}
		if !found {
			t.Errorf("the directories %v do not contain %q", dirs, path)
		}
	}
}

// TestDirsPutTheProjectFirst checks the precedence.
//
// The project's skills come first, so a project that defines a name
// owns it; the user's own directory is the fallback for the names the
// project does not use.
func TestDirsPutTheProjectFirst(t *testing.T) {
	workdir := t.TempDir()
	agentDir := t.TempDir()

	dirs := Dirs(workdir, agentDir)
	if len(dirs) < 2 {
		t.Fatalf("the directories are %v", dirs)
	}
	if dirs[0].Path != filepath.Join(workdir, ".harness", "skills") {
		t.Errorf("the first directory is %q", dirs[0].Path)
	}
}

// TestDirsUseThePortableModeForTheAgentsDirectories checks the mode of
// each directory.
//
// The specification's directories hold the portable form only: a skill
// there is a directory with a SKILL.md, never a bare markdown file.
func TestDirsUseThePortableModeForTheAgentsDirectories(t *testing.T) {
	workdir := t.TempDir()
	agentDir := t.TempDir()

	for _, dir := range Dirs(workdir, agentDir) {
		switch {
		case filepath.Base(dir.Path) == SkillsDirName &&
			filepath.Base(filepath.Dir(dir.Path)) == AgentsDirName:
			if dir.Mode != ModeAgents {
				t.Errorf("%q has mode %v, want the portable one", dir.Path, dir.Mode)
			}
		default:
			if dir.Mode != ModeHarness {
				t.Errorf("%q has mode %v, want the harness one", dir.Path, dir.Mode)
			}
		}
	}
}

// TestLoadDirModeSkipsBareFilesInThePortableMode checks the
// specification's discovery rule.
//
// A bare markdown file in a .agents/skills directory is not a skill:
// the portable form is a directory holding a SKILL.md.
func TestLoadDirModeSkipsBareFilesInThePortableMode(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "one.md", "---\nname: one\ndescription: The first.\n---\n\nBody.\n")
	writeSkill(t, filepath.Join(dir, "two"), "SKILL.md", "---\nname: two\ndescription: The second.\n---\n\nBody.\n")

	found, _ := LoadDirMode(dir, ModeAgents)
	if len(found) != 1 {
		t.Fatalf("skills = %d, want 1", len(found))
	}
	if found[0].Name != "two" {
		t.Errorf("the skill is %q", found[0].Name)
	}
}
