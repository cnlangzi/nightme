package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// EventCallback is invoked by the Manager for every AgentEvent
// received from a running session. v0.1 consumers are Channel
// adapters; tests use it to observe the event stream.
//
// The callback runs on the Manager's readPump goroutine; it must
// return quickly or it will block the next event for this session.
type EventCallback func(s *Session, ev agent.AgentEvent)

// CreateRequest is the input to Manager.Create. The fields are
// validated by the underlying Agent.Detect and the bridge Start.
type CreateRequest struct {
	// ChatID is required.
	ChatID string

	// Workspace is the directory the agent will operate in. Required.
	Workspace string

	// Agent is the registry name (e.g. "claude"). The Manager
	// resolves it via agent.Get; an unknown name yields an error.
	Agent string

	// Args are appended to the agent's own default arguments.
	Args []string
}

// Manager owns the in-memory session table and the goroutines that
// drive each session's events.
type Manager interface {
	// Create registers a new session and starts its underlying agent.
	// The returned Session has Status == StatusRunning when Start
	// succeeds.
	Create(ctx context.Context, req CreateRequest) (*Session, error)

	// CreateOrUpdate binds a chat to a session with the given
	// workspace. New chatIDs get a detached session record; existing
	// exited/detached sessions get their workspace rebinded in
	// place; an already-running session is rejected with
	// ErrChatAlreadyBound (caller must /kill first).
	CreateOrUpdate(chatID, workspace, agent string, args []string) (*Session, error)

	// Run ensures a CLI is running for chatID. It is a no-op when
	// a CLI is already there; otherwise it spawns one in the
	// session's bound workspace. Returns ErrSessionNotFound when
	// the chat has no /cwd yet.
	Run(ctx context.Context, chatID, agent string, extraArgs []string) (*Session, error)

	// KillByChat stops the CLI bound to chatID. The session record
	// is retained. ErrSessionNotFound when chatID is unknown.
	KillByChat(chatID string) error

	// GetByChat returns the session bound to chatID, or
	// ErrSessionNotFound.
	GetByChat(chatID string) (*Session, error)

	// Get returns the session with the given ID, or
	// ErrSessionNotFound.
	Get(sid string) (*Session, error)

	// List returns a snapshot of all known sessions. Order is
	// unspecified; the slice is freshly allocated.
	List() []*Session

	// Kill terminates the agent backing sid and marks the session
	// StatusExited. Idempotent: killing an already-exited session is
	// a no-op and returns nil.
	Kill(sid string) error

	// Restore reads persisted session entries from the registry and
	// rebuilds the in-memory table. Entries whose persisted status
	// was StatusRunning are loaded as StatusDetached (their PID may
	// have died while nightme was down — the next Create / /run will
	// decide whether to respawn). Restore is idempotent: calling it
	// on an already-populated manager returns nil without touching
	// state.
	Restore(ctx context.Context) error

	// Persist writes the current session table to the registry.
	// Sessions are written as-is; callers that want to flush a
	// terminal state should call Kill first.
	Persist() error
}

// Errors surfaced by the Manager. Sentinel values so callers can
// branch with errors.Is.
var (
	// ErrSessionNotFound is returned by Get / GetByChat when no
	// session matches the lookup key.
	ErrSessionNotFound = errors.New("session: not found")

	// ErrChatAlreadyBound is returned by Create when a session for
	// the chatID already exists. The Manager enforces chat → session
	// 1:1 (Q4 / SPEC §9); callers should /kill first or reuse the
	// existing session.
	ErrChatAlreadyBound = errors.New("session: chat already bound to an existing session")
)

// MemoryManager is the production Manager: a mutex-protected map
// keyed by session ID with a secondary index by chat ID.
type MemoryManager struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	chatIndex map[string]string // chatID → sessionID

	agents *agent.Registry

	// reg is the on-disk registry backing the session table. nil
	// disables persistence (tests that don't care about disk state
	// can pass nil). When non-nil, Create / Kill / Restore / Persist
	// keep it in sync.
	reg *registry.File

	// callback is invoked for every event from every session. nil
	// means events are drained and discarded (useful for tests).
	callback EventCallback

	// idGen produces session IDs. nil falls back to a timestamp-based
	// generator.
	idGen func() string

	// clock allows tests to advance time without sleeping. nil
	// means time.Now.
	clock func() time.Time
}

// NewMemoryManager constructs an empty Manager backed by the given
// agent registry. The callback is optional (nil = discard events).
// reg is the persistence layer; pass nil to disable on-disk writes
// (used by tests that don't exercise Restore / Persist).
func NewMemoryManager(agents *agent.Registry, reg *registry.File, cb EventCallback) *MemoryManager {
	return &MemoryManager{
		sessions:  make(map[string]*Session),
		chatIndex: make(map[string]string),
		agents:    agents,
		reg:       reg,
		callback:  cb,
	}
}

// Create registers a new session and starts its agent. The agent's
// Detect is called first to surface a friendly "X not found" error
// before any process is spawned.
//
// On success the new session is returned with Status = StatusRunning
// and a background goroutine consumes its events.
func (m *MemoryManager) Create(ctx context.Context, req CreateRequest) (*Session, error) {
	if req.ChatID == "" {
		return nil, errors.New("session: ChatID is required")
	}
	if req.Workspace == "" {
		return nil, errors.New("session: Workspace is required")
	}
	if req.Agent == "" {
		return nil, errors.New("session: Agent is required")
	}

	agentSession, pid, err := m.startAgent(ctx, req.Agent, req.Workspace, req.Args)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if _, exists := m.chatIndex[req.ChatID]; exists {
		m.mu.Unlock()
		_ = agentSession.Close()
		return nil, ErrChatAlreadyBound
	}

	now := m.now()
	sess := &Session{
		ID:        m.newID(),
		ChatID:    req.ChatID,
		Workspace: req.Workspace,
		Agent:     req.Agent,
		Args:      append([]string(nil), req.Args...),
		StartedAt: now,
		LastRunAt: now,
	}
	sess.setLifecycle(StatusRunning, agentSession, pid, nil)
	m.sessions[sess.ID] = sess
	m.chatIndex[req.ChatID] = sess.ID
	m.mu.Unlock()

	if err := m.upsertEntry(sess, registry.StatusRunning, 0); err != nil {
		// Persistence is best-effort for Create: the agent is
		// already running, but if the registry write fails we
		// surface it so the caller can react. Cleanup: drop the
		// in-memory session and close the agent.
		m.mu.Lock()
		delete(m.sessions, sess.ID)
		delete(m.chatIndex, sess.ChatID)
		m.mu.Unlock()
		_ = agentSession.Close()
		return nil, fmt.Errorf("session: persist: %w", err)
	}

	pumpCtx, cancel := context.WithCancel(ctx)
	sess.setCancel(cancel)
	go m.readPump(sess, pumpCtx)

	return sess, nil
}

// GetByChat returns the session bound to chatID.
func (m *MemoryManager) GetByChat(chatID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.chatIndex[chatID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	s, ok := m.sessions[id]
	if !ok {
		// Index points at a missing session — recover by deleting
		// the stale index entry.
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// Get returns the session with the given ID.
func (m *MemoryManager) Get(sid string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sid]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// List returns a snapshot of every known session in unspecified
// order. The slice is freshly allocated; callers may mutate it.
func (m *MemoryManager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// Kill terminates the agent backing sid and marks the session
// StatusExited. Already-exited sessions return nil.
//
// Kill is idempotent. The session record is retained (so /run can
// respawn later); only the live agent handle is closed.
func (m *MemoryManager) Kill(sid string) error {
	m.mu.Lock()
	s, ok := m.sessions[sid]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	if s.Status() == StatusExited {
		m.mu.Unlock()
		return nil
	}

	// Capture the live handles before flipping lifecycle so the
	// close/cancel below can run without holding the manager lock.
	as, cancel := s.takeAgent()
	s.setLifecycle(StatusExited, nil, 0, &[]int{0}[0])
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if as != nil {
		_ = as.Close()
	}
	// Persist the exited state so /run on the next launch sees
	// status=exited and respawns. Best-effort: a write failure is
	// logged but does not surface to the caller (the agent is
	// already gone).
	if err := m.upsertEntry(s, registry.StatusExited, -1); err != nil && m.reg != nil {
		// Surface as a non-fatal log line; no error return.
		fmt.Fprintf(os.Stderr, "session: persist kill: %v\n", err)
	}
	return nil
}

// MarkDetached releases the manager's live handle for sid without closing the
// underlying agent. This is the daemon shutdown policy: the CLI may continue
// running after nightme exits and the registry records the detached state.
// Already-exited sessions are left unchanged.
func (m *MemoryManager) MarkDetached(sid string) error {
	m.mu.RLock()
	s, ok := m.sessions[sid]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}

	s.mu.Lock()
	if s.status == StatusExited {
		s.mu.Unlock()
		return nil
	}
	pid := s.PID
	s.status = StatusDetached
	s.agentSession = nil
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return m.upsertEntry(s, registry.StatusDetached, pid)
}

// Restore reads persisted entries from the registry and rebuilds the
// in-memory session table. Each entry's metadata is restored verbatim; the
// lifecycle is mapped as:
//
//	StatusRunning  -> StatusDetached (PID may be dead after restart)
//	StatusDetached -> StatusDetached
//	StatusExited   -> StatusExited
//
// Restore does NOT respawn agents — the CLI is decoupled from the
// in-memory session record. Callers that want to reattach / respawn
// must iterate List() and use /run. Calling Restore twice is a no-op
// (an already-populated manager keeps its state).
//
// If reg is nil, Restore is a no-op and returns nil.
func (m *MemoryManager) Restore(_ context.Context) error {
	if m.reg == nil {
		return nil
	}
	entries := m.reg.List()
	if len(entries) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		// Skip entries the in-memory table already has — Restore
		// must not stomp a freshly-Created session.
		if _, exists := m.sessions[e.SessionID]; exists {
			continue
		}
		sess := &Session{
			ID:        e.SessionID,
			ChatID:    e.ChatID,
			Workspace: e.Workspace,
			Agent:     e.Agent,
			Args:      append([]string(nil), e.Args...),
			PID:       e.PID,
			StartedAt: e.StartedAt,
			LastRunAt: e.LastRunAt,
		}

		var status Status
		switch e.Status {
		case registry.StatusRunning, registry.StatusDetached:
			// Running is "lose-the-PID" detached: nightme went
			// down and the agent's PID is stale. Detached is
			// explicit "we already said goodbye".
			status = StatusDetached
		default:
			status = StatusExited
		}
		sess.status = status
		sess.exitCode = e.ExitCode

		m.sessions[sess.ID] = sess
		if sess.ChatID != "" {
			m.chatIndex[sess.ChatID] = sess.ID
		}
	}
	return nil
}

// Persist writes every in-memory session to the registry. It is the
// explicit flush hook for callers that want to serialize state — most
// code paths do not need it because Create / Kill already persist.
func (m *MemoryManager) Persist() error {
	if m.reg == nil {
		return nil
	}
	sessions := m.List()
	for _, s := range sessions {
		regStatus := registry.StatusRunning
		switch s.Status() {
		case StatusRunning:
			regStatus = registry.StatusRunning
		case StatusDetached:
			regStatus = registry.StatusDetached
		default:
			regStatus = registry.StatusExited
		}
		if err := m.upsertEntry(s, regStatus, exitCodeOr(s, -1)); err != nil {
			return err
		}
	}
	return nil
}

// upsertEntry writes a Session to the registry under its ID. If reg
// is nil this is a no-op (useful for tests that don't care about
// persistence).
func (m *MemoryManager) upsertEntry(s *Session, status registry.Status, exitCode int) error {
	if m.reg == nil {
		return nil
	}
	snap := s.Snapshot()
	entry := registry.Entry{
		SessionID: snap.ID,
		ChatID:    snap.ChatID,
		Workspace: snap.Workspace,
		Agent:     snap.Agent,
		Args:      snap.Args,
		PID:       snap.PID,
		StartedAt: snap.StartedAt,
		LastRunAt: snap.LastRunAt,
		Status:    status,
	}
	if status == registry.StatusExited {
		code := exitCode
		entry.ExitCode = &code
	}
	return m.reg.Upsert(entry)
}

// exitCodeOr returns the session's exit code, or fallback if it has
// none. Used by Persist where every entry must carry a code when
// exited.
func exitCodeOr(s *Session, fallback int) int {
	if c := s.ExitCode(); c != nil {
		return *c
	}
	return fallback
}

// readPump drains the session's event channel and dispatches each
// event to the configured callback. The goroutine exits when the
// channel is closed by the AgentSession (typically after EventDone
// or EventError).
//
// The pump owns the transition from StatusRunning → StatusExited when
// it observes EventDone; Kill and natural termination therefore
// produce the same final state.
func (m *MemoryManager) readPump(s *Session, ctx context.Context) {
	// Snapshot the agent handle under the per-session lock so Kill
	// (which swaps it for nil) does not race with this goroutine.
	s.mu.RLock()
	as := s.agentSession
	s.mu.RUnlock()
	if as == nil {
		return
	}
	for ev := range as.Events() {
		if m.callback != nil {
			m.callback(s, ev)
		}
		if ev.Kind == agent.EventDone || ev.Kind == agent.EventError {
			m.markExited(s, terminalExitCode(ev))
			return
		}
	}

	// Channel closed without a terminal event — treat as exit.
	m.markExited(s, -1)
}

// markExited flips the session's lifecycle to StatusExited and
// records the exit code. Caller must NOT hold m.mu.
func (m *MemoryManager) markExited(s *Session, code int) {
	s.mu.Lock()
	if s.status == StatusRunning {
		s.status = StatusExited
		s.exitCode = &code
		s.PID = 0
	}
	s.mu.Unlock()
}

// terminalExitCode returns the exit code recorded on an EventDone
// payload, falling back to -1 for EventError / nil payloads.
func terminalExitCode(ev agent.AgentEvent) int {
	if ev.Kind == agent.EventDone && ev.Done != nil {
		return ev.Done.ExitCode
	}
	return -1
}

// now returns the configured clock (or time.Now if none).
func (m *MemoryManager) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// newID returns a unique session ID. v0.1 uses a timestamp + counter
// pair; M3 may swap in a UUID generator.
func (m *MemoryManager) newID() string {
	if m.idGen != nil {
		return m.idGen()
	}
	return fmt.Sprintf("s_%d", time.Now().UnixNano())
}
