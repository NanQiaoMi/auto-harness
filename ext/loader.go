package ext

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Extension is a unit of harness customization. Setup runs once at load
// time and receives the extension API. Extensions should register hooks,
// tools and commands in Setup; long-lived resources belong in a
// session_start handler so they can be rebuilt on reload.
type Extension interface {
	Name() string
	Setup(api *API) error
}

// SetupFunc adapts a plain function into an Extension.
type SetupFunc struct {
	ExtName string
	Fn      func(api *API) error
}

func (s SetupFunc) Name() string         { return s.ExtName }
func (s SetupFunc) Setup(api *API) error { return s.Fn(api) }

// Named wraps a setup function as a named extension.
func Named(name string, fn func(api *API) error) Extension {
	return SetupFunc{ExtName: name, Fn: fn}
}

// Teardown is implemented by extensions that hold resources (child
// processes, listeners) which must be released when the extension is
// unloaded. It is called during reload and shutdown, in reverse load
// order.
type Teardown interface {
	Teardown() error
}

// Manager loads extensions against an API, tracking what is loaded and
// tearing down in reverse order on shutdown.
//
// The manager supports hot reload: Reload() unloads every extension and
// loads the current set again, emitting session_shutdown (reason
// "reload") and session_start (reason "reload") around the swap so
// extensions can rebuild their state. State captured in the old
// closure becomes stale after a reload, which mirrors pi's semantics.
type Manager struct {
	mu       sync.Mutex
	api      *API
	loaded   []loadedExtension
	shutdown bool

	// reloadMu serializes reloads so a reload cannot interleave with
	// another reload or with shutdown.
	reloadMu sync.Mutex
}

// loadedExtension pairs an extension with the scope recording what it
// registered, so the manager can undo it on unload.
type loadedExtension struct {
	ext   Extension
	scope *scope
}

func NewManager(api *API) *Manager {
	return &Manager{api: api}
}

// Load runs an extension's Setup. Failures leave previously loaded
// extensions intact.
func (m *Manager) Load(e Extension) error {
	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return fmt.Errorf("ext: manager is shut down")
	}
	m.mu.Unlock()

	// The scope collects everything the extension registers, so a later
	// reload can remove it again.
	sc := m.api.pushScope()
	err := e.Setup(m.api)
	m.api.popScope(sc)
	if err != nil {
		// Undo partial registrations so a failed load leaves no trace.
		sc.undo(m.api)
		return fmt.Errorf("ext: loading %s: %w", e.Name(), err)
	}

	m.mu.Lock()
	m.loaded = append(m.loaded, loadedExtension{ext: e, scope: sc})
	m.mu.Unlock()
	return nil
}

// LoadAll loads extensions in order.
func (m *Manager) LoadAll(exts ...Extension) error {
	for _, e := range exts {
		if err := m.Load(e); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown emits session_shutdown, unsubscribes all handlers and
// releases extension resources. Idempotent.
func (m *Manager) Shutdown() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return
	}
	m.shutdown = true
	loaded := m.loaded
	m.loaded = nil
	m.mu.Unlock()

	_ = m.api.Bus.Emit(&Event{Type: EventSessionShutdown, Reason: "quit"})
	m.unloadAll(loaded)
}

// unloadAll releases every loaded extension: undo its registrations,
// then run its Teardown, in reverse load order.
func (m *Manager) unloadAll(loaded []loadedExtension) {
	for i := len(loaded) - 1; i >= 0; i-- {
		loaded[i].scope.undo(m.api)
		if td, ok := loaded[i].ext.(Teardown); ok {
			_ = td.Teardown()
		}
	}
}

// LoadedNames lists loaded extension names in load order.
func (m *Manager) LoadedNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.loaded))
	for _, e := range m.loaded {
		names = append(names, e.ext.Name())
	}
	return names
}

// Reload unloads every extension and loads the given set again.
//
// The sequence mirrors pi's reload semantics:
//  1. emit session_shutdown with reason "reload"
//  2. unsubscribe handlers, remove registered tools and commands,
//     release extension resources
//  3. run Setup for the new extension set
//  4. emit session_start with reason "reload"
//
// Extensions that fail to load are reported, but the ones that did
// load stay active.
func (m *Manager) Reload(exts ...Extension) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return fmt.Errorf("ext: manager is shut down")
	}
	old := m.loaded
	m.loaded = nil
	m.mu.Unlock()

	// 1. Tell extensions the session is going away.
	_ = m.api.Bus.Emit(&Event{Type: EventSessionShutdown, Reason: "reload"})

	// 2. Release everything the old extensions registered.
	m.unloadAll(old)

	// 3. Load the new set.
	var loadErr error
	for _, e := range exts {
		if err := m.Load(e); err != nil {
			loadErr = errors.Join(loadErr, err)
		}
	}

	// 4. Tell extensions the session is back.
	_ = m.api.Bus.Emit(&Event{Type: EventSessionStart, Reason: "reload"})
	return loadErr
}

// Discovery directories follow pi's convention: a global dir and a
// project-local dir. Extensions found there are process extensions:
// an executable speaking the JSON-RPC protocol over stdio.
const (
	GlobalExtensionsDir  = ".harness/extensions"
	ProjectExtensionsDir = ".harness/extensions"
)

// Discover returns extension candidate paths found in the global and
// project directories (one level deep). The returned paths are sorted;
// what to do with them is decided by the loader in use.
func Discover(projectRoot string) []string {
	return discoverIn([]string{
		filepath.Join(homeDir(), GlobalExtensionsDir),
		filepath.Join(projectRoot, ProjectExtensionsDir),
	})
}

// DiscoverIn returns extension candidate paths in the given directories.
func DiscoverIn(dirs []string) []string {
	return discoverIn(dirs)
}

// discoverIn scans directories for process extension candidates: each
// subdirectory is a candidate when it contains an executable file, and
// so is each executable file directly inside a directory.
func discoverIn(dirs []string) []string {
	var found []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			name := ent.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			if ent.IsDir() {
				found = append(found, executablesIn(filepath.Join(dir, name))...)
				continue
			}
			if isExecutableFile(filepath.Join(dir, name)) {
				found = append(found, filepath.Join(dir, name))
			}
		}
	}
	sort.Strings(found)
	return found
}

// executablesIn returns the executable files directly inside dir.
func executablesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		if ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		if isExecutableFile(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// isExecutableFile reports whether path is a regular file with the
// executable bit set.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// homeDir returns the user's home directory, or "" if unknown.
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
