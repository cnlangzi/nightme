// Package gateway routes incoming chat messages to slash commands
// registered by nightme, or to a fallback handler (typically the
// session manager forwarding text to the live agent). See
// docs/feat/F-20-gateway.md for the original router design and
// docs/feat/F-26-gateway-hub.md for the Stage-3 responsibility-
// isolation spec.
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
	Handle(ctx context.Context, msg *InboundMessage) (*CommandResult, error)
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
	fb    FallbackHandler

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

// New constructs a Gateway. The optional fallback handler is invoked
// when no command matches the inbound message. mgr is the session
// factory Gateway uses to look up sessions by ID and to spawn
// agents; passing nil disables those operations (cmd / run / kill
// won't work, but a debug-only Gateway stays operational).
func New(fallback FallbackHandler, mgr session.Manager) Gateway {
	return &gateway{
		cmds:       make(map[string]Command),
		fb:         fallback,
		attached:   make(map[string]struct{}),
		chatToChan: make(map[string]Channel),
		bindings:   make(map[string]*BindingEntry),
		receipts:   make(map[string]*receiptEntry),
		mgr:        mgr,
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

// translateAndSend does the actual work: look up the chat for the
// session, translate the event, send to the channel, and (for
// terminal events) flip receipts. This is the body of
// OnSessionEvent when no runtime-installed callback is present
// (the default).
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

	if out, send := Translate(chatID, ev); send {
		if err := ch.Send(context.Background(), out); err != nil {
			log.Printf("gateway: send failed (chat=%s, kind=%s): %v", chatID, out.Kind, err)
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
