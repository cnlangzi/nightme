package gtw

import (
	"context"
	"log/slog"
	"sync"
	"time"

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
// senders) is keyed by chatID and protected by a single
// sync.RWMutex.
type Manager struct {
	mu      sync.RWMutex
	states  map[string]Context              // chatID -> active /gtw fix snapshot
	drafts  map[string]map[string]*Draft    // chatID -> userMsgID -> pending draft
	senders map[string]Sender               // chatID -> outbound sender (cached per factory invocation)

	// senderFactory is the runtime-supplied constructor for
	// per-chat Sender instances. Called lazily on GetSender
	// miss. Set via SetSenderFactory at runtime startup.
	// nil = no factory; GetSender returns nil and the gtw
	// command fails loudly (rather than silently corrupting
	// state with a nil deref).
	senderFactory func(chatID string) Sender

	// now is overridable for tests; defaults to time.Now.
	now func() time.Time
	// deps is the HandlerDeps shared by all reaction handlers
	// (Send / SendCard / Git / Prober / Detect / Now). Set via
	// SetHandlerDeps at runtime startup; may be nil before
	// the runtime wires the action pipeline.
	deps HandlerDeps
}

// NewManager returns an empty Manager ready to receive per-chat
// state. Send funnels and providers are wired at runtime.
func NewManager() *Manager {
	return &Manager{
		states:  make(map[string]Context),
		drafts:  make(map[string]map[string]*Draft),
		senders: make(map[string]Sender),
		now:     time.Now,
	}
}

// SetSender registers the outbound Sender for a chat. Called
// by the runtime after a chat is created or restored. gtw uses
// this to send replies and PATCHes without holding a *chatsession
// pointer.
func (m *Manager) SetSender(chatID string, s Sender) {
	if chatID == "" || s == nil {
		return
	}
	m.mu.Lock()
	m.senders[chatID] = s
	m.mu.Unlock()
}

// SetSenderFactory installs a function that creates a Sender
// on demand when GetSender is called for a chatID that has no
// pre-registered Sender. The factory is called under the
// manager's read lock (the result is cached, so subsequent
// GetSender calls are O(1)).
//
// F-51 runtime wiring: cmd/nightme/run.go installs a factory
// that wraps the SessionService adapter + Channel adapter;
// per-chat Sender instances back onto the live chat session
// for ActiveCwd / SetActiveCwd and onto the channel for Send.
//
// nil disables the lazy path (GetSender returns nil).
func (m *Manager) SetSenderFactory(fn func(chatID string) Sender) {
	m.mu.Lock()
	m.senderFactory = fn
	m.mu.Unlock()
}

// UnsetSender drops the Sender for a chat (e.g. on chat delete).
func (m *Manager) UnsetSender(chatID string) {
	m.mu.Lock()
	delete(m.senders, chatID)
	m.mu.Unlock()
}

// GetSender returns the Sender registered for chatID. If no
// Sender is pre-registered AND a senderFactory is installed,
// the factory is called once and the result is cached. Returns
// nil when no Sender is available.
//
// Used internally by RunFix / HandleDraftReaction to push
// outbound without going through ChatSession.
func (m *Manager) GetSender(chatID string) Sender {
	if chatID == "" {
		return nil
	}
	m.mu.RLock()
	s, ok := m.senders[chatID]
	factory := m.senderFactory
	m.mu.RUnlock()
	if ok {
		return s
	}
	if factory == nil {
		return nil
	}
	// Call factory outside the lock (factory may itself
	// acquire other locks — e.g. *chatsession.Manager's Get
	// takes a read lock).
	fresh := factory(chatID)
	if fresh == nil {
		return nil
	}
	m.mu.Lock()
	// Double-check after re-acquiring: another goroutine may
	// have raced us to install a sender.
	if existing, ok := m.senders[chatID]; ok {
		m.mu.Unlock()
		return existing
	}
	m.senders[chatID] = fresh
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

// --- drafts (per-chat, per-userMsgID) ---

// GetDraft returns the pending draft keyed by userMsgID for the
// given chatID, or nil if no draft is registered. Used by
// HandleReaction to look up the context of the user's reaction
// target.
func (m *Manager) GetDraft(chatID, userMsgID string) *Draft {
	if chatID == "" || userMsgID == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.drafts[chatID][userMsgID]
}

// StoreDraft registers a draft under (chatID, userMsgID).
// Overwrites any previous draft under the same key (rare;
// reactions are usually one-shot per card).
func (m *Manager) StoreDraft(chatID, userMsgID string, d *Draft) {
	if chatID == "" || userMsgID == "" || d == nil {
		return
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = m.now()
	}
	m.mu.Lock()
	if _, ok := m.drafts[chatID]; !ok {
		m.drafts[chatID] = make(map[string]*Draft)
	}
	m.drafts[chatID][userMsgID] = d
	m.mu.Unlock()
}

// TakeDraft atomically reads and deletes the draft. Returns nil
// if no draft was registered. Used by HandleReaction to ensure
// a single ✅ / 🆕 / 🔗 / ❌ / 🔄 is acted on exactly once.
func (m *Manager) TakeDraft(chatID, userMsgID string) *Draft {
	if chatID == "" || userMsgID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drafts[chatID] == nil {
		return nil
	}
	d := m.drafts[chatID][userMsgID]
	delete(m.drafts[chatID], userMsgID)
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
// sender). Used by `nightme gtw reset` debug command.
func (m *Manager) Reset(chatID string) {
	m.mu.Lock()
	delete(m.states, chatID)
	delete(m.drafts, chatID)
	delete(m.senders, chatID)
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
	if ev.TargetMsgID == "" || ev.ChatID == "" {
		return false
	}
	m.mu.RLock()
	deps := m.deps
	m.mu.RUnlock()
	if deps.Send == nil {
		// Deps not yet wired — log and fall through.
		slog.Default().Warn("gtw: HandleReaction called before SetHandlerDeps",
			"chat_id", ev.ChatID,
			"target_msg_id", ev.TargetMsgID)
		return false
	}
	consumed, err := HandleDraftReaction(ctx, m, deps, ev)
	if err != nil {
		slog.Default().Error("gtw: HandleReaction error",
			"err", err,
			"chat_id", ev.ChatID,
			"target_msg_id", ev.TargetMsgID,
			"emoji", ev.Emoji)
	}
	return consumed
}
