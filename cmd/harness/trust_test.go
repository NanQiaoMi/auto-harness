package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
)

// isolateHome points HOME at a temporary directory, so the trust store
// and the user's own resources stay out of the real home.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// writeExecutable writes an executable file, which is what extension
// discovery looks for.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// writeProjectSkill puts a valid skill under the project's directory.
func writeProjectSkill(t *testing.T, workdir, name string) {
	t.Helper()
	dir := filepath.Join(workdir, ".harness", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "---\nname: " + name + "\ndescription: A project skill.\n---\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestTrustStoreRoundtrip(t *testing.T) {
	isolateHome(t)
	path := trustStorePath()
	store := loadTrustStore(path)
	if store.decision("/some/project") != "" {
		t.Fatal("an empty store returned a decision")
	}

	if err := store.set("/some/project", trustYes); err != nil {
		t.Fatalf("set: %v", err)
	}
	reloaded := loadTrustStore(path)
	if got := reloaded.decision("/some/project"); got != trustYes {
		t.Errorf("decision = %q, want yes", got)
	}

	if err := reloaded.set("/some/project", ""); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := loadTrustStore(path).decision("/some/project"); got != "" {
		t.Errorf("decision after reset = %q, want none", got)
	}
}

func TestTrustStoreIgnoresAMalformedFile(t *testing.T) {
	isolateHome(t)
	path := trustStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := loadTrustStore(path)
	if len(store.Projects) != 0 {
		t.Errorf("a malformed file produced %d decisions, want none", len(store.Projects))
	}
}

func TestProjectResources(t *testing.T) {
	isolateHome(t)

	empty := t.TempDir()
	if got := projectResources(empty); len(got) != 0 {
		t.Errorf("an empty project has resources: %+v", got)
	}

	workdir := t.TempDir()
	writeExecutable(t, filepath.Join(workdir, ".harness", "extensions", "demo"))
	if err := os.WriteFile(filepath.Join(workdir, ".harness", "mcp.json"), []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workdir, ".harness", "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".harness", "prompts", "hello.md"), []byte("Hello."), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeProjectSkill(t, workdir, "demo-skill")

	labels := map[string]bool{}
	for _, r := range projectResources(workdir) {
		labels[r.Label] = true
	}
	for _, want := range []string{"extensions", "MCP servers", "prompt templates", "skills"} {
		if !labels[want] {
			t.Errorf("resource %q was not found; got %v", want, labels)
		}
	}
}

func TestResolveTrustOrder(t *testing.T) {
	home := isolateHome(t)
	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, ".harness", "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".harness", "prompts", "hello.md"), []byte("Hello."), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := absolutePath(workdir)
	storePath := filepath.Join(home, ".harness", "trust.json")
	cfg := &config.Config{}

	t.Run("extension decision wins and is remembered", func(t *testing.T) {
		bus := ext.NewBus()
		bus.On(ext.EventProjectTrust, func(ev *ext.Event) error {
			if ev.Cwd != dir {
				t.Errorf("cwd = %q, want %q", ev.Cwd, dir)
			}
			ev.Trusted = trustYes
			ev.Remember = true
			return nil
		})
		setup := resolveProjectTrust(cfg, workdir, bus, false, nil)
		if !setup.decision.Trusted || setup.decision.Source != "extension" {
			t.Fatalf("decision = %+v, want trusted by an extension", setup.decision)
		}
		if got := loadTrustStore(storePath).decision(dir); got != trustYes {
			t.Errorf("stored decision = %q, want yes", got)
		}
	})

	t.Run("an extension's no beats a saved yes", func(t *testing.T) {
		if err := loadTrustStore(storePath).set(dir, trustYes); err != nil {
			t.Fatalf("seed: %v", err)
		}
		bus := ext.NewBus()
		bus.On(ext.EventProjectTrust, func(ev *ext.Event) error {
			ev.Trusted = trustNo
			return nil
		})
		setup := resolveProjectTrust(cfg, workdir, bus, false, nil)
		if setup.decision.Trusted || setup.decision.Source != "extension" {
			t.Fatalf("decision = %+v, want untrusted by an extension", setup.decision)
		}
	})

	t.Run("a saved decision is used", func(t *testing.T) {
		if err := loadTrustStore(storePath).set(dir, trustYes); err != nil {
			t.Fatalf("seed: %v", err)
		}
		setup := resolveProjectTrust(&config.Config{}, workdir, ext.NewBus(), false, nil)
		if !setup.decision.Trusted || setup.decision.Source != "saved" {
			t.Fatalf("decision = %+v, want the saved yes", setup.decision)
		}
	})

	t.Run("trust_project in the config trusts every project", func(t *testing.T) {
		if err := loadTrustStore(storePath).set(dir, ""); err != nil {
			t.Fatalf("reset: %v", err)
		}
		setup := resolveProjectTrust(&config.Config{TrustProject: true}, workdir, ext.NewBus(), false, nil)
		if !setup.decision.Trusted || setup.decision.Source != "config" {
			t.Fatalf("decision = %+v, want the config to trust it", setup.decision)
		}
	})

	t.Run("with nobody to ask the project stays untrusted", func(t *testing.T) {
		if err := loadTrustStore(storePath).set(dir, ""); err != nil {
			t.Fatalf("reset: %v", err)
		}
		setup := resolveProjectTrust(&config.Config{}, workdir, ext.NewBus(), false, nil)
		if setup.decision.Trusted || setup.decision.Source != "not asked" {
			t.Fatalf("decision = %+v, want an untrusted project", setup.decision)
		}
	})

	t.Run("an interactive answer is remembered", func(t *testing.T) {
		if err := loadTrustStore(storePath).set(dir, ""); err != nil {
			t.Fatalf("reset: %v", err)
		}
		called := false
		ask := func(question string) bool {
			called = true
			if question == "" {
				t.Error("the question is empty")
			}
			return true
		}
		setup := resolveProjectTrust(&config.Config{}, workdir, ext.NewBus(), true, ask)
		if !called {
			t.Fatal("the user was not asked")
		}
		if !setup.decision.Trusted || setup.decision.Source != "prompt" {
			t.Fatalf("decision = %+v, want the prompt to trust it", setup.decision)
		}
		if got := loadTrustStore(storePath).decision(dir); got != trustYes {
			t.Errorf("stored decision = %q, want yes", got)
		}
	})

	t.Run("a project without resources is not asked about", func(t *testing.T) {
		plain := t.TempDir()
		ask := func(string) bool {
			t.Error("a project without resources was asked about")
			return false
		}
		setup := resolveProjectTrust(&config.Config{}, plain, ext.NewBus(), true, ask)
		if setup.decision.Trusted || setup.decision.Source != "no resources" {
			t.Fatalf("decision = %+v, want no resources", setup.decision)
		}
	})
}

func TestProjectExtensionsWaitForTrust(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	extensionPath := filepath.Join(workdir, ".harness", "extensions", "demo")
	writeExecutable(t, extensionPath)

	loader := &extensionLoader{projectRoot: workdir, cfg: &config.Config{}}
	for _, e := range loader.build() {
		if e.Name() == extensionPath {
			t.Fatal("the project extension loaded before the trust decision")
		}
	}
	if got := loader.buildProject(); len(got) != 0 {
		t.Fatalf("an untrusted project produced %d project extensions, want none", len(got))
	}

	loader.trusted = true
	project := loader.buildProject()
	if len(project) != 1 || project[0].Name() != extensionPath {
		t.Fatalf("project extensions = %v, want %s", names(project), extensionPath)
	}
	// The reload path builds both scopes at once.
	found := false
	for _, e := range loader.build() {
		if e.Name() == extensionPath {
			found = true
		}
	}
	if !found {
		t.Error("a trusted project's extension is missing from the reload set")
	}
}

// names returns the extension names, for error messages.
func names(exts []ext.Extension) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		out = append(out, e.Name())
	}
	return out
}

func TestSkillsWaitForTrust(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	writeProjectSkill(t, workdir, "demo-skill")

	cfg := &config.Config{}
	untrusted := loadSkills(cfg, workdir, false)
	if got := untrusted.registry.Len(); got != 0 {
		t.Errorf("an untrusted project loaded %d skills, want none", got)
	}
	trusted := loadSkills(cfg, workdir, true)
	if got := trusted.registry.Len(); got != 1 {
		t.Errorf("a trusted project loaded %d skills, want 1", got)
	}
}

func TestPromptsWaitForTrust(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	dir := filepath.Join(workdir, ".harness", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.md"), []byte("Say hello.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &config.Config{}
	if got := len(loadPrompts(cfg, workdir, false).registry.Templates()); got != 0 {
		t.Errorf("an untrusted project loaded %d templates, want none", got)
	}
	if got := len(loadPrompts(cfg, workdir, true).registry.Templates()); got != 1 {
		t.Errorf("a trusted project loaded %d templates, want 1", got)
	}
}

func TestTrustCommandSavesDecisions(t *testing.T) {
	isolateHome(t)
	setup := &trustSetup{
		store: loadTrustStore(trustStorePath()),
		decision: trustDecision{
			Dir: "/some/project",
		},
	}

	msg, err := setup.apply("yes")
	if err != nil {
		t.Fatalf("apply yes: %v", err)
	}
	if msg == "" {
		t.Error("the command reported nothing")
	}
	if got := loadTrustStore(setup.store.Path).decision("/some/project"); got != trustYes {
		t.Errorf("stored decision = %q, want yes", got)
	}

	if _, err := setup.apply("reset"); err != nil {
		t.Fatalf("apply reset: %v", err)
	}
	if got := loadTrustStore(setup.store.Path).decision("/some/project"); got != "" {
		t.Errorf("decision after reset = %q, want none", got)
	}

	if _, err := setup.apply("maybe"); err == nil {
		t.Error("an unknown argument was accepted")
	}
}
