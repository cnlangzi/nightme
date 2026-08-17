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
// it across all chats. Per-chat substate (states / drafts /
// chatSessions cache) is keyed by chatID and protected by a
// single sync.RWMutex.
type Manager struct {
	mu           sync.RWMutex
	states       map[string]Context                  // chatID -> active /gtw fix snapshot
	drafts       map[string]map[string]*Draft        // chatID -> requestID -> pending draft
	chatSessions map[string]*chatsession.ChatSession // chatID -> live ChatSession (cached per factory invocation)

	// chatSessionLookup is the runtime-supplied closure that
	// resolves a chatID to its *chatsession.ChatSession on
	// demand. Called lazily on GetChatSession miss. Set via
	// SetGetChatSession at runtime startup. nil = no factory;
	// GetChatSession returns nil and the gtw command fails
	// loudly (rather than silently corrupting state with a nil
	// deref).
	chatSessionLookup func(chatID string) *chatsession.ChatSession

	// now is overridable for tests; defaults to time.Now.
	now func() time.Time
	// deps is the HandlerDeps shared by all reaction handlers
	// (Git / Prober / Detect / Now). Set via
	// SetHandlerDeps at runtime startup; may be nil before
	// the runtime wires the action pipeline.
	deps HandlerDeps
}

// NewManager returns an empty Manager ready to receive per-chat
// state. ChatSession lookup is wired at runtime.
func NewManager() *Manager {
	return &Manager{
		states:       make(map[string]Context),
		drafts:       make(map[string]map[string]*Draft),
		chatSessions: make(map[string]*chatsession.ChatSession),
		now:          time.Now,
	}
}

// SetGetChatSession installs a closure that resolves chatID to
// its *chatsession.ChatSession on demand. The factory is called
// under the manager's read lock; the result is cached, so
// subsequent GetChatSession calls are O(1).
//
// F-XX runtime wiring: cmd/nightme/run.go installs a closure
// that holds a *chatsession.Manager reference. The closure
// delegates to mgr.GetOrCreate(chatID, primary) so per-chat
// session materialisation follows the same path as the
// runtime's other consumers.
//
// Tests install a closure that returns a per-chat fake
// *chatsession.ChatSession (see manager_test.go's
// fakeChatSession helper).
//
// nil disables the lazy path (GetChatSession returns nil).
//
// F-XX: replaces the old SetSender / SetChatSession pair —
// the runtime never needs to pre-populate; the closure-based
// lookup is enough.
func (m *Manager) SetGetChatSession(fn func(chatID string) *chatsession.ChatSession) {
	m.mu.Lock()
	m.chatSessionLookup = fn
	m.mu.Unlock()
}

// GetChatSession returns the *chatsession.ChatSession
// registered for chatID. If none is pre-registered AND a
// chatSessionLookup is installed, the lookup is called once
// and the result is cached. Returns nil when no ChatSession is
// available.
//
// Used internally by RunFix / HandleDraftReaction to access
// SelectedCwd / SetSelectedCwd / QueueUserMessage without going
// through an interface boundary.
//
// F-XX: replaces GetSender.
func (m *Manager) GetChatSession(chatID string) *chatsession.ChatSession {
	if chatID == "" {
		return nil
	}
	m.mu.RLock()
	cs, ok := m.chatSessions[chatID]
	lookup := m.chatSessionLookup
	m.mu.RUnlock()
	if ok {
		return cs
	}
	if lookup == nil {
		return nil
	}
	// Call lookup outside the lock (lookup may itself acquire
	// other locks — e.g. *chatsession.Manager.GetOrCreate takes
	// a write lock).
	fresh := lookup(chatID)
	if fresh == nil {
		return nil
	}
	m.mu.Lock()
	// Double-check after re-acquiring: another goroutine may
	// have raced us to install a ChatSession.
	if existing, ok := m.chatSessions[chatID]; ok {
		m.mu.Unlock()
		return existing
	}
	m.chatSessions[chatID] = fresh
	m.mu.Unlock()
	return fresh
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
// `/gtw test reset` and any future "abort all cards" path.
// Does not touch the context state — call ClearContext
// separately when both must go.
func (m *Manager) ClearDrafts(chatID string) {
	m.mu.Lock()
	delete(m.drafts, chatID)
	m.mu.Unlock()
}

// Reset clears all state for chatID (context + drafts +
// ChatSession cache). Used by `nightme gtw reset` debug command.
func (m *Manager) Reset(chatID string) {
	m.mu.Lock()
	delete(m.states, chatID)
	delete(m.drafts, chatID)
	delete(m.chatSessions, chatID)
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

// HandleReaction is the callback gtw registers with
// services.ReactionRouter at startup. Looks up the chatID's
// drafts map and dispatches to the per-draft action executor
// (action.go's HandleDraftReaction).
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
//	router.Register("*", gtwMgr.HandleReaction)
func (m *Manager) HandleReaction(ctx context.Context, ev services.ReactionEvent) bool {
	if ev.RequestID == "" || ev.ChatID == "" {
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
	consumed, err := HandleDraftReaction(ctx, m, deps, ev)
	if err != nil {
		slog.Default().Error("gtw: HandleReaction error",
			"err", err,
			"chat_id", ev.ChatID,
			"request_id", ev.RequestID,
			"emoji", ev.Emoji)
	}
	return consumed
}
