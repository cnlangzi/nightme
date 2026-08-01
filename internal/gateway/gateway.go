// Package gateway routes incoming chat messages to slash commands
// registered by nightme, or to a fallback handler (typically the
// session manager forwarding text to the live agent). See
// docs/feat/F-20-gateway.md for the original router design and
// docs/feat/F-26-gateway-hub.md for the Stage-1 hub-and-spoke
// extension.
//
// Stage 2 (this file) makes Gateway a real dispatcher: Start()
// launches a goroutine per Channel (reads InboundMessage → Handle)
// and a goroutine per discovered Session (reads AgentEvent →
// OutboundMessage → Channel.Send).
//
// The session manager is NOT imported by gateway — instead, the
// runtime provides a SweepSessions callback that returns the
// currently-running sessions as OutboundSources. The runtime
// (cmd/nightme) has access to both gateway and session, so it
// bridges them. This pattern keeps gateway free of the
// session → channel/feishu → channel → gateway import cycle.
package gateway

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Channel is the abstract surface the Gateway needs from any IM
// backend. Re-declared here (rather than imported from
// internal/channel) to keep the import graph acyclic: the channel
// package imports gateway for the abstract types, so gateway
// cannot import channel in turn. The mirror is kept in sync
// manually (and the test suite enforces the implementation match).
type Channel interface {
	Name() string
	Incoming() <-chan InboundMessage
	Send(ctx context.Context, msg OutboundMessage) error
}

// Command is one nightme-level slash command.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Handler     func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error)
}

// CommandResult is the per-dispatch outcome.
type CommandResult struct {
	Reply    string
	Consumed bool
}

// FallbackHandler is invoked when the Gateway decides the message is
// not a nightme command.
type FallbackHandler func(ctx context.Context, msg *InboundMessage) error

// OutboundSource is one running session's outbound event stream.
// The runtime (typically cmd/nightme) provides these to the
// Gateway via the SweepSessions callback. Each source maps a chat
// to a single event channel; the Gateway attaches one outbound
// pump per source.
type OutboundSource struct {
	SessionID string
	ChatID    string
	Events    <-chan agent.AgentEvent
}

// SweepSessions returns the currently-running OutboundSources. The
// Gateway polls this on a ticker (every 5s) to discover new
// sessions the /run command creates between sweeps. Returning nil
// or an empty slice is fine — the Gateway simply has nothing to
// attach.
type SweepSessions func() []OutboundSource

// Gateway is the public contract for the slash-command router.
type Gateway interface {
	Register(cmd Command) (replaced bool)
	Handle(ctx context.Context, msg *InboundMessage) (*CommandResult, error)
	ListCommands() []Command
}

// Router is the exported alias of the runtime-internal
// concrete struct. cmd/nightme (and any other runtime) type-assert
// Gateway to *Router so it can call package-private hooks
// like AttachSweeper / AttachChannels / Start / Stop. The Gateway
// interface is the public contract; Router is the runtime
// handle that also owns the per-channel / per-session goroutines.
type Router = gateway

// gateway is the concrete implementation + dispatch runtime. (It
// carries the method set; Router is just an alias so external
// packages can type-assert.)
type gateway struct {
	mu    sync.RWMutex
	cmds  map[string]Command
	order []string
	fb    FallbackHandler

	// Stage 2 runtime state. fields below are read/written under mu.
	channels       []Channel
	channelCh      chan InboundMessage
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	attached       map[string]struct{} // SessionID -> already-has-a-pump
	chatToChan     map[string]Channel  // ChatID -> the channel that owns the chat
	defaultChannel Channel             // fallback channel for chats we haven't seen yet
	sweeper        SweepSessions
}

// New constructs a Gateway. The optional fallback handler is invoked
// when no command matches the inbound message.
func New(fallback FallbackHandler) Gateway {
	return &gateway{
		cmds:       make(map[string]Command),
		fb:         fallback,
		attached:   make(map[string]struct{}),
		chatToChan: make(map[string]Channel),
	}
}

// AttachSweeper registers the callback the Gateway uses to discover
// running sessions. Required before Start. The callback is invoked
// every 5s; it should be cheap and non-blocking.
func (g *gateway) AttachSweeper(s SweepSessions) {
	g.mu.Lock()
	g.sweeper = s
	g.mu.Unlock()
}

// AttachChannels registers the channels the gateway will read from
// and dispatch to. In Stage 2 only one channel is supported per
// Gateway; multi-channel (F-11) will be a separate commit.
func (g *gateway) AttachChannels(channels ...Channel) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.channels = append(g.channels, channels...)
	if len(channels) > 0 {
		for _, c := range channels {
			if c != nil {
				g.defaultChannel = c
				break
			}
		}
	}
}

// Register stores cmd. Names and aliases are case-folded on insert.
func (g *gateway) Register(cmd Command) (replaced bool) {
	name := strings.ToLower(cmd.Name)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.cmds[name]; exists {
		return true
	}
	for _, a := range cmd.Aliases {
		alias := strings.ToLower(a)
		if _, exists := g.cmds[alias]; exists {
			return true
		}
	}
	for _, a := range cmd.Aliases {
		g.cmds[strings.ToLower(a)] = cmd
	}
	g.cmds[name] = cmd
	g.order = append(g.order, name)
	return false
}

// Handle implements the Gateway interface.
func (g *gateway) Handle(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}
	name, args, err := ParseCommand(strings.TrimSpace(msg.Text))
	matched := err == nil
	if err != nil {
		if g.fb != nil {
			return &CommandResult{}, g.fb(ctx, msg)
		}
		return &CommandResult{}, nil
	}
	if !matched {
		if g.fb != nil {
			return &CommandResult{}, g.fb(ctx, msg)
		}
		return &CommandResult{Consumed: false}, nil
	}
	g.mu.RLock()
	cmd, ok := g.cmds[strings.ToLower(name)]
	g.mu.RUnlock()
	if !ok || cmd.Handler == nil {
		if g.fb != nil {
			return &CommandResult{}, g.fb(ctx, msg)
		}
		return &CommandResult{Consumed: false}, nil
	}
	return cmd.Handler(ctx, msg, args)
}

// ListCommands returns the registered commands in
// case-insensitive alphabetical order.
func (g *gateway) ListCommands() []Command {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Command, 0, len(g.order))
	seen := make(map[string]bool, len(g.order))
	for _, name := range g.order {
		if seen[name] {
			continue
		}
		if c, ok := g.cmds[name]; ok {
			out = append(out, c)
			seen[name] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- Stage 2: dispatch runtime ----------------------------------------

// Start launches the per-channel inbound pump, the central
// dispatch loop, and the per-session outbound sweeper. Blocks
// until ctx is cancelled or Stop is called.
func (g *gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.stopCh != nil {
		g.mu.Unlock()
		return errors.New("gateway: already started")
	}
	g.stopCh = make(chan struct{})
	g.channelCh = make(chan InboundMessage, 64)
	chans := g.channels
	g.mu.Unlock()

	if len(chans) == 0 {
		g.mu.Lock()
		g.stopCh = nil
		g.channelCh = nil
		g.mu.Unlock()
		return errors.New("gateway: no channels attached")
	}

	for _, ch := range chans {
		g.wg.Add(1)
		go g.pumpInbound(ctx, ch)
	}

	g.wg.Add(1)
	go g.dispatchLoop(ctx)

	g.wg.Add(1)
	go g.sweepSessions(ctx)

	return nil
}

// Stop signals all dispatch goroutines to exit and waits for them
// to drain. Idempotent.
func (g *gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	stopCh := g.stopCh
	g.mu.Unlock()
	if stopCh == nil {
		return nil
	}
	g.stopOnce.Do(func() { close(stopCh) })
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pumpInbound reads InboundMessage from ch.Incoming() and pushes it
// into the central dispatch channel.
func (g *gateway) pumpInbound(ctx context.Context, ch Channel) {
	defer g.wg.Done()
	in := ch.Incoming()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			g.mu.Lock()
			g.chatToChan[msg.ChatID] = ch
			g.mu.Unlock()
			select {
			case g.channelCh <- msg:
			case <-ctx.Done():
				return
			case <-g.stopCh:
				return
			}
		}
	}
}

// dispatchLoop reads from the central InboundMessage channel and
// routes through Handle.
func (g *gateway) dispatchLoop(ctx context.Context) {
	defer g.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case msg, ok := <-g.channelCh:
			if !ok {
				return
			}
			// Install the Gateway into ctx so handlers that need
			// to look it up (currently just /help) can recover it
			// without taking it as a closure.
			result, err := g.Handle(withGateway(ctx, g), &msg)
			if err != nil {
				log.Printf("gateway: dispatch %s failed: %v", msg.ChatID, err)
			}
			if result == nil {
				continue
			}
		}
	}
}

// sweepSessions polls the SweepSessions callback and attaches a
// new outbound pump to every OutboundSource the runtime has not
// reported yet.
func (g *gateway) sweepSessions(ctx context.Context) {
	defer g.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.sweepOnce(ctx)
		}
	}
}

// sweepOnce walks the runtime's OutboundSource list and attaches
// a pump to every source we haven't seen yet.
func (g *gateway) sweepOnce(ctx context.Context) {
	g.mu.RLock()
	sweeper := g.sweeper
	if sweeper == nil {
		g.mu.RUnlock()
		return
	}
	g.mu.RUnlock()

	for _, src := range sweeper() {
		if src.SessionID == "" || src.ChatID == "" || src.Events == nil {
			continue
		}

		g.mu.Lock()
		if _, ok := g.attached[src.SessionID]; ok {
			g.mu.Unlock()
			continue
		}
		g.attached[src.SessionID] = struct{}{}
		ch := g.chatToChan[src.ChatID]
		if ch == nil {
			ch = g.defaultChannel
		}
		g.mu.Unlock()

		if ch == nil {
			continue
		}

		g.wg.Add(1)
		go g.pumpOutbound(ctx, ch, src.SessionID, src.ChatID, src.Events)
	}
}

// pumpOutbound reads AgentEvent from the session's events channel,
// translates via Translate, and dispatches to the right Channel.
func (g *gateway) pumpOutbound(ctx context.Context, ch Channel, sessionID, chatID string, events <-chan agent.AgentEvent) {
	defer g.wg.Done()
	defer func() {
		g.mu.Lock()
		delete(g.attached, sessionID)
		g.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			msg, send := Translate(chatID, ev)
			if !send {
				continue
			}
			if err := ch.Send(ctx, msg); err != nil {
				// Fire-and-ack: log and continue. Retry queue is
				// explicitly out of scope for v0.3 — failures
				// surface via the channel's own error reporting
				// (Feishu's UpdateMessage / AddReaction paths log
				// their own failures at warn level).
				log.Printf("gateway: channel send failed (chat=%s, kind=%s): %v", chatID, msg.Kind, err)
			}
		}
	}
}

// withGateway installs gw into ctx so handlers that need it
// (currently just /help) can recover it without taking it as a
// closure. Exported as WithGateway so external runtimes (cmd/nightme,
// tests) can install the gateway from their own context as well.
func withGateway(ctx context.Context, gw Gateway) context.Context {
	return context.WithValue(ctx, GatewayKey{}, gw)
}

// WithGateway is the exported alias of withGateway for runtime
// callers (cmd/nightme uses this from main to install the
// gateway before the dispatch loop starts).
func WithGateway(ctx context.Context, gw Gateway) context.Context {
	return withGateway(ctx, gw)
}

// GatewayKey is the unexported context key re-exported as a type
// (so handlers in gateway/cmd can fetch the gateway from the
// same context value the gateway's dispatchLoop installed).
type GatewayKey struct{}

// contextKey aliases GatewayKey so the type used by withGateway
// and gwFromContext is identical (no duplicate struct declaration
// in the cmd subpackage).
type contextKey = GatewayKey
