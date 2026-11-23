package sandbox

import (
	"context"
	"os/exec"
	"sync"
)

// Backend isolates subprocess execution using a platform or runtime
// confinement mechanism (such as macOS Seatbelt, Linux Landlock, or a
// future container/WASM adapter).
type Backend interface {
	// Name returns a short identifier for diagnostics (e.g. "seatbelt",
	// "landlock", "none").
	Name() string
	// Available reports whether this backend can enforce confinement on
	// the current host.
	Available() bool
	// Command wraps name with args inside the sandbox described by
	// profile, returning the command to run and whether it is actually
	// confined.
	Command(ctx context.Context, name string, args []string, profile *Profile, workdir string) (*exec.Cmd, bool, error)
}

var (
	backendMu     sync.RWMutex
	activeBackend Backend
)

// DefaultBackend returns the currently configured sandbox backend,
// defaulting to the host platform's native backend.
func DefaultBackend() Backend {
	backendMu.RLock()
	b := activeBackend
	backendMu.RUnlock()
	if b != nil {
		return b
	}
	return platformBackend()
}

// BackendName returns the active sandbox backend identifier.
func BackendName() string {
	return DefaultBackend().Name()
}

// SetBackend overrides the active sandbox backend. Passing nil restores
// the host platform's native backend.
func SetBackend(b Backend) {
	backendMu.Lock()
	activeBackend = b
	backendMu.Unlock()
}
