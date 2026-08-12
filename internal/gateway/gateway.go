// Package gateway — the inbound pump and binding table for every
// IM Channel attached to the daemon. It owns transport (reading
// InboundMessage from each channel, multiplexing into a central
// dispatch channel) and routing (which chat belongs to which
// channel), and delegates per-message dispatch to a wired
// *inbound.Router.
//
// Layered architecture (top-down, no cycles):
//
//	gateway                — pump + binding table
//	  └─ inbound (package)  — priority dispatch chain
//	                         (chatsession / command / shell / services)
//
// Gateway itself does NOT know about chatsession, command,
// shell, or services. The dispatch chain is constructed by
// the runtime (cmd/nightme/run.go) and passed to New. This
// keeps the layering clean: every dependency points downward
// (gateway → inbound → dispatch targets), never up.
//
// Responsibilities:
//
//   - AttachChannels + Start/Stop lifecycle + the per-channel
//     pumpInbound goroutine.
//   - chatToChan + defaultChannel routing table (which Channel
//     serves which chatID; used for outbound Channel resolution
//     from a chatID).
//   - DispatchInbound: thin delegate to the wired
//     *inbound.Router. Returns *inbound.CommandResult
//     (defined in the inbound package — gateway has no
//     per-dispatch state of its own).
//   - Bind / LookupByChat / ListBindings / RestoreBindings /
//     Unbind: the chat → ChatSession binding table (v1.2
//     surface; reserved for future multi-session work).
//
// F-58 history: gateway previously exposed a `Gateway`
// interface with a `Router = gateway` type alias. The interface
// was dropped in favour of a single explicit `Router` struct —
// production code always type-asserted to *Router immediately
// after New, so the interface had no implementors and no
// consumers that benefited from the abstraction.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Router is the inbound pump + binding table. Constructed
// once per daemon via New; the runtime holds the *Router and
// uses it to AttachChannels / Start / Stop / DispatchInbound.
type Router struct {
	mu sync.RWMutex

	// emitter (set via New) is the outbound chokepoint every
	// downstream caller goes through for send-side operations.
	// The runtime injects an outbound.Emitter at construction;
	// the gateway stores it for chat-session bind / wire
	// helpers to fetch via the Emitter() method.
	emitter outbound.Emitter

	// inbound is the dispatch chain. Set at New; the gateway
	// delegates DispatchInbound to it.
	inbound *inbound.Router

	// Stage 2 / v1.2 pump state. fields below are read/written under mu.
	channels       []channel.Channel
	channelCh      chan messages.InboundMessage
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	chatToChan     map[string]channel.Channel // ChatID -> the channel that owns the chat
	defaultChannel channel.Channel            // fallback channel for chats we haven't seen yet

	// v1.2 binding table (chat_id → session_id). Owned by Router.
	bindings map[string]*BindingEntry
}

// New constructs a Router (the inbound pump + binding table).
//
// ir is the dispatch chain. em is the outbound chokepoint.
// Both are required and must be non-nil — the gateway is
// useless without either.
//
// Runtime wiring (cmd/nightme/run.go):
//
//	gw := gateway.New(inbound.New(mgr, commander, shellDisp, action, primary), em)
func New(ir *inbound.Router, em outbound.Emitter) *Router {
	if ir == nil {
		panic("gateway.New: inbound.Router must not be nil")
	}
	if em == nil {
		panic("gateway.New: Emitter must not be nil")
	}
	return &Router{
		inbound:     ir,
		emitter:     em,
		chatToChan:  make(map[string]channel.Channel),
		bindings:    make(map[string]*BindingEntry),
	}
}

// Emitter returns the outbound chokepoint bound to this Router.
// The runtime (cmd/nightme) fetches it once after New to bind
// the same Emitter to chatsession.Manager (so every chat
// session's outbound path goes through this single chokepoint).
//
// Lock-free: emitter is set once at construction.
func (r *Router) Emitter() outbound.Emitter {
	return r.emitter
}

// ResolveChannel is the IM Channel that serves chatID. F-58:
// was exported so cmd/nightme could look up a chat's channel
// for the v0.x WithChannel shim; no production caller remains
// after the shims were deleted. Kept as a package-private
// helper (resolveChannel, below) for any future debug /
// recovery path that needs chatID → Channel.
func (r *Router) ResolveChannel(chatID string) channel.Channel {
	return r.resolveChannel(chatID)
}

// AttachChannels registers the channels the gateway will read from
// and dispatch to. Multi-channel is supported.
func (r *Router) AttachChannels(channels ...channel.Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = append(r.channels, channels...)
	if len(channels) > 0 {
		for _, c := range channels {
			if c != nil {
				r.defaultChannel = c
				break
			}
		}
	}
}

// DispatchInbound is the inboundDispatcher entry point. Thin
// delegate to the wired *inbound.Router — gateway itself owns
// no dispatch logic.
//
// Returns (nil, nil) when msg is nil (defensive; the pump
// should never produce a nil message but tests sometimes do).
func (r *Router) DispatchInbound(ctx context.Context, msg *messages.InboundMessage) (*inbound.CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}
	return r.inbound.Dispatch(ctx, msg)
}

// Start launches the per-channel inbound pump and the central
// dispatch loop. Blocks until ctx is cancelled or Stop is called.
func (r *Router) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.stopCh != nil {
		r.mu.Unlock()
		return errors.New("gateway: already started")
	}
	r.stopCh = make(chan struct{})
	r.channelCh = make(chan messages.InboundMessage, 64)
	chans := r.channels
	r.mu.Unlock()

	if len(chans) == 0 {
		r.mu.Lock()
		r.stopCh = nil
		r.channelCh = nil
		r.mu.Unlock()
		return errors.New("gateway: no channels attached")
	}

	for _, ch := range chans {
		r.wg.Add(1)
		go r.pumpInbound(ctx, ch)
	}

	r.wg.Add(1)
	go r.dispatchLoop(ctx)

	return nil
}

// Stop signals all dispatch goroutines to exit and waits for them
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

// pumpInbound reads messages.InboundMessage from ch.Incoming()
// and pushes it into the central dispatch channel.
func (r *Router) pumpInbound(ctx context.Context, ch channel.Channel) {
	defer r.wg.Done()
	in := ch.Incoming()
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
			r.mu.Lock()
			r.chatToChan[msg.ChatID] = ch
			r.mu.Unlock()
			select {
			case r.channelCh <- msg:
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			}
		}
	}
}

// dispatchLoop reads from the central messages.InboundMessage
// channel and routes through the wired *inbound.Router.
func (r *Router) dispatchLoop(ctx context.Context) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case msg, ok := <-r.channelCh:
			if !ok {
				return
			}
			result, err := r.DispatchInbound(withRouter(ctx, r), &msg)
			if err != nil {
				slog.Default().Warn("gateway: dispatch failed",
					"chat_id", msg.ChatID, "err", err)
			}
			if result == nil {
				continue
			}
			// Slash commands return a CommandResult whose Reply
			// field carries the bot's text. The previous loop
			// dropped it on the floor — /new / /use / /close /
			// /gtw subcommands all hung silently after the
			// F-58 dispatcher rewrite. Forward Reply through
			// the wired Emitter so the user sees the expected
			// "Now using pi…", "Reset N session(s)…", etc.
			//
			// ReplyTo = msg.MessageID anchors the chat-side
			// thread so Feishu renders the reply as a thread
			// reply to the slash command, not as a free-floating
			// bot message.
			if result.Reply != "" && r.emitter != nil {
				if sendErr := r.emitter.Send(ctx, messages.OutboundMessage{
					ChatID:  msg.ChatID,
					Kind:    messages.OutReply,
					Text:    result.Reply,
					ReplyTo: msg.MessageID,
				}); sendErr != nil {
					slog.Default().Warn("gateway: emit reply failed",
						"chat_id", msg.ChatID, "err", sendErr)
				}
			}
		}
	}
}

// withRouter installs r into ctx so handlers that need it can
// recover it without taking it as a closure. Currently unused
// by production code (the only handler that historically read
// gateway from ctx — /help — does so via direct injection in
// cmd/nightme); kept as a private helper so dispatchLoop can
// still install the router in ctx without taking it as an
// explicit parameter.
func withRouter(ctx context.Context, r *Router) context.Context {
	return context.WithValue(ctx, routerKey{}, r)
}

// routerKey is the unexported context key. Kept private —
// no external package currently reads the router back out of
// ctx.
type routerKey struct{}

// --- v1.1 binding table (commit 3) ----------------------------------

// Bind registers the binding (chatID → sessionID). Called by
// the /cwd handler after it creates a fresh session via
// MemoryManager.Register. Workspace / Agent are denormalized
// onto the row so subsequent /cwd replies don't have to
// re-query the session. The chatType argument was removed in
// F-33 (D1); nightme no longer carries chat-type at the
// binding layer.
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
// underlying session. Reserved for v0.4 multi-session; not
// used by current handlers.
func (r *Router) Unbind(chatID string) {
	r.mu.Lock()
	delete(r.bindings, chatID)
	r.mu.Unlock()
}

// resolveChannel returns the channel that owns chatID, falling
// back to the default channel for chats we haven't seen yet.
func (r *Router) resolveChannel(chatID string) channel.Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ch, ok := r.chatToChan[chatID]; ok && ch != nil {
		return ch
	}
	return r.defaultChannel
}
