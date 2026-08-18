// Package gateway — the multi-channel inbound pump and the
// chat → session binding table. v1.3+ multi-channel: each
// attached (Channel, Manager) tuple runs its own pumpOne
// goroutine that calls the shared inbound.Router's Dispatch
// with the per-channel Manager (the chatID → channel mapping
// is implicit via the per-pump mgr closure).
//
// Layered architecture (top-down, no cycles):
//
//	gateway                — multi-pump + binding table
//	  └─ inbound (package)  — priority dispatch chain
//	                         (chatsession / command / shell / services)
//
// Gateway itself does NOT know about chatsession, command,
// shell, or services. The dispatch chain is constructed by
// the runtime (cmd/nightme/run.go) and passed to New.
//
// Responsibilities:
//   - AttachPumps: register (Channel, Manager) tuples. Each
//     tuple gets its own pumpOne goroutine on Start.
//   - Start / Stop lifecycle.
//   - DispatchInbound: thin delegate to the wired
//     *inbound.Router, used by the v0.x single-channel tests
//     and the legacy call sites.
//   - Bind / LookupByChat / ListBindings / RestoreBindings /
//     Unbind: the chat → ChatSession binding table (v1.2
//     surface).
//
// v1.3 history: AttachChannels + pumpInbound + dispatchLoop
// (the central goroutine that read from a fan-in channelCh
// and called DispatchInbound) were replaced with AttachPumps
// + pumpOne. Each pump's goroutine calls Dispatch directly
// with its own mgr, eliminating the routing table
// (chatToChan / defaultChannel / channelCh).
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Router is the multi-pump + binding table. Constructed once
// per daemon via New; the runtime holds the *Router and calls
// AttachPumps + Start + Stop.
type Router struct {
	mu sync.RWMutex

	// inbound is the dispatch chain. Set at New; the gateway
	// delegates DispatchInbound to it.
	inbound *inbound.Router

	// v1.3+ multi-channel pump state.
	pumps    []Pump
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// v1.2 binding table (chat_id → session_id). Owned by Router.
	bindings map[string]*BindingEntry

	// mgrFallback is the chatsession.Manager of the first
	// attached pump. It is used by the legacy DispatchInbound
	// path (single-channel tests and the v0.x call sites) when
	// no Pump context is available. Set automatically when
	// AttachPumps is called.
	mgrFallback *chatsession.Manager
}

// Pump pairs a channel with its per-channel chatsession.Manager.
// The Manager owns the ChatSession for the chatID and carries
// the Emitter bound to the channel that produced the inbound.
// Each Pump runs its own pumpOne goroutine.
type Pump struct {
	Channel channel.Channel
	Manager *chatsession.Manager
}

// New constructs a Router (the multi-pump + binding table).
//
// ir is the dispatch chain. It is required and must be
// non-nil — the gateway is useless without it.
//
// Runtime wiring (cmd/nightme/run.go):
//
//	for _, ch := range deps.NewChannels(cfg) {
//	    mgr := buildStack(ch, ...)
//	    pumps = append(pumps, gateway.Pump{Channel: ch, Manager: mgr})
//	}
//	gw := gateway.New(inbound.New(commander, shellDispatcher, reactionRouter, cfg.Primary))
//	gw.AttachPumps(pumps...)
//	gw.Start(ctx)
func New(ir *inbound.Router) *Router {
	if ir == nil {
		panic("gateway.New: inbound.Router must not be nil")
	}
	return &Router{
		inbound:  ir,
		bindings: make(map[string]*BindingEntry),
	}
}

// AttachPumps registers one (Channel, Manager) tuple per
// channel. Must be called before Start. The first pump's
// Manager is captured as the mgrFallback for the legacy
// DispatchInbound path.
func (r *Router) AttachPumps(pumps ...Pump) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pumps = append(r.pumps, pumps...)
	if r.mgrFallback == nil && len(pumps) > 0 && pumps[0].Manager != nil {
		r.mgrFallback = pumps[0].Manager
	}
}

// Emitter returns the outbound chokepoint bound at
// construction. Reserved for back-compat with v0.x call sites
// that fetched the Emitter via Router.Emitter(); production
// multi-channel code uses Manager.emitter (per-channel).
//
// Returns nil for v1.3+ Routers (no shared emitter). Use
// ch.Manager.Emitter() instead.
func (r *Router) Emitter() *struct{} { return nil }

// Start launches one pumpOne goroutine per attached pump. Each
// pump reads from its own channel and calls the shared
// dispatch chain with its own mgr (per-channel). Blocks until
// ctx is cancelled or Stop is called.
func (r *Router) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.stopCh != nil {
		r.mu.Unlock()
		return errors.New("gateway: already started")
	}
	r.stopCh = make(chan struct{})
	pumps := r.pumps
	r.mu.Unlock()

	if len(pumps) == 0 {
		r.mu.Lock()
		r.stopCh = nil
		r.mu.Unlock()
		return errors.New("gateway: no pumps attached")
	}

	for _, p := range pumps {
		p := p
		r.wg.Add(1)
		go r.pumpOne(ctx, p)
	}
	return nil
}

// Stop signals all pump goroutines to exit and waits for them
// to drain. Idempotent.
func (r *Router) Stop(ctx context.Context) error {
	r.mu.Lock()
	stopCh := r.stopCh
	r.mu.Unlock()
	if stopCh == nil {
		return nil
	}
	r.stopOnce.Do(func() { close(stopCh) })
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pumpOne is the per-channel pump goroutine. It reads
// InboundMessage from p.Channel.Incoming() and calls the
// shared dispatch chain with p.Manager. The closure captures
// p, so each pump knows which channel + mgr it serves — no
// routing table needed.
func (r *Router) pumpOne(ctx context.Context, p Pump) {
	defer r.wg.Done()
	in := p.Channel.Incoming()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			// Per-pump mgr: the chat this msg came from is
			// in this pump's chatID namespace (chatIDs are
			// channel-namespaced naturally), so the mgr closure
			// is the right one to resolve / persist it.
			if _, err := r.inbound.Dispatch(ctx, p.Manager, &msg); err != nil {
				slog.Default().Warn("gateway: dispatch failed",
					"channel", p.Channel.Name(),
					"chat_id", msg.ChatID,
					"err", err)
			}
		}
	}
}

// DispatchInbound is a thin delegate to the wired
// *inbound.Router. Used by the v0.x single-channel call sites
// (and tests that pre-date AttachPumps). For multi-channel
// production code, the per-pump pumpOne calls
// r.inbound.Dispatch directly with the per-pump mgr —
// DispatchInbound's single-mgr path is the legacy contract.
func (r *Router) DispatchInbound(ctx context.Context, msg *messages.InboundMessage) (*inbound.CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}
	return r.inbound.Dispatch(ctx, r.mgrFallback, msg)
}

// --- v1.1 binding table (commit 3) ----------------------------------

// BindingEntry is the (chat_id → session_id) row stored in
// chat_sessions.json. See internal/registry for the persisted
// schema.
type BindingEntry struct {
	ChatID    string
	SessionID string
	Workspace string
	Agent     string
}

// Bind registers the binding (chatID → sessionID). Called by
// the /cwd handler after it creates a fresh session via
// MemoryManager.Register.
func (r *Router) Bind(chatID, sessionID, workspace, agent string) *BindingEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := &BindingEntry{
		ChatID:    chatID,
		SessionID: sessionID,
		Workspace: workspace,
		Agent:     agent,
	}
	r.bindings[chatID] = b
	return b
}

// LookupByChat returns the binding for chatID, or nil.
func (r *Router) LookupByChat(chatID string) *BindingEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[chatID]
}

// ListBindings returns a snapshot of every binding in
// unspecified order. The slice is freshly allocated.
func (r *Router) ListBindings() []BindingEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]BindingEntry, 0, len(r.bindings))
	for _, b := range r.bindings {
		out = append(out, *b)
	}
	return out
}

// RestoreBindings populates the binding table from a snapshot
// (typically loaded from the registry at startup). Replaces
// any existing entries.
func (r *Router) RestoreBindings(entries []BindingEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings = make(map[string]*BindingEntry, len(entries))
	for i := range entries {
		e := entries[i]
		r.bindings[e.ChatID] = &e
	}
}

// Unbind removes the binding for chatID without affecting the
// underlying session.
func (r *Router) Unbind(chatID string) {
	r.mu.Lock()
	delete(r.bindings, chatID)
	r.mu.Unlock()
}
