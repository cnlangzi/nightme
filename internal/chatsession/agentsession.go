// Package chatsession — AgentSession (v1.2 per-CLI-process handle).
//
// See docs/SPEC.md v1.2 §1.1 and docs/feat/F-29-agent-session-pool.md
// for the full model. In v1.2 the AgentSession replaces v1.1's
// Session type for process ownership; the per-chat ChatSession owns
// the pool of AgentSessions.
package chatsession

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
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
// commit 7: actual spawn integration via Spawner. The bridge-level
// handle (agent.AgentSession) is stored in `handle` and is the
// source of Events / SendText / SendBlocks / Close. Lifecycle
// transitions (Running → Exited) are observed via the handle's
// events channel and trigger SetExited on this struct.
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

	// handle is the bridge-level live session (returned by
	// agent.Start). nil until Spawn succeeds. Committed to the
	// caller (readPump) only after SetRunning is called.
	handleMu sync.RWMutex
	handle   agent.AgentSession

	// events is a tee of handle.Events() that signals handle-side
	// close (last chan close → SetExited). Created in Spawn; nil
	// before that.
	handleEventsClosed chan struct{}
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

// ErrNoActiveAgent is returned by LookupActiveAgentSession when
// neither cs.activeAgent (set via /use) nor cs.defaultAgent
// (snapshot of cfg.Primary at creation) is set. The runtime
// must configure defaultAgent on ChatSession construction
// (chatsession.NewManager.GetOrCreate takes it as a parameter);
// an empty defaultAgent indicates a misconfigured daemon.
var ErrNoActiveAgent = errors.New("chatsession: no activeAgent and no defaultAgent")

// ErrNotRunning is returned by SendText/SendBlocks/Close when called
// before Spawn() succeeds.
var ErrNotRunning = errors.New("chatsession: AgentSession not running (Spawn not called or failed)")

// Spawn materializes the bridge-level child process via the given
// Spawner. On success, the AgentSession transitions from
// Detached → Running and PID is set. Spawn is idempotent: a second
// call on an already-running session is a no-op (returns nil).
//
// If a previous spawn left status=Exited (e.g., the child died),
// Spawn acts as Respawn: a fresh child is forked, but the
// AgentSession.ID is preserved (pool identity continuity).
func (as *AgentSession) Spawn(ctx context.Context, spawner Spawner) error {
	if spawner == nil {
		return ErrSpawnerNotSet
	}

	as.handleMu.Lock()
	defer as.handleMu.Unlock()

	if as.handle != nil && as.Status() == StatusRunning {
		return nil // already running
	}

	handle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args)
	if err != nil {
		return fmt.Errorf("chatsession: spawn %s at %s: %w", as.Agent, as.Cwd, err)
	}

	as.handle = handle
	as.SetRunning(handle.PID())
	return nil
}

// Handle returns the bridge-level agent.AgentSession (nil if not yet
// spawned). Exposed for callers that need direct access (e.g., the
// ChatSession EventCallback installer).
func (as *AgentSession) Handle() agent.AgentSession {
	as.handleMu.RLock()
	defer as.handleMu.RUnlock()
	return as.handle
}

// Events returns the live event channel from the bridge. Returns nil
// before Spawn succeeds. The returned channel is closed by the
// bridge after EventDone / EventError / child EOF; AgentSession
// status transitions to Exited at that point (see ObserveClose).
func (as *AgentSession) Events() <-chan agent.AgentEvent {
	as.handleMu.RLock()
	defer as.handleMu.RUnlock()
	if as.handle == nil {
		return nil
	}
	return as.handle.Events()
}

// SendText delivers text to the bridge child. Returns ErrNotRunning
// if Spawn has not been called.
func (as *AgentSession) SendText(text string) error {
	as.handleMu.RLock()
	h := as.handle
	as.handleMu.RUnlock()
	if h == nil {
		return ErrNotRunning
	}
	return h.SendText(text)
}

// SendBlocks delivers structured content blocks. Returns ErrNotRunning
// if Spawn has not been called.
func (as *AgentSession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	as.handleMu.RLock()
	h := as.handle
	as.handleMu.RUnlock()
	if h == nil {
		return ErrNotRunning
	}
	return h.SendBlocks(ctx, blocks)
}

// Close terminates the bridge child (sends shutdown signal to the
// underlying bridge). Idempotent. Marks status=Exited on success.
func (as *AgentSession) Close() error {
	as.handleMu.Lock()
	h := as.handle
	as.handleMu.Unlock()
	if h == nil {
		return nil // not running
	}
	if err := h.Close(); err != nil {
		return err
	}
	return nil
}

// ObserveClose runs in a goroutine after Spawn to watch the bridge
// events channel. When the channel closes (EventDone / EventError /
// child EOF), the AgentSession transitions to Exited. Returns a
// channel that the caller can wait on for clean shutdown.
//
// Convention: ChatSession starts one ObserveClose per AgentSession
// in its pool.
func (as *AgentSession) ObserveClose() <-chan struct{} {
	done := make(chan struct{})
	as.handleMu.RLock()
	ev := as.handle.Events()
	as.handleMu.RUnlock()

	go func() {
		defer close(done)
		if ev == nil {
			return
		}
		// Drain until close.
		for range ev {
		}
		as.SetExited(0)
	}()
	return done
}