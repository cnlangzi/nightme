// Package chatsession — ChatSession (v1.2 per-chat session context).
//
// ChatSession owns the persistent per-chat state and the pool of
// AgentSessions. It replaces v1.1's Session + Gateway BindingEntry
// pair with a single coherent structure.
//
// See docs/SPEC.md v1.2 §1.1 and docs/feat/F-27-chatsession.md.
package chatsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/registry"
)

// ChatSession is the persistent per-chat session context.
//
// One ChatSession is bound 1:1 to an IM chat (chatID), enforced by
// the UNIQUE constraint on registry.ChatSessionEntry.ChatID. Each
// ChatSession owns a pool of AgentSessions keyed by (agent, cwd);
// the active one (or nil) is tracked in selectedAS.
//
// Concurrency: state fields (selectedCwd, selectedAgent, primaryAgent,
// pool, selectedAS) are guarded by mu. Reads take RLock; writes take
// Lock. /use / /cwd take RLock for the mutation + Lock for the
// pool mutation when an AgentSession is added.
type ChatSession struct {
	ID     string
	ChatID string

	mu sync.RWMutex

	// Active routing state. selectedAgent is mutable via /use;
	// selectedCwd via /cwd. primaryAgent is the cfg.Primary snapshot
	// at ChatSession construction; read-only post-construction
	// (Q-A: no /default command, no per-chat override).
	//
	// At New() time selectedAgent is seeded from primaryAgent so the
	// runtime never sees an empty selectedAgent on a fresh chat.
	selectedCwd    string
	selectedAgent  string
	primaryAgent string

	// F-watch §3.1.1: per-chat message-watch mode. Default
	// WatchModeMention (only @ bot or @_all messages are
	// processed); /watch on switches to WatchModeAll. See
	// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.10/§6.11.
	// DM chats always behave as if WatchMode==WatchModeAll
	// (every DM message is "addressed to bot"); the gate logic in
	// gateway.Handle enforces that — this field is only consulted
	// for group chats via the channel-supplied HasMention bool.
	watchMode WatchMode

	// F-think §3.1.2: per-chat thinking-content visibility. Default
	// ThinkModeShow (runtime forwards OutThinking to the Channel,
	// which renders it as a lark_md card in the user-message
	// thread); /think off switches to ThinkModeHide (runtime
	// drops OutThinking at the EventHandler gate). Unlike
	// WatchMode this is chat-type-independent — DMs and group
	// chats behave identically. See docs/SPEC.md §3.1.2.
	thinkMode ThinkMode

	// F-38 §3.1.3: per-chat tool-event visibility. Default
	// ToolsModeHide (runtime drops OutToolStart and OutToolEnd at
	// the EventHandler gate — tool spam is the loudest part of
	// the agent progress stream and most users do not want it by
	// default); /tools on switches to ToolsModeShow (Feishu
	// adapter merges each pair into a single thread reply via
	// PATCH on the same message_id). Like ThinkMode and unlike
	// WatchMode, this is chat-type-independent — DMs and group
	// chats behave identically. See docs/SPEC.md §3.1.3. The
	// default direction is OPPOSITE of ThinkMode's: ThinkMode's
	// zero value is Show (preserve existing F-thread-route UX);
	// ToolsMode's zero value is Hide (quiet by default; opt in).
	toolsMode agent.ToolsMode

	// Pool of AgentSessions keyed by (agent, cwd).
	pool map[agentCwdKey]*AgentSession

	// Currently active AgentSession (pointer into pool). nil means
	// no active session (ChatSession exists but no /cwd + /use yet).
	selectedAS *AgentSession

	// Timestamps.
	createdAt        time.Time
	lastInteractionAt time.Time

	// Persistence handles (optional — nil means no persistence).
	csFile *registry.ChatSessionFile
	asFile *registry.AgentSessionFile

	// spawner is used by LookupSelectedAgentSession to fork new
	// children on miss. nil means no spawn (test-friendly default;
	// production wires a registrySpawner at runtime).
	spawner Spawner

	// queue (CS-AS 边界重构 Phase 1) is the at-least-once
	// successful submission queue. Head element is currently
	// being attempted; failed Submit keeps the head in place.
	// Locked by MessageQueue's own mutex; callers may use it
	// without holding cs.mu.
	queue *MessageQueue

	// commit 8c: per-ChatSession readPump controller. Only one
	// CS-AS 边界重构 Phase 1: readpump is now per-AS (started
	// by Spawn inside AgentSession). The chat-layer no longer
	// owns a pump goroutine — it just consumes the enriched
	// event stream via cs.PumpEvents (launched by the runtime
	// for each ChatSession).

	// CS-AS 边界重构 Phase 1: exitObserver is now removed.
	// Lifecycle events surface via KindLifecycle in the
	// EnrichedEvent stream; the runtime registers its handler
	// by reading the stream (not via a per-CS callback).

	// Event buses (F-54). Each ChatSession owns one *services.EventBus[X]
	// per event kind. The runtime wires subscribers by calling
	// .Subscribe on these public fields directly (no getter).
	// Multiple subscribers may register; first to return true consumes
	// the event. nil-safe: Publish on an un-wired bus is a no-op. The
	// buses are not closed when the ChatSession ends — their
	// lifetime matches the owning ChatSession, which is itself GC'd
	// when no references remain.
	//
	// Concurrency: Subscribe is called once at startup (no lock).
	// Publish is called from the PumpEvents goroutine without
	// holding cs.mu (writebackMessageState fix). The EventBus's
	// own mutex protects the Publish/Subscribe race.
	//
	// AS-level lifecycle transitions (Spawned / Exited) are not
	// fanned out via a bus — the routeEvent KindLifecycle handler
	// directly flips AgentSession.SetExited and logs at Info level.
	// A LifecycleBus was prototyped in F-54 but no consumer ever
	// materialised; re-add it here (plus a LifecycleEvent envelope)
	// the day an audit / metrics / HUD subscriber actually needs it.
	AgentEventBus   *services.EventBus[AgentEventEnvelope]
	MessageStateBus *services.EventBus[MessageStateEvent]
	PromptEndBus    *services.EventBus[PromptEndedEvent]

	// subs tracks AgentSession.IDs for which an EventBus
	// subscription has been installed by attachAgentSubscription.
	// Keyed by AS.ID; values are placeholders. Guarded by mu.
	subs map[string]struct{}

	// ctx is the per-ChatSession context. Lives for the chat's
	// lifetime (until daemon shutdown). It is the PARENT context
	// every AgentSession active on this chat derives its own
	// per-AS ctx from. selectAgentSessionLocked owns that handover: on
	// every change of active AS it Background()s the outgoing one
	// (cancelling its derived ctx) and Activate(cs.ctx)s the
	// incoming one. Cancelling cs.ctx itself cascades through every
	// active AS (used by the runtime during graceful shutdown).
	//
	// Per-AS lifecycle control lives on AgentSession, not on
	// ChatSession — the chat holds the parent, the AS owns the
	// derived child. ChatSession does not need its own AS-level
	// or turn-level ctx fields.
	//
	// Guarded by ctxMu; reads via Context(), writes via
	// ResetContext().
	ctx    context.Context
	cancel context.CancelFunc
	ctxMu  sync.Mutex

	// channel is the IM Channel bound to this chat session.
	// Set once at New() time (production) or by WithChannel
	// (test path / late binding). Immutable post-binding; no
	// lock needed. nil means "no channel bound yet" — commands
	// must nil-check via Channel() before calling Send/SendCard.
	channel Channel

	// --- F-45 teamflow (gtw) state ------------------------------
	//
	// REMOVED in F-51: gtwContext, gtwDrafts, and the
	// onReaction reaction handler all moved to
	// gtw.Manager (see internal/command/gtw/manager.go).
	// chatsession is no longer aware of gtw — it has no
	// GTWContext, no GTWDraft, no ReactionEvent type, no
	// SetActionHandler / HandleAction methods.
}

// New creates a fresh ChatSession in memory. The caller is
// responsible for persisting via Persist().
//
// primaryAgent is the cfg.Primary snapshot at creation time. It
// seeds selectedAgent so the runtime always has an effective agent
// to dispatch to (no runtime fallback: the lookup only ever reads
// selectedAgent). The snapshot itself is read-only post-construction
// (Q-A: no /default command, no per-chat override).
//
// ch is the IM channel bound to this chat; nil is permitted (test
// scenarios) but production wiring must always pass a real Channel.
// When ch is nil the constructor logs a warning and proceeds
// with cs.channel = nil (no panic; daemon stability principle).
// Callers can detect this via cs.Channel() == nil and skip
// channel-dependent operations. Returns (*cs, nil) regardless
// of ch — construction never fails on the channel binding.
func New(chatID, primaryAgent string, ch Channel) (*ChatSession, error) {
	if ch == nil {
		slog.Default().Warn("chatsession.New: channel is nil (chatID=" + chatID + ")")
	}
	cs := &ChatSession{
		ID:               deriveIDFromChatID(chatID),
		ChatID:           chatID,
		selectedAgent:      primaryAgent, // init seed
		primaryAgent:     primaryAgent, // historical snapshot, read-only
		pool:             make(map[agentCwdKey]*AgentSession),
		watchMode:        WatchModeMention, // F-watch default
		thinkMode:        ThinkModeShow,    // F-think default
		toolsMode:        agent.ToolsModeHide, // F-38 default (quiet by default)
		channel:          ch,
		createdAt:        time.Now(),
		lastInteractionAt: time.Now(),
	}
	cs.ctx, cs.cancel = context.WithCancel(context.Background())
	// F-54: one Bus per event kind. Constructed eagerly so the
	// runtime can Subscribe even before the first Publish fires.
	cs.AgentEventBus = services.NewEventBus[AgentEventEnvelope]()
	cs.MessageStateBus = services.NewEventBus[MessageStateEvent]()
	cs.PromptEndBus = services.NewEventBus[PromptEndedEvent]()
	cs.subs = make(map[string]struct{})
	cs.queue = NewMessageQueue(QueueMaxMsgs)
	return cs, nil
}

// WithPersistence attaches registry stores. Both can be nil (no
// persistence); typically both are non-nil in production.
func (cs *ChatSession) WithPersistence(csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile) *ChatSession {
	cs.mu.Lock()
	cs.csFile = csFile
	cs.asFile = asFile
	cs.mu.Unlock()
	return cs
}

// WithSpawner attaches a Spawner used by LookupSelectedAgentSession
// to fork child processes. nil means no spawn (lookup returns
// AgentSession with status=Detached, no process running).
func (cs *ChatSession) WithSpawner(spawner Spawner) *ChatSession {
	cs.mu.Lock()
	cs.spawner = spawner
	cs.mu.Unlock()
	return cs
}

// Spawner returns the configured Spawner (nil if none).
func (cs *ChatSession) Spawner() Spawner {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.spawner
}

// deriveIDFromChatID produces a deterministic ID from the chat ID
// for the 1:1 invariant. Real implementation should use a hash; for
// commit 6 we use a plain prefix to keep things readable.
func deriveIDFromChatID(chatID string) string {
	if chatID == "" {
		return ""
	}
	return "cs_" + chatID
}

// ErrNoSelectedCwd is returned by LookupSelectedAgentSession when the
// ChatSession has no selectedCwd set (user has not /cwd'd yet).
var ErrNoSelectedCwd = errors.New("chatsession: no active workspace (send /cwd first)")

// ErrUnknownAgent indicates the requested agent is not registered.
// (Validation should happen at the gateway layer; this is a safety net.)
var ErrUnknownAgent = errors.New("chatsession: unknown agent")

// ResetResult is one row of the /new reply. It captures what happened
// to a single pool entry during NewActiveAgentSessions so the handler
// can render a per-agent status instead of a bare count.
//
// See docs/feat/F-43-kill-new-graceful-and-reset.md §5.2.
type ResetResult struct {
	Agent       string // e.g. "claude", "codex"
	Cwd         string // e.g. "/code/A"
	BeforeState Status // StatusRunning / StatusDetached / StatusExited
	Action      string // "in-place-reset" | "marked-fresh"
	Error       error  // nil on success

	// Session is the underlying AgentSession — populated so the
	// caller (handleNew in F-45) can perform per-row follow-up
	// actions such as PersistAgentSession without re-walking the
	// pool. nil only when targets were empty (matched == 0);
	// always set otherwise.
	Session *AgentSession
}

// SetSelectedCwd changes the active workspace. Does NOT spawn or kill
// any AgentSession; the pool is preserved. Next message triggers
// LookupSelectedAgentSession which may spawn or reuse.
func (cs *ChatSession) SetSelectedCwd(cwd string) error {
	if cwd == "" {
		return errors.New("chatsession: empty cwd")
	}
	cs.mu.Lock()
	cs.selectedCwd = cwd
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetWatchMode changes the per-chat message-watch mode. Persists to
// registry on success so it survives daemon restart. No spawn /
// kill / event side-effects — purely a routing-state mutation.
//
// F-watch §3.1.1: a no-op in DM chats at the gateway layer (DM
// messages always pass HasMention=true), but the mode value is
// still written to keep /watch semantics consistent across chat
// types — switching from group to DM and back preserves the
// user's last-set preference.
func (cs *ChatSession) SetWatchMode(mode WatchMode) error {
	cs.mu.Lock()
	cs.watchMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetThinkMode changes the per-chat thinking-content visibility.
// Persists to registry on success so /think off survives daemon
// restart. No spawn / kill / re-dispatch side effects.
//
// State semantics:
//   - ThinkModeShow — runtime forwards every OutThinking event to
//     the Channel (rendered as a lark_md card in the user-message
//     thread; see internal/channel/feishu/thinking_card.go).
//   - ThinkModeHide — runtime drops OutThinking events at the
//     EventHandler gate (after Translate + ReplyTo stamping,
//     before ch.Send). Other OutboundKinds are unaffected.
//
// Concurrency: same pattern as SetWatchMode — take ChatSession
// mutex, write, persist, release. The lock is NOT held across
// any channel.Send reply call.
func (cs *ChatSession) SetThinkMode(mode ThinkMode) error {
	cs.mu.Lock()
	cs.thinkMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetSelectedAgent changes the active agent name. Does NOT spawn or
// kill; caller must invoke LookupSelectedAgentSession to materialize.
func (cs *ChatSession) SetSelectedAgent(agent string) error {
	if agent == "" {
		return errors.New("chatsession: empty agent")
	}
	cs.mu.Lock()
	cs.selectedAgent = agent
	// F-53: no more `currentTurnUserMsgID` to clear — the anchor
	// now lives on `AgentSession.currentPrompt.LastMessageID`,
	// which is set/cleared by `defaultPromptHookLocked` /
	// `endPrompt`. Switching selectedAgent here just kicks the
	// hook closure (which captures `cs.selectedAS` at flush time)
	// to look up the new AS on the next flush.
	//
	// KNOWN LIMITATION (Phase 0, deferred to "Prompt 投递稳定性
	// 优化" PR — see docs/feat/message_lifecycle.md §8): if the old AS had an in-flight Prompt (currentPrompt
	// != nil), it stays installed on the old AS after the switch.
	// The new AS starts with currentPrompt=nil, so subsequent
	// events on the new AS use the new anchor. The old Prompt's
	// EndedAt remains zero until either the old AS process dies
	// (at which point Phase 1 will call endPrompt(ProcessDied))
	// or the old AS is /killed. endPrompt itself only operates
	// on cs.selectedAS, so it cannot clean the old one. This is
	// a deliberate Phase 0 simplification; the cost is one
	// orphaned Prompt struct per /use-while-busy until the old
	// AS exits.
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SelectedCwd returns the current active workspace.
func (cs *ChatSession) SelectedCwd() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.selectedCwd
}

// SelectedAgent returns the current active agent name.
func (cs *ChatSession) SelectedAgent() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.selectedAgent
}

// WatchMode returns the current per-chat message-watch mode.
// Default value when never set is WatchModeMention (set in New
// when the registry has no persisted value). See
// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.11.
func (cs *ChatSession) WatchMode() WatchMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.watchMode
}

// ThinkMode returns the current per-chat thinking-content
// visibility. Default value when never set is ThinkModeShow
// (set in New when the registry has no persisted value). See
// docs/SPEC.md §3.1.2.
func (cs *ChatSession) ThinkMode() ThinkMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.thinkMode
}

// SetToolsMode changes the per-chat tool-event visibility.
// Persists to registry on success so /tools on survives daemon
// restart. No spawn / kill / re-dispatch side effects.
//
// State semantics:
//   - ToolsModeShow — runtime forwards OutToolStart / OutToolEnd
//     to the Channel. Feishu adapter merges each pair into a
//     single thread reply via PATCH on the start message_id (see
//     internal/channel/feishu/tool_thread_merge.go).
//   - ToolsModeHide — runtime drops OutToolStart and OutToolEnd
//     at the EventHandler gate (after Translate + ReplyTo
//     stamping, before ch.Send). Other OutboundKinds are
//     unaffected.
//
// Concurrency: same pattern as SetWatchMode / SetThinkMode — take
// ChatSession mutex, write, persist, release. The lock is NOT
// held across any channel.Send reply call.
func (cs *ChatSession) SetToolsMode(mode agent.ToolsMode) error {
	cs.mu.Lock()
	cs.toolsMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// ToolsMode returns the current per-chat tool-event visibility.
// Default value when never set is ToolsModeHide (set in New when
// the registry has no persisted value). Direction is OPPOSITE of
// ThinkMode's default — see internal/agent/tools_mode.go doc
// for the rationale. See docs/SPEC.md §3.1.3.
func (cs *ChatSession) ToolsMode() agent.ToolsMode {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.toolsMode
}

// PrimaryAgent returns the per-chat primary agent (snapshot of
// cfg.Primary at ChatSession creation). v1.2 (Q-A) does not
// allow post-creation mutation; the field is read-only. The
// selectedAgent is seeded from this value at construction; /use
// overrides selectedAgent but does NOT mutate primaryAgent.
func (cs *ChatSession) PrimaryAgent() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.primaryAgent
}

// Pool returns a snapshot of all AgentSessions in the pool.
func (cs *ChatSession) Pool() []*AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]*AgentSession, 0, len(cs.pool))
	for _, as := range cs.pool {
		out = append(out, as)
	}
	return out
}

// SelectedAgentSession returns the current active AgentSession (or nil).
func (cs *ChatSession) SelectedAgentSession() *AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.selectedAS
}

// --- InputBuffer FSM (F-53) ---------------------------------------

// ensureBuffer lazily creates the InputBuffer on first use. Called
// from QueueUserMessage / SetBusy / SetIdle / OnTurnEnded so tests
// that don't dispatch messages don't allocate the FSM.
//
// Construction installs a default PromptHook that handles the
// entire submission transaction (see defaultPromptHookLocked
// emitMessageDropped publishes a MessageDropped wire event for
// the given message. Post-refactor the Message is immutable, so
// this is a pure side-effect: fire the bus event with the
// canonical transition state. The caller holds the Message
// (typically from a `queue.Clear()` slice); cs.mu is not
// required.
func (cs *ChatSession) emitMessageDropped(msg Message) {
	cs.mu.RLock()
	bus := cs.MessageStateBus
	chatID := cs.ChatID
	cs.mu.RUnlock()
	bus.Publish(MessageStateEvent{
		ChatID:    chatID,
		UserMsgID: msg.ID,
		State:     agent.MessageDropped,
		At:        time.Now(),
	})
}

// PromptEndBus (F-53 follow-up → F-54) is the runtime-installed callback
// fired by `endPrompt` after the Prompt's terminal fields were
// stamped. F-54 replaced it with cs.PromptEndBus.Publish(PromptEndedEvent{...});
// subscribers register via PromptEndBus().Subscribe. See
// docs/feat/F-54-event-bus.md for the migration rationale.

// Channel returns the IM channel bound to this chat session.
// Returns nil if no channel was bound at New() time (test path)
// or if WithChannel was never called.
//
// Lock-free: channel is set once and never mutated.
func (cs *ChatSession) Channel() Channel {
	if cs == nil {
		return nil
	}
	return cs.channel
}

// WithChannel binds a Channel to this ChatSession. Returns
// the receiver for chaining. Used by Manager.GetOrCreate via
// the channelResolver path; not normally called by commands.
//
// Idempotent: subsequent calls with the same Channel are no-ops;
// calls with a different Channel panic — a chat's channel binding
// is immutable for the daemon's lifetime.
func (cs *ChatSession) WithChannel(ch Channel) *ChatSession {
	if cs == nil {
		return cs
	}
	if ch == nil {
		return cs
	}
	if cs.channel != nil && cs.channel != ch {
		panic(fmt.Sprintf("chatsession: ChatSession %s already bound to a different Channel", cs.ChatID))
	}
	cs.channel = ch
	return cs
}

// Context returns the per-ChatSession ctx. Every AgentSession
// active on this chat derives its own per-AS ctx from
// Context() via AgentSession.Activate(parent). The runtime reads
// Context() when wiring up a freshly-activated AS; consumers
// (InputBuffer FlushHook, runtime EventHandler) don't reach for
// it — they go through AgentSession.OpContext() instead.
//
// To tear down every operation on this chat at once (graceful
// shutdown path), call ResetContext(). All active ASes'
// derived ctxs cascade.
//
// Safe to call concurrently; the underlying context is replaced
// atomically under ctxMu.
func (cs *ChatSession) Context() context.Context {
	cs.ctxMu.Lock()
	defer cs.ctxMu.Unlock()
	return cs.ctx
}

// ResetContext cancels the per-ChatSession ctx and installs a
// fresh one derived from context.Background(). Reserved for the
// runtime's graceful-shutdown path — an active-AS switch does NOT
// use this; per-AS teardown belongs to selectAgentSessionLocked.
//
// After ResetContext, every AgentSession whose opCtx was derived
// from the previous cs.ctx has its operations cancelled
// (cascade). The fresh cs.ctx is what the next promotion's
// Activate() call will derive from.
//
// Always installs a fresh ctx; idempotent in spirit.
func (cs *ChatSession) ResetContext() {
	cs.ctxMu.Lock()
	defer cs.ctxMu.Unlock()
	if cs.cancel != nil {
		cs.cancel()
	}
	cs.ctx, cs.cancel = context.WithCancel(context.Background())
}

// QueueUserMessage (CS-AS 边界重构 Phase 1) accepts an already-built
// `*Message` from the runtime dispatcher, stores it in
// `cs.messagesByID`, appends to the at-least-once queue, and
// fires a TryFlush to immediately dispatch if the active AS is
// ready.
//
// The dispatcher (cmd/nightme/run.go newMessageDispatcher) is
// responsible for emitting `MessageQueued` BEFORE calling this —
// see the `OnInbound` wiring comment in run.go for the timing.
//
// TryFlush covers the "queue was empty when this message arrived"
// case: AS is ready, so the prompt is built and submitted right
// away. If queue is non-empty, TryFlush sees !IsReady() and is a
// no-op; the next KindPromptEnded event will trigger a retry.
//
// Message is taken by value; ownership passes to the queue on
// successful Push. The caller may not retain or mutate the
// passed Message after this returns nil.
func (cs *ChatSession) QueueUserMessage(msg Message) error {
	if msg.ID == "" {
		return nil
	}
	if err := cs.queue.Push(msg); err != nil {
		return err
	}
	_ = cs.TryFlush()
	return nil
}

// buildPromptLocked (CS-AS 边界重构 Phase 1) constructs a candidate
// Prompt from the current queue. Caller MUST hold cs.mu.
//
// The Prompt's CS-side fields are populated here (ChatSessionID,
// Messages, MessageIDs, Blocks, CreatedAt). AS-side fields (ID,
// AckedAt, LastProgressAt, AgentSessionID) are filled by
// AgentSession.Submit after SendBlocks succeeds.
//
// `Messages` and `MessageIDs` are populated from the same batch
// — they stay in lock step. AS.Submit only reads `Blocks`, but
// the Prompt retains `Messages` for writeback to mutate
// per-message bookkeeping (LastProcessedAt, LastEndReason,
// Stage) on KindPromptEnded.
func (cs *ChatSession) buildPromptLocked(batch []Message) *Prompt {
	messages := make([]Message, 0, len(batch))
	var blocks []agent.ContentBlock
	for _, m := range batch {
		messages = append(messages, m)
		blocks = append(blocks, m.Blocks...)
	}
	p := &Prompt{
		ChatSessionID: cs.ID,
		Messages:      messages,
		Blocks:        blocks,
		CreatedAt:     time.Now(),
	}
	// LastMessageID is the anchor for the EventHandler — the
	// readpump reads it from `as.currentPrompt.LastMessageID` at
	// enrichment time and stamps it as UserMsgID on every
	// EnrichedEvent. The feishu adapter uses it to route
	// OutReply/OutResult to the right per-userMsgID receipt card
	// (ensureReceiptForReplyWithFooter / ensureReceiptForTask).
	//
	// Without this, every AgentEvent reaches the channel with
	// ReplyTo="", which makes the placeholder card creation
	// (F-46 lazy-receipt path) silently fail because the receipt
	// helper requires userMsgID != "". Multi-message turns
	// (merged queue) anchor on the LAST message id, matching
	// the pre-Phase-1 behavior.
	if n := len(messages); n > 0 {
		p.LastMessageID = messages[n-1].ID
	}
	return p
}

// TryFlush (CS-AS 边界重构 Phase 1) attempts to submit the queue
// to the active AgentSession. Driver of the "at-least-once
// successful submission" semantic.
//
// Behavior:
//   - Peek the next batch from the queue (Peek auto-segments at
//     MessageKindQueue barriers).
//   - If batch is empty or no active AS, return nil (and Rewind
//     the in-flight batch so it can be retried later).
//   - If active AS is not ready (currentPrompt in flight), return
//     nil (and Rewind).
//   - Build a candidate Prompt from the batch.
//   - Call as.Submit(p) outside cs.mu (SendBlocks can block).
//   - On success: Commit the in-flight batch, flip Stage =
//     Submitted, emit MessageSubmitted wire events.
//   - On error: Rewind so the batch returns to pending for the
//     next TryFlush (driven by KindPromptEnded → IsReady flip).
//
// Concurrency: cs.mu is held only briefly to read selectedAS. The
// queue has its own mutex, and the batch slice returned by Peek
// is a stable snapshot — the caller may iterate it freely while
// the queue is being mutated concurrently.
func (cs *ChatSession) TryFlush() error {
	// Peek the next batch. This advances the queue's checkpoint
	// past the returned items so subsequent Peek calls won't
	// return them again.
	batch := cs.queue.Peek()
	if len(batch) == 0 {
		// Empty-queue is the common steady-state case (every
		// KindPromptEnded re-triggers TryFlush "just in case" —
		// see routeEvent). Debug-only: logging this at Info would
		// spam the daemon log on every turn for no diagnostic
		// value.
		slog.Debug("chatsession: TryFlush SKIP",
			"chat_id", cs.ChatID, "reason", "queue_empty")
		return nil
	}

	cs.mu.Lock()
	as := cs.selectedAS
	cs.mu.Unlock()
	if as == nil {
		// activeAS_nil / as_not_ready DO indicate real backpressure
		// (a queued message waiting on an AS that isn't ready yet)
		// — worth Debug-level visibility when actively diagnosing
		// a stuck chat, but not Info-level noise in steady state.
		slog.Debug("chatsession: TryFlush SKIP",
			"chat_id", cs.ChatID, "reason", "activeAS_nil",
			"queue_len", cs.queue.Len())
		cs.queue.Rewind()
		return nil
	}
	if !as.IsReady() {
		slog.Debug("chatsession: TryFlush SKIP",
			"chat_id", cs.ChatID, "reason", "as_not_ready",
			"queue_len", cs.queue.Len(), "as_id", as.ID)
		cs.queue.Rewind()
		return nil
	}

	// Build the Prompt from the peek'd batch. The batch is a
	// stable snapshot owned by us; the queue can be mutated
	// concurrently without affecting this slice. Prompt.Messages
	// is the same set — it's later read by writebackMessageState
	// (and by the AS-side readpump) but never mutated.
	p := cs.buildPromptLocked(batch)

	// Submit runs OUTSIDE cs.mu and outside the queue's mutex —
	// SendBlocks can block on a hung prompt RPC. Errors mean
	// "queue stays put" — we Rewind so the items are retryable.
	err := as.Submit(p)
	if err != nil {
		cs.queue.Rewind()
		return err
	}

	// On success: commit the in-flight batch (removes it from
	// the queue), then emit MessageSubmitted wire events
	// OUTSIDE the lock (the callback may run ch.Send() which
	// can block on Feishu API). The batch slice is NOT mutated
	// — Messages are immutable, and the wire event is the
	// canonical signal of the transition.
	cs.queue.Commit()
	for i := range batch {
		cs.EmitMessageState(batch[i].ID, agent.MessageSubmitted)
	}
	return nil
}

// writebackMessageState (CS-AS 边界重构 Phase 1) fires the
// PromptEndBus event for a just-ended Prompt. Post-refactor the
// Message type is immutable, so there is nothing to stamp on
// the messages themselves — the per-message state transition
// has already been emitted on Submit success (via
// EmitMessageState in TryFlush). This method's only remaining
// job is to fire the terminal prompt-end event so adapters
// (typically the feishu adapter) can flip their receipt cards
// from 🔄 to ✅ (or ❌).
//
// `as` is the AgentSession that owned the prompt; its ID is
// denormalized into the PromptEndedEvent so subscribers don't
// have to consult cs.selectedAS (which only routes input).
//
// Called by the runtime's KindPromptEnded handler (pump_events.go
// routeEvent).
func (cs *ChatSession) writebackMessageState(as *AgentSession, p *Prompt) {
	if p == nil {
		return
	}
	cs.mu.RLock()
	bus := cs.PromptEndBus
	chatID := cs.ChatID
	cs.mu.RUnlock()

	// Lock-ordering caveat (F-54 review fix): we previously held
	// cs.mu across Publish, blocking every concurrent cs.mu caller
	// (/use, /kill, QueueUserMessage, TryFlush) for the duration
	// of any subscriber's work — most importantly the feishu
	// adapter's AddReaction HTTP round-trip (~200-500ms). We now
	// snapshot the payload under the lock, release it, and then
	// Publish. The payload's fields are values (no shared state),
	// so post-unlock Publish is race-free.
	if p.LastMessageID == "" {
		return
	}
	asID := ""
	if as != nil {
		asID = as.ID
	}
	bus.Publish(PromptEndedEvent{
		ChatID:         chatID,
		UserMsgID:      p.LastMessageID,
		PromptID:       p.ID,
		Reason:         p.EndReason,
		EndedAt:        p.EndedAt,
		AgentSessionID: asID,
	})
}

// QueueMaxMsgs is the maximum number of queued messages a
// ChatSession can hold before QueueUserMessage returns
// ErrQueueFull. Phase 1 default: 50 (matches v1.3 inputBuffer).
const QueueMaxMsgs = 50

// DropQueue (CS-AS 边界重构 Phase 1) empties the at-least-once
// queue, marks each dropped message as MessageDropped, and emits
// the wire event. Returns the number dropped.
//
// Used by /kill and /new to clear queued messages on a forced
// buffer reset. SendBlocks failures DO NOT trigger this path —
// failed messages stay Queued for natural retry (see
// docs/feat/message_lifecycle.md §5.1).
func (cs *ChatSession) DropQueue() int {
	cleared := cs.queue.Clear()
	for _, m := range cleared {
		cs.emitMessageDropped(m)
	}
	return len(cleared)
}

// QueueLen returns the current queue size (0 if empty).
func (cs *ChatSession) QueueLen() int {
	return cs.queue.Len()
}

// --- F-54 event bus accessors ---------------------------------------
//
// Each ChatSession owns one *services.EventBus[X] per event kind.
// Runtime subscribers register via Bus().Subscribe(handler); multiple
// subscribers may coexist (first to return true consumes the
// event). The buses are constructed eagerly in New(); callers that
// reach them via these accessors see a non-nil Bus even before any
// subscriber has registered.

// EmitMessageState (F-54) fires the MessageStateBus for a single
// userMsgID. Public entry point for external lifecycle triggers
// (e.g. newMessageDispatcher in cmd/nightme calling
// cs.EmitMessageState(userMsgID, MessageQueued) before spawn).
// Internal lifecycle hooks call this too. No-op when the bus has
// no subscribers.
//
// Caller MUST NOT hold cs.mu (the Publish call invokes handlers
// synchronously; a handler may call back into ChatSession methods).
func (cs *ChatSession) EmitMessageState(userMsgID string, state agent.MessageState) {
	cs.mu.RLock()
	bus, chatID := cs.MessageStateBus, cs.ChatID
	cs.mu.RUnlock()
	bus.Publish(MessageStateEvent{
		ChatID:    chatID,
		UserMsgID: userMsgID,
		State:     state,
		At:        time.Now(),
	})
}

// LookupInPool returns the AgentSession matching (agent, cwd) if
// present in the pool (regardless of status). Returns
// ErrAgentNotFound if not in pool.
func (cs *ChatSession) LookupInPool(agent, cwd string) (*AgentSession, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	as, ok := cs.pool[agentCwdKey{Agent: agent, Cwd: cwd}]
	if !ok {
		return nil, ErrAgentNotFound
	}
	return as, nil
}

// attachAgentSession adds as to the pool and installs an EventBus
// subscription that routes its events to cs.routeEvent. Idempotent:
// if as is already attached (by key), the call is a no-op.
//
// Subscribing OUTSIDE cs.mu: we acquire mu only to insert into
// cs.pool, release it, then call EventBus.Subscribe (which takes
// the bus's own lock, not cs.mu). Holding cs.mu across the bus's
// lock would create lock-ordering risk if a bus handler ever
// needed to call back into ChatSession.
func (cs *ChatSession) attachAgentSession(as *AgentSession) {
	if as == nil {
		return
	}
	cs.mu.Lock()
	cs.attachAgentSessionLocked(as)
	cs.mu.Unlock()
	// Install the bus subscription OUTSIDE cs.mu — see the lock
	// ordering note above. attachAgentSubscription is itself
	// idempotent so a race with PumpEvents' periodic
	// attachAllPendingSubscriptions is safe.
	cs.attachAgentSubscription(as)
}

// attachAgentSessionLocked is attachAgentSession without the lock
// acquire. MUST be called with cs.mu held (write). Idempotent.
func (cs *ChatSession) attachAgentSessionLocked(as *AgentSession) {
	if as == nil {
		return
	}
	key := agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}
	if _, exists := cs.pool[key]; exists {
		return
	}
	cs.pool[key] = as
}

// detachAgentSession removes as from the pool and tears down its
// subscription record. Idempotent.
//
// MUST hold cs.mu (write).
func (cs *ChatSession) detachAgentSession(as *AgentSession) {
	if as == nil {
		return
	}
	delete(cs.pool, agentCwdKey{Agent: as.Agent, Cwd: as.Cwd})
}

// selectAgentSessionLocked makes as the chat's active AgentSession and
// owns the per-AS context lifecycle for that promotion.
//
// Context wiring belongs here, at the single point where the active
// AgentSession changes, rather than in individual handlers. Before
// this, only /use called Activate — so any chat that never ran /use
// left every AgentSession unactivated. Two consequences:
//
//   - opCtx stayed at the constructor's Background value (or, for
//     sessions restored from disk, nil — which panicked the daemon on
//     the first message after every restart, because the default
//     FlushHook passes OpContext() straight into bridge.SendBlocks and
//     the pi bridge dereferences it on entry);
//   - Background() was a silent no-op (opCancel nil), so /use could
//     not actually interrupt an in-flight turn on the outgoing AS, and
//     ResetContext could not cascade a shutdown into it.
//
// Idempotent by design. Activate CANCELS the previous opCtx, so
// calling it on every lookup — the per-message hot path — would kill
// the running turn. We only (re)activate when the active AgentSession
// actually changes, or when it has never been activated.
//
// Phase 1 (CS-AS 边界重构): the previous prev.Background() call is
// gone. /use no longer cancels the old AS's opCtx — the old AS
// keeps running in the background, its readpump continues to
// consume events, and ChatSession can resume reading from the same
// channel on re-/use. Real cancellation happens via Shutdown()
// (called only by /kill or CS shutdown).
//
// Caller must hold cs.mu. Takes cs.ctxMu (via Context()) and the
// target's asMu; neither is ever held while acquiring cs.mu, so the
// ordering is safe.
func (cs *ChatSession) selectAgentSessionLocked(as *AgentSession) {
	cs.selectedAS = as
	if as == nil {
		return
	}
	if as.IsActivated() {
		return
	}
	as.Activate(cs.Context())
}

// LookupSelectedAgentSession resolves the active AgentSession.
//
// Single-path resolution (no runtime fallback):
//
//   - selectedAgent is always non-empty for a ChatSession constructed
//     by Manager.GetOrCreate (init-time seed from cfg.Primary
//     snapshot). The runtime never needs to choose between two
//     agents at lookup time.
//   - Resolve pool[(selectedAgent, selectedCwd)]:
//     · hit (StatusRunning) → reuse
//     · miss (or non-Running, e.g. Detached after daemon restart,
//       or Exited after CLI died) → spawn (selectedAgent, selectedCwd)
//
// Returns ErrNoSelectedCwd if selectedCwd is empty. Returns
// ErrNoSelectedAgent if selectedAgent is empty (misconfigured daemon —
// cfg.Primary snapshot was empty at ChatSession creation).
func (cs *ChatSession) LookupSelectedAgentSession() (*AgentSession, error) {
	cs.mu.Lock()

	if cs.selectedCwd == "" {
		cs.mu.Unlock()
		return nil, ErrNoSelectedCwd
	}

	if cs.selectedAgent == "" {
		// Misconfigured: Manager.GetOrCreate should have seeded
		// selectedAgent from cfg.Primary at construction. An empty
		// primary at construction means the daemon has no global
		// default configured; the runtime cannot choose an agent.
		cs.mu.Unlock()
		return nil, ErrNoSelectedAgent
	}

	// commit fix-6: pool hit only returns if the entry is still
	// Running. A Detached entry (process state unknown after
	// restart) or Exited entry (CLI died) falls through to the
	// spawn path below.
	if as, ok := cs.pool[agentCwdKey{Agent: cs.selectedAgent, Cwd: cs.selectedCwd}]; ok && as.Status() == StatusRunning && as.Handle() != nil {
		cs.selectAgentSessionLocked(as)
		cs.mu.Unlock()
		// Pool hit: install subscription if not already present.
		// attachAgentSubscription is idempotent.
		cs.attachAgentSubscription(as)
		return as, nil
	}

	// Reuse a non-Running pool entry (Detached after daemon restart,
	// or Exited after CLI died) when one exists for this (agent,
	// cwd) tuple. The existing entry preserves identity and
	// — critically — the captured SessionID from the prior run, so
	// the next Spawn replays `--resume <id>` to the bridge. Creating
	// a fresh entry here would discard the resume id and force a
	// brand-new agent session after every daemon restart.
	newAS, hadPrior := cs.pool[agentCwdKey{Agent: cs.selectedAgent, Cwd: cs.selectedCwd}]
	if !hadPrior {
		newAS = NewAgentSession(
			newAgentSessionID(),
			cs.ID,
			cs.selectedAgent,
			cs.selectedCwd,
			nil,
		)
		cs.attachAgentSessionLocked(newAS)
	}
	// If hadPrior, the entry's ID + SessionID + Args are preserved
	// from the prior construction or RestoreFromRegistry. Spawn
	// will fork a new process and SetRunning will clear the stale
	// exit code and flip stat back to Running.
	cs.selectAgentSessionLocked(newAS)
	if cs.asFile != nil {
		_ = cs.asFile.Upsert(newAS.Entry())
	}

	// commit 7: actually fork the child via the configured Spawner.
	// If no Spawner is set (test-friendly default), the
	// AgentSession stays in status=Detached with no process — the
	// caller can still see it in the pool, but SendBlocks will
	// return ErrNotRunning until a Spawner is wired in.
	if cs.spawner != nil {
		// Spawn outside of cs.mu to avoid holding the write lock
		// across a fork+exec. We re-acquire mu for the subsequent
		// persistence + selectedAS assignment.
		spawner := cs.spawner
		cs.mu.Unlock()
		spawnErr := newAS.Spawn(context.Background(), spawner)
		cs.mu.Lock()

		if spawnErr != nil {
			// Spawn failed; keep the entry in the pool but mark
			// detached so the next lookup can re-attempt. Caller
			// will see an error from LookupSelectedAgentSession.
			cs.mu.Unlock()
			cs.attachAgentSubscription(newAS)
			return newAS, fmt.Errorf("chatsession: spawn failed (selectedAgent=%q, cwd=%q): %w", cs.selectedAgent, cs.selectedCwd, spawnErr)
		}
		// Refresh registry entry with updated PID/Status.
		if cs.asFile != nil {
			_ = cs.asFile.Upsert(newAS.Entry())
		}
	}

	cs.persistChatEntryLocked()
	cs.mu.Unlock()
	// Install the bus subscription OUTSIDE cs.mu — see the lock
	// ordering note in attachAgentSubscription. attachAgentSubscription
	// is idempotent so a race with PumpEvents' periodic scan is safe.
	cs.attachAgentSubscription(newAS)

	// commit 8c: readPump is NOT auto-started here. The runtime
	// (cmd/nightme) explicitly calls cs.StartReadPump() after
	// the spawn resolves, typically from the /use handler or first
	// message dispatch. Tests that don't go through the runtime
	// are unaffected (no leak).

	return newAS, nil
}

// AgentSessionsInCwd returns a fresh slice of every AgentSession in
// the pool whose Cwd == cwd. The returned slice is independent of
// cs.pool (callers may mutate / range without disturbing subsequent
// reads). Returns nil when no entries match — including when cwd is
// empty (no selection yet).
func (cs *ChatSession) AgentSessionsInCwd(cwd string) []*AgentSession {
	if cwd == "" {
		return nil
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]*AgentSession, 0)
	for _, as := range cs.pool {
		if as.Cwd == cwd {
			out = append(out, as)
		}
	}
	return out
}

// DropAgentSession is the per-entry cleanup primitive used by
// /kill's batch path. For a single kill it composes:
//
//  1. detachAgentSubscription — tears down the EventBus subscriber
//     so the AS no longer feeds the ChatSession receive loop.
//  2. detachAgentSession — removes from cs.pool.
//  3. selectedAS clear — if the dropped AS was the selected one,
//     null out cs.selectedAS so the next message goes through
//     LookupSelectedAgentSession → fresh spawn.
//  4. asFile.Delete — removes the persistent agent_sessions.json
//     entry so the next daemon restart does not resurrect a corpse.
//
// Safe to call concurrently with LookupSelectedAgentSession: takes
// cs.mu once. Caller (kill.KillAgent / KillAllAgents) is responsible
// for invoking as.Close() first so the bridge process is gone before
// the disk entry is deleted (F-43 §5.3 invariant).
func (cs *ChatSession) DropAgentSession(as *AgentSession) {
	if as == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.detachAgentSubscriptionLocked(as)
	cs.detachAgentSession(as)
	if cs.selectedAS == as {
		cs.selectedAS = nil
	}
	if cs.asFile != nil {
		_ = cs.asFile.Delete(as.ID)
	}
}

// NewActiveAgentSessions resets the conversation context on the
// AgentSessions associated with cs.SelectedCwd(). Filters the pool by
// selectedCwd, optionally narrowing further by agentName (when non-empty),
// AND by Status == StatusRunning — only RUNNING sessions have a live
// bridge handle and therefore have conversation context to reset.
//
// F-34 §3.4 + §6 Q-N4: this is the core batch primitive behind the
// `/new` slash command. Behavior contract:
//
//   - Pool identity preserved: AgentSession.ID / Cwd / Agent / args
//     are NOT touched. Only the underlying bridge's conversation
//     state is reset (via AgentSession.New → bridge.New).
//   - Serial execution: AgentSession.New calls are issued one at a
//     time so stdin / RPC traffic does not interleave.
//   - Status filter: ONLY StatusRunning entries are candidates.
//     Exited / Detached entries are skipped SILENTLY (do not count
//     as matched; do not trigger a lazy spawn just to clear
//     conversation that does not exist). This was clarified by
//     product on 2026-08-04: "如果没启动过的agentsession,则不需要
//     去启动AgentSession.因为它本身就没启动,所以不需要New".
//   - InputBuffer cleared: queued user messages are dropped before
//     the function returns, regardless of how many bridges succeeded.
//     This matches /kill semantics (F-34 §6 Q-N4).
//   - currentTurnUserMsgID NOT cleared: the next user message after
//     /new naturally opens a new turn + new anchor + cold-creates a
//     new receipt (Channel-autonomous per v1.3 §2.4).
//
// Returns:
//   - matched: pool entries that passed ALL filters (cwd + agentName +
//     Status == Running). These are the entries that were attempted.
//   - reset:   pool entries whose bridge.New succeeded.
//   - firstErr: the first non-nil bridge.New error (other errors are
//     counted into matched but not reset; callers report them in the
//     reply via the returned error).
func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, results []ResetResult, firstErr error) {
	cs.mu.RLock()
	cwd := cs.selectedCwd
	if cwd == "" {
		cs.mu.RUnlock()
		return 0, 0, nil, nil
	}
	cs.mu.RUnlock()

	// 1. Snapshot ALL (cwd, [agentName]) targets (no lock held across
	//    bridge calls). F-43 §5.4: dead/detached entries are NOT
	//    silently skipped — their stale SessionID would resurrect a
	//    dead session on next spawn, defeating /new's intent.
	cs.mu.RLock()
	targets := make([]*AgentSession, 0)
	for _, as := range cs.pool {
		if as.Cwd != cwd {
			continue
		}
		if agentName != "" && as.Agent != agentName {
			continue
		}
		targets = append(targets, as)
	}
	cs.mu.RUnlock()

	// The queue is deliberately NOT dropped here. /new resets the
	// agent's conversation context; queued messages are still owed
	// a reply and flush into the fresh context on the next
	// TryFlush. (Earlier revisions cleared the queue on /new — see
	// internal/command/newcmd/cmd.go for the rationale.)

	if len(targets) == 0 {
		return 0, 0, nil, nil
	}

	results = make([]ResetResult, 0, len(targets))

	// 2. Serial reset. Each entry's action branches on its Status:
	//    - StatusRunning:   in-place reset via bridge.New(ctx)
	//    - StatusDetached / StatusExited: clear SessionID (no spawn)
	for _, as := range targets {
		matched++
		result := ResetResult{
			Agent:       as.Agent,
			Cwd:         as.Cwd,
			BeforeState: as.Status(),
			Session:     as,
		}

		if as.Status() != StatusRunning {
			// F-43 §5.4: dead/detached entry has no live conversation
			// to reset, but its stale SessionID must NOT be replayed on
			// the next spawn. Clear SessionID in-memory + persist so the
			// next LookupSelectedAgentSession spawns fresh.
			//
			// NO lazy spawn (F-34 §6 Q-N4 / product clarification
			// 2026-08-04): spawning just to reset a dead agent would
			// waste resources and implicitly activate it.
			as.SetSessionID("")
			if cs.asFile != nil {
				// Review finding #B3: previously `_ = ...` swallowed
				// the write error. If Upsert fails after the in-memory
				// SessionID clear, the on-disk entry still carries the
				// stale SessionID, and the next restore on daemon restart
				// would replay `--resume <dead-id>` — the exact bug
				// F-43 §5.4 was designed to fix. Surface the error so
				// the handler can report it (and the handler treats any
				// per-row error as ✗ rather than ✓).
				if err := cs.asFile.Upsert(as.Entry()); err != nil {
					result.Error = err
				}
			}
			result.Action = "marked-fresh"
			results = append(results, result)
			reset++
			continue
		}

		// Running: in-place reset via bridge.New(ctx).
		//
		// Active AgentSession pump coordination (F-34 Phase 3 review):
		//
		// The readPump captures as.Events() ONCE before its loop, so
		// a kill+respawn that swaps the bridge handle would orphan the
		// pump on the old (now-closed) events chan. Worse, the pump's
		// closing branch calls as.SetExited(0) which races with our
		// SetRunning after the swap.
		//
		// Pump coordination: capture the selectedAS identity and
		// current handle under one RLock, then stop the pump (if
		// this AS is the active one), call as.New, and restart the
		// pump on the new handle. The handleChanged flag is
		// needed because kill+respawn swaps the bridge handle (and
		// its events chan); for in-place reset the chan is
		// unchanged but the pump goroutine still needs to be
		// restarted because StopReadPump signaled it to exit.
		isActive := false
		var oldHandle *agent.LiveAgent
		cs.mu.RLock()
		isActive = (as == cs.selectedAS)
		if isActive {
			oldHandle = as.Handle()
		}
		cs.mu.RUnlock()
		// CS-AS 边界重构 Phase 1: no per-CS StopReadPump / StartReadPump
		// calls. as.New handles in-place reset; the per-AS readpump
		// follows whatever the bridge does (close on reset, restart on
		// new process).
		//
		// PTY fallback: as.New returns agent.ErrRestartRequired for
		// bridges that can't do in-place reset (raw PTY). The
		// kill+respawn path is the chat layer's responsibility now —
		// AgentSession.New deliberately doesn't take a Spawner, since
		// 4/5 bridges never need one.
		err := as.New(ctx)
		if errors.Is(err, agent.ErrRestartRequired) {
			err = cs.restartAgentSession(ctx, as)
		}
		handleChanged := isActive && (as.Handle() != oldHandle)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			result.Action = "in-place-reset"
			result.Error = err
			results = append(results, result)
			continue
		}
		reset++
		result.Action = "in-place-reset"
		results = append(results, result)
		// Persist after a successful kill+respawn so the registry
		// stays in sync with the in-memory state. For in-place
		// resets the bridge will also emit a fresh EventAgentReady which
		// the runtime's AgentEventBus subscriber captures via
		// PersistAgentSession; for kill+respawn the new child hasn't
		// started yet, so this Upsert captures the new PID +
		// cleared SessionID before any subsequent EventAgentReady arrives
		// (PTY's new child won't emit init at all, so this is the
		// ONLY persistence opportunity for that path).
		if handleChanged && cs.asFile != nil {
			_ = cs.asFile.Upsert(as.Entry())
		}
	}

	return matched, reset, results, firstErr
}

// restartAgentSession is the chat-layer fallback for bridges that
// can't reset conversation in-place (raw PTY). Called when
// AgentSession.New returns agent.ErrRestartRequired.
//
// Contract:
//   - Kill the old handle (clean shutdown via bridge.Close).
//   - Reset the per-AS readpump (it saw !ok from the closed handle
//     and exited; the respawn will start a fresh drainer).
//   - Call AgentSession.respawn with sessionID="" (we deliberately
//     want a fresh conversation — not a resume).
//   - Clear the persisted SessionID so a stale id never gets replayed
//     on the next respawn.
//
// Returns the original ErrRestartRequired when cs.spawner is nil
// (chat was constructed without WithSpawner, e.g. in unit tests that
// only exercise the in-place path).
func (cs *ChatSession) restartAgentSession(ctx context.Context, as *AgentSession) error {
	if cs.spawner == nil {
		return agent.ErrRestartRequired
	}
	if h := as.Handle(); h != nil {
		_ = h.Close()
	}
	as.asMu.Lock()
	as.readpumpStarted = false
	as.asMu.Unlock()
	if err := as.respawn(ctx, cs.spawner, as.Args(), ""); err != nil {
		as.SetSessionID("")
		return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
	}
	as.SetSessionID("")
	return nil
}

// Entry returns a snapshot of this ChatSession as a registry entry.
func (cs *ChatSession) Entry() *registry.ChatSessionEntry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.entryLocked()
}

// CreatedAt returns when this ChatSession was first created.
func (cs *ChatSession) CreatedAt() time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.createdAt
}

// LastInteractionAt returns when this ChatSession last had user
// interaction.
func (cs *ChatSession) LastInteractionAt() time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.lastInteractionAt
}

// entryLocked is the same as Entry but assumes cs.mu is held.
func (cs *ChatSession) entryLocked() *registry.ChatSessionEntry {
	agentIDs := make([]string, 0, len(cs.pool))
	var activeASID *string
	for _, as := range cs.pool {
		agentIDs = append(agentIDs, as.ID)
		if cs.selectedAS != nil && as.ID == cs.selectedAS.ID {
			id := as.ID
			activeASID = &id
		}
	}
	return &registry.ChatSessionEntry{
		ID:                   cs.ID,
		ChatID:               cs.ChatID,
		SelectedCwd:            cs.selectedCwd,
		SelectedAgent:          cs.selectedAgent,
		PrimaryAgent:         cs.primaryAgent,
		AgentSessionIDs:      agentIDs,
		SelectedAgentSessionID: activeASID,
		CreatedAt:            cs.createdAt,
		LastInteractionAt:    cs.lastInteractionAt,
		WatchMode:            cs.watchMode,
		ThinkMode:            cs.thinkMode,
		ToolsMode:            cs.toolsMode,
	}
}

// persistChatEntry writes the ChatSessionEntry to disk (if persistence
// is configured). Best-effort: errors are returned but not propagated
// through call sites (logged at higher level).
func (cs *ChatSession) persistChatEntry() {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	cs.persistChatEntryLocked()
}

// persistChatEntryLocked writes ChatSessionEntry. Caller must hold
// cs.mu (RLock or Lock).
func (cs *ChatSession) persistChatEntryLocked() {
	if cs.csFile == nil {
		return
	}
	_ = cs.csFile.Upsert(cs.entryLocked())
}

// newAgentSessionID returns a unique ID for an AgentSession. v1.2
// commit 6 uses a simple counter-based scheme for testability;
// commit 7 may swap to UUID v7.
var agentSessionCounter atomic.Uint64

func newAgentSessionID() string {
	n := agentSessionCounter.Add(1)
	return fmt.Sprintf("as_%d_%d", time.Now().UnixNano(), n)
}
