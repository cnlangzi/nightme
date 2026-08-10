// Registry implementation for the agent package. See agent.go for the
// contract; this file is just the map + helpers and is concurrency-safe.
package agent

import (
	"sync"
)

// Builtins is the package-level registry of agents that ship with
// the nightme binary. Each agent package's init() registers itself
// here, so whatever is implemented is what /run will dispatch to —
// no name-based switch in user code, no defaults table to drift out
// of sync with the source.
//
// There is no fallback: if a name is not in Builtins and the user
// has not configured it, /run <name> returns "unknown agent".
//
// The registry now stores Starter values (P1 migration). Bridges
// that have not yet been refactored into driver + starter can be
// registered via AsStarter(legacyAgent) during the P1-P3 transition;
// that wrapper is removed in P4 when all bridges migrate.
var Builtins = New()

// Registry is a thread-safe map of Starter instances keyed by
// Info().Name.
//
// Stores Starter rather than the previous Agent interface — the
// static metadata (Info/Detect) is in Starter, and Starter.Start
// is the only polymorphic operation. See agent.go for the
// rationale.
//
// The zero value is not usable; create one with New().
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Starter
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]Starter)}
}

// Register stores a starter under s.Info().Name. If a starter
// with the same name is already registered, the new instance
// replaces it (the most recent call wins). The replaced boolean
// reports whether a replacement happened.
func (r *Registry) Register(s Starter) (replaced bool) {
	info := s.Info()
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.entries[info.Name]
	r.entries[info.Name] = s
	return existed
}

// Get returns the Starter registered under name, or ErrUnknownAgent.
// Callers drive the returned Starter via Info/Detect/Start.
func (r *Registry) Get(name string) (Starter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.entries[name]
	if !ok {
		return nil, ErrUnknownAgent
	}
	return s, nil
}

// List returns all registered starters in unspecified order. The
// slice is freshly allocated; callers may mutate it.
func (r *Registry) List() []Starter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Starter, 0, len(r.entries))
	for _, s := range r.entries {
		out = append(out, s)
	}
	return out
}

// MakeChanAlias returns a writable channel that forwards values
// from the read-only source. Used by legacyStarter to bridge a
// legacy Agent.Events() (<-chan) into LiveAgent.events (chan) so
// the shared struct can have a single field type. Exported because
// test bridges outside the agent package need to construct a
// LiveAgent from a legacy Agent and must wire the events chan the
// same way.
func MakeChanAlias(src <-chan AgentEvent) chan AgentEvent {
	out := make(chan AgentEvent)
	go func() {
		for ev := range src {
			out <- ev
		}
		close(out)
	}()
	return out
}