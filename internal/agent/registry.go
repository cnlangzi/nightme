// Registry implementation for the agent package. See agent.go for the
// contract; this file is just the map + helpers and is concurrency-safe.
package agent

import (
	"context"
	"errors"
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

// LegacyRegister is a transitional helper for code paths that
// still hold a legacy Agent (the old interface) and need to put
// it into the new registry. Equivalent to Register(AsStarter(a)).
// Used by migration shims and tests during P1-P3; safe to remove
// in P4.
func (r *Registry) LegacyRegister(a Agent) bool {
	return r.Register(AsStarter(a))
}

// AsStarter wraps a legacy Agent (the old interface) in a
// Starter. The wrapper delegates Info/Detect/Start to the inner
// Agent and wraps the returned live handle in a *LiveAgent via a
// legacyDriver that forwards Send*/Reset/Close back to the
// legacy Agent's methods.
//
// Removed in P4 after all bridges migrate to driver + starter.
func AsStarter(a Agent) Starter {
	return &legacyStarter{inner: a}
}

// legacyStarter adapts a legacy Agent to the Starter interface.
type legacyStarter struct {
	inner Agent
}

func (l *legacyStarter) Info() Info {
	return NewInfo(
		l.inner.Name(),
		l.inner.Mode(),
		l.inner.Command(),
		l.inner.Args(),
		l.inner.Env(),
	)
}

func (l *legacyStarter) Detect() error { return l.inner.Detect() }

func (l *legacyStarter) Start(ctx context.Context, cfg StartConfig) (*LiveAgent, error) {
	live, err := l.inner.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	events := live.Events()
	return &LiveAgent{
		Info:   l.Info(),
		pid:    live.PID(),
		events: MakeChanAlias(events),
		driver: &legacyDriver{inner: live},
		closed: make(chan struct{}),
	}, nil
}

// legacyDriver forwards the driver interface methods back to a
// legacy Agent. The legacy Agent already implements Close/New/
// SendText/SendBlocks/SendPermission; we just relay them.
type legacyDriver struct {
	inner Agent
}

func (l *legacyDriver) SendText(text string) error { return l.inner.SendText(text) }
func (l *legacyDriver) SendBlocks(ctx context.Context, blocks []ContentBlock) error {
	return l.inner.SendBlocks(ctx, blocks)
}
func (l *legacyDriver) SendPermission(resp string) error { return l.inner.SendPermission(resp) }
func (l *legacyDriver) Reset(ctx context.Context) error    { return l.inner.New(ctx) }
func (l *legacyDriver) Close() error                      { return l.inner.Close() }

// liveAgentAsAgent wraps a *LiveAgent in an Agent interface. Used
// by registrySpawner.Spawn so the Spawner.Spawn return type
// remains agent.Agent (unchanged) even though the registry now
// stores Starters and Start returns *LiveAgent.
//
// The wrapper captures the static metadata (Info) at wrap time
// since Starter.Start has already returned — we don't have the
// Starter handle anymore at the call site, just the Info we read
// from it before Start.
type liveAgentAsAgent struct {
	live *LiveAgent
	info Info
}

func (w *liveAgentAsAgent) Name() string    { return w.info.Name }
func (w *liveAgentAsAgent) Mode() Mode      { return w.info.Mode }
func (w *liveAgentAsAgent) Command() string { return w.info.Command }
func (w *liveAgentAsAgent) Args() []string  { return w.info.Args }
func (w *liveAgentAsAgent) Env() []string   { return w.info.Env }
func (w *liveAgentAsAgent) Detect() error   { return nil }

func (w *liveAgentAsAgent) Start(context.Context, StartConfig) (Agent, error) {
	return nil, errors.New("liveAgentAsAgent: already started")
}
func (w *liveAgentAsAgent) Close() error                       { return w.live.Close() }
func (w *liveAgentAsAgent) Events() <-chan AgentEvent           { return w.live.Events() }
func (w *liveAgentAsAgent) PID() int                            { return w.live.PID() }
func (w *liveAgentAsAgent) SendText(text string) error          { return w.live.SendText(text) }
func (w *liveAgentAsAgent) SendBlocks(ctx context.Context, blocks []ContentBlock) error {
	return w.live.SendBlocks(ctx, blocks)
}
func (w *liveAgentAsAgent) SendPermission(resp string) error { return w.live.SendPermission(resp) }
func (w *liveAgentAsAgent) New(ctx context.Context) error    { return w.live.New(ctx) }

// WrapAsAgent wraps a *LiveAgent in an Agent interface, capturing
// the static metadata from info. Used by Spawner implementations
// that hold a Starter (the new registry type) but need to satisfy
// the legacy Spawner.Spawn(ctx,…) (agent.Agent, error) signature.
func WrapAsAgent(live *LiveAgent, info Info) Agent {
	return &liveAgentAsAgent{live: live, info: info}
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