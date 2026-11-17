package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlugin(t *testing.T, root, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestManifestExtensionAndDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PLUGIN_TOKEN", "secret")
	writePlugin(t, root, "pdf-tools", `{
	  "schema_version": 1,
	  "name": "pdf-tools",
	  "version": "0.1.0",
	  "description": "PDF tools",
	  "command": ["python3", "plugin.py"],
	  "cwd": "${pluginDir}/.",
	  "env": {"TOKEN": "${PLUGIN_TOKEN}"},
	  "capabilities": ["tools", "hooks"]
	}`)
	// A legacy executable in a manifest-free directory.
	legacyDir := filepath.Join(root, "legacy")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, "old-plugin")
	if err := os.WriteFile(legacy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	candidates, errs := Discover([]string{root})
	if len(errs) != 0 {
		t.Fatalf("diagnostics: %v", errs)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2: %#v", len(candidates), candidates)
	}
	var manifest Candidate
	for _, candidate := range candidates {
		if candidate.Name() == "pdf-tools" {
			manifest = candidate
		}
	}
	if manifest.Manifest.Name != "pdf-tools" {
		t.Fatalf("manifest plugin missing: %#v", candidates)
	}
	extension, err := manifest.Extension(map[string]string{"PROFILE_VALUE": "yes"})
	if err != nil {
		t.Fatalf("Extension: %v", err)
	}
	if extension.Dir != manifest.Dir {
		t.Fatalf("Dir = %q, want %q", extension.Dir, manifest.Dir)
	}
	if !contains(extension.Env, "TOKEN=secret") || !contains(extension.Env, "PROFILE_VALUE=yes") {
		t.Fatalf("Env = %#v", extension.Env)
	}
	if !contains(extension.Env, "HARNESS_PLUGIN_NAME=pdf-tools") {
		t.Fatalf("reserved env missing: %#v", extension.Env)
	}
	if len(extension.Capabilities) != 2 || extension.Capabilities[0] != "tools" || extension.Capabilities[1] != "hooks" {
		t.Fatalf("capabilities = %#v", extension.Capabilities)
	}
}

func TestManifestRejectsCWDEscape(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, root, "escape", `{
	  "schema_version": 1,
	  "name": "escape",
	  "version": "1",
	  "command": ["python3", "plugin.py"],
	  "cwd": "../"
	}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestManifestRejectsUnsafeExecutableSymlink(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, root, "escape-command", `{
	  "schema_version": 1,
	  "name": "escape-command",
	  "version": "1",
	  "command": ["./run"]
	}`)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "run")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestDiscoveryDefersLaunchEnvironment(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, root, "deferred", `{
	  "schema_version": 1,
	  "name": "deferred",
	  "version": "1",
	  "command": ["python3", "plugin.py"],
	  "env": {"TOKEN": "${ONLY_NEEDED_WHEN_SELECTED}"}
	}`)
	candidates, errs := Discover([]string{root})
	if len(candidates) != 1 || len(errs) != 0 {
		t.Fatalf("discovery = %d candidates, errors %v", len(candidates), errs)
	}
	if _, err := candidates[0].Extension(nil); err == nil ||
		!strings.Contains(err.Error(), "ONLY_NEEDED_WHEN_SELECTED") {
		t.Fatalf("selected launch error = %v", err)
	}
	if _, err := Load(dir); err == nil ||
		!strings.Contains(err.Error(), "ONLY_NEEDED_WHEN_SELECTED") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestManifestStrictFields(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown", `{"schema_version":1,"name":"x","version":"1","command":["x"],"wat":true}`, "unknown field"},
		{"version", `{"schema_version":1,"name":"x","command":["x"]}`, "version is required"},
		{"command", `{"schema_version":1,"name":"x","version":"1","command":[]}`, "command"},
		{"capability", `{"schema_version":1,"name":"x","version":"1","command":["x"],"capabilities":["magic"]}`, "not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestDuplicatePluginNameIsRejected(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a")
	second := filepath.Join(root, "b")
	writePlugin(t, first, "same", `{
	  "schema_version": 1, "name": "same", "version": "1", "command": ["python3", "plugin.py"]
	}`)
	writePlugin(t, second, "same", `{
	  "schema_version": 1, "name": "same", "version": "2", "command": ["python3", "plugin.py"]
	}`)
	candidates, errs := Discover([]string{first, second})
	if len(candidates) != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "duplicate") {
		t.Fatalf("candidates/errors = %d/%v", len(candidates), errs)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestLegacyExtensionKeepsWorking cwd documents compatibility: an old
// executable is still launched by its absolute path, without inventing
// a working directory or descriptor fields.
func TestLegacyExtensionKeepsWorkingCwd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Path: path, Dir: filepath.Dir(path), Legacy: true}
	extension, err := candidate.Extension(nil)
	if err != nil {
		t.Fatal(err)
	}
	if extension.Dir != "" {
		t.Fatalf("legacy Dir = %q, want inherited cwd", extension.Dir)
	}
}
