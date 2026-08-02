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
// received from a running session. v1.1 consumers are the Gateway
// (see docs/feat/F-26-gateway-hub.md §2.3) — it drives Translate +
// channel.Send + receipt state transitions. Tests use it to observe
// the event stream.
//
// The callback runs on the Manager's readPump goroutine; it must
// return quickly or it will block the next event for this session.
//
// v1.1: this is the ONLY consumer of session.Events() (single-
// consumer rule). Earlier versions had a second reader (the gateway
// pumpOutbound goroutine) racing on the same channel — that race
// was the v0.2.x output bug. See F-26 §2.3.
type EventCallback func(s *Session, ev agent.AgentEvent)

// CreateRequest is the input to Manager.Create.
//
// v1.1: ChatID / ChatType / OnUserMessage removed. Chat-binding is
// no longer the Session Manager's concern (see F-26 §5); the
// Gateway (or, for commit 2, the runtime bridge in cmd/nightme)
// owns the chat → session mapping.
//
// Agent integration hooks move to the Gateway:
type CreateRequest struct {
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
//
// v1.1: the Manager is a pure-process factory. It knows nothing
// about chat IDs, channels, receipts, or bindings. Chat-keyed
// operations (CreateOrUpdate, Run, GetByChat, KillByChat) lived
// here in v0.x but were removed by F-26 §6 commits 2 + 3. The
// binding table is now owned by the Gateway itself
// (see internal/gateway/gateway.go).
type Manager interface {
	// Register creates a session record WITHOUT spawning the
	// agent. The returned session has Status == StatusDetached;
	// /run will spawn the agent later via Create.
	//
	// v1.1 (F-26 §6 commit 2): the two-step /cwd + /run flow
	// (per PRD §4) is preserved — /cwd calls Register, /run calls
	// Create (or noop if the session is already running).
	Register(ctx context.Context, req CreateRequest) (*Session, error)

	// Create registers a new session and starts its underlying
	// agent. The returned Session has Status == StatusRunning when
	// Start succeeds.
	Create(ctx context.Context, req CreateRequest) (*Session, error)

	// Get returns the session with the given ID, or
	// ErrSessionNotFound.
	Get(sid string) (*Session, error)

	// List returns a snapshot of all known sessions. Order is
	// unspecified; the slice is freshly allocated.
	List() []*Session

	// Kill terminates the agent backing sid and marks the session
	// StatusExited. Idempotent: killing an already-exited session
	// is a no-op and returns nil.
	Kill(sid string) error

	// SetEventCallback installs the per-event callback. Called by
	// the runtime at startup; passing nil restores the no-op
	// default (events drained and discarded). Single-threaded;
	// not safe to call concurrently with itself.
	SetEventCallback(cb EventCallback)

	// Restore reads persisted session entries from the registry and
	// rebuilds the in-memory table. Entries whose persisted status
	// was StatusRunning are loaded as StatusDetached (their PID may
	// have died while nightme was down — the next Create / /run
	// will decide whether to respawn). Restore is idempotent:
	// calling it on an already-populated manager returns nil
	// without touching state.
	Restore(ctx context.Context) error

	// Persist writes the current session table to the registry.
	// Sessions are written as-is; callers that want to flush a
	// terminal state should call Kill first.
	Persist() error

	// MarkDetached releases the live handle without closing the
	// underlying agent. The daemon shutdown policy (default
	// "detach, don't kill") goes through this method. Already-
	// exited sessions are left unchanged.
	MarkDetached(sid string) error
}

// Errors surfaced by the Manager. Sentinel values so callers can
// branch with errors.Is.
var (
	// ErrSessionNotFound is returned by Get when no session matches
	// the lookup key.
	ErrSessionNotFound = errors.New("session: not found")
)

// MemoryManager is the production Manager: a mutex-protected map
// keyed by session ID.
//
// v1.1: chatIndex removed. The chat → session binding lives in the
// runtime bridge (commit 2) or in the Gateway (commit 3) — see
// F-26 §6.
type MemoryManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session

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
		sessions: make(map[string]*Session),
		agents:   agents,
		reg:      reg,
		callback: cb,
	}
}

// Register creates a new session record in StatusDetached without
// spawning the agent. The /cwd slash command calls this to record
// a chat → workspace binding before /run is invoked.
//
// v1.1 (F-26 §6 commit 2): replaces the v0.x CreateOrUpdate flow.
// The Manager stays chat-id-agnostic; the runtime chatCoordinator
// maintains the chatID → sessionID map.
func (m *MemoryManager) Register(ctx context.Context, req CreateRequest) (*Session, error) {
	if req.Workspace == "" {
		return nil, errors.New("session: Workspace is required")
	}
	if req.Agent == "" {
		return nil, errors.New("session: Agent is required")
	}

	now := m.now()
	sess := &Session{
		ID:        m.newID(),
		Workspace: req.Workspace,
		Agent:     req.Agent,
		Args:      append([]string(nil), req.Args...),
		StartedAt: now,
		LastRunAt: now,
	}
	sess.setLifecycle(StatusDetached, nil, 0, nil)

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	if err := m.upsertEntry(sess, registry.StatusDetached, -1); err != nil {
		m.mu.Lock()
		delete(m.sessions, sess.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("session: persist: %w", err)
	}

	_ = ctx // reserved for future use (e.g. ctx-aware persist)
	return sess, nil
}

// Create spawns the agent for an existing registered session (or
// spawns a fresh session if sid is empty). The session transitions
// StatusDetached/StatusExited → StatusRunning.
//
// On success the new/already-created session is returned with
// Status = StatusRunning and a background goroutine consumes its
// events.
func (m *MemoryManager) Create(ctx context.Context, req CreateRequest) (*Session, error) {
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

	now := m.now()
	sess := &Session{
		ID:        m.newID(),
		Workspace: req.Workspace,
		Agent:     req.Agent,
		Args:      append([]string(nil), req.Args...),
		StartedAt: now,
		LastRunAt: now,
	}
	sess.setLifecycle(StatusRunning, agentSession, pid, nil)

	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	// Pre-warm the InputBuffer so the readPump can route events
	// into the right state from the very first tick. The default
	// flush callback already calls SendText on the live agent.
	sess.EnsureInputBuffer()

	if err := m.upsertEntry(sess, registry.StatusRunning, 0); err != nil {
		// Persistence is best-effort for Create: the agent is
		// already running, but if the registry write fails we
		// surface it so the caller can react. Cleanup: drop the
		// in-memory session and close the agent.
		m.mu.Lock()
		delete(m.sessions, sess.ID)
		m.mu.Unlock()
		_ = agentSession.Close()
		return nil, fmt.Errorf("session: persist: %w", err)
	}

	pumpCtx, cancel := context.WithCancel(ctx)
	sess.setCancel(cancel)
	go m.readPump(sess, pumpCtx)

	return sess, nil
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

// MarkDetached releases the manager's live handle for sid without
// closing the underlying agent. This is the daemon shutdown policy:
// the CLI may continue running after nightme exits and the registry
// records the detached state. Already-exited sessions are left
// unchanged.
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

// SetEventCallback installs (or replaces) the per-event callback.
// Called by the runtime at startup. Passing nil restores the no-op
// default. Not safe to call concurrently with itself.
func (m *MemoryManager) SetEventCallback(cb EventCallback) {
	m.mu.Lock()
	m.callback = cb
	m.mu.Unlock()
}

// Restore reads persisted entries from the registry and rebuilds
// the in-memory session table. Each entry's metadata is restored
// verbatim; the lifecycle is mapped as:
//
//	StatusRunning  -> StatusDetached (PID may be dead after restart)
//	StatusDetached -> StatusDetached
//	StatusExited   -> StatusExited
//
// v1.1: no chatIndex rebuild (chat-binding moved to Gateway).
// Restore does NOT respawn agents — the CLI is decoupled from the
// in-memory session record. Callers that want to reattach /
// respawn must iterate List() and use /run. Calling Restore twice
// is a no-op (an already-populated manager keeps its state).
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
	}
	return nil
}

// Persist writes every in-memory session to the registry. It is the
// explicit flush hook for callers that want to serialize state —
// most code paths do not need it because Create / Kill already
// persist.
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
//
// v1.1: ChatID field of registry.Entry is always written as "" (the
// binding belongs to the Gateway's BindingEntry, not the session
// record — see F-05 and commit 5).
func (m *MemoryManager) upsertEntry(s *Session, status registry.Status, exitCode int) error {
	if m.reg == nil {
		return nil
	}
	snap := s.Snapshot()
	entry := registry.Entry{
		SessionID: snap.ID,
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
//
// F-25 integration: the pump also drives the InputBuffer state
// machine. Any "agent is working" event (text/tool/permission) flips
// the buffer to StateBusy; a terminal event (done/error) flips it
// back to StateIdle AND flushes the buffer. This means the channel
// layer doesn't need to know about the agent's internal state — it
// just calls QueueUserMessage and the manager figures out dispatch
// vs buffer.
//
// v1.1: this is the SINGLE CONSUMER of session.Events() (see
// docs/feat/F-26-gateway-hub.md §2.3). The v0.2.x double-reader
// race is gone.
func (m *MemoryManager) readPump(s *Session, ctx context.Context) {
	// Snapshot the agent handle under the per-session lock so Kill
	// (which swaps it for nil) does not race with this goroutine.
	s.mu.RLock()
	as := s.agentSession
	s.mu.RUnlock()
	if as == nil {
		return
	}
	m.mu.RLock()
	cb := m.callback
	m.mu.RUnlock()

	for ev := range as.Events() {
		if cb != nil {
			cb(s, ev)
		}
		// F-25 buffer state tracking. We treat any non-terminal
		// event as "agent is working" (BUSY) so user messages that
		// arrive during this period get buffered. We do NOT touch
		// the buffer when ev is a system/init event since it
		// doesn't indicate meaningful work yet.
		if ev.Kind != agent.EventDone && ev.Kind != agent.EventError {
			if buf := s.InputBuffer(); buf != nil {
				buf.SetState(StateBusy)
			}
		}
		if ev.Kind == agent.EventDone || ev.Kind == agent.EventError {
			// Flush buffer BEFORE marking exited so any
			// queued messages reach the agent before it
			// shuts down (or before the channel reports
			// the session as ended to the user).
			if buf := s.InputBuffer(); buf != nil {
				buf.SetState(StateIdle)
				_ = buf.OnTurnEnded() // best-effort; logged inside buffer
			}
			m.markExited(s, terminalExitCode(ev))
			return
		}
	}

	// Channel closed without a terminal event — treat as exit.
	// Flush any pending buffer (the session is going away; user
	// will see the queued messages lost on next retry).
	if buf := s.InputBuffer(); buf != nil {
		buf.SetState(StateIdle)
	}
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

// newID returns the next session ID. v0.1 uses a timestamp-based
// generator; tests inject a deterministic one via WithIDGenerator.
func (m *MemoryManager) newID() string {
	if m.idGen != nil {
		return m.idGen()
	}
	return fmt.Sprintf("s_%d", time.Now().UnixNano())
}

// WithIDGenerator returns a copy of m that uses gen to mint session
// IDs. Intended for tests; production callers leave idGen nil.
func (m *MemoryManager) WithIDGenerator(gen func() string) *MemoryManager {
	cp := &MemoryManager{
		sessions: m.sessions,
		agents:   m.agents,
		reg:      m.reg,
		callback: m.callback,
		idGen:    gen,
		clock:    m.clock,
	}
	cp.mu = sync.RWMutex{}
	return cp
}

// WithClock returns a copy of m that uses clock as its time source.
// Intended for tests.
func (m *MemoryManager) WithClock(clock func() time.Time) *MemoryManager {
	cp := &MemoryManager{
		sessions: m.sessions,
		agents:   m.agents,
		reg:      m.reg,
		callback: m.callback,
		idGen:    m.idGen,
		clock:    clock,
	}
	cp.mu = sync.RWMutex{}
	return cp
}