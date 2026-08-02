// Package session is the Session Manager: a pure-process factory
// for nightme sessions.
//
// In v1.1 (see docs/feat/F-26-gateway-hub.md §5 and §6 commit 2)
// the package is a pure domain object — it knows nothing about
// chat_id, channel, receipt, or binding. It exposes only
// session-ID-keyed operations; the chat → session binding lives in
// the Gateway (commit 3) or, in this commit, in a thin runtime
// bridge in cmd/nightme.
//
// A Session wraps one agent.AgentSession (PTY / ACP / SDK / JSON-IO)
// and the metadata needed to spawn / introspect it (workspace,
// agent name, args, pid). The Manager owns the lifecycle:
//
//   - Create    — register a new session and spawn its agent
//   - Get       — resolve session ID → Session
//   - List      — snapshot all sessions
//   - Kill      — terminate the agent and mark exited
//   - Restore   — rehydrate in-memory state from the registry
//   - Persist   — flush every session to the registry
//   - MarkDetached — release the live handle without killing the
//                    child process (default shutdown policy)
//
// Persistence lives in the registry.File added in PR #1; the
// MemoryManager here is the in-memory companion that drives event
// consumption.
package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Status is the lifecycle state of a Session. It mirrors the
// registry.Status vocabulary but is independent — the in-memory
// Manager does not persist (that's the registry's job).
type Status string

const (
	// StatusRunning means the underlying agent process is alive and
	// the Session is consuming its events.
	StatusRunning Status = "running"

	// StatusDetached means the process is alive but nightme no
	// longer holds it (e.g. after a graceful shutdown). v0.1 does
	// not surface this state to users; it is reserved for the
	// reattach logic in M2 / M3.
	StatusDetached Status = "detached"

	// StatusExited means the agent process has terminated and the
	// Session has no live AgentSession behind it.
	StatusExited Status = "exited"
)

// Session is the in-memory representation of one agent process.
// Fields are exported so v1.1 callers (Gateway handlers, status
// commands) can render them in user-facing messages; the
// agentSession / cancel fields are unexported because the Manager
// owns them.
//
// v1.1: Session no longer carries ChatID / ChatType. The chat →
// session binding is owned by the Gateway (see F-26 §2.4).
//
// The mutable fields (Status, PID, ExitCode, the unexported
// agentSession, cancel) are guarded by mu. Callers that read or
// write them MUST take mu via Snapshot / SetLifecycle. The metadata
// fields (ID, Workspace, Agent, Args, StartedAt, LastRunAt) are
// immutable after Create returns.
type Session struct {
	mu sync.RWMutex

	// ID is the unique session identifier.
	ID string

	// Workspace is the directory the agent operates in. It is bound
	// at creation time and never changes for the lifetime of the
	// Session.
	Workspace string

	// Agent is the registry name of the Agent (e.g. "claude").
	Agent string

	// Args is the argv passed to the agent at spawn time. Empty in
	// v0.1.
	Args []string

	// PID is the OS process id of the spawned CLI. Zero means the
	// session has no live child (StatusExited).
	PID int

	// StartedAt is when Create ran.
	StartedAt time.Time

	// LastRunAt is when the agent was last (re)started; same as
	// StartedAt for the initial Create.
	LastRunAt time.Time

	// status mirrors the agent lifecycle (see Status constants).
	status Status

	// exitCode is set when status == StatusExited; nil otherwise.
	exitCode *int

	// agentSession is the live agent handle owned by the Manager.
	agentSession agent.AgentSession

	// cancel terminates the per-session readPump goroutine.
	cancel context.CancelFunc

	// inputBuffer queues user messages arriving while the agent is
	// busy (F-25). Lazily created by EnsureInputBuffer; never
	// serialised / persisted (per spec: in-memory only, restart loses).
	inputBuffer *InputBuffer
}

// Status returns the current lifecycle status. It acquires the
// per-session read lock so it is safe to call concurrently with the
// readPump goroutine.
func (s *Session) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// MarkDetached changes a live session to detached without touching the
// underlying agent handle. The MemoryManager uses this during daemon shutdown
// before it releases its own handle.
func (s *Session) MarkDetached() {
	s.mu.Lock()
	if s.status != StatusExited {
		s.status = StatusDetached
	}
	s.mu.Unlock()
}

// ExitCode returns the exit code recorded for an exited session, or
// nil if the session is still running.
func (s *Session) ExitCode() *int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitCode
}

// Snapshot returns an immutable copy of the session's user-visible
// state. Callers can read every field without holding the lock.
// Internal handles (agentSession / cancel) are deliberately omitted.
//
// v1.1: no ChatID / ChatType fields (binding is Gateway's).
type Snapshot struct {
	ID        string
	Workspace string
	Agent     string
	Args      []string
	PID       int
	StartedAt time.Time
	LastRunAt time.Time
	Status    Status
	ExitCode  *int
}

// Snapshot copies the session's public state under the read lock.
func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		ID:        s.ID,
		Workspace: s.Workspace,
		Agent:     s.Agent,
		Args:      append([]string(nil), s.Args...),
		PID:       s.PID,
		StartedAt: s.StartedAt,
		LastRunAt: s.LastRunAt,
		Status:    s.status,
		ExitCode:  s.exitCode,
	}
}

// setLifecycle atomically updates the mutable lifecycle fields
// (status, PID, exitCode) and the live agent handles. The Manager
// uses this on Create and Kill; it is not part of the public API.
func (s *Session) setLifecycle(status Status, as agent.AgentSession, pid int, code *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.PID = pid
	s.exitCode = code
	s.agentSession = as
}

// takeAgent returns the live agent handle and its cancel func, then
// clears them so the session is left without an active agent. Used
// by Kill to capture the handles under the per-session lock before
// invoking Close() outside the lock.
func (s *Session) takeAgent() (agent.AgentSession, context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	as := s.agentSession
	cancel := s.cancel
	s.agentSession = nil
	s.cancel = nil
	return as, cancel
}

// setCancel stores the cancel func that terminates this session's
// readPump. Called by Create after the goroutine is spawned.
func (s *Session) setCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

// Events exposes the underlying AgentSession's event channel to
// external consumers (CLI tools, tests). Returns nil when the
// session has no live agent handle (e.g. StatusExited). Callers
// must respect the agent.AgentSession contract: the channel closes
// after the terminal EventDone / EventError.
func (s *Session) Events() <-chan agent.AgentEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.agentSession == nil {
		return nil
	}
	return s.agentSession.Events()
}

// SendText proxies to the underlying AgentSession. Returns an error
// when the session has no live agent handle; callers should check
// Status() before invoking SendText.
func (s *Session) SendText(text string) error {
	s.mu.RLock()
	as := s.agentSession
	s.mu.RUnlock()
	if as == nil {
		return errors.New("session: no live agent")
	}
	return as.SendText(text)
}

// SendBlocks proxies to the underlying AgentSession for structured
// (rich-text) user input. Same nil-handle semantics as SendText.
func (s *Session) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	s.mu.RLock()
	as := s.agentSession
	s.mu.RUnlock()
	if as == nil {
		return errors.New("session: no live agent")
	}
	return as.SendBlocks(ctx, blocks)
}

// EnsureInputBuffer lazily constructs and returns the per-session
// InputBuffer. The first call wires up the onFlush hook to call
// SendBlocks on the live agent; subsequent calls return the existing
// buffer.
//
// The buffer is in-memory only. If the session is reloaded from
// disk after a nightme restart, a fresh empty buffer is created —
// any pending messages from before the restart are silently lost
// (per F-25 spec: "重启全丢，user retry").
func (s *Session) EnsureInputBuffer() *InputBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputBuffer != nil {
		return s.inputBuffer
	}
	hook := func(blocks []agent.ContentBlock, _ []string) error {
		s.mu.RLock()
		as := s.agentSession
		s.mu.RUnlock()
		if as == nil {
			return errors.New("session: no live agent for flush")
		}
		return as.SendBlocks(context.Background(), blocks)
	}
	s.inputBuffer = NewInputBuffer(hook, 50, 100*1024)
	return s.inputBuffer
}

// InputBuffer returns the existing buffer or nil if EnsureInputBuffer
// has not been called. Read-only access for state inspection
// (e.g. /status command).
func (s *Session) InputBuffer() *InputBuffer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputBuffer
}

// QueueUserMessage is the F-25 entry point for IM-arriving messages.
// The session decides whether to dispatch immediately (state=Idle)
// or buffer (state=Busy). The caller does not need to know.
//
// blocks is the structured user turn — text + optional image /
// file attachments. The buffer stores blocks verbatim (not as
// strings) so image references survive across the busy → idle
// flush boundary without losing the attachment path.
//
// userMsgID is propagated through the buffer so the channel layer
// can update the corresponding MessageReceipt on flush.
func (s *Session) QueueUserMessage(blocks []agent.ContentBlock, userMsgID string) error {
	if len(blocks) == 0 {
		return nil
	}
	buf := s.EnsureInputBuffer()
	return buf.Add(blocks, userMsgID)
}
