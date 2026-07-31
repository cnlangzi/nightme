package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
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
func NewMemoryManager(reg *agent.Registry, cb EventCallback) *MemoryManager {
	return &MemoryManager{
		sessions:  make(map[string]*Session),
		chatIndex: make(map[string]string),
		agents:    reg,
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
	if m.agents == nil {
		return nil, errors.New("session: agent registry is nil")
	}

	a, err := m.agents.Get(req.Agent)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	if err := a.Detect(); err != nil {
		return nil, fmt.Errorf("session: detect %s: %w", req.Agent, err)
	}

	agentSession, err := a.Start(ctx, agent.StartConfig{
		Workspace: req.Workspace,
		Args:      req.Args,
	})
	if err != nil {
		return nil, fmt.Errorf("session: start %s: %w", req.Agent, err)
	}

	m.mu.Lock()
	if _, exists := m.chatIndex[req.ChatID]; exists {
		m.mu.Unlock()
		_ = agentSession.Close()
		return nil, ErrChatAlreadyBound
	}

	now := m.now()
	sess := &Session{
		ID:           m.newID(),
		ChatID:       req.ChatID,
		Workspace:    req.Workspace,
		Agent:        req.Agent,
		Args:         append([]string(nil), req.Args...),
		StartedAt:    now,
		LastRunAt:    now,
	}
	sess.setLifecycle(StatusRunning, agentSession, 0, nil)
	m.sessions[sess.ID] = sess
	m.chatIndex[req.ChatID] = sess.ID
	m.mu.Unlock()

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
	return nil
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
	if s.agentSession == nil {
		return
	}
	for ev := range s.agentSession.Events() {
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