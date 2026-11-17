package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NanQiaoMi/auto-harness/agentprofile"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/tools"
)

func TestLoadExtensionSetContinuesAfterFailure(t *testing.T) {
	registry := tools.NewRegistry()
	manager := ext.NewManager(ext.NewAPI(ext.NewBus(), registry, ext.NewCommandRegistry()))
	failing := ext.Named("broken", func(*ext.API) error {
		return errors.New("broken plugin")
	})
	working := ext.Named("working", func(api *ext.API) error {
		return api.RegisterTool(&tools.Tool{
			Name: "working_tool",
			Execute: func(context.Context, string, json.RawMessage, func(tools.Update)) (tools.Result, error) {
				return tools.Result{}, nil
			},
		})
	})

	err := loadExtensionSet(manager, []ext.Extension{failing, working})
	if err == nil {
		t.Fatal("the broken extension was not reported")
	}
	if _, ok := registry.Get("working_tool"); !ok {
		t.Fatal("the working extension was skipped after the broken one")
	}
}

func TestProjectProfilePluginDirsWaitForTrust(t *testing.T) {
	isolateHome(t)
	workdir := t.TempDir()
	pluginRoot := filepath.Join(t.TempDir(), "profile-plugins")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifestPlugin(t, pluginRoot, "profile-plugin", "1.0.0")

	loader := &extensionLoader{projectRoot: workdir, cfg: &config.Config{}}
	loader.configure(&agentprofile.Profile{
		PluginDirs: []string{pluginRoot},
		Plugins:    []string{"profile-plugin"},
		Env:        map[string]string{"PROFILE_VALUE": "trusted"},
	}, true)
	loader.build()
	for _, record := range loader.pluginRecords {
		if record.Candidate.Name() == "profile-plugin" {
			t.Fatal("a project profile plugin loaded before trust")
		}
	}

	loader.trusted = true
	loader.buildProject()
	loaded := false
	for _, record := range loader.pluginRecords {
		if record.Candidate.Name() == "profile-plugin" {
			loaded = true
		}
	}
	if !loaded {
		t.Fatal("a trusted project profile plugin was not discovered")
	}
}

func TestManagedPluginDirectoryIsDiscovered(t *testing.T) {
	home := isolateHome(t)
	managed := filepath.Join(home, ".harness", "plugins")
	writeManifestPlugin(t, managed, "managed", "1.2.3")

	loader := &extensionLoader{projectRoot: t.TempDir(), cfg: &config.Config{}}
	loader.configure(&agentprofile.Profile{
		Plugins: []string{"managed"},
		Env:     map[string]string{"PROFILE_VALUE": "managed"},
	}, false)
	extensions := loader.processExtensions(loader.trustedDirs())
	if len(extensions) != 1 {
		t.Fatalf("managed plugins = %d, want 1", len(extensions))
	}
	if len(loader.pluginRecords) != 1 || loader.pluginRecords[0].Candidate.Name() != "managed" {
		t.Fatalf("records = %+v", loader.pluginRecords)
	}
}
