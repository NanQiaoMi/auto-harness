package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NanQiaoMi/auto-harness/agentprofile"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/plugins"
)

// loadExtensionSet loads every extension independently. One broken
// plugin must not prevent the rest of the user's or project's selected
// environment from assembling.
func loadExtensionSet(manager *ext.Manager, extensions []ext.Extension) error {
	var errs []error
	for _, extension := range extensions {
		if err := manager.Load(extension); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

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

	// pluginDirs are additional profile-selected plugin directories.
	pluginDirs []string
	// profileFromProject delays pluginDirs until project trust.
	profileFromProject bool
	// pluginFilter is the profile's plugin allowlist. nil means all.
	pluginFilter map[string]bool
	// pluginEnv is inherited by plugin child processes.
	pluginEnv map[string]string
	// pluginRecords is the discovery/launch state for /plugins.
	pluginRecords []pluginRecord
	// selectedPlugins tracks profile-selected names that were found.
	selectedPlugins map[string]bool
	// missingPlugins are profile selections no discovery root provided.
	missingPlugins []string
	// diagnostics are non-fatal plugin discovery problems.
	diagnostics []error
}

// pluginRecord is one plugin the loader considered.
type pluginRecord struct {
	Candidate plugins.Candidate
	Extension *ext.ProcessExtension
	Selected  bool
	Err       error
}

// configure applies an agent profile to plugin discovery.
func (l *extensionLoader) configure(profile *agentprofile.Profile, fromProject bool) {
	if profile == nil {
		return
	}
	l.pluginDirs = append([]string{}, profile.PluginDirs...)
	l.profileFromProject = fromProject
	l.pluginEnv = profile.Env
	if profile.Plugins != nil {
		l.pluginFilter = map[string]bool{}
		for _, name := range profile.Plugins {
			l.pluginFilter[name] = true
		}
	}
}

// trustedDirs returns the directories that load without a trust
// decision: the configured ones and the user's own.
func (l *extensionLoader) trustedDirs() []string {
	var dirs []string
	dirs = append(dirs, l.cfg.ExtensionDirs...)
	if !l.profileFromProject {
		dirs = append(dirs, l.pluginDirs...)
	}
	dirs = append(dirs, plugins.InstallDir())
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
		if l.profileFromProject {
			dirs = append(dirs, l.pluginDirs...)
		}
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
	return uniquePaths(dirs)
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
	var dirs []string
	project := l.projectDir()
	trusted := l.trustedDirs()
	already := false
	for _, dir := range trusted {
		if dir == project {
			already = true
			break
		}
	}
	if !already {
		dirs = append(dirs, project)
	}
	if l.profileFromProject {
		dirs = append(dirs, l.pluginDirs...)
	}
	return l.processExtensions(uniquePaths(dirs))
}

func uniquePaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
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

	l.pluginRecords = nil
	l.diagnostics = nil
	l.selectedPlugins = map[string]bool{}
	l.missingPlugins = nil
	return append(exts, l.processExtensions(l.extensionDirs())...)
}

// processExtensions discovers and wraps process plugins from dirs.
func (l *extensionLoader) processExtensions(dirs []string) []ext.Extension {
	candidates, errs := plugins.DiscoverWithEnv(dirs, l.pluginEnv)
	for _, err := range errs {
		l.diagnostics = append(l.diagnostics, err)
		fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
	}
	if l.selectedPlugins == nil {
		l.selectedPlugins = map[string]bool{}
	}
	var exts []ext.Extension
	for _, candidate := range candidates {
		record := pluginRecord{
			Candidate: candidate,
			Selected:  l.pluginFilter == nil || l.pluginFilter[candidate.Name()],
		}
		if !record.Selected {
			l.pluginRecords = append(l.pluginRecords, record)
			continue
		}
		l.selectedPlugins[candidate.Name()] = true
		extension, err := candidate.Extension(l.pluginEnv)
		record.Extension = extension
		record.Err = err
		if err != nil {
			l.diagnostics = append(l.diagnostics, err)
			fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
		} else {
			exts = append(exts, extension)
		}
		l.pluginRecords = append(l.pluginRecords, record)
	}
	return exts
}

// reportMissingPlugins warns about profile selections that no
// discovered plugin provided. It is called after both the trusted and
// project discovery passes, so a plugin in either scope is not reported
// prematurely.
func (l *extensionLoader) reportMissingPlugins() {
	if l.pluginFilter == nil {
		return
	}
	names := make([]string, 0, len(l.pluginFilter))
	for name := range l.pluginFilter {
		if !l.selectedPlugins[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		err := fmt.Errorf("plugin %q was selected by the profile but was not found", name)
		l.missingPlugins = append(l.missingPlugins, name)
		l.diagnostics = append(l.diagnostics, err)
		fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
	}
}

func (l *extensionLoader) missingPluginSelections() []string {
	return append([]string{}, l.missingPlugins...)
}

func (l *extensionLoader) missingPluginError() error {
	l.reportMissingPlugins()
	missing := l.missingPluginSelections()
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("agent profile selected plugins that were not found: %s",
		strings.Join(missing, ", "))
}

// selectedPluginFailures returns selected plugins that did not reach a
// loaded process extension. A profile's declared environment is not
// complete while one of them is missing.
func (l *extensionLoader) selectedPluginFailures(manager *ext.Manager) []string {
	if l.pluginFilter == nil {
		return nil
	}
	var failed []string
	for _, record := range l.pluginRecords {
		if !record.Selected {
			continue
		}
		if record.Err != nil || record.Extension == nil ||
			!manager.ProcessExtensionLoaded(record.Extension) {
			failed = append(failed, record.Candidate.Name())
		}
	}
	sort.Strings(failed)
	return failed
}

func (l *extensionLoader) selectedPluginError(manager *ext.Manager) error {
	failed := l.selectedPluginFailures(manager)
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("agent profile selected plugins that failed to load: %s",
		strings.Join(failed, ", "))
}

// report prints the plugin catalog and its live process state.
func (l *extensionLoader) report(manager *ext.Manager) {
	if len(l.pluginRecords) == 0 && len(l.diagnostics) == 0 {
		fmt.Println("no plugins discovered")
		return
	}
	records := append([]pluginRecord{}, l.pluginRecords...)
	sort.Slice(records, func(i, j int) bool {
		return records[i].Candidate.Name() < records[j].Candidate.Name()
	})
	for _, record := range records {
		state := "skipped by profile"
		if record.Selected {
			switch {
			case record.Err != nil:
				state = "launch error"
			case manager.ProcessExtensionLoaded(record.Extension):
				state = "loaded"
			default:
				state = "not loaded"
			}
		}
		version := record.Candidate.Version()
		if version == "" {
			version = "legacy"
		}
		fmt.Printf("%-20s %-10s %-16s %s\n",
			record.Candidate.Name(), version, state, record.Candidate.Path)
		if record.Err != nil {
			fmt.Printf("  error: %v\n", record.Err)
		}
	}
	for _, err := range l.diagnostics {
		fmt.Printf("warning: %v\n", err)
	}
}
