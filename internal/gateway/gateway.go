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
// v1.1 (commit 3 + 4): the Gateway owns the chat → session binding
// table and the per-userMessage receipt FSM. Channel is the dumb
// renderer that paints receipt state transitions; Session is the
// pure-process factory; Gateway sits between them and drives the
// lifecycle. See F-26 §2 for the full picture.
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
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/receipt"
	"github.com/cnlangzi/nightme/internal/session"
)

// Channel is the abstract surface the Gateway needs from any IM
// backend. Re-declared here (rather than imported from
// internal/channel) to keep the import graph acyclic: the channel
// package imports gateway for the abstract types, so gateway
// cannot import channel in turn. The mirror is kept in sync
// manually (and the test suite enforces the implementation match).
//
// v1.1: extended with the receipt lifecycle methods. Gateway
// drives the FSM; Channel renders the state transitions in its
// native UI.
type Channel interface {
	Name() string
	Incoming() <-chan InboundMessage
	Send(ctx context.Context, msg OutboundMessage) error

	CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (receipt.Receipt, error)
	UpdateReceipt(ctx context.Context, receipt receipt.Receipt, state receipt.ReceiptState) error
	DisposeReceipt(ctx context.Context, receipt receipt.Receipt) error
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
// v1.1: the interface adds binding-table operations (Bind / Rebind /
// LookupByChat / LookupSessionByChat / SpawnAgent / ListBindings /
// RestoreBindings) and the receipt FSM (CreateReceipt /
// UpdateReceipt / DisposeReceipt). cmd/nightme wires the session
// manager at construction time; handler code uses the binding +
// receipt surface instead of the v0.x chat-keyed SessionManager
// interface that was deleted from gateway/cmd.
type Gateway interface {
	Register(cmd Command) (replaced bool)

	// DispatchInbound is the inboundDispatcher entry point: every
	// InboundMessage produced by an attached Channel flows through
	// here. It parses the message text and branches to either
	//   - dispatchSlashCommand (when the text matches a registered
	//     Command), or
	//   - the MessageDispatcher injected at construction time
	//     (default branch for plain text).
	DispatchInbound(ctx context.Context, msg *InboundMessage) (*CommandResult, error)

	ListCommands() []Command

	// v1.1 binding table (commit 3):
	Bind(chatID, chatType, sessionID, workspace, agent string) *BindingEntry
	LookupByChat(chatID string) *BindingEntry
	LookupSessionByChat(chatID string) (*session.Session, error)
	SpawnAgent(ctx context.Context, chatID, agentName string, args []string) (*session.Session, error)
	ListBindings() []BindingEntry

	// v1.1 receipt FSM (commit 3):
	CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (receipt.Receipt, error)
	UpdateReceipt(ctx context.Context, userMsgID string, state receipt.ReceiptState) error
	DisposeReceipt(ctx context.Context, userMsgID string) error

	// OnSessionEvent is the v1.1 single-consumer event handler.
	// Wired by the runtime into MemoryManager.SetEventCallback at
	// startup; the manager invokes it once per AgentEvent from
	// inside readPump. Translates, sends, and flips receipts.
	// (Defined on the interface so cmd/nightme can wire it
	// without a type-assertion back to *Router.)
	OnSessionEvent(s *session.Session, ev agent.AgentEvent)

	// OnMessageState is the v1.3 (F-31) ChatSession-callback
	// entry point. The runtime (cmd/nightme) wires gw.OnMessageState
	// into every ChatSession at startup via SetMessageStateHandler.
	// ChatSession calls it on lifecycle events (received /
	// forwarded / done / error); Gateway translates to
	// OutboundMessage{Kind: OutMessageState} and forwards to the
	// appropriate channel via resolveChannel + Send.
	//
	// Decoupled from OnSessionEvent: agent events flow through
	// OnSessionEvent; message lifecycle events flow through
	// OnMessageState. Both end up at Channel.Send but carry
	// different semantics.
	OnMessageState(chatID, userMsgID string, state receipt.MessageState)
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
//
// v1.1 fields (commits 3 + 4): the Gateway owns the chat → session
// binding table (BindingEntry) and the per-userMessage receipt
// FSM (receiptEntry). These were previously split across the
// cmd/nightme runtime chatCoordinator (bindings) and the
// session.InputBuffer receipts map (receipts). See F-26 §2.
type gateway struct {
	mu    sync.RWMutex
	cmds  map[string]Command
	order []string
	// messageDispatcher is the runtime-injected handler for the
	// messageDispatcher branch (non-slash-command inbound). When
	// nil, such messages are silently dropped.
	messageDispatcher MessageDispatcher

	// Session factory + lookup. Set at construction time by the
	// runtime (cmd/nightme). Used by LookupSessionByChat and
	// SpawnAgent. The Gateway does not own the manager — it
	// borrows it for the chat-binding semantic.
	mgr session.Manager

	// Stage 2 / v1.1 dispatch state. fields below are read/written under mu.
	channels       []Channel
	channelCh      chan InboundMessage
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	attached       map[string]struct{} // SessionID -> already-has-a-pump (commit 4 will remove)
	chatToChan     map[string]Channel  // ChatID -> the channel that owns the chat
	defaultChannel Channel             // fallback channel for chats we haven't seen yet
	sweeper        SweepSessions       // commit 4 will remove

	// v1.1 binding table (chat_id → session_id). Owned by Gateway.
	bindings map[string]*BindingEntry

	// v1.1 receipt FSM bookkeeping (userMsgID → receipt state).
	// Owned by Gateway; Channel renders transitions.
	receipts map[string]*receiptEntry

	// eventCallback is the optional runtime-installed callback.
	// When set, OnSessionEvent delegates to it. When nil,
	// OnSessionEvent does the default translate + send + receipt
	// flip work (used by the runtime's startup wiring where the
	// runtime simply passes gw.OnSessionEvent as the callback —
	// the indirection here is for tests).
	eventCallback func(s *session.Session, ev agent.AgentEvent)
}

// New constructs a Gateway (the inboundDispatcher).
//
// messageDispatcher is the runtime-injected handler for the
// messageDispatcher branch (default, non-slash-command inbound).
// Pass nil to drop such messages (debug-only Gateway).
//
// mgr is the session factory Gateway uses to look up sessions by
// ID and to spawn agents; passing nil disables those operations
// (cmd / run / kill won't work, but a debug-only Gateway stays
// operational).
func New(messageDispatcher MessageDispatcher, mgr session.Manager) Gateway {
	return &gateway{
		cmds:              make(map[string]Command),
		messageDispatcher: messageDispatcher,
		attached:          make(map[string]struct{}),
		chatToChan:        make(map[string]Channel),
		bindings:          make(map[string]*BindingEntry),
		receipts:          make(map[string]*receiptEntry),
		mgr:               mgr,
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
	if err != nil {
		// Parse failure (e.g. "/" with no command): treat as plain
		// message and forward to the messageDispatcher branch.
		return g.dispatchMessage(ctx, msg)
	}
	if name == "" {
		return g.dispatchMessage(ctx, msg)
	}
	g.mu.RLock()
	cmd, ok := g.cmds[strings.ToLower(name)]
	g.mu.RUnlock()
	if !ok || cmd.Handler == nil {
		return g.dispatchMessage(ctx, msg)
	}
	return g.dispatchSlashCommand(ctx, msg, name, args, cmd)
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

// --- v1.1 binding table (commit 3) ----------------------------------

// Bind registers the binding (chatID → sessionID). Called by the
// /cwd handler after it creates a fresh session via
// MemoryManager.Register. ChatType / Workspace / Agent are
// denormalized onto the row so subsequent /cwd replies don't have
// to re-query the session.
func (g *gateway) Bind(chatID, chatType, sessionID, workspace, agent string) *BindingEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := &BindingEntry{
		ChatID:    chatID,
		ChatType:  chatType,
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

// LookupSessionByChat returns the session bound to chatID. Returns
// session.ErrSessionNotFound when chatID has no binding; callers
// can distinguish "no binding" (binding == nil) from "binding
// points at missing session" (binding != nil + error).
func (g *gateway) LookupSessionByChat(chatID string) (*session.Session, error) {
	if g.mgr == nil {
		return nil, errors.New("gateway: no session manager wired")
	}
	b := g.LookupByChat(chatID)
	if b == nil {
		return nil, session.ErrSessionNotFound
	}
	return g.mgr.Get(b.SessionID)
}

// SpawnAgent spawns an agent in the chat's bound workspace and
// updates the binding to point at the new session ID. Used by the
// /run handler. If a session is already running for chatID, this
// is a no-op (returns the existing session).
func (g *gateway) SpawnAgent(ctx context.Context, chatID, agentName string, args []string) (*session.Session, error) {
	if g.mgr == nil {
		return nil, errors.New("gateway: no session manager wired")
	}
	b := g.LookupByChat(chatID)
	if b == nil {
		return nil, session.ErrSessionNotFound
	}
	sess, err := g.mgr.Get(b.SessionID)
	if err != nil {
		return nil, err
	}
	if sess.Status() == session.StatusRunning {
		return sess, nil
	}
	newSess, err := g.mgr.Create(ctx, session.CreateRequest{
		Workspace: b.Workspace,
		Agent:     agentName,
		Args:      args,
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	b.SessionID = newSess.ID
	b.Agent = agentName
	g.mu.Unlock()
	return newSess, nil
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

// --- v1.1 receipt FSM (commit 3) -------------------------------------

// CreateReceipt creates a receipt for chatID/userMsgID with the given
// blocks, stores it in the receipts map, and returns the channel's
// opaque Receipt handle. If CreateReceipt fails (e.g., the channel
// can't reach the IM backend) the entry is still stored so a
// subsequent UpdateReceipt / DisposeReceipt on a different code
// path doesn't panic; callers should still treat err != nil as a
// signal to skip receipt bookkeeping for that userMsgID.
func (g *gateway) CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (receipt.Receipt, error) {
	ch := g.resolveChannel(chatID)
	if ch == nil {
		return nil, errors.New("gateway: no channel for chat " + chatID)
	}
	rcpt, err := ch.CreateReceipt(ctx, chatID, userMsgID, blocks)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	binding := g.bindings[chatID]
	var sessionID string
	if binding != nil {
		sessionID = binding.SessionID
	}
	g.receipts[userMsgID] = &receiptEntry{
		chatID:    chatID,
		sessionID: sessionID,
		receipt:   rcpt,
		state:     receipt.ReceiptPending,
	}
	g.mu.Unlock()
	return rcpt, nil
}

// UpdateReceipt transitions the receipt for userMsgID to state.
// The receipt must already have been created via CreateReceipt.
// Returns nil if the receipt doesn't exist (caller may have given
// up on bookkeeping for that message — Gateway is best-effort).
func (g *gateway) UpdateReceipt(ctx context.Context, userMsgID string, state receipt.ReceiptState) error {
	g.mu.RLock()
	entry := g.receipts[userMsgID]
	g.mu.RUnlock()
	if entry == nil {
		return nil
	}
	ch := g.resolveChannel(entry.chatID)
	if ch == nil {
		return nil
	}
	entry.mu.Lock()
	entry.state = state
	entry.mu.Unlock()
	return ch.UpdateReceipt(ctx, entry.receipt, state)
}

// DisposeReceipt removes the receipt from the receipts map and
// asks the channel to clean up. Idempotent.
func (g *gateway) DisposeReceipt(ctx context.Context, userMsgID string) error {
	g.mu.Lock()
	entry := g.receipts[userMsgID]
	if entry != nil {
		delete(g.receipts, userMsgID)
	}
	g.mu.Unlock()
	if entry == nil {
		return nil
	}
	ch := g.resolveChannel(entry.chatID)
	if ch == nil {
		return nil
	}
	return ch.DisposeReceipt(ctx, entry.receipt)
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

// SetEventCallback is the v1.1 single-consumer wiring point. The
// runtime (cmd/nightme) calls this with gw.OnSessionEvent at
// startup; the MemoryManager's readPump invokes the callback once
// per AgentEvent (the only consumer of session.Events()).
func (g *gateway) SetEventCallback(cb func(s *session.Session, ev agent.AgentEvent)) {
	// No-op when the gateway already routes events via its own
	// OnSessionEvent method; the runtime wires gw.OnSessionEvent
	// directly into mgr.SetEventCallback. This method exists for
	// tests / alternate runtimes that want to install a custom
	// callback without depending on the gateway's default.
	if cb == nil {
		g.mu.Lock()
		g.eventCallback = nil
		g.mu.Unlock()
		return
	}
	g.mu.Lock()
	g.eventCallback = cb
	g.mu.Unlock()
}

// OnSessionEvent is the v1.1 single-consumer event handler. The
// runtime wires this into MemoryManager.EventCallback at startup;
// the manager invokes it once per AgentEvent from inside readPump
// (the only consumer of session.Events()). Translates the event,
// dispatches the OutboundMessage, and (for terminal events) flips
// any still-open receipts to Done / Error and disposes them.
func (g *gateway) OnSessionEvent(s *session.Session, ev agent.AgentEvent) {
	g.mu.RLock()
	cb := g.eventCallback
	g.mu.RUnlock()
	if cb != nil {
		cb(s, ev)
		return
	}
	g.translateAndSend(s, ev)
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
func (g *gateway) OnMessageState(chatID, userMsgID string, state receipt.MessageState) {
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

// translateAndSend does the actual work: look up the chat for the
// session, translate the event, fan out to every receipt bound
// to this session, and (for terminal events) flip those receipts
// to Done/Error. This is the body of OnSessionEvent when no
// runtime-installed callback is present (the default).
//
// Receipt fan-out (1 request : n response per user message, plus
// the buffered-batch case where one agent turn answers n user
// messages): each bound receipt gets its own OutboundMessage
// carrying ReplyTo=<its userMsgID>. The channel then anchors
// each one to its user message via ReplyMessage and appends the
// event to that receipt's rolling log. Orphan events (no bound
// receipt, e.g. session already torn down) fall back to a plain
// text message with ReplyTo="" — the channel sends it without
// any anchoring, which is the right behavior for genuine
// unsolicited output.
//
// OutboundMessage.Meta is enriched with session-level metadata
// before the channel sees it — agent name, workspace, and (when
// available) the upstream provider. Channels like Feishu use
// these to render the receipt footer (Agent / cwd / Provider /
// tokens). The enrichment is a no-op for events whose Meta the
// translator doesn't initialize (Text / ToolStart / etc.) — we
// only touch OutInit where the enrichment is meaningful.
func (g *gateway) translateAndSend(s *session.Session, ev agent.AgentEvent) {
	chatID := g.lookupChatBySession(s.ID)
	if chatID == "" {
		// Orphan event — the session has no binding. Drop silently
		// (the runtime probably just shut down and a stray event
		// arrived).
		return
	}
	ch := g.resolveChannel(chatID)
	if ch == nil {
		return
	}

	out, send := Translate(chatID, ev)
	if send {
		enrichOutboundMeta(out, s)
		targets := g.receiptsForSession(s.ID)
		if len(targets) == 0 {
			// No bound receipt (orphan event). Send as plain
			// text — no anchor, no rolling-log card.
			out.ReplyTo = ""
			if err := ch.Send(context.Background(), out); err != nil {
				log.Printf("gateway: send failed (chat=%s, kind=%s, no anchor): %v", chatID, out.Kind, err)
			}
		} else {
			for _, userMsgID := range targets {
				// Each bound receipt gets its own OutboundMessage
				// anchored to its userMsgID. Same body, N
				// deliveries — each receipt card independently
				// edits in place.
				fanout := out
				fanout.ReplyTo = userMsgID
				if err := ch.Send(context.Background(), fanout); err != nil {
					log.Printf("gateway: send failed (chat=%s, kind=%s, anchor=%s): %v", chatID, out.Kind, userMsgID, err)
				}
			}
		}
	}

	if ev.Kind == agent.EventDone || ev.Kind == agent.EventError {
		target := receipt.ReceiptDone
		if ev.Kind == agent.EventError {
			target = receipt.ReceiptError
		}
		g.mu.RLock()
		var toDispose []string
		for umid, entry := range g.receipts {
			if entry.sessionID == s.ID {
				toDispose = append(toDispose, umid)
			}
		}
		g.mu.RUnlock()
		for _, umid := range toDispose {
			_ = g.UpdateReceipt(context.Background(), umid, target)
			_ = g.DisposeReceipt(context.Background(), umid)
		}
	}
}

// receiptsForSession returns the userMsgIDs of every receipt
// currently bound to the given session. The result is the fan-out
// list for translateAndSend: an agent event for this session
// becomes one OutboundMessage per entry, each anchored to its
// own userMsgID. Linear scan over the receipts map; the typical
// case is 1 (no buffer) or up to a handful (buffered batch).
//
// Callers MUST treat the returned slice as a snapshot — receipt
// mutations can invalidate it concurrently. The translateAndSend
// loop copies each userMsgID into a per-iteration OutboundMessage
// before sending, so concurrent Create/Dispose during the fan-out
// is safe (a stale userMsgID just sends to a receipt that no
// longer exists; the channel handles that as a plain text).
func (g *gateway) receiptsForSession(sid string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var ids []string
	for umid, entry := range g.receipts {
		if entry.sessionID == sid {
			ids = append(ids, umid)
		}
	}
	return ids
}

// lookupChatBySession finds the chatID whose binding points at
// sessionID. Linear scan — bindings are O(chats) which is fine at
// v0.3 scale (1 DM + maybe a few groups).
func (g *gateway) lookupChatBySession(sid string) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for chatID, b := range g.bindings {
		if b.SessionID == sid {
			return chatID
		}
	}
	return ""
}

// enrichOutboundMeta layers session-level metadata onto the
// OutboundMessage's Meta map. Currently the only consumer is
// OutInit (channels read agent_name / workspace / provider to
// render the receipt footer); other kinds pass through
// untouched. Extending to other kinds is a single switch case.
//
// Keys written:
//   - agent_name : session.Agent (registry name, e.g. "claude")
//   - workspace  : session.Workspace (cwd, may be absolute or "~"-relative)
//   - provider   : session's upstream LLM provider when the
//     runtime injects it; empty for now (the
//     OpenClaw wrapper is the future injection point)
//
// Writes are no-ops when the session field is empty so the
// receipt's footer composer skips those segments.
func enrichOutboundMeta(out OutboundMessage, s *session.Session) {
	if out.Meta == nil || s == nil {
		return
	}
	switch out.Kind {
	case OutInit:
		if _, ok := out.Meta["agent_name"]; !ok && s.Agent != "" {
			out.Meta["agent_name"] = s.Agent
		}
		if _, ok := out.Meta["workspace"]; !ok && s.Workspace != "" {
			out.Meta["workspace"] = s.Workspace
		}
	}
}
