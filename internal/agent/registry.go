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

// Registry is a thread-safe map of Agent instances keyed by Name().
//
// Stores the merged Agent interface (spec-half + live-half), not
// the narrower AgentSpec — see Specs() below for spec-only access.
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
// Callers can drive the returned Agent (Start it, etc.) — the
// runtime path uses this; tooling that only needs spec data should
// use Specs() or accept AgentSpec.
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
// is freshly allocated; callers may mutate it. Use Specs() instead
// when only static metadata is needed.
func (r *Registry) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Agent, 0, len(r.entries))
	for _, a := range r.entries {
		out = append(out, a)
	}
	return out
}

// Specs returns the spec-only view of every registered agent.
// Callers that only need Name / Mode / Command / Args / Env /
// Detect should use this rather than List() — the static type
// makes it impossible to accidentally call Start or Send* on the
// returned values.
//
// `nightme agents` (cmd/nightme/agents_cmd.go) is the primary
// consumer.
func (r *Registry) Specs() []AgentSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentSpec, 0, len(r.entries))
	for _, a := range r.entries {
		out = append(out, a)
	}
	return out
}