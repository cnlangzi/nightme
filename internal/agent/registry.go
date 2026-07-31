// Registry implementation for the agent package. See agent.go for the
// contract; this file is just the map + helpers and is concurrency-safe.
package agent

import "sync"

// Registry is a thread-safe map of Agent instances keyed by Name().
//
// The zero value is not usable; create one with New().
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Agent
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]Agent)}
}

// Register stores an agent under its Name(). If an agent with the same
// name is already registered, the new instance replaces it (the most
// recent call wins). The replaced boolean reports whether a
// replacement happened.
func (r *Registry) Register(a Agent) (replaced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.entries[a.Name()]
	r.entries[a.Name()] = a
	return existed
}

// Get returns the Agent registered under name, or ErrUnknownAgent.
func (r *Registry) Get(name string) (Agent, error) {
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
func (r *Registry) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Agent, 0, len(r.entries))
	for _, a := range r.entries {
		out = append(out, a)
	}
	return out
}
