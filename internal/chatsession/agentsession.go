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

	// Agent-session-resume id (e.g. Claude Code's
	// `system/init.session_id`). Captured from EventInit on the
	// first run; persisted via Entry; replayed on the next Spawn as
	// `--resume <id>` (Claude Code currently translates this; other
	// bridges ignore it). Empty when the agent has no resume
	// semantics or has not yet emitted its init event.
	resumeIDMu sync.RWMutex
	resumeID   string

	// handle is the bridge-level live session (returned by
	// agent.Start). nil until Spawn succeeds. Committed to the
	// caller (readPump) only after SetRunning is called.
	handleMu sync.RWMutex
	handle   agent.AgentSession

	// events is a tee of handle.Events() that signals handle-side
	// close (last chan close → SetExited). Created in Spawn; nil
	// before that.
	handleEventsClosed chan struct{}

	// F-45: model captured on first EventInit (e.g.
	// "claude-opus-4-5-20250929"). SetModel is idempotent — empty
	// incoming values do NOT overwrite a previously-captured model.
	// Persisted via Entry → AgentSessionEntry.Model; restored on
	// restart via FromAgentSessionEntry. Empty before the first
	// EventInit lands.
	modelMu sync.RWMutex
	model   string

	// F-45: per-AgentSession running total of token / cost stats.
	// AccumulateUsage adds turn-level counts; ResetCumulative zeroes
	// (called only by /new handler); PersistIfDirty writes the entry
	// to disk when dirty. Persisted via Entry → AgentSessionEntry.
	//CumulativeUsage; restored on restart from FromAgentSessionEntry.
	cumulativeUsageMu sync.RWMutex
	cumulativeUsage   agent.UsageInfo
	cumulativeDirty   bool
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
// data. Process is not running on restart — the in-memory handle
// is lost (we don't persist it), so we mark anything persisted as
// StatusRunning as StatusDetached to force a re-spawn on next
// LookupActiveAgentSession. This prevents the "spawned but
// handle=nil" silent-drop bug where SendBlocks returns
// ErrNotRunning and the default FlushHook ignores it.
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
		resumeID:      e.ResumeID,
		model:         e.Model,
	}
	// F-45: restore cumulative token / cost stats. nil on legacy
	// entries (zero-value default = "never ran", will start
	// counting from first EventUsage). We copy the struct by value
	// (not by pointer) so the in-memory state is decoupled from
	// the persisted entry.
	if e.CumulativeUsage != nil {
		as.cumulativeUsage = *e.CumulativeUsage
	}
	// commit fix-6: any persisted "running" agent is actually dead
	// after daemon restart (the process handle is in-memory only).
	// Demote to Detached so the next LookupActiveAgentSession will
	// re-spawn. Persisted PID is also stale; clear it.
	status := e.Status
	if status == StatusRunning {
		status = StatusDetached
	}
	as.stat = status
	as.pid.Store(0)
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

// ResumeID returns the agent's own session id (e.g. Claude Code's
// `system/init.session_id`) captured on the last run. Empty when
// the agent has no resume semantics or has not yet emitted its
// init event.
func (as *AgentSession) ResumeID() string {
	as.resumeIDMu.RLock()
	defer as.resumeIDMu.RUnlock()
	return as.resumeID
}

// SetResumeID records the agent's own session id. Called by the
// runtime's EventHandler when it receives an EventInit with a
// non-empty SessionID. Safe to call concurrently with Spawn /
// SetRunning / SetExited.
func (as *AgentSession) SetResumeID(id string) {
	as.resumeIDMu.Lock()
	as.resumeID = id
	as.resumeIDMu.Unlock()
}

// --- F-45: model + cumulative usage API ----------------------------

// SetModel records the agent's selected model (e.g. Claude Code:
// system/init.model). Idempotent: an empty incoming value does NOT
// overwrite a previously-captured model — bridges may re-emit
// EventInit after a child restart with a blank Model and we don't
// want to wipe the prior capture. Called by the runtime's
// EventHandler closure on EventInit.
//
// Safe to call concurrently with Model() and other lifecycle
// methods.
func (as *AgentSession) SetModel(m string) {
	if m == "" {
		return
	}
	as.modelMu.Lock()
	as.model = m
	as.modelMu.Unlock()
}

// Model returns the captured model name. Empty before the first
// EventInit lands or when the bridge does not report one.
func (as *AgentSession) Model() string {
	as.modelMu.RLock()
	defer as.modelMu.RUnlock()
	return as.model
}

// AccumulateUsage atomically adds u's per-turn counters to this
// AgentSession's cumulative state and marks it dirty so the next
// PersistIfDirty writes the entry. Called by the runtime's
// EventHandler closure on every EventUsage arriving from the
// bridge — typically once per turn. nil u is a no-op (defensive:
// some bridges emit empty EventUsage as "no usage this turn").
//
// RWMutex lets concurrent footer rendering skip the lock when no
// EventUsage is in flight; writers (one EventHandler closure) and
// readers (footer rendering, future /cost) contend at turn-end
// frequency which is human-paced (≤ 1/s).
func (as *AgentSession) AccumulateUsage(u *agent.UsageEvent) {
	if u == nil {
		return
	}
	as.cumulativeUsageMu.Lock()
	defer as.cumulativeUsageMu.Unlock()
	as.cumulativeUsage.InputTokens += u.InputTokens
	as.cumulativeUsage.OutputTokens += u.OutputTokens
	as.cumulativeUsage.CacheCreationInputTokens += u.CacheCreationInputTokens
	as.cumulativeUsage.CacheReadInputTokens += u.CacheReadInputTokens
	as.cumulativeUsage.CostUSD += u.CostUSD
	as.cumulativeDirty = true
}

// CumulativeUsage returns a snapshot of this AgentSession's
// running totals. Safe to call from any goroutine; readers
// (footer rendering, future /cost) get the consistent struct
// copy under RLock without contending with EventUsage writers.
func (as *AgentSession) CumulativeUsage() agent.UsageInfo {
	as.cumulativeUsageMu.RLock()
	defer as.cumulativeUsageMu.RUnlock()
	return as.cumulativeUsage
}

// ResetCumulative zeroes the cumulative state and marks the entry
// dirty for the next PersistIfDirty. Called only by /new handler
// — /kill, /cwd, /use, daemon restart and process crash all leave
// the cumulative intact (history is valuable to the user).
//
// The handle / pool / status are not touched — ResetCumulative is
// purely "clear the counter and mark dirty"; the caller
// (handleNew) is responsible for the surrounding PersistAgentSession.
func (as *AgentSession) ResetCumulative() {
	as.cumulativeUsageMu.Lock()
	as.cumulativeUsage = agent.UsageInfo{}
	as.cumulativeDirty = true
	as.cumulativeUsageMu.Unlock()
}

// PersistIfDirty writes the AgentSession entry to disk when the
// cumulative stats have changed since the last persist. The
// runtime calls this on EventDone (turn end) so each turn costs
// at most one file write; on clean state it is a no-op.
//
// persist is the registry callback (typically
// Manager.PersistAgentSession). Returns nil when clean (no I/O).
func (as *AgentSession) PersistIfDirty(persist func(*registry.AgentSessionEntry) error) error {
	if persist == nil {
		return nil
	}
	as.cumulativeUsageMu.Lock()
	if !as.cumulativeDirty {
		as.cumulativeUsageMu.Unlock()
		return nil
	}
	as.cumulativeDirty = false
	as.cumulativeUsageMu.Unlock()
	return persist(as.Entry())
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

	// F-45: snapshot cumulative stats under RLock so we publish a
	// consistent copy. Always emit a non-nil pointer — the legacy
	// "never ran" case is distinguishable only by zero-value
	// counters inside, and omitempty would skip the field for
	// "ran but all zero" which obscures the persisted state.
	as.cumulativeUsageMu.RLock()
	cum := as.cumulativeUsage
	as.cumulativeUsageMu.RUnlock()

	return &registry.AgentSessionEntry{
		ID:              as.ID,
		ChatSessionID:   as.ChatSessionID,
		Agent:           as.Agent,
		Cwd:             as.Cwd,
		PID:             as.PID(),
		Status:          as.Status(),
		Args:            as.Args(),
		ResumeID:        as.ResumeID(),
		CreatedAt:       as.createdAt,
		LastRunAt:       as.lastRunAt,
		ExitCode:        ec,
		Model:           as.Model(),
		CumulativeUsage: &cum,
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
// cs.activeAgent is empty. The runtime seeds activeAgent from
// cfg.Primary at ChatSession construction (via
// chatsession.NewManager.GetOrCreate); an empty primary at
// construction indicates a misconfigured daemon (no global default
// set in config.yaml).
var ErrNoActiveAgent = errors.New("chatsession: activeAgent is empty (cfg.Primary snapshot was empty at construction)")

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

	handle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args, as.ResumeID())
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

// New delegates to the bridge AgentSession.New(). Returns ErrNotRunning
// if the bridge handle is not currently attached (status is Detached
// or Exited).
//
// F-34: resets the conversation context on the running session.
// Three outcomes, in priority order:
//
//   - Bridge handles in-place reset (pi's new_session RPC, claude
//     code's writeLine("/clear"), acp's session/new): New returns
//     nil. AgentSession.ID / Cwd / pool membership are preserved;
//     only the bridge's internal conversation state is cleared.
//     The bridge is expected to emit a fresh EventInit carrying the
//     new SessionID; the runtime's eventHandler (cmd/nightme/run.go
//     newEventHandler) captures it via SetResumeID and persists.
//
//   - Bridge cannot do in-place reset (raw PTY bridge): bridge.New
//     returns agent.ErrRestartRequired. The wrapper then kills the
//     existing bridge handle and spawns a fresh one via spawner
//     (with ResumeID="" so the new child starts with no --resume).
//     ResumeID is explicitly cleared on the wrapper so persistence
//     stays consistent.
//
//   - Bridge tried but failed (transient error): wrapped and
//     propagated. InputBuffer is NOT cleared by the wrapper in this
//     case (caller's responsibility).
//
// spawner may be nil when the bridge is known to handle in-place
// reset; in that case agent.ErrRestartRequired from the bridge
// surfaces as-is. ChatSession.NewActiveAgentSessions always passes
// the chat's configured spawner.
func (as *AgentSession) New(ctx context.Context, spawner Spawner) error {
	as.handleMu.Lock()
	defer as.handleMu.Unlock()

	h := as.handle
	if h == nil {
		return ErrNotRunning
	}

	if err := h.New(ctx); !errors.Is(err, agent.ErrRestartRequired) {
		// nil or real (non-restart) error: pass through.
		return err
	}

	// Bridge cannot reset in-place. Fall back to kill + respawn via
	// the Spawner. This path is taken by the raw PTY bridge today;
	// claudecode / pi / acp all handle reset in-place and never
	// return ErrRestartRequired.
	if spawner == nil {
		return agent.ErrRestartRequired
	}

	// Close the old handle before spawning the replacement so the
	// underlying process / transport tears down cleanly. We swallow
	// the close error — the new spawn below is the source of truth
	// for "did reset succeed".
	_ = h.Close()
	as.handle = nil

	newHandle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args, "")
	if err != nil {
		// F-34 Phase 3 review: previously this branch returned
		// without updating status, leaving as.status=StatusRunning
		// with the OLD PID and as.handle=nil. Subsequent
		// LookupActiveAgentSession would see Running + nil handle
		// and fail every SendBlocks with ErrNotRunning. Mark as
		// Exited so the next user message lazy-spawns a fresh AS.
		as.SetExited(-1)
		as.SetResumeID("")
		return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
	}
	as.handle = newHandle
	as.SetRunning(newHandle.PID())
	// Explicitly clear ResumeID so a stale id never gets replayed on
	// the next respawn (the new child will emit its own EventInit,
	// and the runtime's eventHandler will SetResumeID via the normal
	// path).
	as.SetResumeID("")
	return nil
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