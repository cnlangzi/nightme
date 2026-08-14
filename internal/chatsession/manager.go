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

	// (no Manager-level gitStatusDeps — the cache is per-ChatSession
// and the Emitter's GitStatusLookup reaches the chat via mgr.Get
// at message time, using cs.gitStatusDeps that WithGitStatusDeps
// propagated to each chat.)
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
// gitStatusDeps wired and the per-chat RefreshGitStatus would
// no-op (depsConfigured=false) on every subsequent dispatcher
// call.
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
// refresh its GitStatus (called via cs.RefreshGitStatus or
// implicitly via cs.GitStatus(ctx) on cache miss). Called once
// at startup. Returns self for chaining.
//
// Propagates to every existing chat AND registers an OnCreate
// hook for future chats created via GetOrCreate. This way the
// chat's own refresh is always callable with non-nil deps —
// no "no deps wired yet" silent-skip failure mode.
func (m *Manager) WithGitStatusDeps(deps GitStatusDeps) *Manager {
	m.mu.Lock()
	// Propagate to every existing chat so per-chat
	// RefreshGitStatus (called by /gtw commit pre/post,
	// /gtw pr, SetSelectedCwd, and the Emitter's
	// GitStatusLookup on cache miss) has the deps it needs.
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
	m.mu.RUnlock()
	return cs, nil // existing cs → emitter already bound
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
		slog.Default().Info("chatsession: drop non-mention group message (WatchMode != All)",
			"chat_id", msg.ChatID, "message_id", msg.MessageID)
		return nil
	}
	userMsgID := msg.MessageID
	if userMsgID == "" {
		userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
	}

	cs, _ := m.GetOrCreate(msg.ChatID, m.primaryAgent)

	// F-31 / F-53: ⏳ immediately, before spawn resolves.
	cs.EmitMessageState(userMsgID, agent.MessageQueued)

	// Resolve active AgentSession (lazy spawn on miss).
	_, err := cs.LookupSelectedAgentSession()
	if err != nil {
		if errors.Is(err, ErrNoSelectedCwd) {
			return m.sendError(ctx, msg.ChatID, "No workspace set. Send /cwd <path> first.")
		}
		// Spawn failed (binary missing, etc.).
		return m.sendError(ctx, msg.ChatID, fmt.Sprintf("Failed to spawn agent: %v", err))
	}

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

		// Replay any in-flight messages that the killed AS had been
		// processing. Push directly into the queue (NOT via
		// QueueUserMessage) — the AS isn't spawned yet, so an
		// immediate TryFlush would race against the spawn. The
		// next TryFlush call (triggered by /use or by the first
		// user message after restore) will pick these up, the
		// Spawn will resume the agent via SessionID, and the agent
		// decides how to handle the duplicate.
		//
		// Blocks is defensively copied (and the queue stores
		// Message by value) so the queue's Message.Blocks is
		// independent of as.inFlightMessages and of any other
		// slice owned by the registry / Entry() snapshot. The
		// cost is one small slice per replayed message; the
		// safety is that no future mutation of the source can
		// reach the queue's snapshot.
		for _, as := range agentsByCS[entry.ID] {
			for _, ref := range as.Entry().InFlightMessages {
				msg := Message{
					ID:         ref.ID,
					ChatID:     entry.ChatID,
					Blocks:     append([]agent.ContentBlock(nil), ref.Blocks...),
					ReceivedAt: ref.ReceivedAt,
					// Kind zero value == MessageKindNormal (default
					// user input). Replayed messages are not
					// "must stand alone" queued turns.
				}
				if err := cs.queue.Push(msg); err != nil {
					// Should not happen at startup (queue is empty
					// pre-restore). If it ever does, the message is
					// lost — log loudly so the user knows the AS
					// is now silently dropping an in-flight reply.
					slog.Warn("Manager.RestoreFromRegistry: replay dropped in-flight message",
						"chat_id", entry.ChatID, "as_id", as.ID,
						"msg_id", ref.ID, "err", err)
				}
			}
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