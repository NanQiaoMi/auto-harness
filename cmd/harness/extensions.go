package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
)

// extensionLoader builds the extension list from the config: the
// built-in examples plus any process extensions found in the
// configured directories.
//
// Process extensions are executables that speak the extension RPC
// protocol over stdio, so an extension can be written in any language.
type extensionLoader struct {
	// projectRoot is where the project-local extension dir lives.
	projectRoot string
	// cfg supplies the extension directories and commands.
	cfg *config.Config
}

// extensionDirs returns the directories scanned for process
// extensions: the configured ones, then the global and project
// defaults.
func (l *extensionLoader) extensionDirs() []string {
	var dirs []string
	dirs = append(dirs, l.cfg.ExtensionDirs...)
	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ext.GlobalExtensionsDir))
	}
	dirs = append(dirs, filepath.Join(l.projectRoot, ext.ProjectExtensionsDir))
	return dirs
}

// build returns the extension list: built-ins first, then process
// extensions discovered on disk.
func (l *extensionLoader) build() []ext.Extension {
	exts := exampleExtensions()

	// A single explicit command (e.g. "python3 ext.py").
	if cmd := strings.TrimSpace(l.cfg.ExtensionCommand); cmd != "" {
		fields := strings.Fields(cmd)
		if len(fields) > 0 {
			exts = append(exts, ext.NewProcessExtension(fields[0], fields[1:]...))
		}
	}

	for _, path := range ext.DiscoverIn(l.extensionDirs()) {
		exts = append(exts, ext.NewProcessExtension(path))
	}
	return exts
}
