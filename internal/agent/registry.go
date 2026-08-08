// Registry implementation for the agent package. See agent.go for the
// contract; this file is just the map + helpers and is concurrency-safe.
package agent

import "sync"

// Builtins is the package-level registry of agents that ship with
// the nightme binary. Each agent package's init() registers itself
// here, so whatever is implemented is what /run will dispatch to —
// no name-based switch in user code, no defaults table to drift out
// of sync with the source.
//
// There is no fallback: if a name is not in Builtins and the user
// has not configured it, /run <name> returns "unknown agent".
var Builtins = New()

// Registry is a thread-safe map of AgentSpec instances keyed by Name().
//
// The zero value is not usable; create one with New().
type Registry struct {
	mu      sync.RWMutex
	entries map[string]AgentSpec
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]AgentSpec)}
}

// Register stores an agent under its Name(). If an agent with the same
// name is already registered, the new instance replaces it (the most
// recent call wins). The replaced boolean reports whether a
// replacement happened.
func (r *Registry) Register(a AgentSpec) (replaced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.entries[a.Name()]
	r.entries[a.Name()] = a
	return existed
}

// Get returns the AgentSpec registered under name, or ErrUnknownAgent.
func (r *Registry) Get(name string) (AgentSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.entries[name]
	if !ok {
		return nil, ErrUnknownAgent
	}
	return a, nil
}

// List returns all registered agents in unspecified order. The slice
// is freshly allocated; callers may mutate it.
func (r *Registry) List() []AgentSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentSpec, 0, len(r.entries))
	for _, a := range r.entries {
		out = append(out, a)
	}
	return out
}
