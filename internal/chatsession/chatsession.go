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
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatstore"
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/registry"
)

// Type aliases — let ChatSession use AgentSession types without
// fully qualifying them. The aliases are intentional: ChatSession
// is the higher-level pool manager and should not feel the cost of
// the agentsession extraction at every call site.
type (
	AgentSession       = agentsession.AgentSession
	Status             = agentsession.Status
	Spawner            = agentsession.Spawner
	Message            = agentsession.Message
	EnrichedEvent      = agentsession.EnrichedEvent
	EnrichedEventKind  = agentsession.EnrichedEventKind
	PromptEndedChange  = agentsession.PromptEndedChange
	LifecycleChange    = agentsession.LifecycleChange
	AgentEventEnvelope = agentsession.AgentEventEnvelope
	MessageStateEvent  = agentsession.MessageStateEvent
	PromptEndedEvent   = agentsession.PromptEndedEvent
	Prompt             = agentsession.Prompt
)

// Constructor + restorer re-exports.
var (
	NewAgentSession        = agentsession.NewAgentSession
	FromAgentSessionEntry  = agentsession.FromAgentSessionEntry
	NewRegistrySpawner     = agentsession.NewRegistrySpawner
	WithEventQueueCapacity = agentsession.WithEventQueueCapacity
)

// Sentinel error re-exports. The canonical definitions live in
// agentsession; these aliases let CS callers keep using the
// chatsession.X form without churn.
var (
	ErrNotRunning = agentsession.ErrNotRunning
)

// Re-exported constants.
const (
	MessageKindNormal = agentsession.MessageKindNormal
	MessageKindQueue  = agentsession.MessageKindQueue

	KindAgentEvent  = agentsession.KindAgentEvent
	KindPromptEnded = agentsession.KindPromptEnded
	KindLifecycle   = agentsession.KindLifecycle

	PromptEndClean       = agentsession.PromptEndClean
	PromptEndUserKilled  = agentsession.PromptEndUserKilled
	PromptEndUserStopped = agentsession.PromptEndUserStopped
)

// MessageKind is a type alias (re-export).
type (
	MessageKind = agentsession.MessageKind
)

// PromptEndReason is a type; re-export it as a type alias.
type (
	PromptEndReason = agentsession.PromptEndReason
	PromptState     = agentsession.PromptState
)

// Prompt state constants — re-export so downstream packages
// (channel/feishu) can keep using agentsession.PromptRunning etc.
const (
	PromptRunning = agentsession.PromptRunning
	PromptDone    = agentsession.PromptDone
)

// agentCwdKey is the ChatSession pool map key. Moved back from
// agentsession package — it's a CS-level concept (the pool key
// for CS's map[agentCwdKey]*AgentSession).
type agentCwdKey struct {
	Agent string
	Cwd   string
}

// ErrAgentNotFound indicates a pool lookup miss. Lives in agentsession
// (canonical), re-exported here so existing CS call sites can keep
// using chatsession.ErrAgentNotFound without an import churn.
var ErrAgentNotFound = agentsession.ErrAgentNotFound

// ErrNoSelectedAgent is returned by LookupSelectedAgentSession when
// cs.selectedAgent is empty. The runtime seeds selectedAgent from
// cfg.Primary at ChatSession construction (via
// chatsession.NewManager.GetOrCreate); an empty primary at
// construction indicates a misconfigured daemon (no global default
// set in config.yaml).
var ErrNoSelectedAgent = errors.New("chatsession: selectedAgent is empty (cfg.Primary snapshot was empty at construction)")

// Re-export Status constants so call sites can stay terse.
const (
	StatusRunning  = agentsession.StatusRunning
	StatusDetached = agentsession.StatusDetached
	StatusExited   = agentsession.StatusExited
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

	// routeMu serializes store-backed routing setters so disk writes
	// and the in-memory cache cannot complete out of order across
	// concurrent /cwd /use /watch mutations (docs/CHATSTORE.md).
	routeMu sync.Mutex

	// gitStatusDeps bundles the CollectGit + LookupPR closures the
	// runtime injects at startup; ChatSession.GitStatus reads them
	// on every call (no per-chat cache layer — the workspace
	// snapshot is rebuilt fresh each time so a footer always
	// reflects the latest file edits). Wired at chatsession
	// construction via WithGitStatusDeps (manager.go). PR caching
	// lives in prcache.Cache (60s TTL), not here.
	gitStatusDeps GitStatusDeps

	// Active routing state. selectedAgent is mutable via /use;
	// selectedCwd via /cwd. primaryAgent is the cfg.Primary snapshot
	// at ChatSession construction; read-only post-construction
	// (Q-A: no /default command, no per-chat override).
	//
	// At New() time selectedAgent is seeded from primaryAgent so the
	// runtime never sees an empty selectedAgent on a fresh chat.
	selectedCwd   string
	selectedAgent string
	primaryAgent  string

	// F-watch §3.1.1: per-chat message-watch mode. Default
	// WatchModeMention (only @ bot or @_all messages are
	// processed); /watch on switches to WatchModeAll. See
	// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.10/§6.11.
	// DM chats always behave as if WatchMode==WatchModeAll
	// (every DM message is "addressed to bot"); the gate logic in
	// Manager.AcceptInbound enforces that — this field is only
	// consulted for group chats via the channel-supplied
	// HasMention bool.
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
	toolsMode ToolsMode

	// F-watch hint tombstone: true once the one-time `/watch on`
	// hint has been emitted for this chat. Lived on the registry
	// entry first (Manager.maybeEmitWatcherHint v1 wrote a stub
	// entry directly to ChatSessionFile), but that path races
	// against any other persist that builds the entry from this
	// struct (SetWatchMode / SetThinkMode / SetToolsMode etc. all
	// go through entryLocked(), which silently dropped the flag
	// and clobbered the tombstone back to false on the next
	// write). Tracking it as a first-class field here mirrors
	// the WatchMode / ThinkMode / ToolsMode pattern: included in
	// entryLocked() so every persist path preserves it, hydrated
	// from entry in RestoreFromRegistry so daemon restart doesn't
	// re-emit. Zero value (false) is the safe "no hint yet" state.
	watcherHintEmitted bool

	// Pool of AgentSessions keyed by (agent, cwd).
	pool map[agentCwdKey]*AgentSession

	// Currently active AgentSession (pointer into pool). nil means
	// no active session (ChatSession exists but no /cwd + /use yet).
	selectedAS *AgentSession

	// Timestamps.
	createdAt         time.Time
	lastInteractionAt time.Time

	// Persistence handles (optional — nil means no persistence).
	csStore *chatstore.Store
	asFile  *registry.AgentSessionFile
	asPool  *AgentSessionPool // global warm pool; nil in some tests

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

	// heartbeat (F-63) is the per-chat progress counter store,
	// keyed by userMsgID. See internal/chatsession/heartbeat.go
	// for the LRU semantics and the Observe() choke point. The
	// runtime handler calls cs.Heartbeat().Observe() on every
	// outbound event BEFORE the policy chain (see F-63 §3.2);
	// channels read the resulting snapshot via OutboundMessage
	// (Kind=OutHeartbeat). nil only if construction was bypassed
	// (test fakes); production always populates it in New().
	heartbeat *HeartbeatTracker

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

	// emitter is the outbound chokepoint bound to this chat
	// session. Set once at WithEmitter time (production wires it
	// from Manager, which holds the single daemon-wide Emitter);
	// test paths may pass a fake. Immutable post-binding; no
	// lock needed. nil means "no emitter bound yet" — commands
	// must nil-check via Emitter() before calling Send.
	emitter outbound.Emitter

	// watchdog (F-61) is the per-chat diagnostic timer. nil
	// until lazily initialized by Watchdog(). Owned by the chat
	// session; lifetime matches the CS. watchdogOnce guarantees
	// the first-init write is visible to all subsequent
	// readers — without it, two goroutines (e.g. TryFlush in
	// the dispatcher and routeEvent on KindLifecycle) race on
	// the lazy-init read/write pair (F-61 data race fix).
	watchdog     *Watchdog
	watchdogOnce sync.Once

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
func New(chatID, primaryAgent string) (*ChatSession, error) {
	cs := &ChatSession{
		ID:                deriveIDFromChatID(chatID),
		ChatID:            chatID,
		selectedAgent:     primaryAgent, // init seed
		primaryAgent:      primaryAgent, // historical snapshot, read-only
		pool:              make(map[agentCwdKey]*AgentSession),
		watchMode:         WatchModeMention, // F-watch default
		thinkMode:         ThinkModeShow,    // F-think default
		toolsMode:         ToolsModeHide,    // F-38 default (quiet by default)
		createdAt:         time.Now(),
		lastInteractionAt: time.Now(),
		// F-63: per-chat heartbeat tracker (LRU). The runtime
		// handler calls cs.Heartbeat().Observe() on every outbound
		// event BEFORE the policy chain (see F-63 §3.2); the
		// tracker decides whether to fire an OutHeartbeat to the
		// channel so the receipt card can show the live "agent is
		// working" header.
		heartbeat: NewHeartbeatTracker(DefaultHeartbeatCap),
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
func (cs *ChatSession) WithPersistence(csFile *chatstore.Store, asFile *registry.AgentSessionFile) *ChatSession {
	cs.mu.Lock()
	cs.csStore = csFile
	cs.asFile = asFile
	cs.mu.Unlock()
	return cs
}

// WithAgentSessionPool attaches the process-wide warm AgentSession pool.
func (cs *ChatSession) WithAgentSessionPool(p *AgentSessionPool) *ChatSession {
	if cs == nil {
		return cs
	}
	cs.mu.Lock()
	cs.asPool = p
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

// Watchdog returns the per-chat watchdog, lazily constructing it
// on first call. Used by routeEvent to arm/disarm timers; tests
// inject fakes by setting cs.watchdog directly.
//
// sync.Once provides the happens-before guarantee needed for
// safe concurrent lazy init — without it, callers racing on the
// first Watchdog() would see a torn pointer write (data race).
// After Once returns, the pointer is safely published to all
// subsequent goroutines.
func (cs *ChatSession) Watchdog() *Watchdog {
	cs.watchdogOnce.Do(func() {
		if cs.watchdog == nil {
			cs.watchdog = newWatchdog(cs)
		}
	})
	return cs.watchdog
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
// See docs/feat/F-43-close-new-graceful-and-reset.md §5.2.
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

// clearQueueOnCwdBoundary drains the inbound queue at a cwd
// boundary. Tolerates a nil queue (tests that construct a bare
// ChatSession{} without New).
func (cs *ChatSession) clearQueueOnCwdBoundary() {
	if cs == nil || cs.queue == nil {
		return
	}
	for _, msg := range cs.queue.Clear() {
		cs.emitMessageDropped(msg)
	}
}

// ensureStoreBootstrapped creates a chatstore entry for this CS if
// persistence is wired and the chatID is not yet on disk. Used by
// store-first setters so tests that call WithPersistence without an
// explicit Bootstrap still work (production GetOrCreate always
// Bootstraps first — docs/CHATSTORE.md).
func (cs *ChatSession) ensureStoreBootstrapped() error {
	if cs == nil || cs.csStore == nil {
		return nil
	}
	if _, ok := cs.csStore.Get(cs.ChatID); ok {
		return nil
	}
	primary := cs.primaryAgent
	if primary == "" {
		primary = cs.selectedAgent
	}
	_, err := cs.csStore.Bootstrap(cs.ChatID, primary)
	return err
}

// SetSelectedCwd changes the active workspace. Stops active interaction
// with the previous cwd's working set (docs/CHATSTORE.md): AS stay in
// the global asPool warm; EventBus subscriptions are kept. Does NOT
// spawn. Next LookupSelectedAgentSession mounts from asPool/asFile.
func (cs *ChatSession) SetSelectedCwd(cwd string) error {
	if cwd == "" {
		return errors.New("chatsession: empty cwd")
	}
	cwd = filepath.Clean(cwd)

	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()

	cs.mu.RLock()
	unchanged := filepath.Clean(cs.selectedCwd) == cwd
	cs.mu.RUnlock()
	if unchanged {
		return nil
	}
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return err
		}
		if err := cs.csStore.SetSelectedCwd(cs.ChatID, cwd); err != nil {
			return err
		}
	}

	cs.mu.Lock()
	oldAS := cs.selectedAS
	if oldAS != nil &&
		oldAS.Agent == cs.selectedAgent &&
		filepath.Clean(oldAS.Cwd) == cwd {
		oldAS = nil
	}
	cs.detachActiveWorkingSetLocked()
	cs.selectedCwd = cwd
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()

	if oldAS != nil {
		oldAS.ClearInFlight()
	}
	cs.clearQueueOnCwdBoundary()

	// No cache to invalidate anymore: GitStatus(ctx) now rebuilds
	// a fresh snapshot on every call, so the next outbound stamp
	// against this chat will naturally read the new cwd.
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
	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return err
		}
		if err := cs.csStore.SetWatchMode(cs.ChatID, int(mode)); err != nil {
			return err
		}
	}
	cs.mu.Lock()
	cs.watchMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
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
	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return err
		}
		if err := cs.csStore.SetThinkMode(cs.ChatID, int(mode)); err != nil {
			return err
		}
	}
	cs.mu.Lock()
	cs.thinkMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	return nil
}

// SetSelectedAgent switches the active agent family. Does NOT
// spawn or kill any AgentSession; the pool is preserved. Next
// message triggers LookupSelectedAgentSession which may spawn or
// reuse for the (newAgent, currentCwd) tuple.
func (cs *ChatSession) SetSelectedAgent(agent string) error {
	if agent == "" {
		return errors.New("chatsession: empty agent")
	}
	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()

	cs.mu.RLock()
	unchanged := cs.selectedAgent == agent
	cs.mu.RUnlock()
	if unchanged {
		return nil
	}
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return err
		}
		if err := cs.csStore.SetSelectedAgent(cs.ChatID, agent); err != nil {
			return err
		}
	}
	cs.mu.Lock()
	changed := cs.selectedAgent != agent
	var oldAS *AgentSession
	if changed {
		oldAS = cs.selectedAS
		cs.selectedAS = nil
	}
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
	// or the old AS is /closed. endPrompt itself only operates
	// on cs.selectedAS, so it cannot clean the old one. This is
	// a deliberate Phase 0 simplification; the cost is one
	// orphaned Prompt struct per /use-while-busy until the old
	// AS exits.
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()

	// Drop the old AS's in-flight mirror outside cs.mu.
	// ClearInFlight is idempotent and self-locks on asMu.
	if oldAS != nil {
		oldAS.ClearInFlight()
	}
	return nil
}

// SelectedCwd returns the current active workspace.
func (cs *ChatSession) SelectedCwd() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.selectedCwd
}

// GitStatus returns the chat's current workspace + git status +
// open PR snapshot, built fresh on every call: CollectGit runs
// `git status --porcelain --branch --untracked-files=normal`
// synchronously against SelectedCwd with a hard 3s cap so a hung
// git cannot wedge an outbound stamp (see the body for the cap),
// and LookupPR reads the PR from prcache.Cache.PR synchronously.
// PR caching stays where it belongs — in the dedicated prcache.Cache
// with its own 60s TTL, failure-backoff, and background refresh
// — because the PR lookup costs a `gh/glab` API round-trip.
//
// We intentionally do NOT cache the snapshot at this layer:
// the footer must reflect the latest file edits, not whatever
// snapshot was last taken (which could be minutes stale in a
// session that has been paused). Local `git status` is cheap
// (~25-35ms for normal repos, see git_status.go benchmarks),
// so per-call rebuild is the right trade-off.
//
// Concurrency: GitStatus does not mutate chat state, so multiple
// goroutines may call it freely. The per-chat serialisation the
// previous cache layer brought in is gone with the cache; a
// 30-event turn on the readpump goroutine will trigger N git
// invocations. For typical repos the inter-event delay dominates
// this overhead; for repos where it doesn't, add
// `--untracked-files=no` to gtw.CollectReadiness as a follow-up.
//
// Called by the outbound Emitter's GitStatusLookup closure
// (cmd/nightme wiring), the SINGLE chokepoint where every
// outbound message picks up its git snapshot. Business code
// (handler.go / eventbus.go / gtw/fix.go) does not call this
// directly — they send through em.Send and the Emitter stamps
// the snapshot.
//
// Returns nil when there is no workspace (cwd == ""), the
// AgentSession is unset, the deps are not wired, or git
// produces nothing usable. formatGitLine drops the footer line
// on nil — same contract as before the refactor.
func (cs *ChatSession) GitStatus(ctx context.Context) *messages.GitStatus {
	return cs.GitStatusAt(ctx, cs.SelectedCwd(), cs.SelectedAgentSession())
}

// GitStatusAt rebuilds a git footer for an explicit workspace /
// AgentSession. Warm AgentEvent outbounds must pass the envelope
// AS cwd — not SelectedCwd — so footer metadata matches the
// emitting session (docs/CHATSTORE.md §3.3).
func (cs *ChatSession) GitStatusAt(ctx context.Context, cwd string, as *AgentSession) *messages.GitStatus {
	deps := cs.gitStatusDeps
	if deps.CollectGit == nil && deps.LookupPR == nil {
		return nil
	}
	if cwd == "" {
		return nil
	}
	var snap *messages.GitStatusSnapshot
	if deps.CollectGit != nil {
		runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		snap, _ = deps.CollectGit(runCtx, cwd)
		cancel()
	}
	var pr *messages.PR
	if deps.LookupPR != nil && as != nil {
		pr = deps.LookupPR(as.ID, cwd)
	}
	return &messages.GitStatus{
		Workspace:   cwd,
		Snapshot:    snap,
		PullRequest: pr,
	}
}

// WithGitStatusDeps wires the CollectGit / LookupPR closures used
// by ChatSession.GitStatus. Called once at chatsession startup
// (directly from the constructor / restore path and via
// Manager.WithGitStatusDeps for the per-chat OnCreate hook).
// Returns self for chaining.
//
// WithGitStatusDeps is the only mutator of cs.gitStatusDeps;
// callers should not bypass it via direct field access.
func (cs *ChatSession) WithGitStatusDeps(deps GitStatusDeps) *ChatSession {
	cs.gitStatusDeps = deps
	return cs
}

// ClearSelectedCwd removes the active workspace. Used by /gtw
// close when the directory the chat is sitting on has been
// deleted out from under it (most often by another chat's /gtw
// close running `git worktree remove` against the same
// worktree): the chat's selected cwd now points at a path
// that does not exist, so the safe fallback is to drop it and
// let the user re-cwd.
//
// Unlike SetSelectedCwd, an empty value is the whole point —
// no validation rejection. Persists unconditionally so the
// cleared state survives daemon restart. KNOWN FOLLOW-UP:
// persistChatEntryLocked (chatsession.go:1650-1655) does not
// dedup on unchanged state, so a clear-after-empty still
// writes to csFile; flagged in the audit, not addressed here.
//
// Does NOT touch the AgentSession pool: SetSelectedCwd's
// "no spawn, no kill" contract applies. Callers that need to
// tear down in-flight sessions should follow up with
// EvictAgentSessionsInCwd.
func (cs *ChatSession) ClearSelectedCwd() {
	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return
		}
		if err := cs.csStore.SetSelectedCwd(cs.ChatID, ""); err != nil {
			return
		}
	}
	cs.mu.Lock()
	oldAS := cs.selectedAS
	cs.detachActiveWorkingSetLocked()
	cs.selectedCwd = ""
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	if oldAS != nil {
		oldAS.ClearInFlight()
	}
	cs.clearQueueOnCwdBoundary()

	// No cache to clear anymore — ChatSession.GitStatus checks
	// cwd on every call and returns nil when cwd == "".
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

// WatcherHintEmitted reports whether the one-time `/watch on`
// hint has already been sent for this chat. Default false (hint
// not yet sent). Once true, Manager.HandleInbound's drop branch
// skips the hint entirely. Hydrated from the registry entry on
// RestoreFromRegistry so daemon restart preserves the
// tombstone. See Manager.maybeEmitWatcherHint for the trigger
// contract.
func (cs *ChatSession) WatcherHintEmitted() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.watcherHintEmitted
}

// MarkWatcherHintEmitted stamps the hint tombstone on the
// ChatSession and persists the change. Called by
// Manager.maybeEmitWatcherHint immediately after the Emitter
// dispatch (so a transient Emitter.Send failure doesn't leave
// the in-memory state ahead of the persisted state on retry).
//
// Deliberately does NOT touch lastInteractionAt: the hint
// emission is a system event triggered by a dropped user
// message, not a user interaction. Bumping the "last user
// message" timestamp to the hint-emission time would
// misrepresent when the user actually last interacted, and
// (when the future idle-expiry feature lands) could mask the
// fact that a chat has only ever received dropped non-mention
// traffic.
//
// Concurrency: same pattern as SetWatchMode / SetThinkMode /
// SetToolsMode — take ChatSession mutex, write field, release,
// persist. The lock is NOT held across any channel.Send call.
func (cs *ChatSession) MarkWatcherHintEmitted() error {
	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return err
		}
		if err := cs.csStore.SetWatcherHintEmitted(cs.ChatID, true); err != nil {
			return err
		}
	}
	cs.mu.Lock()
	cs.watcherHintEmitted = true
	cs.mu.Unlock()
	return nil
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
func (cs *ChatSession) SetToolsMode(mode ToolsMode) error {
	cs.routeMu.Lock()
	defer cs.routeMu.Unlock()
	if cs.csStore != nil {
		if err := cs.ensureStoreBootstrapped(); err != nil {
			return err
		}
		if err := cs.csStore.SetToolsMode(cs.ChatID, int(mode)); err != nil {
			return err
		}
	}
	cs.mu.Lock()
	cs.toolsMode = mode
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	return nil
}

// ToolsMode returns the current per-chat tool-event visibility.
// Default value when never set is ToolsModeHide (set in New when
// the registry has no persisted value). Direction is OPPOSITE of
// ThinkMode's default — see tools_mode.go in this package
// for the rationale. See docs/SPEC.md §3.1.3.
func (cs *ChatSession) ToolsMode() ToolsMode {
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

// LookupAS returns the AgentSession with the given ID from the
// active pool or the global warm asPool, or nil if not present.
// Used by event subscribers to recover the source AS from an
// AgentSessionID (warm ASes are not in cs.pool after /cwd).
func (cs *ChatSession) LookupAS(id string) *AgentSession {
	if cs == nil || id == "" {
		return nil
	}
	for _, as := range cs.Pool() {
		if as.ID == id {
			return as
		}
	}
	cs.mu.RLock()
	asPool := cs.asPool
	cs.mu.RUnlock()
	if asPool != nil {
		return asPool.FindByID(id)
	}
	return nil
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

// Emitter returns the outbound chokepoint bound to this chat
// session. nil when no Emitter has been wired yet (e.g. before
// Manager.WithEmitter has been called or before GetOrCreate has
// applied the Manager's emitter). Callers must nil-check before
// Send.
//
// Lock-free: emitter is set once and never mutated.
func (cs *ChatSession) Emitter() outbound.Emitter {
	if cs == nil {
		return nil
	}
	return cs.emitter
}

// Heartbeat (F-63) returns the per-chat heartbeat tracker. The
// runtime handler calls .Observe() on every outbound event BEFORE
// the policy chain; channels read the resulting snapshot via
// OutboundMessage (Kind=OutHeartbeat). Returns nil only if the
// ChatSession was constructed via a test fake that bypassed
// New(); production callers can rely on a non-nil result.
func (cs *ChatSession) Heartbeat() *HeartbeatTracker {
	if cs == nil {
		return nil
	}
	return cs.heartbeat
}

// WithEmitter binds the outbound chokepoint to this chat session.
// Set once and never mutated; subsequent calls with the same
// emitter are no-ops, a different emitter panics.
//
// Test paths can use this to inject a fake; production wiring
// goes through Manager.WithEmitter → Manager.GetOrCreate.
func (cs *ChatSession) WithEmitter(em outbound.Emitter) *ChatSession {
	if cs == nil {
		return cs
	}
	if em == nil {
		return cs
	}
	if cs.emitter != nil && cs.emitter != em {
		panic(fmt.Sprintf("chatsession: ChatSession %s already bound to a different Emitter", cs.ChatID))
	}
	cs.emitter = em
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

// SteerUserMessage is the runtime entry point for the `/steer <msg>`
// slash command. It does two things:
//
//  1. Try to accelerate the busy-guard release on the current
//     in-flight turn by calling `as.Stop(ctx)` in a background
//     goroutine. The goroutine outlives the SteerUserMessage
//     call, so a slow bridge RPC (opencode's HTTP interrupt,
//     claudecode's SIGINT + drain, pi's abort RPC) cannot block
//     the slash command reply.
//
//  2. Push msg to the HEAD of the pending region via
//     `queue.PushFront`. The steered message will be the first
//     thing the agent sees on the next turn — even if other
//     user messages had piled up in the queue while the
//     current turn was running.
//
// Ordering note: capacity is checked BEFORE the Stop call. If
// the queue is at QueueMaxMsgs, PushFront returns ErrFull and
// we skip Stop — aborting the bridge for a message we couldn't
// enqueue would leave the user's intent silently lost (the
// current turn is gone, the new message is gone).
//
// Skipped Stop when:
//   - no AS is selected (Handle() == nil)
//   - the AS is in StatusExited (e.g. after /close killed the
//     bridge process; calling Stop on a defunct handle is a
//     wasted RPC + an ignored error)
//
// Distinct from QueueUserMessage in two ways: (a) it tries to
// abort the current turn first, and (b) the new message goes to
// the front of the queue instead of the back. Both are needed
// for "stop + redirect" semantics.
//
// The caller (slash command factory) is responsible for emitting
// `MessageQueued` BEFORE calling this — same timing contract as
// QueueUserMessage. TryFlush is NOT called here; the FlushHook
// path on the next KindPromptEnded (which Stop may accelerate)
// handles submission.
func (cs *ChatSession) SteerUserMessage(msg Message) error {
	if msg.ID == "" {
		return nil
	}

	// Step 1: capacity check. If we can't enqueue the steered
	// message, skip Stop entirely — aborting the bridge for a
	// message we lost is worse than not steering at all.
	if err := cs.queue.PushFront(msg); err != nil {
		return err
	}

	// Step 2: fire-and-forget Stop on a background goroutine.
	// The call returns immediately; the bridge's terminal event
	// drives the FlushHook to drain the queue on its own clock.
	if as := cs.SelectedAgentSession(); as != nil && as.Status() != StatusExited {
		if h := as.Handle(); h != nil {
			go func() {
				_ = h.Stop(cs.Context())
			}()
		}
	}
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
	if as.Status() == StatusExited {
		// The bridge process is gone (e.g. after /close) but the
		// AS entry is still in the pool with sessionID preserved.
		// Don't try to Submit — wait for the next
		// LookupSelectedAgentSession call which detects
		// StatusRunning && Handle() != nil mismatch and respawns
		// a fresh bridge with --resume.
		slog.Debug("chatsession: TryFlush SKIP",
			"chat_id", cs.ChatID, "reason", "as_status_exited",
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

	// Pre-Submit re-check under cs.mu. Between the snapshot of
	// `as` above and the Submit call below, the selected AS can
	// be respawned by LookupSelectedAgentSession (after /close
	// killed it) or its status can flip. Submitting the peek'd
	// batch to a freshly-respawned bridge would re-issue the
	// message — same race as the SteerUserMessage / Submit
	// interaction the review flagged.
	//
	// Re-acquire and verify:
	//   1. The selected AS pointer is unchanged (no respawn).
	//   2. The status hasn't flipped to Exited (no /close).
	// If either fails, Rewind so the batch is retried against
	// the correct AS on the next FlushHook.
	cs.mu.Lock()
	currentAS := cs.selectedAS
	statusNow := StatusRunning
	if currentAS != nil {
		statusNow = currentAS.Status()
	}
	cs.mu.Unlock()
	if currentAS != as || statusNow == StatusExited {
		slog.Debug("chatsession: TryFlush SKIP",
			"chat_id", cs.ChatID, "reason", "as_changed_pre_submit",
			"queue_len", cs.queue.Len(), "as_id", as.ID)
		cs.queue.Rewind()
		return nil
	}

	// Submit runs OUTSIDE cs.mu and outside the queue's mutex —
	// SendBlocks can block on a hung prompt RPC. Errors mean
	// "queue stays put" — we Rewind so the items are retryable.
	//
	// Submit's Keepalive backstop internally handles the
	// "bridge reported dead" case (PID gone for subprocess
	// bridges, WS host severed for dsh, transport nil for
	// pty/acp) — it demotes + respawns in one cohesive step
	// before SendBlocks lands. We don't need a separate
	// auto-recover here; the chat layer just surfaces any
	// persistent error from Submit.
	err := as.Submit(p)
	if err != nil {
		cs.queue.Rewind()
		return err
	}

	// F-61: arm HungPrompt. If no KindPromptEnded arrives
	// within HungMins, the watchdog marks the active AS as
	// Suspect("hung_prompt") so the prober revives it under
	// cooldown. Submit succeeded — the prompt is in flight.
	cs.Watchdog().ArmHungPrompt()

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
	// (/use, /close, QueueUserMessage, TryFlush) for the duration
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
// ErrQueueFull. Raised from the v1.3 default of 50 → 4096 so that
// restart-replay (which pushes every AS's InFlightMessages into
// the queue) plus normal user input doesn't hit backpressure on
// chats with many parallel agents or long-running prompts.
const QueueMaxMsgs = 4096

// DropQueue (CS-AS 边界重构 Phase 1) empties the at-least-once
// queue, marks each dropped message as MessageDropped, and emits
// the wire event. Returns the number dropped.
//
// Used by /close and /new to clear queued messages on a forced
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

// PublishMessageState is the framework-level helper for emitting
// MessageState events on a chat's MessageStateBus. It centralises
// the three-guard contract (nil cs / empty MessageID / nil bus) so
// framework-level dispatchers — the slash-command commander
// (internal/command/commander.go) and the shell dispatcher
// (internal/shell/dispatch.go) — share one guard implementation.
//
// The guards let tests construct bare &ChatSession{} without a
// fully-wired bus; production code always passes a session built
// via New, which wires the bus eagerly. Direct callers that know
// cs is non-nil and the bus is wired can call cs.EmitMessageState
// directly and skip this helper — it exists for the framework
// dispatcher use case where every callsite would otherwise
// duplicate the same three-guard block.
func PublishMessageState(cs *ChatSession, userMsgID string, state agent.MessageState) {
	if cs == nil || userMsgID == "" || cs.MessageStateBus == nil {
		return
	}
	cs.EmitMessageState(userMsgID, state)
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

// AttachAgentSessionForTest inserts as into cs.pool WITHOUT
// going through Spawn / Spawner (no bus subscription, no
// emit-buffer, no readpump). Used by tests outside this
// package that need to seed AgentSessions for code paths
// that read cs.pool — currently
// internal/command/gtw/close_test.go's step-0.5 / step-5.5
// / happy-path-with-orphaned-AS assertions.
//
// Naming follows the SetHandleForTest / SetStatusForTest
// convention on AgentSession: only test code should call
// this. Caller is expected to wire Handle via SetHandleForTest
// before any code path that drives as.Close() executes.
//
// Idempotent on the same (Agent, Cwd) key — overwrites the
// prior entry. Fine for tests; a leaked AS from a previous
// test case is re-stamped, not duplicated.
func (cs *ChatSession) AttachAgentSessionForTest(as *AgentSession) {
	if as == nil {
		return
	}
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
	asPool := cs.asPool
	chatID := cs.ChatID
	cs.mu.Unlock()
	if asPool != nil {
		asPool.Put(chatID, as)
	}
}

// SelectedAgentSessionForTest installs as as the chat's
// selectedAS WITHOUT going through selectAgentSessionLocked (no
// Activate call, no bus subscription). Used by runtime-package
// tests that need to exercise the "subscriber reads
// cs.SelectedAgentSession()" contract but can't reach into
// cs.mu / selectedAS directly (cross-package access). Pool
// membership is independent — if the test also needs the AS in
// the pool, call AttachAgentSessionForTest first.
//
// Passing nil is a no-op (selectedAS stays whatever it was).
// Production code MUST use LookupSelectedAgentSession or
// selectAgentSessionLocked — this helper exists for tests only.
func (cs *ChatSession) SelectedAgentSessionForTest(as *AgentSession) {
	if as == nil {
		return
	}
	cs.mu.Lock()
	cs.selectedAS = as
	cs.mu.Unlock()
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
	// Wire the per-AS persist callback so Submit and endPrompt can
	// flush their own state transitions (specifically
	// InFlightMessages) without depending on the caller to call
	// asFile.Upsert at the right moments. nil asFile (chat
	// constructed without persistence) leaves persist nil and the
	// in-memory mirror still works.
	if cs.asFile != nil {
		asFile := cs.asFile
		as.SetPersist(func(e *registry.AgentSessionEntry) error {
			return asFile.Upsert(e)
		})
	}
	// Wire the spawner so Submit's liveness backstop can
	// rebuild the bridge via spawner.Spawn when the bridge's
	// Keepalive detects a dead state. Without this, the
	// recovery callback in Submit has nothing to call.
	as.SetSpawner(cs.spawner)
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

// detachActiveWorkingSetLocked removes every actively mounted
// AgentSession while preserving the global warm pool and subscriptions.
// Caller must hold cs.mu for writing.
func (cs *ChatSession) detachActiveWorkingSetLocked() []*AgentSession {
	detached := make([]*AgentSession, 0, len(cs.pool))
	for _, as := range cs.pool {
		detached = append(detached, as)
	}
	cs.pool = make(map[agentCwdKey]*AgentSession)
	cs.selectedAS = nil
	return detached
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
// (called only by /close or CS shutdown).
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
//     or Exited after CLI died) → spawn (selectedAgent, selectedCwd)
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

	selectedCwd := cs.selectedCwd
	selectedAgent := cs.selectedAgent
	poolKey := agentCwdKey{Agent: selectedAgent, Cwd: selectedCwd}
	if as, ok := cs.pool[poolKey]; ok && as.Status() == StatusRunning && as.Handle() != nil {
		cs.selectAgentSessionLocked(as)
		cs.mu.Unlock()
		cs.attachAgentSubscription(as)
		return as, nil
	}

	// Snapshot dependencies, then do global-pool and disk I/O without
	// holding cs.mu. A non-running local entry is retained as a
	// compatibility fallback for tests that do not inject asPool.
	localPrior := cs.pool[poolKey]
	asPool := cs.asPool
	asFile := cs.asFile
	spawner := cs.spawner
	cs.mu.Unlock()

	// Serialize cold resolve+spawn for this key (docs/CHATSTORE.md
	// review: concurrent miss must not create divergent AS objects).
	unlockResolve := asPool.lockResolve(cs.ChatID, selectedCwd, selectedAgent)
	defer unlockResolve()

	as := asPool.Get(cs.ChatID, selectedCwd, selectedAgent)
	hadPrior := as != nil
	if as == nil && localPrior != nil {
		as = asPool.GetOrPut(cs.ChatID, localPrior)
		hadPrior = true
	}

	if as == nil && asFile != nil {
		wantCwd := filepath.Clean(selectedCwd)
		for _, entry := range asFile.List() {
			if entry.ChatSessionID == cs.ID &&
				filepath.Clean(entry.Cwd) == wantCwd &&
				entry.Agent == selectedAgent {
				candidate := FromAgentSessionEntry(entry)
				as = asPool.GetOrPut(cs.ChatID, candidate)
				hadPrior = true
				break
			}
		}
	}

	if as == nil {
		candidate := NewAgentSession(
			newAgentSessionID(),
			cs.ID,
			selectedAgent,
			selectedCwd,
			nil,
		)
		as = asPool.GetOrPut(cs.ChatID, candidate)
		if as == candidate && asFile != nil {
			_ = asFile.Upsert(as.Entry())
		}
	}

	cs.mu.Lock()
	// A concurrent /cwd or /use changed the target while disk lookup
	// was in progress. Leave the resolved AS warm and retry the new key.
	if cs.selectedCwd != selectedCwd || cs.selectedAgent != selectedAgent {
		cs.mu.Unlock()
		return cs.LookupSelectedAgentSession()
	}
	// Another concurrent lookup may already have mounted a live entry.
	if mounted := cs.pool[poolKey]; mounted != nil &&
		mounted.Status() == StatusRunning && mounted.Handle() != nil {
		cs.selectAgentSessionLocked(mounted)
		cs.mu.Unlock()
		cs.attachAgentSubscription(mounted)
		return mounted, nil
	}
	cs.attachAgentSessionLocked(as)
	cs.selectAgentSessionLocked(as)
	cs.mu.Unlock()
	cs.attachAgentSubscription(as)

	if as.Status() == StatusRunning && as.Handle() != nil {
		return as, nil
	}
	if hadPrior {
		as.ClearInFlight()
	}

	var spawnErr error
	if spawner != nil {
		spawnErr = as.Spawn(context.Background(), spawner)

		// fix-stop (2026-08-15): when the bridge rejected the
		// saved sessionID with agent.ErrResumeUnhealthy, the
		// AgentSession.respawn path has already cleared the
		// sessionID inside its own error branch. The next
		// Spawn (without a resume id) should land on a fresh
		// session — retry once before surfacing the error to
		// the dispatcher. Without this, the user would see
		// "Failed to spawn agent" on every inbound message
		// after /close+--resume-rejection until they ran
		// `/new` or hand-edited agent_sessions.json.
		//
		// Limit to ONE retry: the second attempt is a fresh
		// spawn (no resume) so it can only fail with a
		// different class of error (binary missing, handshake
		// refused, etc.) — not the resume-stale-id loop. If
		// the retry fails, surface the most recent error so
		// the dispatcher can render it.
		if spawnErr != nil && errors.Is(spawnErr, agent.ErrResumeUnhealthy) {
			slog.Warn("chatsession: spawn retry without resume id after ErrResumeUnhealthy",
				"chat_id", cs.ChatID, "as_id", as.ID, "agent", selectedAgent)
			spawnErr = as.Spawn(context.Background(), spawner)
		}
	}

	if spawner != nil {
		if spawnErr != nil {
			return as, fmt.Errorf("chatsession: spawn failed (selectedAgent=%q, cwd=%q): %w", selectedAgent, selectedCwd, spawnErr)
		}
		if asFile != nil {
			_ = asFile.Upsert(as.Entry())
		}
	}

	return as, nil
}

// AgentSessionsInCwd returns a fresh slice of every AgentSession in
// the pool whose Cwd matches cwd after filepath.Clean on both
// sides — keeps the comparison behaviour consistent with
// EvictAgentSessionsInCwd so a caller that switches between the
// two helpers gets the same answer for the same AS. The returned
// slice is independent of cs.pool (callers may mutate / range
// without disturbing subsequent reads). Returns nil when no
// entries match — including when cwd is empty (no selection yet).
func (cs *ChatSession) AgentSessionsInCwd(cwd string) []*AgentSession {
	if cwd == "" {
		return nil
	}
	want := filepath.Clean(cwd)
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]*AgentSession, 0)
	for _, as := range cs.pool {
		if filepath.Clean(as.Cwd) == want {
			out = append(out, as)
		}
	}
	return out
}

// AgentSessionsForCwd returns AS for cwd from the global asPool
// when wired (includes warm), else falls back to cs.pool. Used by
// /close so parked warm sessions for the selected cwd are still
// terminated (docs/CHATSTORE.md §7.4).
func (cs *ChatSession) AgentSessionsForCwd(cwd string) []*AgentSession {
	if cwd == "" {
		return nil
	}
	cs.mu.RLock()
	asPool := cs.asPool
	chatID := cs.ChatID
	cs.mu.RUnlock()
	if asPool != nil {
		return asPool.ListByChatCwd(chatID, cwd)
	}
	return cs.AgentSessionsInCwd(cwd)
}

// DropAgentSession is the per-entry cleanup primitive used by
// callers that want to fully discard a session entry. NOT used
// by /close itself (which preserves the AgentSession so respawn
// can continue via --resume <sessionID>). Reach this primitive
// when:
//
//   - Daemon shutdown reaps a stale Detached entry.
//   - A future /forget slash command wants to drop both the
//     process AND the persisted session identity.
//   - Manual cleanup scripts (e.g. editing agent_sessions.json
//     outside the daemon).
//
// Composes four steps:
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
// cs.mu once. Callers that wish to terminate the bridge process
// first should invoke as.Close() before calling this; DropAgentSession
// itself does NOT signal the bridge (the two-step pattern keeps
// graceful-shutdown timing in the caller's hands).
func (cs *ChatSession) DropAgentSession(as *AgentSession) {
	if as == nil {
		return
	}
	cs.mu.Lock()
	cs.detachAgentSubscriptionLocked(as)
	cs.detachAgentSession(as)
	if cs.selectedAS == as {
		cs.selectedAS = nil
	}
	asPool := cs.asPool
	asFile := cs.asFile
	chatID := cs.ChatID
	cs.mu.Unlock()

	asPool.Delete(chatID, as.Cwd, as.Agent)
	if asFile != nil {
		_ = asFile.Delete(as.ID)
	}
}

// DropResult describes the outcome of closing+dropping one
// AgentSession from a pool. Returned per-entry by
// EvictAgentSessionsInCwd so callers can surface a
// per-agent status if they want (the /gtw close path chooses
// to log only, not emit to the user).
type DropResult struct {
	Agent string
	Cwd   string
}

// evictGraceTotal bounds the close phase of
// EvictAgentSessionsInCwd. Wedged bridges that bypass
// their own SIGKILL fallback would otherwise pin the
// goroutine indefinitely. 5s mirrors the bound used by
// command/close's CloseAllAgents (close.go:238). Kept local
// rather than exported to avoid cross-package coupling.
const evictGraceTotal = 5 * time.Second

// evictStragglerBound bounds the drain phase after
// the close phase has timed out — bridges may unblock late;
// we keep draining for at most this long before declaring the
// remaining pool entries dropped-anyway (the goroutine will
// exit on its own later).
const evictStragglerBound = 2 * time.Second

// evictOutcome is the per-AS close result produced by
// EvictAgentSessionsInCwd's close goroutines. err is the error
// from as.Close() (nil on clean shutdown).
type evictOutcome struct {
	as  *AgentSession
	err error
}

// drainCloseOutcomes reads up to `alive` outcomes from closeCh
// and returns them in channel-receive order. Used by
// EvictAgentSessionsInCwd as the post-timeout drain phase.
//
// Invariants (the helper does not relax these — callers depend
// on them):
//
//   - alive >= 0. When alive == 0 the helper short-circuits to
//     nil without touching closeCh (no bridges were spawned).
//   - If timedOut == false, len(returned) == alive on return.
//     Every bridge reported back before the outer timeout; the
//     caller knows the pool entries line up with outcomes.
//   - If timedOut == true, len(returned) ∈ [0, alive]. Late
//     bridges may still be on the channel after the helper
//     returns; the caller must sweep the snapshot via
//     DropAgentSession to keep the pool consistent.
//   - The straggler timer fires exactly ONCE (NewTimer + defer
//     Stop), so evictStragglerBound is total budget for the
//     whole drain. `time.After` inside a select would reset the
//     budget on every receive — that was the pre-fix bug.
func drainCloseOutcomes(closeCh <-chan evictOutcome, alive int, timedOut bool) []evictOutcome {
	if alive <= 0 {
		return nil
	}
	if !timedOut {
		// Fast path: every bridge reported back before the
		// outer timeout. Drain the channel without a timer.
		results := make([]evictOutcome, 0, alive)
		for drained := 0; drained < alive; drained++ {
			results = append(results, <-closeCh)
		}
		return results
	}

	// Slow path: outer timeout fired. Bridges may unblock
	// late; give them evictStragglerBound total before
	// declaring the rest dropped-anyway. Single-shot timer
	// is critical (see invariants above).
	straggler := time.NewTimer(evictStragglerBound)
	defer straggler.Stop()
	results := make([]evictOutcome, 0, alive)
	for drained := 0; drained < alive; {
		select {
		case oc := <-closeCh:
			results = append(results, oc)
			drained++
		case <-straggler.C:
			return results
		}
	}
	return results
}

// EvictAgentSessionsInCwd tears down every
// AgentSession in the chat that is pinned to cwd: closes the
// bridge process (graceful, bounded by evictGraceTotal)
// and removes the entry from the pool and asFile. Returns
// the total number of evicted sessions (alive + stale; the
// "did anything happen" answer the user-facing path cares
// about) and a slice of per-alive-session outcomes for
// callers that want richer status.
//
// Distinct from /close's CloseAllAgents
// (internal/command/close/close.go:190): /close keeps the
// pool intact so respawn can resume each conversation after
// the user reopens it. This helper is for the
// worktree-is-gone case where there is nothing to respawn
// into — the AS is orphaned at birth.
//
// Wedged bridges are bounded by evictGraceTotal;
// stragglers are logged and dropped anyway. After the bound
// elapses the helper still proceeds to DropAgentSession for
// every entry in the snapshot — a wedged goroutine is briefly
// leaked (it will exit on its own once the bridge unwedges),
// but the pool entry is removed so the chat's view of the
// world is consistent. The returned count includes wedged
// ASes; the returned results slice only contains the ones
// whose close goroutine responded before the straggler
// timeout.
//
// Filtering by cwd keeps the operation scoped to a known
// workspace; the helper does not consult cs.selectedCwd.
func (cs *ChatSession) EvictAgentSessionsInCwd(cwd string) (int, []DropResult) {
	if cwd == "" {
		return 0, nil
	}

	cs.mu.RLock()
	asPool := cs.asPool
	chatID := cs.ChatID
	cs.mu.RUnlock()
	var snapshot []*AgentSession
	if asPool != nil {
		snapshot = asPool.ListByChatCwd(chatID, cwd)
	} else {
		snapshot = cs.AgentSessionsInCwd(cwd)
	}
	if len(snapshot) == 0 {
		return 0, nil
	}

	closeCh := make(chan evictOutcome, len(snapshot))
	var wg sync.WaitGroup
	alive := 0
	for _, as := range snapshot {
		st := as.Status()
		handle := as.Handle()
		isAlive := st == StatusRunning || (st == StatusDetached && handle != nil)
		if !isAlive {
			// Stale entry — no live process to signal. Drop
			// directly so the pool doesn't carry dead refs.
			cs.DropAgentSession(as)
			continue
		}
		alive++
		wg.Add(1)
		go func(as *AgentSession) {
			defer wg.Done()
			closeCh <- evictOutcome{as: as, err: as.Close()}
		}(as)
	}

	// Phase 1 — outer timeout on the close phase. If every
	// bridge reports back before evictGraceTotal we set
	// timedOut = false and drainCloseOutcomes returns exactly
	// `alive` outcomes. Otherwise the slow path adds the
	// straggler bound.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timedOut := false
	select {
	case <-done:
	case <-time.After(evictGraceTotal):
		timedOut = true
		slog.Warn("chatsession: agent close timed out",
			"cwd", cwd, "alive", alive)
	}

	outcomes := drainCloseOutcomes(closeCh, alive, timedOut)

	// Drop pool entries for every received outcome AND for
	// any snapshot entry whose bridge wedged without reporting
	// back. DropAgentSession is idempotent (delete-by-key +
	// nil-out selectedAS) so a second drop on an already-drained
	// entry is a no-op.
	for _, oc := range outcomes {
		cs.DropAgentSession(oc.as)
	}
	if timedOut && len(outcomes) < alive {
		slog.Warn("chatsession: straggler close",
			"cwd", cwd,
			"drained", len(outcomes),
			"alive", alive)
		for _, as := range snapshot {
			cs.DropAgentSession(as)
		}
	}

	results := make([]DropResult, 0, len(outcomes))
	for _, oc := range outcomes {
		results = append(results, DropResult{
			Agent: oc.as.Agent,
			Cwd:   oc.as.Cwd,
		})
	}
	// The user-facing count includes wedged ASes — they
	// were pinned to the now-gone worktree and were dropped
	// from the pool even though their close goroutine never
	// reported back. Reporting "0 dropped" when 3 bridges
	// wedged and 3 pool entries were still removed would
	// be a misleading UX.
	return len(snapshot), results
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
//     This matches /close's queue-handling (F-34 §6 Q-N4).
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
	asPool := cs.asPool
	chatID := cs.ChatID
	cs.mu.RUnlock()

	// Snapshot targets from asPool (warm + active) without
	// permanently remounting warm ASes into cs.pool — /new must
	// reset SessionIDs but must not enlarge the active working
	// set / prober scan (docs/CHATSTORE.md).
	seen := make(map[string]struct{})
	targets := make([]*AgentSession, 0)
	if asPool != nil {
		for _, as := range asPool.ListByChatCwd(chatID, cwd) {
			if as == nil {
				continue
			}
			if agentName != "" && as.Agent != agentName {
				continue
			}
			targets = append(targets, as)
			seen[as.ID] = struct{}{}
		}
	}
	cs.mu.RLock()
	for _, as := range cs.pool {
		if as == nil || as.Cwd != cwd {
			continue
		}
		if agentName != "" && as.Agent != agentName {
			continue
		}
		if _, ok := seen[as.ID]; ok {
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
		var oldHandle *agent.Agent
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
	// HandlePTYRestart owns the full kill + respawn + readpump-reset
	// + sessionID-clear lifecycle. CS just provides the launcher.
	if err := as.HandlePTYRestart(ctx, cs.spawner); err != nil {
		return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
	}
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
	if cs != nil && cs.csStore != nil {
		if entry, ok := cs.csStore.Get(cs.ChatID); ok && entry != nil {
			return entry.LastInteractionAt
		}
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.lastInteractionAt
}

// entryLocked is the same as Entry but assumes cs.mu is held.
func (cs *ChatSession) entryLocked() *registry.ChatSessionEntry {
	return &registry.ChatSessionEntry{
		ID:                cs.ID,
		ChatID:            cs.ChatID,
		SelectedCwd:       cs.selectedCwd,
		SelectedAgent:     cs.selectedAgent,
		PrimaryAgent:      cs.primaryAgent,
		CreatedAt:         cs.createdAt,
		LastInteractionAt: cs.lastInteractionAt,
		WatchMode:         int(cs.watchMode),
		ThinkMode:         int(cs.thinkMode),
		ToolsMode:         int(cs.toolsMode),
		// F-watch: propagate the hint tombstone so any persist
		// path (SetWatchMode / SetThinkMode / SetToolsMode / SetSelectedCwd
		// / ClearSelectedCwd / QueueUserMessage's lastInteractionAt
		// bump) preserves it instead of clobbering back to false.
		WatcherHintEmitted: cs.watcherHintEmitted,
	}
}

// persistChatEntry writes the ChatSessionEntry to disk (if persistence
// is configured). Best-effort: errors are returned but not propagated
// through call sites (logged at higher level).
func (cs *ChatSession) persistChatEntry() {
}

// persistChatEntryLocked writes ChatSessionEntry. Caller must hold
// cs.mu (RLock or Lock).
func (cs *ChatSession) persistChatEntryLocked() {
}

// newAgentSessionID returns a unique ID for an AgentSession. v1.2
// commit 6 uses a simple counter-based scheme for testability;
// commit 7 may swap to UUID v7.
var agentSessionCounter atomic.Uint64

func newAgentSessionID() string {
	n := agentSessionCounter.Add(1)
	return fmt.Sprintf("as_%d_%d", time.Now().UnixNano(), n)
}
