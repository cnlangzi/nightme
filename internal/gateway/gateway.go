// Package gateway routes incoming chat messages. It is the
// inboundDispatcher for every InboundMessage produced by a
// Channel: each message flows in via pumpInbound → dispatchLoop →
// DispatchInbound, which branches to either the
// slashCommandDispatcher (when the message text is a registered
// slash command) or the messageDispatcher (default branch; for
// plain text the runtime injects a MessageDispatcher that routes
// to ChatSession + Agent).
//
// Three layers, named to match the v1.2 mental model:
//
//   inboundDispatcher        (this package; Gateway interface)
//     ├─ slashCommandDispatcher  (dispatchSlashCommand; inline)
//     └─ messageDispatcher       (runtime-injected MessageDispatcher)
//
// See docs/feat/F-20-gateway.md for the original router design
// and docs/feat/F-26-gateway-hub.md for the Stage-3
// responsibility-isolation spec.
//
// v1.3 (SPEC §0.1): the per-userMessage receipt FSM and all
// associated bookkeeping have been removed from Gateway. The
// receipt OBJECT is now entirely Channel-internal (each Channel
// picks its own state shape and storage form). Gateway's outbound
// flow simply stamps `OutboundMessage.ReplyTo = currentTurnUserMsgID`
// and lets each Channel route by that userMsgID. See SPEC §2.2 /
// §2.4 for the new shape.
//
// Imports: gateway imports session (v1.1). The session package
// does NOT import gateway (verified), so this direction is cycle-
// free. channel also imports gateway for the abstract message
// types (InboundMessage / OutboundMessage); session does not, so
// gateway's new session dep doesn't create a new cycle.
package gateway

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// Channel is the abstract surface the Gateway needs from any IM
// backend. Re-declared here (rather than imported from
// internal/channel) to keep the import graph acyclic: the channel
// package imports gateway for the abstract types, so gateway
// cannot import channel in turn. The mirror is kept in sync
// manually (and the test suite enforces the implementation match).
//
// v1.3: only the 4 lifecycle/messaging methods. The receipt FSM
// API has been removed — receipts are Channel-internal.
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
	// Dropped indicates the gateway intentionally did not forward
	// the inbound to the messageDispatcher (e.g. F-watch per-chat
	// gate). Distinct from Consumed=true (a slash command ran) so
	// log lines can distinguish "the slash command replied" vs
	// "the message was silently dropped".
	Dropped bool
}

// MessageDispatcher is the runtime-injected handler for the
// messageDispatcher branch of the inboundDispatcher.
//
// When DispatchInbound parses an inbound message and decides it is
// NOT a registered slash command (i.e., plain text, attachments,
// or an unrecognised "/foo"), it forwards the message to the
// MessageDispatcher. The runtime (cmd/nightme) wires the
// production implementation, which is responsible for:
//   - looking up or creating the ChatSession for the chat,
//   - resolving the active AgentSession,
//   - queueing the user turn into the InputBuffer FSM,
//   - bookkeeping receipts.
//
// A nil MessageDispatcher means messages that don't match a
// slash command are silently dropped (debug / test wiring).
type MessageDispatcher func(ctx context.Context, msg *InboundMessage) error

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
//
// v1.2: the interface carries binding-table operations (Bind /
// LookupByChat / ListBindings). cmd/nightme wires the session
// manager at construction time; handler code uses the binding
// surface instead of the v0.x chat-keyed SessionManager interface.
//
// v1.3: the receipt FSM API (CreateReceipt / UpdateReceipt /
// DisposeReceipt) has been removed. Outbound events are routed
// by stamping msg.ReplyTo = currentTurnUserMsgID; each Channel
// looks up its own receipt by that userMsgID.
type Gateway interface {
	Register(cmd Command) (replaced bool)

	// DispatchInbound is the inboundDispatcher entry point: every
	// InboundMessage produced by an attached Channel flows through
	// here. It parses the message text and branches to either
	//   - dispatchSlashCommand (when the text matches a registered
	//     Command), or
	//   - the MessageDispatcher injected at construction time
	//     (default branch for plain text).
	//
	// F-watch: non-slash-command messages are subject to the
	// per-chat WatchMode gate; messages with HasMention==false
	// and a WatchMode != WatchModeAll are dropped at this layer
	// before reaching the MessageDispatcher. Slash commands
	// bypass the gate.
	DispatchInbound(ctx context.Context, msg *InboundMessage) (*CommandResult, error)

	ListCommands() []Command

	// v1.2 binding table (commit 3); chatType removed in F-33 (D1):
	Bind(chatID, sessionID, workspace, agent string) *BindingEntry
	LookupByChat(chatID string) *BindingEntry
	ListBindings() []BindingEntry

	// OnMessageState is the v1.3 (F-31) ChatSession-callback
	// entry point. The runtime (cmd/nightme) wires gw.OnMessageState
	// into every ChatSession at startup via SetMessageStateHandler.
	// ChatSession calls it on lifecycle events (received /
	// forwarded / done / error); Gateway translates to
	// OutboundMessage{Kind: OutMessageState} and forwards to the
	// appropriate channel via resolveChannel + Send.
	OnMessageState(chatID, userMsgID string, state agent.MessageState)

	// Lifecycle
	AttachChannels(channels ...Channel)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Router is the exported alias of the runtime-internal
// concrete struct. cmd/nightme (and any other runtime) type-assert
// Gateway to *Router so it can call package-private hooks
// like AttachSweeper / AttachChannels / Start / Stop. The Gateway
// interface is the public contract; Router is the runtime
// handle that also owns the per-channel / per-session goroutines.
type Router = gateway

// WithWatchModeResolver installs the per-chat WatchMode lookup
// function used by the F-watch gate in DispatchInbound. The
// runtime (cmd/nightme/run.go) wires this after constructing
// the chatsession.Manager so the gateway can ask "what's the
// WatchMode for chat X?" without importing the chatsession
// package directly.
//
// Signature:
//   - (mode, true)  when the chat has a known mode
//   - (zero, false) when there is no ChatSession yet
//
// A nil resolver disables the gate entirely (current behaviour
// for tests and for runtimes that don't wire it).
func (g *gateway) WithWatchModeResolver(resolver func(chatID string) (chatsession.WatchMode, bool)) *gateway {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.watchModeResolver = resolver
	return g
}

// gateway is the concrete implementation + dispatch runtime. (It
// carries the method set; Router is just an alias so external
// packages can type-assert.)
//
// v1.3: the per-userMessage receipt bookkeeping
// (`receipts map[string]*receiptEntry`) has been removed. Gateway
// no longer knows about receipts at all — only the binding table
// (chat → ChatSession) remains.
type gateway struct {
	mu    sync.RWMutex
	cmds  map[string]Command
	order []string
	// messageDispatcher is the runtime-injected handler for the
	// messageDispatcher branch (non-slash-command inbound). When
	// nil, such messages are silently dropped.
	messageDispatcher MessageDispatcher

	// Stage 2 / v1.2 dispatch state. fields below are read/written under mu.
	channels       []Channel
	channelCh      chan InboundMessage
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	chatToChan     map[string]Channel // ChatID -> the channel that owns the chat
	defaultChannel Channel            // fallback channel for chats we haven't seen yet

	// v1.2 binding table (chat_id → session_id). Owned by Gateway.
	bindings map[string]*BindingEntry

	// F-watch §3.1.1: per-chat WatchMode resolver. Returns
	// (mode, true) when the chat has a known mode; (zero, false)
	// when there is no ChatSession yet or resolver is unset.
	// Wired by cmd/nightme/run.go via WithWatchModeResolver.
	// Reading is concurrent-safe; updating takes mu.
	watchModeResolver func(chatID string) (chatsession.WatchMode, bool)
}

// New constructs a Gateway (the inboundDispatcher).
//
// messageDispatcher is the runtime-injected handler for the
// messageDispatcher branch (default, non-slash-command inbound).
// Pass nil to drop such messages (debug-only Gateway).
//
// v1.2 runtime closes over *chatsession.Manager via the
// messageDispatcher closure; the Gateway itself no longer holds a
// session manager reference. ChatSession lifecycle is owned by
// the runtime + the chatsession package.
func New(messageDispatcher MessageDispatcher) Gateway {
	return &gateway{
		cmds:              make(map[string]Command),
		messageDispatcher: messageDispatcher,
		chatToChan:        make(map[string]Channel),
		bindings:          make(map[string]*BindingEntry),
	}
}

// AttachChannels registers the channels the gateway will read from
// and dispatch to. Multi-channel is supported.
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

// DispatchInbound is the inboundDispatcher entry point (implements
// the Gateway interface). Every InboundMessage from an attached
// Channel flows through here. It parses the message text and
// branches to one of:
//
//   - dispatchSlashCommand(ctx, msg, name, args) when the text is a
//     recognised slash command; or
//   - the runtime-injected MessageDispatcher for plain text,
//     attachments, or unrecognised "/foo".
//
// Either branch may produce a CommandResult; the caller
// (dispatchLoop) does not interpret it further.
func (g *gateway) DispatchInbound(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}
	name, args, err := ParseCommand(strings.TrimSpace(msg.Text))

	// F-watch §3.1.1: per-chat message-watch gate (single entry,
	// both branches below). Slash commands ALWAYS pass the gate
	// — `/watch on` must work even from non-mention group
	// messages, otherwise users can't opt back in once they've
	// opted out. Only the plain-text / unrecognised-slash paths
	// consult the gate.
	//
	// Drop condition: HasMention==false (group, no bot/@_all)
	// AND chat is configured to drop non-mention messages.
	//
	// DM chats always pass: HasMention is set to true by the
	// channel adapter for every DM (every DM is implicitly
	// addressed to the bot). WatchMode is consulted but irrelevant.
	gateAndDispatch := func() (*CommandResult, error) {
		if dropped, _ := g.applyWatchModeGate(msg); dropped {
			return &CommandResult{Consumed: true, Dropped: true}, nil
		}
		return g.dispatchMessage(ctx, msg)
	}

	if err != nil || name == "" {
		return gateAndDispatch()
	}

	g.mu.RLock()
	cmd, ok := g.cmds[strings.ToLower(name)]
	g.mu.RUnlock()
	if !ok || cmd.Handler == nil {
		return gateAndDispatch()
	}
	return g.dispatchSlashCommand(ctx, msg, name, args, cmd)
}

// applyWatchModeGate runs the F-watch §3.1.1 per-chat gate for
// one message. Returns (dropped=true) when the message should be
// silently discarded; (false) when it should proceed to the
// runtime message dispatcher.
//
// The gate NEVER fires for slash commands — callers must invoke
// this only for plain-text / unrecognised-slash paths. The two
// callers in DispatchInbound respect that.
//
// DM invariant: the channel adapter is contractually required to
// set HasMention=true for every DM message (see
// computeHasMention's chat_type == "p2p" branch). Therefore DM
// chats never reach this function with HasMention=false and
// WatchMode is effectively a no-op for them.
func (g *gateway) applyWatchModeGate(msg *InboundMessage) (bool, bool) {
	if msg.HasMention {
		return false, true // pass (mention, regardless of mode)
	}
	if !g.shouldDropForWatchMode(msg.ChatID) {
		return false, true // pass (no preference to enforce)
	}
	log.Printf("gateway: drop non-mention message (WatchMode != All) chat_id=%s message_id=%s",
		msg.ChatID, msg.MessageID)
	return true, true
}

// shouldDropForWatchMode returns true when the chat (if any) is
// configured to drop non-mention messages. A nil / missing
// ChatSession returns false (no preference to enforce — the
// downstream dispatcher will reply with "send /cwd first" as
// before).
//
// DM invariant: DM chats NEVER reach this function with
// HasMention=false. The channel adapter is contractually
// required to set HasMention=true for every DM message (see
// computeHasMention in internal/channel/feishu/mention.go —
// the chat_type == "p2p" branch returns true unconditionally).
// Therefore, even with WatchMode=WatchModeMention, DM messages
// always pass the gate. /watch on/off has no observable effect
// in DM chats — the state is still persisted so a future
// chat-type switch (group) preserves the user's preference.
//
// The check is intentionally a method on *gateway* (rather than
// inlined at every callsite) so future changes (e.g. caching the
// WatchMode lookup) happen in one place. See
// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.11.
func (g *gateway) shouldDropForWatchMode(chatID string) bool {
	if g.watchModeResolver == nil {
		return false
	}
	mode, ok := g.watchModeResolver(chatID)
	if !ok {
		return false
	}
	return mode != chatsession.WatchModeAll
}

// dispatchSlashCommand runs the registered Command.Handler for a
// recognised slash command. This is the slashCommandDispatcher
// branch of the inboundDispatcher.
func (g *gateway) dispatchSlashCommand(ctx context.Context, msg *InboundMessage, name string, args []string, cmd Command) (*CommandResult, error) {
	if cmd.Handler == nil {
		return &CommandResult{Consumed: false}, nil
	}
	return cmd.Handler(ctx, msg, args)
}

// dispatchMessage forwards a non-slash-command inbound message to
// the runtime-injected MessageDispatcher. This is the
// messageDispatcher branch of the inboundDispatcher. A nil
// MessageDispatcher silently drops the message.
func (g *gateway) dispatchMessage(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	if g.messageDispatcher == nil {
		return &CommandResult{Consumed: false}, nil
	}
	if err := g.messageDispatcher(ctx, msg); err != nil {
		return &CommandResult{}, err
	}
	return &CommandResult{}, nil
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
			result, err := g.DispatchInbound(withGateway(ctx, g), &msg)
			if err != nil {
				log.Printf("gateway: dispatch %s failed: %v", msg.ChatID, err)
			}
			if result == nil {
				continue
			}
		}
	}
}

// sweepSessions is a no-op in v1.2: the v1.2 runtime installs one
// readPump per active AgentSession directly via the chatsession
// EventHandler surface; the Gateway no longer needs to poll a
// sweeper to discover new sources. Kept as a stub so the dispatch
// goroutine continues to compile.
func (g *gateway) sweepSessions(ctx context.Context) {
	defer g.wg.Done()
	select {
	case <-ctx.Done():
	case <-g.stopCh:
	}
}

// pumpOutbound reads AgentEvent from the session's events channel,
// translates via Translate, and dispatches to the right Channel.
func (g *gateway) pumpOutbound(ctx context.Context, ch Channel, sessionID, chatID string, events <-chan agent.AgentEvent) {
	defer g.wg.Done()
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

// --- v1.1 binding table (commit 3) ----------------------------------

// Bind registers the binding (chatID → sessionID). Called by the
// /cwd handler after it creates a fresh session via
// MemoryManager.Register. Workspace / Agent are denormalized onto
// the row so subsequent /cwd replies don't have to re-query the
// session. The chatType argument was removed in F-33 (D1); nightme
// no longer carries chat-type at the binding layer.
func (g *gateway) Bind(chatID, sessionID, workspace, agent string) *BindingEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := &BindingEntry{
		ChatID:    chatID,
		SessionID: sessionID,
		Workspace: workspace,
		Agent:     agent,
	}
	g.bindings[chatID] = b
	return b
}

// LookupByChat returns the binding for chatID, or nil.
func (g *gateway) LookupByChat(chatID string) *BindingEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.bindings[chatID]
}

// ListBindings returns a snapshot of every binding in unspecified
// order. The slice is freshly allocated.
func (g *gateway) ListBindings() []BindingEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]BindingEntry, 0, len(g.bindings))
	for _, b := range g.bindings {
		out = append(out, *b)
	}
	return out
}

// RestoreBindings populates the binding table from a snapshot
// (typically loaded from the registry at startup). Replaces any
// existing entries.
func (g *gateway) RestoreBindings(entries []BindingEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bindings = make(map[string]*BindingEntry, len(entries))
	for i := range entries {
		e := entries[i]
		g.bindings[e.ChatID] = &e
	}
}

// Unbind removes the binding for chatID without affecting the
// underlying session. Reserved for v0.4 multi-session; not used
// by current handlers.
func (g *gateway) Unbind(chatID string) {
	g.mu.Lock()
	delete(g.bindings, chatID)
	g.mu.Unlock()
}

// resolveChannel returns the channel that owns chatID, falling back
// to the default channel for chats we haven't seen yet.
func (g *gateway) resolveChannel(chatID string) Channel {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if ch, ok := g.chatToChan[chatID]; ok && ch != nil {
		return ch
	}
	return g.defaultChannel
}

// OnMessageState is the v1.3 (F-31) ChatSession-callback entry
// point. Wired into every ChatSession at startup via
// SetMessageStateHandler. Translates the abstract lifecycle event
// to OutboundMessage{Kind: OutMessageState} and forwards via the
// channel that owns chatID.
//
// Failure semantics (per F-31 §9):
//   - No channel for chatID: silent drop (debug log only).
//   - Channel.Send error: log warn, never block caller.
//     ChatSession lifecycle is unaffected by render failures.
//   - Handler called with empty chatID or userMsgID: silent drop.
//
// Concurrency: callable from any goroutine. resolveChannel takes
// RLock on g.mu briefly; Send runs synchronously per the caller's
// context (background ctx is used internally because ChatSession
// doesn't currently pass a cancellable ctx to emitMessageState).
func (g *gateway) OnMessageState(chatID, userMsgID string, state agent.MessageState) {
	if chatID == "" || userMsgID == "" {
		return
	}
	ch := g.resolveChannel(chatID)
	if ch == nil {
		log.Printf("gateway: OnMessageState no channel for chat=%s, dropping", chatID)
		return
	}
	out := OutboundMessage{
		Kind:   OutMessageState,
		ChatID: chatID,
		Meta: map[string]any{
			"message_id": userMsgID,
			"state":      state,
		},
		MessageState: &MessageStatePayload{
			State: state,
		},
	}
	if err := ch.Send(context.Background(), out); err != nil {
		log.Printf("gateway: MessageState send failed (chat=%s, state=%s): %v", chatID, state, err)
	}
}

