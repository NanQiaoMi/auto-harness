package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/sandbox"
	"github.com/NanQiaoMi/auto-harness/tools"
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

func TestPerPluginSandboxPolicyAndReport(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	dir := filepath.Join(root, "ro-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
	  "schema_version": 1,
	  "name": "ro-plugin",
	  "version": "1.0.0",
	  "command": ["python3", "plugin.py"],
	  "sandbox": {
	    "write_paths": [],
	    "allow_network": true
	  }
	}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Self-opting plugin is confined even when global sandboxPlugins is false,
	// and cannot escalate allow_network over host sandboxNetwork=false.
	loader := &extensionLoader{
		projectRoot: t.TempDir(),
		cfg:         &config.Config{ExtensionDirs: []string{root}},
	}
	exts := loader.build()
	var procs []*ext.ProcessExtension
	for _, e := range exts {
		if p, ok := e.(*ext.ProcessExtension); ok {
			procs = append(procs, p)
		}
	}
	if len(procs) != 1 {
		t.Fatalf("process extensions = %d, want 1", len(procs))
	}
	pe := procs[0]
	if !pe.Sandboxed() {
		t.Fatal("plugin declaring sandbox in plugin.json should be confined")
	}
	prof := pe.SandboxProfile()
	if prof == nil || len(prof.WritePaths) != 0 {
		t.Fatalf("WritePaths = %#v, want empty read-only", prof)
	}
	if prof.AllowNetwork {
		t.Fatal("plugin escalated allow_network over host false")
	}

	// Verify /plugins report prints backend name and profile summary.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	mgr := ext.NewManager(ext.NewAPI(ext.NewBus(), tools.NewRegistry(), ext.NewCommandRegistry()))
	loader.report(mgr)
	_ = w.Close()
	os.Stdout = origStdout
	outBytes, _ := io.ReadAll(r)
	_ = r.Close()
	out := string(outBytes)
	if !strings.Contains(out, "sandbox ("+sandbox.BackendName()+"):") {
		t.Errorf("report missing backend name %q:\n%s", sandbox.BackendName(), out)
	}
}
