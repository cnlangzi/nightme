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
	"fmt"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/gateway/outbound"
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
func (m *Manager) WithOnCreate(fn func(*ChatSession)) *Manager {
	m.mu.Lock()
	m.onCreate = fn
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
	// Phase 1: lock-free fast path. Pure read of the sessions
	// table — RLock lets concurrent GetOrCreate calls for
	// distinct chatIDs run in parallel (Phase 1 was m.mu.Lock()
	// in Commit 8; the lock was functionally safe but serialised
	// reads unnecessarily).
	m.mu.RLock()
	cs, ok := m.sessions[chatID]
	m.mu.RUnlock()
	if ok {
		return cs, nil // existing cs → emitter already bound
	}

	// Phase 2: construct + attach spawner/persistence + bind
	// the Manager's shared emitter (if any). No write lock held
	// here so external callbacks (Spawner, persistence) don't
	// have to re-enter the manager mutex; instead we read all
	// the manager state we need under a single RLock
	// acquisition.
	//
	// A nil emitter is permitted — tests that only care about
	// ChatSession's internal state (InputBuffer FSM, persistence,
	// status transitions) don't need an outbound surface. The
	// production runtime (cmd/nightme) always wires an Emitter
	// before opening chats, so the production path is unaffected.
	// cs.Emitter() returning nil is the contract that downstream
	// callers must already nil-check (see cs.Emitter doc).
	m.mu.RLock()
	spawner := m.spawner
	csFile := m.csFile
	asFile := m.asFile
	emitter := m.emitter
	m.mu.RUnlock()

	cs, err := New(chatID, primaryAgent)
	if err != nil {
		return nil, err
	}
	cs.WithSpawner(spawner).
		WithPersistence(csFile, asFile)
	if emitter != nil {
		cs.WithEmitter(emitter)
	}

	// Phase 3: re-lock for the insert + onCreate publish.
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.sessions[chatID]; ok {
		// Another goroutine won the race; discard our construction.
		// The discarded cs is never visible to anyone (we never
		// ran onCreate, never registered it, never started any
		// background goroutine that holds it). The AgentSession
		// pool is empty so there's nothing to clean up; if a
		// future change populates the pool here, add a cs.Close.
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
// Send / SendCard).
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
func (m *Manager) WithEmitter(em outbound.Emitter) *Manager {
	m.mu.Lock()
	m.emitter = em
	m.mu.Unlock()
	return m
}

// Get returns the ChatSession for chatID, or nil if absent.
func (m *Manager) Get(chatID string) *ChatSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[chatID]
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