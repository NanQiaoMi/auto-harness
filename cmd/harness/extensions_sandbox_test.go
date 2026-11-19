package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
)

// TestPluginSandboxCoversEveryProcessExtension checks that the switch
// reaches both ways a process extension is created.
//
// A sandbox that covered only discovered plugins would leave the single
// extension_command path — the one a user configures by hand — running
// unconfined, and the setting would read like coverage it does not
// provide.
func TestPluginSandboxCoversEveryProcessExtension(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	writeManifestPlugin(t, root, "confined", "1.0.0")

	loader := &extensionLoader{
		projectRoot:    t.TempDir(),
		cfg:            &config.Config{ExtensionDirs: []string{root}, ExtensionCommand: "python3 ext.py"},
		sandboxPlugins: true,
		requireSandbox: true,
		sandboxNetwork: true,
	}
	loader.pluginEnv = map[string]string{"PROFILE_VALUE": "test"}
	exts := loader.build()
	if len(exts) < 2 {
		t.Fatalf("extensions = %d, want the discovered plugin and the explicit command", len(exts))
	}

	var sawPlugin, sawCommand bool
	for _, extension := range exts {
		pe, ok := extension.(*ext.ProcessExtension)
		if !ok {
			continue
		}
		if !pe.Sandboxed() {
			t.Errorf("%s runs unconfined under sandbox_plugins", pe.Name())
			continue
		}
		if !pe.RequireSandbox {
			t.Errorf("%s does not carry sandbox_plugins_require", pe.Name())
		}
		profile := pe.SandboxProfile()
		if profile == nil || !profile.AllowNetwork {
			t.Errorf("%s did not get the network setting", pe.Name())
		}
		// Both are python3; the explicit command carries the script
		// as its argument.
		if len(pe.Args) == 1 && pe.Args[0] == "ext.py" {
			sawCommand = true
		} else {
			sawPlugin = true
		}
	}
	if !sawPlugin || !sawCommand {
		t.Errorf("plugin=%v command=%v, want both paths covered", sawPlugin, sawCommand)
	}
}

// TestPluginSandboxIsOffByDefault checks that the historical behavior is
// unchanged: plugins run with the harness's own access unless asked
// otherwise.
func TestPluginSandboxIsOffByDefault(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	writeManifestPlugin(t, root, "plain", "1.0.0")

	loader := &extensionLoader{
		projectRoot: t.TempDir(),
		cfg:         &config.Config{ExtensionDirs: []string{root}},
	}
	loader.pluginEnv = map[string]string{"PROFILE_VALUE": "test"}
	for _, extension := range loader.build() {
		pe, ok := extension.(*ext.ProcessExtension)
		if !ok {
			continue
		}
		if pe.Sandboxed() {
			t.Errorf("%s was sandboxed without being asked", pe.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "plain", "plugin.py")); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}
