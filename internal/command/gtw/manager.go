package gtw

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/services"
)

// Manager owns the per-chat gtw state. Pre-F-51 these fields
// lived on chatsession.ChatSession (gtwContext / gtwDrafts); the
// "type alias trick" forced chatsession to know about gtw. With
// F-51, Manager is the only place that knows what gtw state
// looks like; chatsession is unaware.
//
// The runtime instantiates one Manager per process and shares
// it across all chats. Per-chat substate (states / drafts) is
// keyed by chatID and protected by a single sync.RWMutex.
//
// ChatSession references are NOT stored or looked up here.
// Slash command paths receive cs from the dispatcher parameter;
// reaction paths receive cs from the runtime-layer wrapper that
// resolves cs before calling HandleReaction. Keeping cs read
// logic out of gtw prevents accidental misuse (e.g. a slash
// command querying a different mgr's cache and missing the
// just-updated cwd).
type Manager struct {
	mu     sync.RWMutex
	states map[string]Context           // chatID -> active /gtw fix snapshot
	drafts map[string]map[string]*Draft // chatID -> requestID -> pending draft

	// runs is the per-chat run lock that serialises /gtw
	// subcommand execution. Acquired at the top of
	// Factory.Handle (cmd.go), released on Handle return.
	//
	// Rationale: F-59 made every slash command async (a fresh
	// goroutine per inbound), which means two /gtw fix / push /
	// pr calls landing in quick succession now race against
	// each other on Manager.states, Manager.drafts, the worktree
	// directory, cs.SelectedCwd, and the agent session. The
	// chatID is the natural serialisation boundary — two chats
	// must remain independent — so we lazy-allocate one mutex
	// per chatID and never free it. Never-freeing matches the
	// chatsession.Manager.hintLocks policy: freeing a *sync.Mutex
	// while another goroutine is blocked on it would race.
	//
	// Memory: one *sync.Mutex + sync.Map overhead (~80 bytes)
	// per chatID seen. A busy daemon with thousands of chats
	// sees sub-megabyte footprint; cleanup isn't worth the
	// race risk.
	runs sync.Map // map[chatID]*sync.Mutex

	// now is overridable for tests; defaults to time.Now.
	now func() time.Time
	// deps is the HandlerDeps shared by all reaction handlers
	// (Git / Prober / Detect / Now). Set via
	// SetHandlerDeps at runtime startup; may be nil before
	// the runtime wires the action pipeline.
	deps HandlerDeps
}

// NewManager returns an empty Manager ready to receive per-chat
// state. ChatSession references are passed in by callers (slash
// command dispatcher parameter; reaction wrapper in runtime)
// and are never cached here.
func NewManager() *Manager {
	return &Manager{
		states: make(map[string]Context),
		drafts: make(map[string]map[string]*Draft),
		now:    time.Now,
	}
}

// --- run lock (per-chat serialisation) ---

// runLockFor returns the per-chat mutex that serialises /gtw
// subcommand execution for chatID. Factory.Handle acquires it
// before the subcommand switch and releases it via defer on
// every return path (early validation, unknown subcommand,
// normal completion).
//
// chatID == "" returns nil so the caller can no-op safely in
// tests and synthetic inputs that drive Handle directly
// without a ChatID. Callers MUST nil-check the return value
// before Lock; the defer Unlock must also be guarded:
//
//	if mu := mgr.runLockFor(input.ChatID); mu != nil {
//	    mu.Lock()
//	    defer mu.Unlock()
//	}
//
// Per-chat (not Manager-wide) so a slow /gtw commit in chat A
// never blocks /gtw sync in chat B. Lazily allocated via
// sync.Map.LoadOrStore; entries are never freed (see the runs
// field doc for the rationale).
func (m *Manager) runLockFor(chatID string) *sync.Mutex {
	if chatID == "" {
		return nil
	}
	if v, ok := m.runs.Load(chatID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := m.runs.LoadOrStore(chatID, mu)
	return actual.(*sync.Mutex)
}

// --- context (per-chat fix snapshot) ---

// GetContext returns the active /gtw fix snapshot for chatID, or
// the zero value (State == "") when no fix is active. Returns
// by value to avoid races on the stored struct.
func (m *Manager) GetContext(chatID string) Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.states[chatID]
}

// SetContext replaces the in-flight /gtw fix snapshot. Pass the
// zero value (Context{}) to clear.
func (m *Manager) SetContext(chatID string, ctx Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if (ctx == Context{}) {
		delete(m.states, chatID)
		return
	}
	if ctx.UpdatedAt.IsZero() {
		ctx.UpdatedAt = m.now()
	}
	m.states[chatID] = ctx
}

// HasContext reports whether a /gtw fix is currently in flight
// for chatID.
func (m *Manager) HasContext(chatID string) bool {
	return m.GetContext(chatID).State != ""
}

// ClearContext is a sugar wrapper for SetContext(chatID, Context{}).
func (m *Manager) ClearContext(chatID string) {
	m.SetContext(chatID, Context{})
}

// --- drafts (per-chat, per-RequestID) ---

// GetDraft returns the pending draft keyed by Choice.RequestID for
// the given chatID, or nil if no draft is registered. Used by
// HandleReaction to look up the context of the user's click.
func (m *Manager) GetDraft(chatID, requestID string) *Draft {
	if chatID == "" || requestID == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.drafts[chatID][requestID]
}

// StoreDraft registers a draft under (chatID, requestID).
// Overwrites any previous draft under the same key (rare;
// reactions are usually one-shot per card).
func (m *Manager) StoreDraft(chatID, requestID string, d *Draft) {
	if chatID == "" || requestID == "" || d == nil {
		return
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = m.now()
	}
	m.mu.Lock()
	if _, ok := m.drafts[chatID]; !ok {
		m.drafts[chatID] = make(map[string]*Draft)
	}
	m.drafts[chatID][requestID] = d
	m.mu.Unlock()
}

// TakeDraft atomically reads and deletes the draft. Returns nil
// if no draft was registered. Used by HandleReaction to ensure
// a single ✅ / 🆕 / 🔗 / ❌ / 🔄 is acted on exactly once.
func (m *Manager) TakeDraft(chatID, requestID string) *Draft {
	if chatID == "" || requestID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts[chatID] == nil {
		return nil
	}
	d := m.drafts[chatID][requestID]
	delete(m.drafts[chatID], requestID)
	if len(m.drafts[chatID]) == 0 {
		delete(m.drafts, chatID)
	}
	return d
}

// ListDrafts returns a snapshot of all currently-pending drafts
// for chatID. Order is unspecified.
func (m *Manager) ListDrafts(chatID string) []*Draft {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Draft, 0, len(m.drafts[chatID]))
	for _, d := range m.drafts[chatID] {
		out = append(out, d)
	}
	return out
}

// DraftCount returns the number of pending drafts for chatID.
func (m *Manager) DraftCount(chatID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.drafts[chatID])
}

// ClearDrafts drops every pending draft for chatID. Used by
// any future "abort all cards" path.
// Does not touch the context state — call ClearContext
// separately when both must go.
func (m *Manager) ClearDrafts(chatID string) {
	m.mu.Lock()
	delete(m.drafts, chatID)
	m.mu.Unlock()
}

// Reset clears all state for chatID (context + drafts).
// Used by `nightme gtw reset` debug command. ChatSession
// references are not cached here, so nothing else to clear.
func (m *Manager) Reset(chatID string) {
	m.mu.Lock()
	delete(m.states, chatID)
	delete(m.drafts, chatID)
	m.mu.Unlock()
}

// --- reaction handler ---

// SetHandlerDeps installs the runtime's HandlerDeps. Must be
// called before Manager.HandleReaction is registered with the
// ReactionRouter, otherwise reactions find no deps and return
// false silently.
func (m *Manager) SetHandlerDeps(deps HandlerDeps) {
	m.mu.Lock()
	m.deps = deps
	m.mu.Unlock()
}

// HandleReaction is the per-event reaction executor gtw exposes
// to the runtime. The runtime wraps it with a closure that
// resolves *chatsession.ChatSession from ev.ChatID and passes
// the result here — gtw does NOT do cs lookup itself.
//
// Looks up the chatID's drafts map and dispatches to the
// per-draft action executor (action.go's HandleDraftReaction).
//
// Returns true if a draft was found and acted on (consumed);
// false if no draft matched OR deps is not yet wired
// (router treats this as "not consumed" and the event falls
// through / drops).
//
// Wire-up example:
//
//	router := services.NewReactionRouter()
//	gtwMgr := gtw.NewManager()
//	gtwMgr.SetHandlerDeps(deps)
//	router.Register("*", func(ctx context.Context, ev services.ReactionEvent) bool {
//	    cs := findChatSession(ev.ChatID, cfg.Primary)
//	    if cs == nil { return false }
//	    return gtwMgr.HandleReaction(ctx, ev, cs)
//	})
func (m *Manager) HandleReaction(ctx context.Context, ev services.ReactionEvent, cs *chatsession.ChatSession) bool {
	if ev.RequestID == "" || ev.ChatID == "" {
		return false
	}
	if cs == nil {
		return false
	}
	m.mu.RLock()
	deps := m.deps
	m.mu.RUnlock()
	if deps.Git == nil {
		// Deps not yet wired — log and fall through.
		slog.Default().Warn("gtw: HandleReaction called before SetHandlerDeps",
			"chat_id", ev.ChatID,
			"request_id", ev.RequestID)
		return false
	}
	consumed, err := HandleDraftReaction(ctx, m, deps, cs, ev)
	if err != nil {
		slog.Default().Error("gtw: HandleReaction error",
			"err", err,
			"chat_id", ev.ChatID,
			"request_id", ev.RequestID,
			"emoji", ev.Emoji)
	}
	return consumed
}
