// Package gateway — the multi-channel inbound pump. Each
// attached (Channel, Manager) tuple runs its own pumpOne
// goroutine that calls the shared inbound.Router's Dispatch
// with the per-channel Manager (the chatID → channel mapping
// is implicit via the per-pump mgr closure).
//
// Layered architecture (top-down, no cycles):
//
//	gateway                — multi-pump
//	  └─ inbound (package)  — priority dispatch chain
//	                         (chatsession / command / shell / services)
//
// Gateway itself does NOT know about chatsession, command,
// shell, or services. The dispatch chain is constructed by
// the runtime (cmd/nightme/run.go) and passed to New.
//
// Responsibilities:
//   - AttachPumps: register (Channel, Manager) tuples. Each
//     tuple gets its own pumpOne goroutine, whether it is
//     attached before or after Start.
//   - Start / Stop lifecycle.
//   - DispatchInbound: thin delegate to the wired
//     *inbound.Router, used by the v0.x single-channel tests
//     and the legacy call sites.
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

// Router is the multi-pump gateway. Constructed once per daemon
// via New; the runtime holds the *Router and calls
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

	// startCtx is the context Start was called with. Pumps
	// attached after Start (the daemon attaches each channel as it
	// finishes starting) reuse it.
	startCtx context.Context

	// stopping latches once Stop has begun draining, so a late
	// AttachPumps cannot call wg.Add while Stop is inside wg.Wait.
	stopping bool

	// mgrFallback is the chatsession.Manager of the first
	// attached pump. It is used by the legacy DispatchInbound
	// path (single-channel tests and the v0.x call sites) when
	// no Pump context is available. Set automatically when
	// AttachPumps is called.
	mgrFallback *chatsession.Manager

	// observer is told about every inbound message before it is
	// dispatched. Nil when nobody watches.
	observer InboundObserver
}

// Pump pairs a channel with its per-channel chatsession.Manager.
// The Manager owns the ChatSession for the chatID and carries
// the Emitter bound to the channel that produced the inbound.
// Each Pump runs its own pumpOne goroutine.
type Pump struct {
	Channel channel.Channel
	Manager *chatsession.Manager
}

// InboundObserver is told about every inbound message before the
// dispatch chain sees it.
//
// The observer runs on the pump goroutine, so it must be quick and
// must not block: a program that wants to react to a person's
// message learns about it here and does its work elsewhere.
type InboundObserver interface {
	Seen(msg *messages.InboundMessage)
}

// New constructs a Router (the multi-pump gateway).
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
		inbound: ir,
	}
}

// WithObserver installs the observer that is told about every
// inbound message before it is dispatched.
//
// It returns the router so it can be chained onto New.
func (r *Router) WithObserver(o InboundObserver) *Router {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observer = o
	return r
}

// AttachPumps registers one (Channel, Manager) tuple per
// channel. Pumps attached after Start are pumped as soon as
// they arrive: the daemon reaches readiness before any channel
// has started and attaches each one as it comes up, so a
// pre-Start-only attach would drop every inbound message. The
// first pump's Manager is captured as the mgrFallback for the
// legacy DispatchInbound path.
func (r *Router) AttachPumps(pumps ...Pump) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pumps = append(r.pumps, pumps...)
	if r.mgrFallback == nil && len(pumps) > 0 && pumps[0].Manager != nil {
		r.mgrFallback = pumps[0].Manager
	}
	if r.stopCh == nil || r.stopping {
		return
	}
	for _, p := range pumps {
		p := p
		r.wg.Add(1)
		go r.pumpOne(r.startCtx, p)
	}
}

// Start launches one pumpOne goroutine per attached pump. Each
// pump reads from its own channel and calls the shared
// dispatch chain with its own mgr (per-channel). Starting with
// zero pumps attached is valid — the daemon declares readiness
// before any channel has started and attaches them as they come
// up via AttachPumps. Blocks until ctx is cancelled or Stop is
// called.
func (r *Router) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopCh != nil {
		return errors.New("gateway: already started")
	}
	r.stopCh = make(chan struct{})
	r.startCtx = ctx
	for _, p := range r.pumps {
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
	if stopCh != nil {
		r.stopping = true
	}
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
			// The observer learns about the message before the
			// dispatch chain does, so a program that tracks the
			// messages it sent knows about them before the daemon
			// says anything about them.
			if r.observer != nil {
				r.observer.Seen(&msg)
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
//
// v1.3+ multi-channel: prefer the inbound.Router's own csMgr
// (which the test path wires with an Emitter) so test scenarios
// that drive DispatchInbound without calling AttachPumps still
// get the per-test Emitter on the ChatSession. Production paths
// always call AttachPumps so mgrFallback is set; this fallback
// is only reachable for tests + direct legacy callers.
func (r *Router) DispatchInbound(ctx context.Context, msg *messages.InboundMessage) (*inbound.CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}
	mgr := r.mgrFallback
	if mgr == nil && r.inbound != nil {
		// Tests that pre-date AttachPumps wire their Emitter
		// onto the inbound.Router's csMgr before constructing
		// the gateway. Reuse that csMgr so the reply path
		// resolves to the test's recording Emitter, not a
		// fresh empty chat session.
		mgr = r.inbound.CsMgr()
	}
	if mgr == nil {
		mgr = chatsession.NewManager()
	}
	return r.inbound.Dispatch(ctx, mgr, msg)
}
