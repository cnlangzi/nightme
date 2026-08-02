// Package chatsession — AgentSession (v1.2 per-CLI-process handle).
//
// See docs/SPEC.md v1.2 §1.1 and docs/feat/F-29-agent-session-pool.md
// for the full model. In v1.2 the AgentSession replaces v1.1's
// Session type for process ownership; the per-chat ChatSession owns
// the pool of AgentSessions.
package chatsession

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// Status mirrors registry.Status (kept in sync; do not introduce
// divergence).
type Status = registry.Status

const (
	StatusRunning   = registry.StatusRunning
	StatusDetached  = registry.StatusDetached
	StatusExited    = registry.StatusExited
)

// AgentSession represents one CLI process handle inside a
// ChatSession's pool.
//
// Identity: (ChatSessionID, Agent, Cwd) is unique within the pool.
// Agent and Cwd are immutable post-construction; the only way to
// "change" them is to spawn a different AgentSession.
//
// v1.2 commit 6: data structures + state tracking only. Actual
// process spawn (Bridge integration, fork-exec) lands in commit 7.
// For now, this struct is a passive handle that tracks status/pid
// but does not fork.
type AgentSession struct {
	// Identity (immutable post-construction).
	ID            string
	ChatSessionID string
	Agent         string
	Cwd           string

	// Lifecycle. pid is atomic; status is mutex-guarded (Status is a
	// string alias, so atomic.Int32 cannot hold it directly).
	pid    atomic.Int32
	status sync.RWMutex // value: Status
	stat   Status

	// Spawn-time args; preserved across respawn.
	args []string

	// Timestamps.
	createdAt time.Time
	lastRunAt time.Time

	// Exit code (when status == exited).
	exitCodeMu sync.RWMutex
	exitCode   *int
}

// NewAgentSession creates a new AgentSession in memory. The pool
// caller is responsible for adding it to the ChatSession's pool and
// persisting via registry.AgentSessionFile.
func NewAgentSession(id, chatSessionID, agent, cwd string, args []string) *AgentSession {
	as := &AgentSession{
		ID:            id,
		ChatSessionID: chatSessionID,
		Agent:         agent,
		Cwd:           cwd,
		args:          append([]string(nil), args...),
		createdAt:     time.Now(),
		lastRunAt:     time.Now(),
		stat:          StatusDetached,
	}
	as.pid.Store(0)
	return as
}

// FromAgentSessionEntry reconstructs an AgentSession from persisted
// data. Process is not running (status is whatever the entry
// recorded; typically Detached or Exited on restart).
func FromAgentSessionEntry(e *registry.AgentSessionEntry) *AgentSession {
	if e == nil {
		return nil
	}
	as := &AgentSession{
		ID:            e.ID,
		ChatSessionID: e.ChatSessionID,
		Agent:         e.Agent,
		Cwd:           e.Cwd,
		args:          append([]string(nil), e.Args...),
		createdAt:     e.CreatedAt,
		lastRunAt:     e.LastRunAt,
		stat:          e.Status,
	}
	as.pid.Store(int32(e.PID))
	if e.ExitCode != nil {
		as.exitCodeMu.Lock()
		as.exitCode = e.ExitCode
		as.exitCodeMu.Unlock()
	}
	return as
}

// PID returns the current OS process ID (0 if not running).
func (as *AgentSession) PID() int {
	return int(as.pid.Load())
}

// Status returns the current lifecycle state.
func (as *AgentSession) Status() Status {
	as.status.RLock()
	defer as.status.RUnlock()
	return as.stat
}

// Args returns a copy of the spawn arguments.
func (as *AgentSession) Args() []string {
	out := make([]string, len(as.args))
	copy(out, as.args)
	return out
}

// CreatedAt returns when this AgentSession was first created.
func (as *AgentSession) CreatedAt() time.Time {
	return as.createdAt
}

// LastRunAt returns when this AgentSession was last touched
// (status change / spawn).
func (as *AgentSession) LastRunAt() time.Time {
	return as.lastRunAt
}

// ExitCode returns the exit code (nil if not exited).
func (as *AgentSession) ExitCode() *int {
	as.exitCodeMu.RLock()
	defer as.exitCodeMu.RUnlock()
	if as.exitCode == nil {
		return nil
	}
	v := *as.exitCode
	return &v
}

// SetRunning marks the AgentSession as running with the given PID.
// Bumps LastRunAt.
func (as *AgentSession) SetRunning(pid int) {
	as.pid.Store(int32(pid))
	as.status.Lock()
	as.stat = StatusRunning
	as.status.Unlock()
	as.lastRunAt = time.Now()
	as.exitCodeMu.Lock()
	as.exitCode = nil
	as.exitCodeMu.Unlock()
}

// SetDetached marks the AgentSession as detached (process alive but
// nightme no longer holds it; e.g. after daemon SIGTERM without
// --cleanup). PID is preserved.
func (as *AgentSession) SetDetached() {
	as.status.Lock()
	as.stat = StatusDetached
	as.status.Unlock()
	as.lastRunAt = time.Now()
}

// SetExited marks the AgentSession as exited with the given exit
// code. PID is cleared.
func (as *AgentSession) SetExited(code int) {
	as.pid.Store(0)
	as.status.Lock()
	as.stat = StatusExited
	as.status.Unlock()
	as.lastRunAt = time.Now()
	as.exitCodeMu.Lock()
	as.exitCode = &code
	as.exitCodeMu.Unlock()
}

// Entry returns a snapshot of this AgentSession as a registry entry
// (for persistence).
func (as *AgentSession) Entry() *registry.AgentSessionEntry {
	as.exitCodeMu.RLock()
	var ec *int
	if as.exitCode != nil {
		v := *as.exitCode
		ec = &v
	}
	as.exitCodeMu.RUnlock()

	return &registry.AgentSessionEntry{
		ID:            as.ID,
		ChatSessionID: as.ChatSessionID,
		Agent:         as.Agent,
		Cwd:           as.Cwd,
		PID:           as.PID(),
		Status:        as.Status(),
		Args:          as.Args(),
		CreatedAt:     as.createdAt,
		LastRunAt:     as.lastRunAt,
		ExitCode:      ec,
	}
}

// agentCwdKey is the pool map key.
type agentCwdKey struct {
	Agent string
	Cwd   string
}

// ErrAgentNotFound indicates a pool lookup miss. Callers may use
// errors.Is to detect and decide whether to spawn.
var ErrAgentNotFound = errors.New("chatsession: agent not in pool")