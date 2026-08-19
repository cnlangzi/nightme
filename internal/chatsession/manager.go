// Package chatsession — Manager (commit 8a).
//
// Manager is the v1.2 equivalent of v1.1's session.MemoryManager: it
// owns the per-chat ChatSession table and exposes lifecycle
// operations needed by the Gateway handlers (/cwd, /use, /close).
//
// Key differences from v1.1 MemoryManager:
//
//   - Bound to ChatSession (not bare Session); one ChatSession per
//     chat_id, with an AgentSession pool inside.
//   - /cwd no longer spawns; /use is lazy (reuse or spawn); /close
//     clears the pool without removing the ChatSession.
//   - Manager doesn't fork directly; it uses a Spawner (see
//     spawn.go) to keep agent/bridge imports out of chatsession.
package chatsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/registry"
)

// Manager owns the per-chat ChatSession table. Safe for concurrent
// use; the chat-id key space is the natural concurrency boundary.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*ChatSession

	// spawner is used by LookupSelectedAgentSession on every chat
	// for new AgentSessions. Shared across all chats (production
	// wires a registrySpawner here).
	spawner Spawner

	// persistence (both optional, nil means in-memory only)
	csFile *registry.ChatSessionFile
	asFile *registry.AgentSessionFile

	// onCreate fires once for every newly-created ChatSession,
	// before GetOrCreate returns. Used by the runtime to wire
	// per-ChatSession handlers (e.g. MessageStateBus in
	// F-31) without requiring the runtime to enumerate sessions
	// after startup. nil = no callback.
	onCreate func(*ChatSession)

	// primaryAgent is the agent name HandleInbound / GetOrCreate
	// uses for new ChatSessions. Set via WithPrimaryAgent.
	primaryAgent string

	// emitter (set via WithEmitter) is the single daemon-wide
	// outbound chokepoint that every newly-created ChatSession is
	// bound to. nil means the runtime has not wired an Emitter
	// yet — GetOrCreate tolerates nil and binds whatever emitter
	// is set; tests that don't exercise the outbound path may
	// leave it nil. Replaces the per-channel-resolver model: in
	// v1 production the runtime owns one Emitter and the chat
	// layer no longer cares about per-chat Channel selection.
	//
	// Read under m.mu (RLock); written under m.mu (Lock) in
	// WithEmitter. See GetOrCreate Phase 2 for the read site.
	emitter outbound.Emitter

	// (no Manager-level gitStatusDeps — gitStatusDeps is
	// per-ChatSession (cs.gitStatusDeps), wired by
	// WithGitStatusDeps on every existing AND future chat.
	// The Emitter's GitStatusLookup reaches the chat via mgr.Get
	// at message time and calls cs.GitStatus(ctx), which
	// rebuilds the snapshot on every call — no per-chat cache
	// layer.)

	// hintLocks holds a *sync.Mutex per chat, lazily created on
	// first hint attempt for that chat. maybeEmitWatcherHint
	// takes the per-chat lock to serialise the "check → send →
	// mark" sequence so two concurrent non-mention drops in the
	// SAME chat can't race past the tombstone check and produce
	// duplicate hints. Per-chat (not Manager-wide) so a slow
	// Emitter.Send or csFile.Upsert for one chat doesn't block
	// hint attempts for every other chat — under load (Feishu
	// outage, slow disk) this matters because the drop branch is
	// on the hot inbound dispatch path.
	//
	// Memory: each entry is one *sync.Mutex + sync.Map overhead
	// (~80 bytes). Even with 10k chats this is <1MB, and the
	// lock is acquired at most once per chat per process lifetime
	// (subsequent attempts short-circuit on the tombstone
	// without locking). We do NOT clean up entries on tombstone
	// set — freeing the *sync.Mutex while another goroutine is
	// blocked on it would race. The unbounded growth is the
	// lesser evil.
	hintLocks sync.Map // map[chatID]*sync.Mutex
}

// NewManager creates an empty Manager. Both spawner and persistence
// can be wired later via WithSpawner / WithPersistence.
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string]*ChatSession),
	}
}

// WithSpawner wires the Spawner (factory pattern; same Spawner may
// be shared across many Managers).
func (m *Manager) WithSpawner(s Spawner) *Manager {
	m.mu.Lock()
	m.spawner = s
	m.mu.Unlock()
	return m
}

// WithPersistence attaches registry stores (also shared-able).
func (m *Manager) WithPersistence(csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile) *Manager {
	m.mu.Lock()
	m.csFile = csFile
	m.asFile = asFile
	m.mu.Unlock()
	return m
}

// WithOnCreate registers a callback fired on every newly-created
// ChatSession before GetOrCreate returns. Restored sessions
// (RestoreFromRegistry) also fire this callback so the runtime
// can wire per-ChatSession handlers uniformly.
//
// Chaining: if a previous onCreate hook was registered (e.g. by
// WithGitStatusDeps) it is invoked first, so deps propagation
// survives later hook registrations. Without this chaining, a
// call sequence of WithGitStatusDeps + WithOnCreate would silently
// drop the deps hook — restored chats would have no
// gitStatusDeps wired and per-chat GitStatus calls would hit
// the unconfigured-deps early return on every stamp.
func (m *Manager) WithOnCreate(fn func(*ChatSession)) *Manager {
	m.mu.Lock()
	prev := m.onCreate
	m.onCreate = func(cs *ChatSession) {
		if prev != nil {
			prev(cs)
		}
		fn(cs)
	}
	m.mu.Unlock()
	return m
}

// WithGitStatusDeps wires the deps every chatsession uses to
// build its GitStatus on every pull-on-read call (CollectGit
// + LookupPR). Called once at startup. Returns self for
// chaining.
//
// Propagates to every existing chat AND registers an OnCreate
// hook for future chats created via GetOrCreate. This way
// every chat always sees non-nil deps — no "no deps wired
// yet" silent-skip failure mode.
func (m *Manager) WithGitStatusDeps(deps GitStatusDeps) *Manager {
	m.mu.Lock()
	// Propagate to every existing chat so per-chat GitStatus
	// calls (from the Emitter's GitStatusLookup on every
	// outbound stamp) have the deps they need.
	for _, cs := range m.sessions {
		if cs != nil {
			cs.WithGitStatusDeps(deps)
		}
	}
	// Install an OnCreate hook so future chats created via
	// GetOrCreate receive the same deps.
	prevOnCreate := m.onCreate
	m.onCreate = func(cs *ChatSession) {
		cs.WithGitStatusDeps(deps)
		if prevOnCreate != nil {
			prevOnCreate(cs)
		}
	}
	m.mu.Unlock()
	return m
}

// GetOrCreate returns the ChatSession for chatID, creating it if
// missing. The chatType parameter was removed in F-33 (D1); nightme
// no longer carries chat-type at the binding layer. primaryAgent
// is the cfg.Primary snapshot from config; ChatSession.primaryAgent
// is captured here and never mutated post-creation (Q-A: no
// /default command, no per-chat override). It also seeds
// selectedAgent so the runtime always has an effective agent to
// dispatch to.
//
// Errors:
//   - New(...) itself returns error → propagated as-is.
//
// Concurrency: double-checked locking. Phase 1 (fast path) takes
// m.mu; Phase 2 (construct outside the lock) takes m.mu in
// RLock mode to snapshot the manager's dependencies (spawner /
// persistence / emitter); Phase 3 (insert) takes m.mu again and
// re-checks for a concurrent winner. The Phase-2 callbacks
// (Spawner, persistence) can do arbitrary work without
// re-entering m.mu. The constructor never holds a manager lock
// across external code.
func (m *Manager) GetOrCreate(chatID, primaryAgent string) (*ChatSession, error) {
	// Phase 1: take a single RLock and snapshot the table +
	// every dependency we need. The previous split (Phase 1 read
	// sessions, release, Phase 2 re-acquire for dependencies)
	// paid for two RLock round-trips on the miss path; one
	// acquisition is sufficient because the reads are mutually
	// consistent with the snapshot of `sessions` we made under
	// the same lock.
	m.mu.RLock()
	cs, ok := m.sessions[chatID]
	if !ok {
		// Phase 2: read the deps we'll need for the new
		// ChatSession. Construction runs OUTSIDE the lock so
		// external callbacks (Spawner, persistence) don't have
		// to re-enter the manager mutex. The deps snapshot is
		// taken under the same RLock as the sessions read so the
		// miss-confirmed-in-Phase-3 invariant still holds.
		// Concurrency: concurrent GetOrCreate calls for distinct
		// chatIDs still run in parallel (Phase 1 was Lock in
		// Commit 8; the lock was functionally safe but serialised
		// reads unnecessarily).
		//
		// A nil emitter is permitted — tests that only care about
		// ChatSession's internal state (InputBuffer FSM,
		// persistence, status transitions) don't need an outbound
		// surface. The production runtime (cmd/nightme) always
		// wires an Emitter before opening chats, so the production
		// path is unaffected. cs.Emitter() returning nil is the
		// contract that downstream callers must already nil-check
		// (see cs.Emitter doc).
		var (
			spawner Spawner
			csFile  *registry.ChatSessionFile
			asFile  *registry.AgentSessionFile
			emitter outbound.Emitter
		)
		spawner = m.spawner
		csFile = m.csFile
		asFile = m.asFile
		emitter = m.emitter
		m.mu.RUnlock()

		// v1.3+ multi-channel lazy restore: before allocating a
		// fresh ChatSession, check csFile (in-memory index) for
		// a persisted entry matching chatID. If present, hydrate
		// the ChatSession from that entry — restore its per-chat
		// state (selectedCwd/Agent, WatchMode/ThinkMode/ToolsMode,
		// lastInteractionAt) and the AgentSession pool as Detached.
		// csFile is the same file the eager Manager.RestoreFromRegistry
		// used to read on startup, but the work is deferred to
		// first-use per-chat now, eliminating the startup-time I/O
		// and the cross-channel partition problem (chatID is namespaced
		// per channel; the entry is created in the Manager whose
		// channel produced the first inbound after restart).
		cs, err := m.constructChatSession(chatID, primaryAgent, spawner, csFile, asFile, emitter)
		if err != nil {
			return nil, err
		}

		// Phase 3: re-lock for the insert + onCreate publish.
		m.mu.Lock()
		defer m.mu.Unlock()
		if existing, ok := m.sessions[chatID]; ok {
			// Another goroutine won the race; discard our construction.
			// The discarded cs (the `cs` from Phase 2 above) is
			// never visible to anyone — we never ran onCreate, never
			// registered it, never started any background goroutine
			// that holds it.
			//
			// NOTE on cleanup: hydrateFromEntry DOES populate the
			// pool, and attachAgentSubscription subscribes a closure
			// to each AS's EventBus that captures `cs`. The closure
			// keeps the discarded cs alive until the AS bus is
			// closed — which never happens for a never-spawned
			// Detached AS. This is a small, bounded memory leak per
			// race occurrence (one orphan cs + its Detached pool,
			// only visible if the chatID is hit concurrently across
			// goroutines on first use after restart). A proper fix
			// requires a ChatSession.Close that closes the buses and
			// cancels ctx; out of scope here.
			return existing, nil
		}
		m.sessions[chatID] = cs

		// Fire onCreate callback before releasing the lock so the
		// callback's own locks see consistent state.
		if m.onCreate != nil {
			m.onCreate(cs)
		}
		return cs, nil
	}
	m.mu.RUnlock()
	return cs, nil // existing cs → emitter already bound
}

// constructChatSession builds a ChatSession for chatID, either by
// hydrating from a persisted csFile entry (if present) or by
// allocating a fresh one. Runs OUTSIDE m.mu (caller is responsible
// for the lock).
//
// Hydration path:
//   - csFile.GetByChat(chatID) returns the entry → call hydrateFromEntry
//   - otherwise → New(chatID, primaryAgent) and wire deps
//
// The hydration path also seeds the AgentSession pool from asFile
// (filtered by the entry's chatSessionId). FromAgentSessionEntry
// already demotes any StatusRunning to StatusDetached, so
// LookupSelectedAgentSession will re-spawn on the next call.
func (m *Manager) constructChatSession(chatID, primaryAgent string, spawner Spawner, csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile, emitter outbound.Emitter) (*ChatSession, error) {
	if csFile != nil {
		if entry, ok := csFile.GetByChat(chatID); ok {
			return m.hydrateFromEntry(entry, spawner, csFile, asFile, emitter)
		}
	}
	cs, err := New(chatID, primaryAgent)
	if err != nil {
		return nil, err
	}
	cs.WithSpawner(spawner).
		WithPersistence(csFile, asFile)
	if emitter != nil {
		cs.WithEmitter(emitter)
	}
	return cs, nil
}

// hydrateFromEntry rebuilds a ChatSession from a persisted entry.
// Per-chat state (selectedCwd/Agent, WatchMode/ThinkMode/ToolsMode,
// watcherHintEmitted, lastInteractionAt) is restored; the
// AgentSession pool is seeded from asFile (filtered by entry.ID).
// selectedAS is forced to nil because the in-memory process handle
// is lost on restart (the next LookupSelectedAgentSession will
// spawn fresh and re-populate selectedAS).
func (m *Manager) hydrateFromEntry(entry *registry.ChatSessionEntry, spawner Spawner, csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile, emitter outbound.Emitter) (*ChatSession, error) {
	cs, err := New(entry.ChatID, entry.PrimaryAgent)
	if err != nil {
		return nil, err
	}
	cs.WithSpawner(spawner).
		WithPersistence(csFile, asFile)
	if emitter != nil {
		cs.WithEmitter(emitter)
	}
	cs.selectedCwd = entry.SelectedCwd
	cs.selectedAgent = entry.SelectedAgent
	// Registry persists bare int; ChatSession fields are typed
	// enums. Cast on read — Go zero-value semantics preserve the
	// safe default when the int is 0.
	cs.watchMode = WatchMode(entry.WatchMode) // 0 == WatchModeMention (default, safe)
	cs.thinkMode = ThinkMode(entry.ThinkMode) // 0 == ThinkModeShow (default; preserve F-thread-route behavior)
	cs.toolsMode = ToolsMode(entry.ToolsMode) // 0 == ToolsModeHide (default; quiet by default)
	cs.watcherHintEmitted = entry.WatcherHintEmitted
	cs.lastInteractionAt = entry.LastInteractionAt
	// commit fix-6: clear selectedAS on restore. The persisted
	// selectedAgentSessionId points at an AgentSession whose
	// handle is in-memory only (lost on restart). Leaving the
	// pointer set would cause SendBlocks (called by the default
	// FlushHook) to return ErrNotRunning and silently drop user
	// messages. The next LookupSelectedAgentSession will spawn
	// fresh and re-populate selectedAS.
	cs.selectedAS = nil
	// Seed the pool from the agent_sessions.json entries that
	// belong to this ChatSession. FromAgentSessionEntry has
	// already demoted any StatusRunning to StatusDetached, so
	// LookupSelectedAgentSession will re-spawn on the next call.
	if asFile != nil {
		for _, aEntry := range asFile.List() {
			if aEntry.ChatSessionID == entry.ID {
				cs.attachAgentSession(FromAgentSessionEntry(aEntry))
			}
		}
	}
	// F-62 §3.3.1: in-flight messages are NOT pushed back into
	// cs.queue at restore (see Manager.RestoreFromRegistry for the
	// full rationale — the queue is chat-level and would bleed
	// the previous (agent, cwd)'s hung messages into the new
	// TryFlush's batch).
	return cs, nil
}

// WithEmitter binds the single daemon-wide outbound chokepoint
// to the Manager. Every ChatSession created (or restored) after
// this call is bound to the same Emitter via
// ChatSession.WithEmitter. Wired once at startup before any
// chat is opened; calling it again with a non-nil Emitter
// replaces the previous one.
//
// nil clears the binding (test teardown). A nil Emitter is
// tolerated by GetOrCreate: tests that don't exercise the
// outbound path can construct ChatSessions without one
// (cs.Emitter() returns nil; senders must nil-check before
// Send).
//
// Concurrency: the write is published through m.mu so
// concurrent GetOrCreate calls see a consistent value (RLock
// acquisition in Phase 2).
//
// Note: ChatSession.WithEmitter panics if a different non-nil
// Emitter is bound to an existing session. RestoreFromRegistry
// therefore always binds the same Emitter the Manager holds
// (or none, if Manager.emitter is nil). A daemon restart that
// constructs a new Emitter instance per process is safe.
// WithPrimaryAgent records the agent name new ChatSessions
// should be bound to. Used by HandleInbound and any other
// path that lazily creates a ChatSession from a chatID alone.
func (m *Manager) WithPrimaryAgent(name string) *Manager {
	m.mu.Lock()
	m.primaryAgent = name
	m.mu.Unlock()
	return m
}

func (m *Manager) WithEmitter(em outbound.Emitter) *Manager {
	m.mu.Lock()
	m.emitter = em
	m.mu.Unlock()
	return m
}

// Emitter returns the wired outbound.Emitter, or nil if the
// runtime has not bound one yet. Used by HandleInbound's
// error-reply path and by runtime shims that need to send
// without going through a ChatSession.
func (m *Manager) Emitter() outbound.Emitter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.emitter
}

// Get returns the ChatSession for chatID, or nil if absent.
func (m *Manager) Get(chatID string) *ChatSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[chatID]
}

// SendPermission feeds a card-button label (or typed answer) to
// the selected AgentSession's pending approval / AskUserQuestion.
// Used by inbound.dispatchPermissionClick. Does not spawn or
// GetOrCreate — a click with no live session is a no-op error.
func (m *Manager) SendPermission(chatID, option string) error {
	if m == nil {
		return errors.New("chatsession: manager is nil")
	}
	cs := m.Get(chatID)
	if cs == nil {
		return fmt.Errorf("chatsession: no chat %s", chatID)
	}
	as := cs.SelectedAgentSession()
	if as == nil {
		return errors.New("chatsession: no selected agent")
	}
	h := as.Handle()
	if h == nil {
		return errors.New("chatsession: agent not spawned")
	}
	return h.SendPermission(option)
}

// AcceptInbound is the F-watch §3.1.1 per-chat gate, owned by
// chatsession (not gateway) so the policy sits next to its state.
// Returns true when the message should proceed to the dispatcher;
// false when it should be silently dropped.
//
// Decision matrix:
//
//	HasMention=true                           → accept (any mode)
//	HasMention=false + no ChatSession yet     → accept (let
//	                                            downstream reply
//	                                            "send /cwd first")
//	HasMention=false + WatchModeAll           → accept
//	HasMention=false + WatchModeMention (def) → drop
//
// The HasMention branch is the DM invariant: the channel adapter
// is contractually required to set HasMention=true for every DM
// message (every DM is implicitly "addressed to bot"), so DM
// chats never reach the WatchMode branch. See
// docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.11.
//
// Relocated from internal/gateway (was gateway.WithWatchModeResolver
// + applyWatchModeGate) so the gate stops needing a callback
// indirection across the import-cycle boundary.
func (m *Manager) AcceptInbound(chatID string, hasMention bool) bool {
	if hasMention {
		return true
	}
	cs := m.Get(chatID)
	if cs == nil {
		return true
	}
	return cs.WatchMode() == WatchModeAll
}

// HandleInbound is the F-58 default-branch entry point of the
// inboundDispatcher. It's called by inbound.tryMessageDispatch
// for every message that wasn't claimed by an action / command /
// shell handler. Owns the full chat-side pipeline:
//
//  1. WatchMode gate (AcceptInbound)
//  2. GetOrCreate(chatID) — lazily create ChatSession
//  3. Emit MessageQueued (FastAck UX: ⏳)
//  4. LookupSelectedAgentSession — lazy spawn if missing
//  5. Emit MessageSubmitted (⏳ → 🔄)
//  6. QueueUserMessage into the per-chat InputBuffer
//
// Error paths (no workspace / spawn failed / queue full) reply
// through the wired outbound.Emitter (m.emitter) so they
// inherit the SessionContext footer.
func (m *Manager) HandleInbound(ctx context.Context, msg *messages.InboundMessage) error {
	if msg == nil {
		return nil
	}
	// F-watch §3.1.1: drop early, before any GetOrCreate / spawn
	// work, so filtered messages don't allocate state or wake
	// pumps. Slash commands never reach this branch.
	if !m.AcceptInbound(msg.ChatID, msg.HasMention) {
		// F-watch first-drop hint: on the very first non-mention
		// group message we silently drop in this chat, tell the
		// user `/watch on` exists. AcceptInbound's drop arm only
		// returns false when cs already exists (its
		// cs==nil short-circuit returns true), so the Get here
		// always returns a non-nil ChatSession in production.
		// The hint tombstone now lives on the ChatSession (see
		// the field comment on chatsession.watcherHintEmitted)
		// — every persist path that goes through entryLocked()
		// preserves it, so SetWatchMode / SetSelectedCwd /
		// ClearSelectedCwd cannot clobber it back to false.
		// See maybeEmitWatcherHint for the full contract.
		cs := m.Get(msg.ChatID)
		m.maybeEmitWatcherHint(ctx, msg, cs)
		slog.Default().Info("chatsession: drop non-mention group message (WatchMode != All)",
			"chat_id", msg.ChatID, "message_id", msg.MessageID)
		return nil
	}
	userMsgID := msg.MessageID
	if userMsgID == "" {
		userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
	}

	cs, _ := m.GetOrCreate(msg.ChatID, m.primaryAgent)

	// fix-placehold-card: resolve AS BEFORE emitting MessageQueued
	// so the Feishu placeholder card can stamp AgentBar (Agent /
	// Cwd / SessionID) on its first render. The runtime eventbus
	// subscriber (see internal/runtime/eventbus.go::MessageStateBus
	// handler) reads cs.SelectedAgentSession() at publish time,
	// which is only set once this lookup completes — emitting
	// MessageQueued first (pre-fix FastAck order) meant the
	// subscriber saw a nil selectedAS and the placeholder card
	// shipped with no AgentBar until the first OutReply /
	// AppendEntryWithFooter overwrote footerLines.
	//
	// Trade-off: on cold-start (no pool hit, spawn takes a few
	// hundred ms) the ⏳ reaction now lands a beat later than
	// before. Pool-hit chats (the common case) see no latency
	// change — LookupSelectedAgentSession returns synchronously.
	// On error paths (no cwd / spawn failed) we deliberately do
	// NOT emit MessageQueued — the user gets an immediate error
	// reply and no orphan ⏳ reaction stays on the user message.
	_, err := cs.LookupSelectedAgentSession()
	if err != nil {
		if errors.Is(err, ErrNoSelectedCwd) {
			return m.sendError(ctx, msg.ChatID, "No workspace set. Send /cwd <path> first.")
		}
		// Spawn failed (binary missing, etc.).
		return m.sendError(ctx, msg.ChatID, fmt.Sprintf("Failed to spawn agent: %v", err))
	}

	// F-31 / F-53: ⏳ immediately (post-fix: with AgentBar, via
	// the subscriber reading cs.SelectedAgentSession()).
	cs.EmitMessageState(userMsgID, agent.MessageQueued)

	// F-31 / F-53: ⏳ → 🔄 before queueing so the transition is
	// visible even if queueing is slow.
	cs.EmitMessageState(userMsgID, agent.MessageSubmitted)

	// Build the per-message domain object. ReceivedAt is the
	// inbound timestamp (not dispatcher-pass time) so debug
	// surfaces see the true arrival time.
	//
	// F-54 / fix-stop bug: Feishu only pre-populates msg.Blocks
	// for text-only or rich-text messages carrying downloaded
	// attachments. The legacy cmd/nightme dispatcher fell back
	// to feishu.BuildBlocks(msg.Text, msg.Attachments) in the
	// empty-Blocks case, but Manager.HandleInbound (the actual
	// inbound path — the cmd/nightme dispatcher is dead code)
	// was missing the fallback, so every short text message was
	// queued with 0 blocks → the bridge's SendBlocks no-op
	// branch fired → the agent never saw the prompt.
	//
	// Inlined here to keep the chatsession↔feishu import cycle
	// closed. Behaviour matches feishu.BuildBlocks: empty-text
	// attachments build a ContentImage/ContentFile block iff
	// the attachment has a LocalPath (downloads succeeded);
	// attachments with empty LocalPath are silently dropped
	// (the channel side is responsible for emitting a
	// user-visible download-failure note).
	blocks := msg.Blocks
	if len(blocks) == 0 {
		if msg.Text != "" {
			blocks = append(blocks, agent.ContentBlock{Type: agent.ContentText, Text: msg.Text})
		}
		for _, a := range msg.Attachments {
			if a.LocalPath == "" {
				continue
			}
			switch a.Type {
			case "image":
				blocks = append(blocks, agent.ContentBlock{
					Type:      agent.ContentImage,
					Path:      a.LocalPath,
					MediaType: a.MimeType,
				})
			default:
				blocks = append(blocks, agent.ContentBlock{
					Type:      agent.ContentFile,
					Path:      a.LocalPath,
					MediaType: a.MimeType,
				})
			}
		}
	}
	userMsg := Message{
		ID:         userMsgID,
		ChatID:     msg.ChatID,
		Blocks:     blocks,
		ReceivedAt: msg.Time,
	}
	if err := cs.QueueUserMessage(userMsg); err != nil {
		if errors.Is(err, ErrQueueFull) {
			return m.sendError(ctx, msg.ChatID, "Input queue full — the agent is behind. Wait for it to catch up before sending more.")
		}
		return err
	}
	return nil
}

// sendError is a small helper that routes an error reply
// through the wired outbound.Emitter. Returns whatever the
// Emitter returns (typically nil; the Emitter logs Send
// failures internally). Falls back to a log line if no
// Emitter is wired (early-startup or test wiring).
func (m *Manager) sendError(ctx context.Context, chatID, text string) error {
	em := m.Emitter()
	if em == nil {
		slog.Default().Warn("chatsession: error reply with no Emitter wired",
			"chat_id", chatID, "text", text)
		return nil
	}
	return em.Send(ctx, messages.OutboundMessage{
		ChatID: chatID,
		Kind:   messages.OutReply,
		Text:   text,
	})
}

// hintLockFor returns the per-chat *sync.Mutex for hint
// serialization, creating it on first use. See hintLocks field
// comment for the rationale (per-chat, not Manager-wide, so a
// slow Send in chat A doesn't block chat B's hint attempts).
func (m *Manager) hintLockFor(chatID string) *sync.Mutex {
	if v, ok := m.hintLocks.Load(chatID); ok {
		return v.(*sync.Mutex)
	}
	fresh := &sync.Mutex{}
	actual, _ := m.hintLocks.LoadOrStore(chatID, fresh)
	return actual.(*sync.Mutex)
}

// maybeEmitWatcherHint is the F-watch first-drop UX. Fires from
// the drop branch of HandleInbound — i.e. exactly when WatchMode
// is WatchModeMention AND msg.HasMention is false (the channel
// adapter stamps HasMention=true on every DM, so DM chats never
// reach this path; slash commands never reach HandleInbound at
// all). On the first qualifying drop for a given chat, sends a
// one-line `/watch on` hint to the user via the wired Emitter
// and stamps the chat's WatcherHintEmitted tombstone so the
// hint never re-fires.
//
// AcceptInbound's drop arm only returns false when cs already
// exists (its cs==nil short-circuit returns true), so the
// caller-supplied cs is non-nil in production. cs may still be
// nil if AcceptInbound's logic changes in the future, OR under
// defensive direct invocation from tests — in that case we
// GetOrCreate so the hint and its tombstone can be tracked on
// a stable ChatSession (writing to ChatSessionFile directly
// races with every other persist path that goes through
// entryLocked()). This costs one ChatSession allocation per
// brand-new group chat — the only "drop early" exception — and
// subsequent drops in the same chat use the Get fast path.
//
// Concurrency: per-chat *sync.Mutex (hintLockFor) serialises
// the "check → send → mark" sequence so two concurrent
// non-mention drops in the SAME chat can't race past the
// tombstone check and produce duplicate hints. The lock is
// per-chat (not Manager-wide) so a slow Emitter.Send or
// csFile.Upsert for one chat does NOT block hint attempts in
// any other chat — see hintLocks field comment.
//
// Retry semantics: the tombstone is stamped ONLY if Emitter.Send
// returned nil. A transient Send failure (Feishu 5xx, network
// timeout) logs a warning and leaves the tombstone false, so
// the very next non-mention drop will retry. This is the
// critical F-watch correctness guarantee: under no circumstance
// does a Send failure permanently deny the user the hint. The
// trade-off is that a persistently broken channel can retry
// forever — acceptable because every retry also logs at Warn
// level so the operator sees the broken channel.
//
// Concurrency: hintMu serialises the check-and-set so two
// concurrent drop-branch messages in the same chat can't both
// pass the in-memory tombstone check and produce duplicate
// hints. hintMu is released before returning; it does NOT
// contend with the hot dispatch path.
//
// Best-effort: any failure (nil Emitter, persist error) is
// logged at Warn and swallowed. The drop decision is unaffected
// — the user simply doesn't get a hint this once.
func (m *Manager) maybeEmitWatcherHint(ctx context.Context, msg *messages.InboundMessage, cs *ChatSession) {
	if cs == nil {
		// Defensive: should not happen in production (see
		// doc comment), but GetOrCreate so we have a stable
		// home for the tombstone.
		newCS, err := m.GetOrCreate(msg.ChatID, m.primaryAgent)
		if err != nil || newCS == nil {
			slog.Default().Warn("chatsession: watcher hint: GetOrCreate failed",
				"chat_id", msg.ChatID, "err", err)
			return
		}
		cs = newCS
	}

	// Per-chat lock (NOT Manager-wide) so a slow Send for chat A
	// doesn't block hint attempts for chat B. See hintLocks
	// field comment.
	mu := m.hintLockFor(msg.ChatID)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the lock (a concurrent caller could have
	// just stamped the flag while we were waiting on the
	// per-chat mutex).
	if cs.WatcherHintEmitted() {
		return
	}

	// Send the hint via the wired Emitter (same pattern as
	// sendError). OutReply renders as a main-chat message in
	// the Feishu adapter; we deliberately do NOT ReplyTo the
	// dropped message — the dropped body is irrelevant to the
	// hint, and a thread reply would hide the hint from the
	// main chat where users look first.
	//
	// Send-failure contract: a transient Emitter.Send error
	// (Feishu 5xx, network timeout) leaves the tombstone FALSE
	// so the next non-mention drop retries. Stamping the
	// tombstone on Send failure would permanently deny the user
	// the hint — the entire point of F-watch is to surface it,
	// not to record an attempt that nobody saw.
	em := m.Emitter()
	if em == nil {
		slog.Default().Warn("chatsession: watcher hint with no Emitter wired; will retry on next drop",
			"chat_id", msg.ChatID)
		return
	}
	hint := "💡 I'm in /watch mention mode and only respond when @-mentioned. " +
		"Run /watch on if you'd like me to respond to every message in this chat."
	if err := em.Send(ctx, messages.OutboundMessage{
		ChatID: msg.ChatID,
		Kind:   messages.OutReply,
		Text:   hint,
	}); err != nil {
		slog.Default().Warn("chatsession: watcher hint Send failed; will retry on next drop",
			"chat_id", msg.ChatID, "err", err)
		return
	}

	// Send succeeded — stamp + persist so future drops
	// short-circuit. MarkWatcherHintEmitted goes through
	// entryLocked() so the flag is durable alongside every
	// other ChatSession field; subsequent SetWatchMode /
	// SetThinkMode / SetToolsMode / SetSelectedCwd etc. preserve
	// it instead of clobbering back to false.
	if err := cs.MarkWatcherHintEmitted(); err != nil {
		slog.Default().Warn("chatsession: mark watcher hint emitted failed",
			"chat_id", msg.ChatID, "err", err)
	}
}

// primaryAgent is the agent name GetOrCreate uses for new
// ChatSessions. Set by the runtime via WithPrimaryAgent at
// construction; defaults to "" (caller's responsibility).

// PersistAgentSession writes the entry for as to the manager's
// agent_sessions.json store. Idempotent; safe to call from event
// handlers (no daemon locks held). Used to durably save the
// agent's resume id the first time it surfaces via EventAgentReady, so
// the next respawn can replay `--resume <id>`.
func (m *Manager) PersistAgentSession(as *AgentSession) error {
	if as == nil {
		return nil
	}
	m.mu.RLock()
	asFile := m.asFile
	m.mu.RUnlock()
	if asFile == nil {
		return nil
	}
	return asFile.Upsert(as.Entry())
}

// List returns a snapshot of all ChatSessions (freshly allocated
// slice; callers may mutate).
func (m *Manager) List() []*ChatSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*ChatSession, 0, len(m.sessions))
	for _, cs := range m.sessions {
		out = append(out, cs)
	}
	return out
}

// RestoreFromRegistry rebuilds the in-memory chat table from
// persisted ChatSessionEntry + AgentSessionEntry on startup.
//
// Each persisted AgentSessionEntry becomes an AgentSession with
// status=Detached (no process running). Subsequent /use will
// re-spawn via the Spawner. ChatSession's selectedAgentSessionId
// reference is restored if it points at a valid AgentSession.
//
// chatIDMap is an optional function that maps a ChatSessionEntry.ID
// back to its original chat_id (e.g., via the binding table). If
// nil, the registry ChatID field is used directly (assuming the
// schema stores it).
//
// v1.2 commit 8a: this is a placeholder; full restore integration
// with the v1.x binding table arrives in commit 8b.
func (m *Manager) RestoreFromRegistry() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.csFile == nil {
		return nil
	}

	// Index persisted AgentSession entries by chatSessionId so we
	// can populate each ChatSession's pool after we create it.
	agentsByCS := make(map[string][]*AgentSession)
	if m.asFile != nil {
		for _, aEntry := range m.asFile.List() {
			as := FromAgentSessionEntry(aEntry)
			agentsByCS[aEntry.ChatSessionID] = append(agentsByCS[aEntry.ChatSessionID], as)
		}
	}

	for _, entry := range m.csFile.List() {
		cs, err := New(entry.ChatID, entry.PrimaryAgent)
		if err != nil {
			slog.Default().Warn("Manager.RestoreFromRegistry: New failed; skipping chat",
				"chat_id", entry.ChatID, "err", err)
			continue
		}
		cs.WithSpawner(m.spawner).
			WithPersistence(m.csFile, m.asFile)
		// Bind the Manager's shared emitter; nil is permitted
		// here (restored sessions may legitimately be opened
		// before WithEmitter is called) — callers must
		// nil-check via cs.Emitter().
		if m.emitter != nil {
			cs.WithEmitter(m.emitter)
		}
		cs.selectedCwd = entry.SelectedCwd
		cs.selectedAgent = entry.SelectedAgent
		// Registry persists bare int; ChatSession fields are
		// typed enums. Cast on read — Go zero-value semantics
		// preserve the safe default when the int is 0.
		cs.watchMode = WatchMode(entry.WatchMode) // 0 == WatchModeMention (default, safe)
		cs.thinkMode = ThinkMode(entry.ThinkMode) // 0 == ThinkModeShow (default; preserve F-thread-route behavior)
		cs.toolsMode = ToolsMode(entry.ToolsMode) // 0 == ToolsModeHide (default; quiet by default)
		cs.watcherHintEmitted = entry.WatcherHintEmitted
		cs.lastInteractionAt = entry.LastInteractionAt
		// commit fix-6: clear selectedAS on restore. The persisted
		// selectedAgentSessionId points at an AgentSession whose
		// handle is in-memory only (lost on restart). Leaving the
		// pointer set would cause SendBlocks (called by the default
		// FlushHook) to return ErrNotRunning and silently drop user
		// messages. The next LookupSelectedAgentSession will spawn
		// fresh and re-populate selectedAS.
		cs.selectedAS = nil
		// Seed the pool from the agent_sessions.json entries that
		// belong to this ChatSession. FromAgentSessionEntry has
		// already demoted any StatusRunning to StatusDetached, so
		// LookupSelectedAgentSession will re-spawn on the next call.
		for _, as := range agentsByCS[entry.ID] {
			cs.attachAgentSession(as)
		}

		// F-62 §3.3.1: in-flight messages are NOT pushed back into
		// cs.queue at restore. The queue is chat-level and would
		// bleed the previous (agent, cwd)'s hung messages into the
		// new TryFlush's batch — both cross-AS misdelivery and
		// receipt-card anchor drift. Drop on the (agent, cwd)
		// "new session" boundary instead (see SetSelectedCwd /
		// LookupSelectedAgentSession hadPrior). The registry's
		// InFlightMessages slice stays on disk for audit and is
		// cleared by the next ClearInFlight; the runtime in-memory
		// view is what we re-hydrate per AS.

		m.sessions[entry.ChatID] = cs

		// Fire onCreate so the runtime can wire per-ChatSession
		// handlers uniformly across fresh + restored chats.
		if m.onCreate != nil {
			m.onCreate(cs)
		}
	}
	return nil
}

// ErrNoSelectedChatSession is returned by handlers when chatID has no
// ChatSession yet. Callers should reply with "/cwd first".
var ErrNoSelectedChatSession = fmt.Errorf("chatsession: no ChatSession for chat (send /cwd <path> first)")
