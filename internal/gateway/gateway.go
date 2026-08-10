// Package gateway routes incoming chat messages. It is the
// inboundDispatcher for every InboundMessage produced by a
// Channel: each message flows in via pumpInbound → dispatchLoop →
// DispatchInbound, which walks a priority chain of tryDispatch*
// methods until one claims the input.
//
// Each tryDispatch* owns its own pattern matching: the chain
// does NOT inspect msg.Reaction / msg.Action / msg.Text at the
// top level. Adding a new dispatch mode = adding one tryDispatch*
// method + one entry in the chain — nothing else changes
// (DispatchInbound itself is a fixed-shape loop).
//
// Current chain (priority order, first match wins):
//
//	1. tryActionDispatch   — msg.Reaction / msg.Action events
//	2. tryCommandDispatch  — /-prefixed text via commander shim
//	3. tryMessageDispatch  — universal fallback (forwards to the
//	                         runtime-injected MessageDispatcher)
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
// F-watch §3.1.1 (post-refactor): the per-chat WatchMode gate
// no longer lives in gateway. It moved to chatsession — see
// Manager.AcceptInbound, called by the runtime messageDispatcher
// closure before any cs.GetOrCreate / cs.QueueUserMessage work.
// Gateway is now purely transport + routing; the chat policy sits
// next to the chat state.
//
// Imports: gateway does NOT import session / chatsession (the
// cycle constraint: channel and chatsession both depend on
// gateway, so the direction must be gateway → them, not the
// reverse). channel imports gateway for the abstract message
// types (InboundMessage / OutboundMessage).
package gateway

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
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
	// SendCard (F-46) is the specialised variant for interactive
	// decision cards. It returns the bot-side message id assigned
	// by the channel so callers can correlate the rendered card
	// with later card.action.trigger callbacks. The inner
	// channel.Channel interface in internal/channel/channel.go
	// carries the same method — this duplicate keeps the gateway
	// package free of the channel-package's circular deps.
	SendCard(ctx context.Context, msg OutboundMessage) (msgID string, err error)
}

// CommandResult is the per-dispatch outcome.
type CommandResult struct {
	Reply    string
	Consumed bool
	// Dropped indicates the gateway intentionally did not forward
	// the inbound to the messageDispatcher. Currently used by
	// dispatchAction (no action handler wired, or handler
	// declined the event). Distinct from Consumed=true (a slash
	// command ran) so log lines can distinguish "the slash
	// command replied" vs "the message was silently dropped".
	//
	// The former WatchMode drop is no longer a gateway concern —
	// chatsession.Manager.AcceptInbound drops those inside the
	// runtime messageDispatcher closure, before DispatchInbound
	// would have seen Dropped semantics anyway.
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
	// DispatchInbound is the inboundDispatcher entry point: every
	// InboundMessage produced by an attached Channel flows through
	// here. It routes to:
	//   - dispatchAction (when msg.Reaction or msg.Action is set)
	//   - the F-51 commander shim installed via WithCommander
	//     (when text starts with "/")
	//   - dispatchMessage (default branch), which forwards to the
	//     MessageDispatcher injected at construction time.
	//
	// F-watch §3.1.1: the per-chat WatchMode gate lives in the
	// runtime messageDispatcher closure (it calls
	// chatsession.Manager.AcceptInbound), not here. Gateway stays
	// transport-only; the policy sits next to its state.
	DispatchInbound(ctx context.Context, msg *InboundMessage) (*CommandResult, error)

	// v1.2 binding table (commit 3); chatType removed in F-33 (D1):
	Bind(chatID, sessionID, workspace, agent string) *BindingEntry
	LookupByChat(chatID string) *BindingEntry
	ListBindings() []BindingEntry

	// WithActionHandler installs the reaction/action
	// router (F-25 + F-45). DispatchInbound calls the handler
	// when msg.Reaction or msg.Action is set; the runtime
	// implements the cross-package lookup (typically
	// mgr.Get(msg.ChatID).HandleAction(ctx, ev)) and reports
	// whether the action was consumed.
	//
	// The fluent return matches WithCommander so the runtime can
	// chain gateway construction in a single line.
	WithActionHandler(handler func(ctx context.Context, msg *InboundMessage) (consumed bool)) Gateway

	// WithCommander installs the F-51 slash command dispatcher.
	// When set, DispatchInbound routes messages whose Text
	// starts with "/" through dispatch(ctx, msg) BEFORE the
	// legacy handleXxx dispatch table. The runtime wires this
	// to a shim that translates *InboundMessage to
	// command.SlashInput, calls command.Commander.Dispatch,
	// and converts the result back to *CommandResult.
	//
	// nil (default) keeps the legacy handleXxx dispatch path.
	WithCommander(dispatch func(ctx context.Context, msg *InboundMessage) (*CommandResult, error)) Gateway

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

// ResolveChannel returns the IM Channel that serves chatID.
// ResolveChannel returns the IM Channel that serves chatID.
// Exposed (vs the package-private resolveChannel) so the runtime
// shim in cmd/nightme can look up the channel for a chatID
// (used in the slash-command shim to bind cs.Channel() via
// WithChannel after GetOrCreate). Falls back to the default
// channel if chatID is not in the chatID→Channel map. Returns
// nil if no default.
func (r *Router) ResolveChannel(chatID string) Channel {
	return r.resolveChannel(chatID)
}

// WithActionHandler installs the reaction/action
// router. DispatchInbound calls this when msg.Reaction or
// msg.Action is set; the runtime implements the cross-package
// lookup (typically mgr.Get(msg.ChatID).HandleAction(ctx, ev))
// and reports whether the action was consumed.
//
// A nil handler means the runtime hasn't wired reaction routing
// yet; DispatchInbound falls through to dispatchMessage in
// that case (existing pre-F-45 behaviour). The runtime MUST wire
// this for reaction-driven decision cards to reach the gtw
// executor; otherwise reactions get sent to the agent loop as
// empty-text messages and silently no-op.
func (g *gateway) WithActionHandler(handler func(ctx context.Context, msg *InboundMessage) (consumed bool)) Gateway {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actionHandler = handler
	return g
}

// WithCommander installs the F-51 slash command dispatcher.
// See the Gateway interface for full docs.
func (g *gateway) WithCommander(dispatch func(ctx context.Context, msg *InboundMessage) (*CommandResult, error)) Gateway {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commandDispatch = dispatch
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
	mu sync.RWMutex
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

	// F-50 §6.1: per-chat action router. When DispatchInbound
	// sees msg.Reaction or msg.Action, it calls this with the
	// event; the runtime implements the cross-package lookup
	// (mgr.Get(chatID).HandleAction(...)) and returns whether
	// the action was consumed. Nil = no action handler; reactions
	// fall through to dispatchMessage (the agent loop, which
	// would just no-op on the empty text).
	actionHandler func(ctx context.Context, msg *InboundMessage) (consumed bool)

	// F-51: slash command dispatcher. When set,
	// DispatchInbound routes messages whose Text starts with
	// "/" through this before the legacy handleXxx dispatch
	// table. The runtime (cmd/nightme/run.go) wires this to a
	// shim that translates *InboundMessage to command.SlashInput,
	// calls command.Commander.Dispatch, and converts the result
	// back to *CommandResult. nil = legacy handleXxx dispatch.
	commandDispatch func(ctx context.Context, msg *InboundMessage) (*CommandResult, error)
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

// DispatchInbound is the inboundDispatcher entry point (implements
// the Gateway interface). Every InboundMessage from an attached
// Channel flows through here.
//
// DispatchInbound walks a fixed priority chain of tryDispatch*
// methods; the first one that claims the input (returns
// handled=true) wins. The chain and DispatchInbound itself do
// NOT inspect msg.Reaction / msg.Action / msg.Text at the top
// level — each tryDispatch* owns its own pattern matching. To
// add a new dispatch mode, add a tryDispatch* method + one
// entry in the chain.
//
// Either branch may produce a CommandResult; the caller
// (dispatchLoop) only checks result == nil and otherwise
// discards the value (it does not interpret Reply — that is
// already sent by the dispatcher that produced the result).
//
// F-watch §3.1.1 note: the per-chat WatchMode gate no longer
// lives in gateway — it moved to chatsession.Manager.AcceptInbound
// (called from the runtime messageDispatcher closure). The
// legacy ParseCommand + g.cmds table is gone — all slash
// commands live in command.Registry now (fa8c6d1 / 69d69e6).
func (g *gateway) DispatchInbound(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}

	for _, try := range []func(context.Context, *InboundMessage) (bool, *CommandResult, error){
		g.tryActionDispatch,   // priority 1: reaction / action events
		g.tryCommandDispatch,  // priority 2: /-prefixed text via commander shim
		g.tryMessageDispatch,  // priority 3: universal fallback (WatchMode + agent loop)
	} {
		handled, result, err := try(ctx, msg)
		if err != nil {
			return nil, err
		}
		if handled {
			return result, nil
		}
	}
	// Unreachable: tryMessageDispatch always returns handled=true.
	// Returning nil here preserves the pre-refactor "no handler
	// → drop silently" behaviour for any future config that might
	// remove tryMessageDispatch from the chain.
	return nil, nil
}

// tryActionDispatch claims Reaction / Action events. The pattern
// (msg.Reaction != nil || msg.Action != nil) lives HERE so the
// chain itself never inspects the message shape.
//
// Priority 1 because action events carry empty Text — letting
// them fall through to the commander would either consume them
// as plain text or as an empty slash command, and the gtw draft
// pipeline would never see its reaction.
//
// For non-action messages returns (false, nil, nil) so the
// chain continues to the next tryDispatch.
func (g *gateway) tryActionDispatch(ctx context.Context, msg *InboundMessage) (bool, *CommandResult, error) {
	if msg.Reaction == nil && msg.Action == nil {
		return false, nil, nil
	}
	result, err := g.dispatchAction(ctx, msg)
	return true, result, err
}

// tryCommandDispatch claims /-prefixed text routed through the
// F-51 commander shim. The pattern (text starts with "/") and
// the shim's fall-through semantics (Consumed=true / Dropped=true
// ⇒ handled; otherwise ⇒ chain continues) live HERE.
//
// Priority 2: runs after tryActionDispatch so reaction events
// never reach the commander, and before tryMessageDispatch so
// recognised slash commands short-circuit the per-chat
// WatchMode gate (otherwise applied by chatsession inside
// the runtime dispatcher). The gate is a no-op for slash
// commands — recognised commands return here, unrecognised
// /-inputs fall through to dispatchMessage with the original
// text intact.
//
// Returns (false, nil, nil) in three cases, all of which the
// chain treats identically as "not my input":
//
//   - text does not start with "/"
//   - commander shim is not installed (no WithCommander call)
//   - shim ran but produced a non-claiming result
//     (result == nil || !result.Consumed && !result.Dropped)
//
// The third case covers the F-51 fall-through contract: unknown
// /-inputs and slash-command attempts with no matching factory
// must reach dispatchMessage with the original text intact so
// they fall through to the agent loop.
func (g *gateway) tryCommandDispatch(ctx context.Context, msg *InboundMessage) (bool, *CommandResult, error) {
	if !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return false, nil, nil
	}
	g.mu.RLock()
	dispatch := g.commandDispatch
	g.mu.RUnlock()
	if dispatch == nil {
		return false, nil, nil
	}
	result, err := dispatch(ctx, msg)
	if err != nil {
		return false, nil, err
	}
	if result == nil || (!result.Consumed && !result.Dropped) {
		return false, nil, nil
	}
	return true, result, nil
}

// tryMessageDispatch is the universal fallback. Always claims
// (handled=true). The MessageDispatcher injection and the
// no-dispatcher drop behaviour live inside dispatchMessage —
// this wrapper just guarantees the chain always terminates with
// a non-nil result.
//
// Sits at the bottom of the priority chain; new dispatchers
// that should run before the agent loop add themselves ABOVE
// this entry.
func (g *gateway) tryMessageDispatch(ctx context.Context, msg *InboundMessage) (bool, *CommandResult, error) {
	result, err := g.dispatchMessage(ctx, msg)
	return true, result, err
}

// dispatchAction is the F-50 §6.1 + F-25 user-action branch.
// It dispatches msg.Action (card button click) and msg.Reaction
// (emoji reaction) to the per-chat action handler installed by
// the runtime. The handler is responsible for cross-package
// concerns (looking up the ChatSession via mgr, calling
// ChatSession.HandleAction, etc.); the gateway itself stays
// transport-only.
//
// Returns a consumed result with Dropped=true when no handler
// is installed — that's the v1 pre-F-45 default and lets the
// runtime come up before the reaction branch is wired.
func (g *gateway) dispatchAction(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	slog.Default().Warn("F-46 debug: gateway.dispatchAction entry",
		"chat_id", msg.ChatID,
		"has_reaction", msg.Reaction != nil)
	g.mu.RLock()
	h := g.actionHandler
	g.mu.RUnlock()
	if h == nil {
		slog.Default().Warn("F-46 debug: gateway.dispatchAction: actionHandler is nil, dropping")
		// Pre-F-45 runtime: action events silently dropped.
		// Better than routing an empty-text event to the agent
		// loop, which would queue a no-op turn and confuse the
		// user with a "thinking…" state.
		return &CommandResult{Consumed: true, Dropped: true}, nil
	}
	slog.Default().Warn("F-46 debug: gateway.dispatchAction: invoking handler")
	consumed := h(ctx, msg)
	if consumed {
		slog.Default().Warn("F-46 debug: gateway.dispatchAction: handler returned true")
		return &CommandResult{Consumed: true}, nil
	}
	// Handler ran but decided not to consume (e.g. no matching
	// gtwDraft, or the reaction emoji wasn't a recognised one).
	// We still mark Consumed=true because DispatchInbound has
	// "owned" the event — re-routing it to dispatchMessage
	// would either send a confusing empty text to the agent
	// (F-45 path) or re-enter the WatchMode gate (F-watch path,
	// now applied inside chatsession.AcceptInbound).
	slog.Default().Warn("F-46 debug: gateway.dispatchAction: handler returned false, dropping")
	return &CommandResult{Consumed: true, Dropped: true}, nil
}

// dispatchMessage hands the message to the runtime-injected
// MessageDispatcher (which queues it into the per-chat input
// buffer / spawns a new agent turn).
//
// F-watch §3.1.1 note: the per-chat WatchMode gate formerly
// lived here and in applyWatchModeGate. It moved to
// chatsession.Manager.AcceptInbound so the policy sits next
// to its state (chatsession now owns both the gate and the
// WatchMode field it consults). The runtime-injected
// MessageDispatcher, constructed in cmd/nightme/run.go, is the
// sole caller of AcceptInbound — every non-action inbound still
// reaches exactly one WatchMode check, just on the other side
// of the gateway/runtime boundary.
//
// A nil MessageDispatcher silently drops the message (returns
// Consumed=false rather than an error — the dispatcher already
// "decided" the message isn't actionable).
func (g *gateway) dispatchMessage(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	if g.messageDispatcher == nil {
		return &CommandResult{Consumed: false}, nil
	}
	if err := g.messageDispatcher(ctx, msg); err != nil {
		return &CommandResult{}, err
	}
	return &CommandResult{}, nil
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

// OnMessageState was the v1.3 (F-31) ChatSession-callback entry
// point. REMOVED (F-54 review): production no longer wires this
// — the runtime's MessageStateBus subscriber in cmd/nightme/run.go
// builds the F-48-stamped OutboundMessage directly and routes it
// via the runtime's channel handle. The un-stamped translation
// helper now lives in message_state_helpers_test.go (test-only)
// for tests that target the translation logic itself.

// (impl removed — see message_state_helpers_test.go for the test helper)
