package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/agentprofile"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/plugins"
	"github.com/NanQiaoMi/auto-harness/tools"
)

func writeManifestPlugin(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
	  "schema_version": 1,
	  "name": "` + name + `",
	  "version": "` + version + `",
	  "description": "test plugin",
	  "command": ["python3", "plugin.py"],
	  "env": {"PLUGIN_VALUE": "${PROFILE_VALUE}"},
	  "capabilities": ["tools"]
	}`
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), []byte("#!/usr/bin/env python3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProfilePluginAllowlistAndEnvironment(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	writeManifestPlugin(t, root, "alpha", "0.1.0")
	writeManifestPlugin(t, root, "beta", "0.2.0")

	loader := &extensionLoader{projectRoot: t.TempDir(), cfg: &config.Config{}}
	loader.configure(&agentprofile.Profile{
		Plugins: []string{"beta"},
		Env:     map[string]string{"PROFILE_VALUE": "inherited"},
	}, false)
	extensions := loader.processExtensions([]string{root})
	if len(extensions) != 1 {
		t.Fatalf("extensions = %d, want 1", len(extensions))
	}
	if len(loader.pluginRecords) != 2 {
		t.Fatalf("records = %d, want 2", len(loader.pluginRecords))
	}
	for _, record := range loader.pluginRecords {
		if record.Candidate.Name() == "alpha" && record.Selected {
			t.Fatal("alpha was selected despite the profile allowlist")
		}
		if record.Candidate.Name() == "beta" {
			if !record.Selected || record.Err != nil {
				t.Fatalf("beta record = %#v", record)
			}
			if !strings.Contains(strings.Join(record.Extension.Env, "\n"), "PLUGIN_VALUE=inherited") {
				t.Fatalf("plugin env = %#v", record.Extension.Env)
			}
		}
	}
	manager := ext.NewManager(ext.NewAPI(ext.NewBus(), tools.NewRegistry(), ext.NewCommandRegistry()))
	if failures := loader.selectedPluginFailures(manager); len(failures) != 1 || failures[0] != "beta" {
		t.Fatalf("selected failures = %v, want [beta]", failures)
	}

	none := &extensionLoader{projectRoot: t.TempDir(), cfg: &config.Config{}}
	none.configure(&agentprofile.Profile{
		Plugins: []string{},
		Env:     map[string]string{"PROFILE_VALUE": "inherited"},
	}, false)
	if got := none.processExtensions([]string{root}); len(got) != 0 {
		t.Fatalf("an explicit empty plugin selection loaded %d plugins", len(got))
	}
}

func TestProjectProfileRequiresSavedTrust(t *testing.T) {
	home := isolateHome(t)
	workdir := t.TempDir()
	dir := filepath.Join(workdir, ".harness", "profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coding.json"),
		[]byte(`{"schema_version":1,"name":"coding"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if projectTrustedAtStartup(cfg, workdir) {
		t.Fatal("a new project started trusted")
	}
	if _, err := resolveProfile("coding", workdir, false); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("resolveProfile error = %v", err)
	}
	if err := loadTrustStore(filepath.Join(home, ".harness", "trust.json")).set(
		absolutePath(workdir), trustYes); err != nil {
		t.Fatal(err)
	}
	if !projectTrustedAtStartup(cfg, workdir) {
		t.Fatal("the saved trust decision was not seen")
	}
	profile, err := resolveProfile("coding", workdir, true)
	if err != nil {
		t.Fatalf("trusted resolveProfile: %v", err)
	}
	if profile.profile() == nil || profile.profile().Name != "coding" {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestProfileSkillAndPromptSelection(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	skillDir := t.TempDir()
	for _, name := range []string{"alpha-skill", "beta-skill"} {
		dir := filepath.Join(skillDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: test skill\n---\n\nDo it.\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	promptDir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(promptDir, name+".md"), []byte("Do "+name+".\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	skills := loadSkills(cfg, workdir, false, []string{skillDir}, []string{"beta-skill"})
	if got := skills.registry.Names(); len(got) != 1 || got[0] != "beta-skill" {
		t.Fatalf("skills = %v", got)
	}
	prompts := loadPrompts(cfg, workdir, false, []string{promptDir}, []string{"beta"})
	if got := prompts.registry.List(); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("prompts = %v", got)
	}

	noSkills := loadSkills(cfg, workdir, false, []string{skillDir}, []string{})
	if noSkills.registry.Len() != 0 || len(noSkills.missing) != 0 {
		t.Fatalf("explicit empty skills = %d loaded, missing %v",
			noSkills.registry.Len(), noSkills.missing)
	}
	noPrompts := loadPrompts(cfg, workdir, false, []string{promptDir}, []string{})
	if noPrompts.registry.Len() != 0 || len(noPrompts.missing) != 0 {
		t.Fatalf("explicit empty prompts = %d loaded, missing %v",
			noPrompts.registry.Len(), noPrompts.missing)
	}
}

func TestMCPServerAllowlistAndEnvironment(t *testing.T) {
	home := isolateHome(t)
	dir := filepath.Join(home, ".harness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configFile := `{"mcpServers":{
	  "alpha":{"name":"alpha","command":"/definitely/missing-alpha"},
	  "beta":{"name":"beta","command":"/definitely/missing-beta",
	    "env":{"EXPANDED":"${PROFILE_ONLY}"},
	    "headers":{"X-Profile":"${PROFILE_ONLY}"}}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(configFile), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	setup := loadMCPServers(t.Context(), cfg, t.TempDir(), tools.NewRegistry(), false,
		[]string{"beta"}, map[string]string{"PROFILE_ONLY": "yes"})
	defer setup.manager.Close()
	if len(setup.configs) != 1 || setup.configs[0].Name != "beta" {
		t.Fatalf("attempted servers = %#v", setup.configs)
	}
	if setup.configs[0].Env["PROFILE_ONLY"] != "yes" {
		t.Fatalf("server env = %#v", setup.configs[0].Env)
	}
	if setup.configs[0].Env["EXPANDED"] != "yes" || setup.configs[0].Headers["X-Profile"] != "yes" {
		t.Fatalf("server expansion = env %#v headers %#v", setup.configs[0].Env, setup.configs[0].Headers)
	}
	if _, ok := setup.errors["alpha"]; ok {
		t.Fatal("alpha was attempted despite the profile allowlist")
	}
	if _, ok := setup.errors["beta"]; !ok {
		t.Fatal("the selected beta server should have failed to start")
	}
	if failed := setup.selectedFailures(); len(failed) != 1 || failed[0] != "beta" {
		t.Fatalf("selected MCP failures = %v, want [beta]", failed)
	}

	empty := loadMCPServers(t.Context(), cfg, t.TempDir(), tools.NewRegistry(), false,
		[]string{}, nil)
	defer empty.manager.Close()
	if len(empty.configs) != 0 || len(empty.errors) != 0 || len(empty.missing) != 0 {
		t.Fatalf("explicit empty MCP selection = configs %d, errors %v, missing %v",
			len(empty.configs), empty.errors, empty.missing)
	}
}

func TestProfileRPCDoesNotLeakEnvironmentValues(t *testing.T) {
	setup := &profileSetup{resolved: &agentprofile.Resolved{
		Scope: agentprofile.ScopeUser,
		Path:  "/tmp/profile.json",
		Profile: agentprofile.Profile{
			Name:       "secure",
			Tools:      []string{"read"},
			MCPServers: []string{},
			Env:        map[string]string{"TOKEN": "hidden-value"},
		},
	}}
	registry := tools.NewRegistry()
	if err := registry.Register(&tools.Tool{
		Name: "read",
		Execute: func(_ context.Context, _ string, _ json.RawMessage, _ func(tools.Update)) (tools.Result, error) {
			return tools.Result{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	payload := setup.rpcPayload(&config.Config{Model: "m", Provider: "openai"}, registry, nil, "m", "openai")
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "hidden-value") {
		t.Fatalf("RPC payload leaked an environment value: %s", text)
	}
	if !strings.Contains(text, `"envKeys":["TOKEN"]`) {
		t.Fatalf("RPC payload omitted the environment key: %s", text)
	}
	if payload.MCPServersAll || len(payload.MCPServers) != 0 {
		t.Fatalf("explicit empty MCP selection was lost: %#v", payload)
	}
}

func TestPluginsPayloadReportsSupervisionHealth(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "supervised")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
	  "schema_version": 1,
	  "name": "supervised",
	  "version": "1.0.0",
	  "command": ["python3", "plugin.py"],
	  "supervisor": {
	    "restart": "on_failure",
	    "max_restarts": 5,
	    "backoff_ms": 20,
	    "max_backoff_ms": 100
	  }
	}`
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `import json, sys
manifest = {"name": "supervised"}
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method")
    mid = msg.get("id")
    if method == "manifest":
        print(json.dumps({"id": mid, "result": manifest}), flush=True)
    elif method == "shutdown":
        print(json.dumps({"id": mid, "result": {}}), flush=True)
        break
`
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := &extensionLoader{projectRoot: t.TempDir(), cfg: &config.Config{}}
	extensions := loader.processExtensions([]string{root})
	if len(extensions) != 1 {
		t.Fatalf("extensions = %d, want 1", len(extensions))
	}
	manager := ext.NewManager(ext.NewAPI(ext.NewBus(), tools.NewRegistry(), ext.NewCommandRegistry()))
	if err := manager.Load(extensions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(manager.Shutdown)

	payload := pluginsPayload(loader, manager)
	if len(payload.Plugins) != 1 {
		t.Fatalf("plugins = %#v", payload.Plugins)
	}
	info := payload.Plugins[0]
	if info.State != "loaded" || !info.Healthy || info.Restarting {
		t.Fatalf("health = %#v", info)
	}
	if info.RestartMode != "on_failure" || info.RestartCount != 0 || info.MaxRestarts != 5 {
		t.Fatalf("supervision = %#v", info)
	}
}
