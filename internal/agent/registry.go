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
// Registration order matters: List() returns starters in the order
// they were registered, and that order drives primary-agent
// auto-detection (see docs/primary-agent-detection.md). New
// builtins MUST be appended to the end of cmd/nightme/agents.go
// init() so the priority chain stays "user-preferred first, fall
// through to fallbacks".
//
// The registry now stores Starter values (P1 migration). Bridges
// that have not yet been refactored into driver + starter can be
// registered via AsStarter(legacyAgent) during the P1-P3 transition;
// that wrapper is removed in P4 when all bridges migrate.
var Builtins = New()

// Registry is a thread-safe map of Starter instances keyed by
// Info().Name, paired with an append-only `order` slice that
// records first-insertion order so List() returns a deterministic
// sequence.
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
	order   []string // first-insertion order; appended only on first Register, never on replacement
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]Starter)}
}

// Register stores a starter under s.Info().Name. If a starter
// with the same name is already registered, the new instance
// replaces it (the most recent call wins); the order slice is
// NOT appended to in the replacement case — the original
// first-insertion position is preserved. The replaced boolean
// reports whether a replacement happened.
func (r *Registry) Register(s Starter) (replaced bool) {
	info := s.Info()
	name := info.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.entries[name]
	r.entries[name] = s
	if !existed {
		r.order = append(r.order, name)
	}
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

// List returns all registered starters in first-insertion order.
// The slice is freshly allocated; callers may mutate it.
//
// Re-registration of an existing name does NOT move its position
// in the returned slice — see Register.
func (r *Registry) List() []Starter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Starter, 0, len(r.order))
	for _, name := range r.order {
		if s, ok := r.entries[name]; ok {
			out = append(out, s)
		}
	}
	return out
}

// MakeChanAlias returns a writable channel that forwards values
// from the read-only source. Used by legacyStarter to bridge a
// legacy Agent.Events() (<-chan) into Agent.events (chan) so
// the shared struct can have a single field type. Exported because
// test bridges outside the agent package need to construct a
// Agent from a legacy Agent and must wire the events chan the
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