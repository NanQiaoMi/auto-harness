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
	// trusted allows the project-local extension dir to load. It is
	// false until the project trust decision is made.
	trusted bool
}

// trustedDirs returns the directories that load without a trust
// decision: the configured ones and the user's own.
func (l *extensionLoader) trustedDirs() []string {
	var dirs []string
	dirs = append(dirs, l.cfg.ExtensionDirs...)
	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ext.GlobalExtensionsDir))
	}
	return dirs
}

// projectDir is the project-local extension directory.
func (l *extensionLoader) projectDir() string {
	return filepath.Join(l.projectRoot, ext.ProjectExtensionsDir)
}

// extensionDirs returns the directories scanned for process
// extensions: the trusted ones, then the project's own when the
// project is trusted.
func (l *extensionLoader) extensionDirs() []string {
	dirs := l.trustedDirs()
	if l.trusted {
		project := l.projectDir()
		already := false
		for _, dir := range dirs {
			if dir == project {
				already = true
				break
			}
		}
		if !already {
			dirs = append(dirs, project)
		}
	}
	return dirs
}

// buildProject returns the project-local extensions for the second
// load pass, after the trust decision. It returns nothing when the
// project is untrusted, or when its extension directory is also one of
// the user's own (a project that is the home directory) and therefore
// already loaded.
func (l *extensionLoader) buildProject() []ext.Extension {
	if !l.trusted {
		return nil
	}
	project := l.projectDir()
	for _, dir := range l.trustedDirs() {
		if dir == project {
			return nil
		}
	}
	var exts []ext.Extension
	for _, path := range ext.DiscoverIn([]string{project}) {
		exts = append(exts, ext.NewProcessExtension(path))
	}
	return exts
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
