package ext

import (
	"sync"

	"github.com/mimi/auto-harness/tools"
)

// scope records everything an extension registered during its Setup, so
// the manager can undo it when the extension is unloaded (hot reload).
//
// The scope is a stack on the API: Manager.Load pushes a scope before
// calling Setup and pops it afterwards, so nested loads nest scopes.
type scope struct {
	mu sync.Mutex
	// unsubscribes remove event handlers the extension added.
	unsubscribes []func()
	// toolNames are tools the extension registered.
	toolNames []string
	// commandNames are commands the extension registered.
	commandNames []string
}

// undo removes everything recorded in the scope. It is safe to call
// more than once.
func (s *scope) undo(api *API) {
	s.mu.Lock()
	unsubs := s.unsubscribes
	toolNames := s.toolNames
	cmdNames := s.commandNames
	s.unsubscribes = nil
	s.toolNames = nil
	s.commandNames = nil
	s.mu.Unlock()

	for i := len(unsubs) - 1; i >= 0; i-- {
		unsubs[i]()
	}
	for _, name := range toolNames {
		_ = api.Tools.Unregister(name)
	}
	for _, name := range cmdNames {
		_ = api.Commands.Unregister(name)
	}
}

// recordUnsubscribe adds an unsubscribe function to the scope.
func (s *scope) recordUnsubscribe(unsub func()) {
	if unsub == nil {
		return
	}
	s.mu.Lock()
	s.unsubscribes = append(s.unsubscribes, unsub)
	s.mu.Unlock()
}

// recordTool adds a registered tool name to the scope.
func (s *scope) recordTool(name string) {
	s.mu.Lock()
	s.toolNames = append(s.toolNames, name)
	s.mu.Unlock()
}

// recordCommand adds a registered command name to the scope.
func (s *scope) recordCommand(name string) {
	s.mu.Lock()
	s.commandNames = append(s.commandNames, name)
	s.mu.Unlock()
}

// scopeStack is the API's stack of active scopes.
type scopeStack struct {
	mu     sync.Mutex
	scopes []*scope
}

// pushScope starts a new registration scope and returns it.
func (a *API) pushScope() *scope {
	sc := &scope{}
	a.scopes.mu.Lock()
	a.scopes.scopes = append(a.scopes.scopes, sc)
	a.scopes.mu.Unlock()
	return sc
}

// popScope ends the given scope.
func (a *API) popScope(sc *scope) {
	a.scopes.mu.Lock()
	for i := len(a.scopes.scopes) - 1; i >= 0; i-- {
		if a.scopes.scopes[i] == sc {
			a.scopes.scopes = append(a.scopes.scopes[:i], a.scopes.scopes[i+1:]...)
			break
		}
	}
	a.scopes.mu.Unlock()
}

// currentScope returns the innermost active scope, or nil when the
// registration happens outside an extension load (e.g. built-in tools).
func (a *API) currentScope() *scope {
	a.scopes.mu.Lock()
	defer a.scopes.mu.Unlock()
	if len(a.scopes.scopes) == 0 {
		return nil
	}
	return a.scopes.scopes[len(a.scopes.scopes)-1]
}

// toolName is a small helper so RegisterTool and the scope stay in sync.
func (a *API) toolName(t *tools.Tool) string {
	if t == nil {
		return ""
	}
	return t.Name
}
