// Package chatsession — Manager (commit 8a).
//
// Manager is the v1.2 equivalent of v1.1's session.MemoryManager: it
// owns the per-chat ChatSession table and exposes lifecycle
// operations needed by the Gateway handlers (/cwd, /use, /kill).
//
// Key differences from v1.1 MemoryManager:
//
//   - Bound to ChatSession (not bare Session); one ChatSession per
//     chat_id, with an AgentSession pool inside.
//   - /cwd no longer spawns; /use is lazy (reuse or spawn); /kill
//     clears the pool without removing the ChatSession.
//   - Manager doesn't fork directly; it uses a Spawner (see
//     spawn.go) to keep agent/bridge imports out of chatsession.
package chatsession

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/registry"
)

// Manager owns the per-chat ChatSession table. Safe for concurrent
// use; the chat-id key space is the natural concurrency boundary.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*ChatSession

	// spawner is used by LookupActiveAgentSession on every chat
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

	// channelResolver is registered once at startup via
	// WithChannelResolver. GetOrCreate calls it (outside m.mu)
	// when creating a new ChatSession; the returned Channel is
	// bound immutably to the new cs. nil = no resolver; tests
	// without a channel pass nil and accept cs.Channel() == nil.
	channelResolver func(chatID string) Channel
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
// activeAgent so the runtime always has an effective agent to
// dispatch to.
//
// Errors:
//   - New(...) itself returns error → propagated as-is.
//
// Concurrency: simple lock-internal critical section (the
// single-threaded runtime path uses GetOrCreate; the per-AS
// lifecycle doesn't touch the manager's table).
// GetOrCreate returns the ChatSession for chatID, creating it if
// missing. The chatType parameter was removed in F-33 (D1); nightme
// no longer carries chat-type at the binding layer. primaryAgent
// is the cfg.Primary snapshot from config; ChatSession.primaryAgent
// is captured here and never mutated post-creation (Q-A: no
// /default command, no per-chat override). It also seeds
// activeAgent so the runtime always has an effective agent to
// dispatch to.
//
// Errors:
//   - channelResolver returns nil for chatID → returns (nil, err)
//     (logs warn). The daemon does not crash; only this dispatch
//     is affected. Caller decides how to handle.
//   - New(...) itself returns error → propagated as-is.
//
// Concurrency: double-checked locking. The channelResolver call
// is always outside m.mu to avoid nesting external locks (current
// gateway.resolveChannel takes its own RLock; future resolver
// implementations should not be required to know about m.mu).
func (m *Manager) GetOrCreate(chatID, primaryAgent string) (*ChatSession, error) {
	// Phase 1: lock-internal fast path
	m.mu.Lock()
	cs, ok := m.sessions[chatID]
	m.mu.Unlock()
	if ok {
		return cs, nil // existing cs → channel already bound
	}

	// Phase 2: call resolver OUTSIDE the lock. Three cases:
	//   - no resolver configured (tests, debug) → pass nil;
	//     cs.Channel() returns nil; callers nil-check.
	//   - resolver set but returns nil → log warn + error
	//     (real production misconfiguration; this chat's outbound
	//     surface is unusable).
	//   - resolver returns a channel → bind to cs.
	var ch Channel
	if m.channelResolver != nil {
		ch = m.channelResolver(chatID)
		if ch == nil {
			slog.Default().Warn("Manager.GetOrCreate: channelResolver returned nil",
				"chat_id", chatID)
			return nil, fmt.Errorf("manager: channel is nil for chatID=%s", chatID)
		}
	}

	// Phase 3: construct + attach spawner/persistence (no lock)
	cs, err := New(chatID, primaryAgent, ch)
	if err != nil {
		return nil, err
	}
	cs.WithSpawner(m.spawner).WithPersistence(m.csFile, m.asFile)

	// Phase 4: re-lock + insert with re-check (race-safe)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.sessions[chatID]; ok {
		// Another goroutine won the race; discard our construction.
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

// WithChannelResolver registers the chatID → Channel resolver used
// by GetOrCreate when creating new ChatSessions. Wired once at
// startup (typically wrapping gateway.resolveChannel).
//
// The same chatID MUST always resolve to the same Channel —
// that's the channel-binding invariant. Violating this would
// cause commands on an existing chat to suddenly route to a
// different IM channel, breaking ReplyTo threading.
//
// nil means "no resolver"; GetOrCreate will return an error when
// it would need to create a new cs. Useful for tests that don't
// care about channel binding.
func (m *Manager) WithChannelResolver(fn func(chatID string) Channel) *Manager {
	m.channelResolver = fn
	return m
}

// channelResolver is registered once at startup via
// WithChannelResolver. GetOrCreate calls it (outside m.mu)
// when creating a new ChatSession; the returned Channel is
// bound immutably to the new cs.
func (m *Manager) channelResolverFn() func(chatID string) Channel {
	return m.channelResolver
}

// Get returns the ChatSession for chatID, or nil if absent.
func (m *Manager) Get(chatID string) *ChatSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[chatID]
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
// re-spawn via the Spawner. ChatSession's activeAgentSessionId
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
		var ch Channel
		if m.channelResolver != nil {
			ch = m.channelResolver(entry.ChatID)
		}
		if ch == nil {
			slog.Default().Warn("Manager.RestoreFromRegistry: channelResolver returned nil; chat's outbound surface will be unavailable",
				"chat_id", entry.ChatID)
		}
		cs, err := New(entry.ChatID, entry.PrimaryAgent, ch)
		if err != nil {
			slog.Default().Warn("Manager.RestoreFromRegistry: New failed; skipping chat",
				"chat_id", entry.ChatID, "err", err)
			continue
		}
		cs.WithSpawner(m.spawner).
			WithPersistence(m.csFile, m.asFile)
		cs.activeCwd = entry.ActiveCwd
		cs.activeAgent = entry.ActiveAgent
		cs.watchMode = entry.WatchMode // F-watch: 0 == WatchModeMention (default, safe)
		cs.thinkMode = entry.ThinkMode // F-think: 0 == ThinkModeShow (default; preserve F-thread-route behavior)
		cs.toolsMode = entry.ToolsMode // F-38: 0 == ToolsModeHide (default; quiet by default)
		cs.lastInteractionAt = entry.LastInteractionAt
		// commit fix-6: clear activeAS on restore. The persisted
		// activeAgentSessionId points at an AgentSession whose
		// handle is in-memory only (lost on restart). Leaving the
		// pointer set would cause SendBlocks (called by the default
		// FlushHook) to return ErrNotRunning and silently drop user
		// messages. The next LookupActiveAgentSession will spawn
		// fresh and re-populate activeAS.
		cs.activeAS = nil
		// Seed the pool from the agent_sessions.json entries that
		// belong to this ChatSession. FromAgentSessionEntry has
		// already demoted any StatusRunning to StatusDetached, so
		// LookupActiveAgentSession will re-spawn on the next call.
		for _, as := range agentsByCS[entry.ID] {
			cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
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

// ErrNoActiveChatSession is returned by handlers when chatID has no
// ChatSession yet. Callers should reply with "/cwd first".
var ErrNoActiveChatSession = fmt.Errorf("chatsession: no ChatSession for chat (send /cwd <path> first)")