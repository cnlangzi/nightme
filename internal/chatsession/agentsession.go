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
	"log/slog"
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
//
// Concurrency: ONE mutex (asMu) guards every mutable field on
// this struct. Why: in this codebase every accessor is called by
// either (a) the runtime EventHandler on a single goroutine per
// turn, or (b) lifecycle methods that already serialize on
// Spawn/Close. turnMu at the bridge layer guarantees only one
// prompt is in flight at a time, so writers within one AS never
// overlap meaningfully. Field-level locks added no real
// parallelism — they added reasoning cost (7-way lock ordering)
// for zero throughput benefit. PID() is hot (log lines, footer,
// KillAll fan-out) so asMu is RWMutex; readers can run
// concurrently without contending with each other.
type AgentSession struct {
	// Identity (immutable post-construction).
	ID            string
	ChatSessionID string
	Agent         string
	Cwd           string

	// asMu guards every field below. RWMutex so concurrent readers
	// (Status, Model, ExitCode, Handle, Events, PID) don't contend
	// with each other when no writer is in flight. (F-49 removed
	// CompactionCount from this list — the compaction counter is
	// gone from AgentSession entirely.)
	asMu sync.RWMutex

	// opCtx is the per-AgentSession operation context — owned by
	// THIS AgentSession, not by ChatSession. The chat layer hands
	// us its parent (cs.Context()) and we derive our own ctx from
	// it via Activate(parent). The bridge layer reads OpContext()
	// and derives its own per-call callCtx inside SendBlocks.
	//
	// Lifecycle is internal to AS:
	//   - Activate(parent) — installs a fresh opCtx derived from
	//     parent; called by the runtime when this AS becomes the
	//     active one for a chat (e.g. /use promoted us, or a
	//     freshly-spawned AS needs a ctx to operate under).
	//   - Background() — cancels the current opCtx; called by the
	//     runtime when this AS is being demoted (e.g. /use is
	//     switching to a different agent). The cancel cascades to
	//     any bridge callCtx derived from opCtx, so an in-flight
	//     SendBlocks waiting on a hung prompt RPC wakes up
	//     immediately with ctx.Canceled.
	//
	// The cancel handle is private — external callers must use the
	// methods, not reach into the field. Guarded by asMu.
	opCtx    context.Context
	opCancel context.CancelFunc

	// Lifecycle.
	pid  int     // OS PID, 0 when not running
	stat Status  // Running / Detached / Exited

	// Spawn-time args; preserved across respawn.
	args []string

	// Timestamps.
	createdAt time.Time
	lastRunAt time.Time

	// Exit code (set when stat == Exited, nil otherwise).
	exitCode *int

	// Agent-session-resume id (e.g. Claude Code's
	// `system/init.session_id`). Captured from EventAgentReady on the
	// first run; persisted via Entry; replayed on the next Spawn as
	// `--resume <id>` (Claude Code currently translates this; other
	// bridges ignore it). Empty when the agent has no resume
	// semantics or has not yet emitted its init event.
	sessionID string

	// handle is the bridge-level live session (returned by
	// agent.Start). nil until Spawn succeeds. Committed to the
	// caller (readPump) only after SetRunning is called.
	handle agent.AgentSession

	// events is a tee of handle.Events() that signals handle-side
	// close (last chan close → SetExited). Created in Spawn; nil
	// before that.
	handleEventsClosed chan struct{}

	// F-45: model captured on first EventAgentReady (e.g.
	// "claude-opus-4-5-20250929"). SetModel is idempotent — empty
	// incoming values do NOT overwrite a previously-captured model.
	// Persisted via Entry → AgentSessionEntry.Model; restored on
	// restart via FromAgentSessionEntry. Empty before the first
	// EventAgentReady lands.
	model string

	// F-53: the in-flight Prompt for this AgentSession, or nil if
	// no Prompt is currently active. Stored on AS (not on
	// ChatSession) because a Prompt is bound to one specific AS
	// for its entire lifetime — see docs/feat/message_lifecycle.md
	// §4.2 存储归属.
	//
	// Concurrency (CS-AS 边界重构 Phase 1+): writes happen inside
	// `AgentSession.Submit` and `AgentSession.endPrompt` under
	// `asMu`. Reads from `as.readpumpLoop` also happen under `asMu`
	// for the LastMessageID anchoring. `cs.mu` does NOT touch this
	// field anymore — Phase 0's awkward "currentPrompt 物理上在 AS
	// 但访问必经 cs.mu" coupling is removed.
	//
	// Set to nil by `endPrompt(reason)`.
	currentPrompt *Prompt

	// F-53: monotonic counter for Prompt IDs within this
	// AgentSession. Incremented by `NewPromptID` to produce
	// `<AS.ID>-p<seq>` (e.g. `as_3-p7`). Atomic — ID generation
	// doesn't need to coordinate with anything else.
	promptCounter atomic.Uint64

	// CS-AS 边界重构 Phase 1: AgentSession 自治 readpump + eventQueue.
	//
	// isReady flips true when the AS is ready to accept a new Submit
	// (initial state true; flips false on Submit success; flips true on
	// endPrompt). Exposed via `IsReady()` for ChatSession.TryFlush.
	// Atomic — read concurrently from ChatSession's main loop.
	isReady atomic.Bool

	// eventQueue is the per-AS enriched-event buffer (cap
	// eventQueueCapacity). Push by readpump; pull by ChatSession
	// via `Events()`. Created at construction time, persists for
	// AS lifetime. Closed by `Shutdown()` after readpump exits.
	eventQueue chan EnrichedEvent

	// readpumpStarted guards single-launch of the readpump goroutine.
	// Set true on first Activate. Re-A activate is a no-op.
	readpumpStarted bool

	// readpumpStop / readpumpDone coordinate orderly shutdown of
	// the readpump goroutine. Stop is closed by Shutdown; Done is
	// closed by the readpump on exit.
	readpumpStop chan struct{}
	readpumpDone chan struct{}

	// shutdownOnce guards Shutdown's idempotency. The first call
	// closes eventQueue; subsequent calls are no-ops (close-on-
	// closed-channel panic guard).
	shutdownOnce sync.Once
}

// newAgentSessionRuntime is the SOLE place that allocates an
// AgentSession's runtime-only fields — the ones that have no
// meaningful "zero value" and MUST be valid before Spawn /
// Activate / the readpump can touch them (channels, atomics,
// contexts, the initial lifecycle state). Both NewAgentSession
// (fresh, in-memory) and FromAgentSessionEntry (restored from
// disk) call this FIRST and then only overlay the fields that are
// specific to their construction path (identity + persisted
// state).
//
// T-alive (2026-08-07) postmortem: eventQueue used to be
// allocated inline in NewAgentSession only. FromAgentSessionEntry
// was written independently and forgot it, leaving every
// post-restart AgentSession with a nil eventQueue — the readpump's
// first `as.eventQueue <- EnrichedEvent{}` then blocked forever
// (send on nil channel), which backed up the bridge's own event
// channel and made a perfectly healthy `claude` child process look
// hung from the outside (SendBlocks succeeded, zero events ever
// surfaced).
//
// This was the SECOND time a runtime field silently diverged
// between the two constructors (isReady missed FromAgentSessionEntry
// first). Recurring pattern: two
// independent struct literals for "the same kind of thing" always
// drift as fields get added. The fix is structural, not another
// diff pass — every future runtime field goes here ONCE, and both
// callers get it automatically. Do NOT construct `&AgentSession{}`
// anywhere else in this package; route through this function.
func newAgentSessionRuntime(id, chatSessionID, agentName, cwd string) *AgentSession {
	as := &AgentSession{
		ID:            id,
		ChatSessionID: chatSessionID,
		Agent:         agentName,
		Cwd:           cwd,
		stat:          StatusDetached,
		eventQueue:    make(chan EnrichedEvent, eventQueueCapacity),
	}
	as.pid = 0
	// Not yet activated; flips false on first Submit success (or,
	// for restored ASes, re-armed defensively — see
	// FromAgentSessionEntry / Spawn).
	as.isReady.Store(true)
	// Pre-install a usable opCtx (Background-derived, no cancel —
	// a "do-nothing" ctx). The first Activate(parent) call from the
	// runtime replaces it; in the meantime OpContext() returns this
	// so callers that read it before activation don't observe a
	// nil ctx (the pi bridge calls ctx.Deadline() on entry, so a
	// nil ctx here previously panicked the whole daemon on the
	// first message after a restart).
	as.opCtx = context.Background()
	return as
}

// NewAgentSession creates a new AgentSession in memory. The pool
// caller is responsible for adding it to the ChatSession's pool and
// persisting via registry.AgentSessionFile.
func NewAgentSession(id, chatSessionID, agent, cwd string, args []string) *AgentSession {
	as := newAgentSessionRuntime(id, chatSessionID, agent, cwd)
	as.args = append([]string(nil), args...)
	as.createdAt = time.Now()
	as.lastRunAt = time.Now()
	return as
}

// FromAgentSessionEntry reconstructs an AgentSession from persisted
// data. Process is not running on restart — the in-memory handle
// is lost (we don't persist it), so we mark anything persisted as
// StatusRunning as StatusDetached to force a re-spawn on next
// LookupActiveAgentSession. This prevents the "spawned but
// handle=nil" silent-drop bug where SendBlocks returns
// ErrNotRunning and the default FlushHook ignores it.
//
// Every runtime-only field (eventQueue, isReady, opCtx, pid, stat
// default) comes from newAgentSessionRuntime — see its doc comment
// for why that indirection exists. Only identity + persisted state
// is set here.
func FromAgentSessionEntry(e *registry.AgentSessionEntry) *AgentSession {
	if e == nil {
		return nil
	}
	as := newAgentSessionRuntime(e.ID, e.ChatSessionID, e.Agent, e.Cwd)
	as.args = append([]string(nil), e.Args...)
	as.createdAt = e.CreatedAt
	as.lastRunAt = e.LastRunAt
	as.sessionID = e.SessionID
	as.model = e.Model
	// commit fix-6: any persisted "running" agent is actually dead
	// after daemon restart (the process handle is in-memory only).
	// Demote to Detached so the next LookupActiveAgentSession will
	// re-spawn. Persisted PID is also stale (newAgentSessionRuntime
	// already zeroed it).
	status := e.Status
	if status == StatusRunning {
		status = StatusDetached
	}
	as.stat = status
	if e.ExitCode != nil {
		as.exitCode = e.ExitCode
	}
	return as
}

// PID returns the current OS process ID (0 if not running).
func (as *AgentSession) PID() int {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.pid
}

// Status returns the current lifecycle state.
func (as *AgentSession) Status() Status {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.stat
}

// NewPromptID (F-53) returns a unique Prompt ID of the form
// `<as.ID>-p<seq>` (e.g. `as_3-p7`). The sequence is monotonic per
// AgentSession (starts at 1; never reused within the daemon's
// lifetime — restored ASes from disk start fresh at 1 again, which
// is acceptable because the ID is only used for log / diagnostics
// correlation, not as a durable key).
//
// Concurrency: safe to call concurrently — backed by an atomic
// counter. The returned ID is reserved at the moment of the call,
// so a subsequent failure to install the Prompt (e.g. SendBlocks
// error) will leave a gap in the sequence. That's intentional —
// the counter tracks "IDs handed out", not "Prompts that
// succeeded".
func (as *AgentSession) NewPromptID() string {
	n := as.promptCounter.Add(1)
	return fmt.Sprintf("%s-p%d", as.ID, n)
}

// CurrentPrompt (F-53) returns the in-flight Prompt for this AS, or
// nil if no Prompt is currently active. Reads happen under
// `ChatSession.mu` (the field is written from
// `defaultPromptHookLocked` and `endPrompt`); callers that already
// hold `cs.mu` may call this directly. Callers that do NOT hold
// `cs.mu` should use `ChatSession.GetCurrentPrompt(as)` instead
// (planned — Phase 0 mostly reads it from inside the locked
// regions of `defaultPromptHookLocked` and `runReadPump`).
//
// CS-AS 边界重构 Phase 1: the field is owned by AgentSession.
// Reads/writes are protected by asMu (used directly inside
// Submit, readpumpLoop, endPrompt). External callers should
// prefer the higher-level API: Submit, IsReady, Events.
func (as *AgentSession) CurrentPrompt() *Prompt {
	// MUST take asMu: Submit installs currentPrompt under
	// asMu.Lock and endPrompt clears it there too, both from other
	// goroutines. Reading the field bare is a data race (caught by
	// TestSubmit_AnchorWriteIsRaceFree under -race).
	//
	// No caller holds asMu when calling this — it is a public
	// accessor used from CS-side code and tests; the internal
	// readpump/endPrompt paths touch as.currentPrompt directly
	// while already holding the lock.
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.currentPrompt
}

// --- per-AS operation context (Background / Activate) -------------

// OpContext returns the per-AgentSession context the bridge layer
// should use as the parent of its per-call callCtx. Lives for the
// duration of one "this AS is active" window — installed by
// Activate(parent) when the runtime promotes this AS to the active
// role, cancelled by Background() when the runtime demotes it (e.g.
// /use switching to a different agent).
//
// Callers MUST NOT cancel the returned ctx — that is the AgentSession's
// responsibility (Background). External code that wants per-turn
// cancellation observes the ctx's Done() channel or derives a child
// with its own deadline; do not reach into the cancel func, which
// is not exposed.
//
// Safe to call concurrently; the underlying context is replaced
// atomically under asMu.
func (as *AgentSession) OpContext() context.Context {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	// Never hand back a nil ctx. Both constructors pre-install one,
	// but callers pass this value straight into bridge code that
	// dereferences it (the pi bridge calls ctx.Deadline() on entry),
	// so a future constructor that forgets would take the daemon down
	// rather than fail locally. Defence in depth for a crash that
	// already happened once.
	if as.opCtx == nil {
		return context.Background()
	}
	return as.opCtx
}

// Background (CS-AS 边界重构 Phase 1) is a no-op.
//
// Phase 0 semantics: cancelled the current opCtx, which caused any
// bridge callCtx derived from opCtx to wake up with ctx.Canceled.
// This was used to "interrupt" an in-flight SendBlocks when /use
// switched active AS.
//
// Phase 1 semantics: /use does NOT cancel the in-flight prompt.
// The old AS continues to run in the background; its readpump
// keeps draining events into eventQueue; ChatSession can resume
// reading from the same channel on re-/use. The Prompt keeps
// running until EventAgentDone/Error/!ok ends it via `endPrompt`.
//
// Reserved as a method (rather than deleted) so callers that
// invoke it don't break at compile time, but the operation is
// intentionally a no-op. Real cancellation happens via Shutdown()
// (whole-AS lifecycle end) or via the underlying handle.Close().
func (as *AgentSession) Background() {
	// no-op: see comment above.
}

// Shutdown (Phase 1) is the orderly AS lifecycle end. It:
//
//  1. Cancels opCtx (cascades to any bridge callCtx derived from
//     it; an in-flight SendBlocks waiting on a hung prompt RPC
//     wakes up with ctx.Canceled).
//  2. Signals the readpump goroutine to exit via readpumpStop,
//     then waits on readpumpDone.
//  3. Closes the eventQueue channel so CS readers see `!ok`.
//
// Called by /kill, ChatSession shutdown, and AS reaping. NOT
// called by /use (which only changes `cs.activeAS` — see
// Background).
//
// Distinct from `as.handle.Close()` (which only closes the bridge
// transport; the wrapper-level AS still has readpump + eventQueue
// running until Shutdown is called).
//
// Idempotent: subsequent calls are no-ops (readpumpDone is closed
// once and stays closed).
func (as *AgentSession) Shutdown() {
	as.asMu.Lock()
	// Step 1: cancel opCtx
	if as.opCancel != nil {
		as.opCancel()
		as.opCancel = nil
	}
	// Step 2: snapshot readpumpStop / readpumpDone under lock
	stop := as.readpumpStop
	done := as.readpumpDone
	as.asMu.Unlock()

	// Closing the stop chan wakes the readpump select.
	if stop != nil {
		select {
		case <-stop:
			// already closed
		default:
			close(stop)
		}
	}

	// Wait for readpump to drain remaining work and exit.
	if done != nil {
		<-done
	}

	// Step 3: close eventQueue. Safe to do here because readpump
	// has exited and won't push more. After this, CS's
	// `for ev := range as.Events()` returns `!ok` and exits.
	//
	// Note: we close but do NOT nil-out eventQueue. Callers that
	// captured a reference to the channel before Shutdown
	// (e.g. time-of-check as.Events() in a long-running reader)
	// must still see the closed channel — nil-ing would lose
	// that reference and reads would block forever.
	//
	// Idempotent via sync.Once: double-Shutdown is safe.
	as.shutdownOnce.Do(func() {
		as.asMu.Lock()
		if as.eventQueue != nil {
			close(as.eventQueue)
		}
		as.asMu.Unlock()
	})
}

// IsActivated reports whether Activate has installed a live per-AS
// ctx (i.e. one derived from the ChatSession, with a working cancel).
// A pre-installed Background ctx from either constructor does NOT
// count — it has no cancel, so Background() would be a silent no-op.
//
// Used by ChatSession.promoteActiveLocked to make activation
// idempotent: re-activating on every lookup would cancel the
// in-flight turn.
func (as *AgentSession) IsActivated() bool {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.opCancel != nil
}

// Activate installs a fresh per-AS opCtx derived from parent.
// Cancels the previous opCtx first (defensive — should already
// be Background'd by the caller; redundant cancel is a no-op).
// After Activate returns, OpContext() yields the new ctx; the
// old one is dead.
//
// The ONLY caller is ChatSession.promoteActiveLocked, which runs on
// every change of active AgentSession. Do not call it from handlers:
// scattering activation is what left non-/use chats with an
// unactivated AS (nil opCtx -> daemon panic on the first message
// after a restart, and a Background() that silently did nothing).
//
// parent comes from ChatSession.Context(); cancelling cs.ctx (via
// cs.ResetContext) cascades through every AS active on that chat,
// providing a clean shutdown path.
func (as *AgentSession) Activate(parent context.Context) {
	as.asMu.Lock()
	// Phase 1: idempotent. Repeated calls (e.g. /use re-promoting
	// the same AS) are no-ops — the first Activate installs opCtx
	// for the AS's whole lifetime. Re-Activate would cancel an
	// in-flight SendBlocks, which is no longer desired (see
	// Background's new no-op semantics).
	if as.opCancel != nil {
		as.asMu.Unlock()
		return
	}
	as.opCtx, as.opCancel = context.WithCancel(parent)
	as.asMu.Unlock()

	// Readpump is started by Spawn (after handle is set), not by
	// Activate. This avoids leaking a polling goroutine on tests
	// that call Activate but never Spawn.
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
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	if as.exitCode == nil {
		return nil
	}
	v := *as.exitCode
	return &v
}

// SessionID returns the agent's own session id (e.g. Claude Code's
// `system/init.session_id`) captured on the last run. Empty when
// the agent has no resume semantics or has not yet emitted its
// init event.
func (as *AgentSession) SessionID() string {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.sessionID
}

// SetSessionID records the agent's own session id. Called by the
// runtime's EventHandler when it receives an EventAgentReady with a
// non-empty SessionID. Safe to call concurrently with Spawn /
// SetRunning / SetExited.
func (as *AgentSession) SetSessionID(id string) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.sessionID = id
}

// --- F-45: model metadata API ------------------------------------

// SetModel records the agent's selected model (e.g. Claude Code:
// system/init.model). Idempotent: an empty incoming value does NOT
// overwrite a previously-captured model — bridges may re-emit
// EventAgentReady after a child restart with a blank Model and we don't
// want to wipe the prior capture. Called by the runtime's
// EventHandler closure on EventAgentReady.
//
// Safe to call concurrently with Model() and other lifecycle
// methods.
func (as *AgentSession) SetModel(m string) {
	if m == "" {
		return
	}
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.model = m
}

// Model returns the captured model name. Empty before the first
// EventAgentReady lands or when the bridge does not report one.
func (as *AgentSession) Model() string {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.model
}


// PersistIfDirty is currently a no-op pass-through (every prior
// caller relied on cumulativeDirty, which is gone with the
// cross-turn usage aggregation). Kept as the single entry point so
// future per-AgentSession dirty state (e.g. status /
// sessionID changes) can hook in without changing call sites.
//
// persist is the registry callback (typically
// Manager.PersistAgentSession). Returns nil when persist is nil.
func (as *AgentSession) PersistIfDirty(persist func(*registry.AgentSessionEntry) error) error {
	if persist == nil {
		return nil
	}
	return persist(as.Entry())
}

// SetRunning marks the AgentSession as running with the given PID.
// Bumps LastRunAt.
func (as *AgentSession) SetRunning(pid int) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.pid = pid
	as.stat = StatusRunning
	as.lastRunAt = time.Now()
	as.exitCode = nil
}

// SetDetached marks the AgentSession as detached (process alive but
// nightme no longer holds it; e.g. after daemon SIGTERM without
// --cleanup). PID is preserved.
func (as *AgentSession) SetDetached() {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.stat = StatusDetached
	as.lastRunAt = time.Now()
}

// SetExited marks the AgentSession as exited with the given exit
// code. PID is cleared.
func (as *AgentSession) SetExited(code int) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.pid = 0
	as.stat = StatusExited
	as.lastRunAt = time.Now()
	as.exitCode = &code
}

// Entry returns a snapshot of this AgentSession as a registry entry
// (for persistence). All snapshots taken under one asMu RLock so
// the persisted fields are internally consistent at one point in
// time.
func (as *AgentSession) Entry() *registry.AgentSessionEntry {
	as.asMu.RLock()
	stat := as.stat
	lastRun := as.lastRunAt
	resume := as.sessionID
	model := as.model
	var ec *int
	if as.exitCode != nil {
		v := *as.exitCode
		ec = &v
	}
	as.asMu.RUnlock()

	return &registry.AgentSessionEntry{
		ID:            as.ID,
		ChatSessionID: as.ChatSessionID,
		Agent:         as.Agent,
		Cwd:           as.Cwd,
		PID:           as.PID(),
		Status:        stat,
		Args:          as.Args(),
		SessionID:     resume,
		CreatedAt:     as.createdAt,
		LastRunAt:     lastRun,
		ExitCode:      ec,
		Model:         model,
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

	as.asMu.Lock()
	alreadyRunning := as.handle != nil && as.stat == StatusRunning
	if alreadyRunning {
		as.asMu.Unlock()
		return nil
	}
	// Copy out the fields we need under lock; spawner.Spawn may
	// take seconds (fork+exec+handshake RPC), and we don't want to
	// hold asMu across it — EventHandler / readPump / footer
	// renderers would block.
	resume := as.sessionID
	args := append([]string(nil), as.args...)
	as.asMu.Unlock()

	handle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, args, resume)
	if err != nil {
		return fmt.Errorf("chatsession: spawn %s at %s: %w", as.Agent, as.Cwd, err)
	}

	as.asMu.Lock()
	as.handle = handle
	as.pid = handle.PID()
	as.stat = StatusRunning
	as.lastRunAt = time.Now()
	as.exitCode = nil
	as.asMu.Unlock()

	// T-alive (2026-08-07): restore isReady on every Spawn. The
	// disk-restore path (FromAgentSessionEntry) constructs the
	// AgentSession literal-style and never initializes isReady,
	// leaving the atomic.Bool zero value of false. Without this
	// reset, TryFlush SKIPs on every restored chat (reason=
	// as_not_ready) → Submit is never called → the bridge's
	// stdin is never written → claude never sees the prompt.
	// The previous turn's isReady=false (set after a successful
	// Submit) would normally be cleared by endPrompt, but if the
	// prompt never completes (e.g. bridge hangs) the flag stays
	// false; the next Spawn is the right place to re-arm it
	// because "we just transitioned back to Running, ready for
	// the next turn".
	as.isReady.Store(true)

	// Start readpump after handle is set. startReadPump is idempotent
	// (subsequent Spawns / re-activations are no-ops). This is the
	// canonical readpump start point, decoupled from Activate so
	// tests that Activate without Spawn don't leak a polling
	// goroutine.
	as.startReadPump()
	return nil
}

// Handle returns the bridge-level agent.AgentSession (nil if not yet
// spawned). Exposed for callers that need direct access (e.g., the
// ChatSession EventCallback installer).
func (as *AgentSession) Handle() agent.AgentSession {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.handle
}

// Events returns the live event channel from the bridge. Returns nil
// Events (CS-AS 边界重构 Phase 1) returns the enriched event stream
// for ChatSession. Reads from this channel receive EnrichedEvent
// values (KindAgentEvent / KindPromptEnded / KindLifecycle) instead
// of raw bridge events, with Prompt anchoring already applied.
//
// The returned channel is closed by Shutdown after the readpump
// goroutine exits. Returns nil before the AgentSession is fully
// constructed (defensive — should not happen in production).
//
// Compare to Phase 0's `Events()` which returned the raw bridge
// channel; that responsibility moved to the readpump (see
// `agentsession_readpump.go`).
func (as *AgentSession) Events() <-chan EnrichedEvent {
	as.asMu.RLock()
	eq := as.eventQueue
	as.asMu.RUnlock()
	return eq
}

// IsReady (CS-AS 边界重构 Phase 1) reports whether the AS can
// accept a new Submit. True when:
//   - No Prompt is currently in flight (currentPrompt == nil), AND
//   - The handle is spawned and process is alive.
//
// ChatSession.TryFlush polls this before calling Submit. Atomic
// load — safe to call concurrently with Submit / endPrompt.
func (as *AgentSession) IsReady() bool {
	return as.isReady.Load()
}

// Submit (CS-AS 边界重构 Phase 1) hands a candidate Prompt to the
// bridge. The CS side builds the Prompt (MessageIDs, Blocks) and
// passes it here; AS owns the rest of the lifecycle (ID
// assignment, ack timestamp, currentPrompt install, isReady flip).
//
// Returns:
//   - ErrNotRunning if Spawn has not been called (handle is nil).
//   - The bridge's SendBlocks error (e.g. ctx.Canceled, network
//     failure). On error, currentPrompt is NOT installed and
//     isReady stays true — caller can retry on next IsReady=true.
//
// On success: currentPrompt is set, isReady is false, the readpump
// will start bridging events for this Prompt's lifetime.
//
// Concurrency: asMu briefly during commit, but SendBlocks runs
// outside the lock (it can block on a hung prompt RPC). Two
// concurrent Submits are serialized by the bridge's turnMu
// (pi) or single-threaded access (claudecode / pty).
func (as *AgentSession) Submit(p *Prompt) error {
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
	if h == nil {
		return ErrNotRunning
	}

	// Assign IDs / timestamps BEFORE the bridge call. These belong
	// to AS (per the design — see wip-cs-as-boundary.md §2.2).
	p.ID = as.NewPromptID()
	p.AgentSessionID = as.ID
	p.AckedAt = time.Now()
	p.LastProgressAt = time.Now()

	// Debug-only per-message trace (use `Logging.Level: debug` in
	// config to enable). Every user message goes through here, so
	// Info-level would spam the daemon log; SendBlocks failure
	// below stays at Warn since that IS an actionable event.
	slog.Debug("chatsession: Submit",
		"chat_id", as.ChatSessionID,
		"as_id", as.ID,
		"blocks", len(p.Blocks),
		"prompt_id", p.ID)

	// SendBlocks can block on a hung prompt RPC; do NOT hold asMu.
	err := h.SendBlocks(as.OpContext(), p.Blocks)
	if err != nil {
		slog.Warn("chatsession: Submit SendBlocks FAILED",
			"chat_id", as.ChatSessionID,
			"as_id", as.ID,
			"err", err)
		return err
	}
	slog.Debug("chatsession: Submit SendBlocks ok",
		"chat_id", as.ChatSessionID,
		"as_id", as.ID,
		"prompt_id", p.ID)

	// Commit: install currentPrompt and flip isReady.
	as.asMu.Lock()
	as.currentPrompt = p
	as.asMu.Unlock()
	as.isReady.Store(false)
	return nil
}

// SendText delivers a single text block to the bridge child.
// Convenience wrapper around SendBlocks: routes through the
// chat's per-AS ctx (OpContext()) instead of touching bridge's
// bare SendText (which has no ctx signature on agent.AgentSession).
//
// The ctx passed to SendBlocks is as.OpContext() — the AS-owned
// ctx installed by Activate(parent). This makes SendText behave
// exactly like SendBlocks wrt cancellation: /use Background()
// wakes any in-flight SendText the same way it wakes an in-flight
// SendBlocks. The bridge's SendBlocks then derives its own
// per-call callCtx from as.OpContext().
//
// Returns ErrNotRunning if Spawn has not been called.
func (as *AgentSession) SendText(text string) error {
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
	if h == nil {
		return ErrNotRunning
	}
	return h.SendBlocks(as.OpContext(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: text},
	})
}

// SendBlocks delivers structured content blocks. Returns ErrNotRunning
// if Spawn has not been called.
func (as *AgentSession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
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
//     The bridge is expected to emit a fresh EventAgentReady carrying the
//     new SessionID; the runtime's AgentEventBus subscriber (cmd/nightme/run.go
//     newEventHandler) captures it via SetSessionID and persists.
//
//   - Bridge cannot do in-place reset (raw PTY bridge): bridge.New
//     returns agent.ErrRestartRequired. The wrapper then kills the
//     existing bridge handle and spawns a fresh one via spawner
//     (with SessionID="" so the new child starts with no --resume).
//     SessionID is explicitly cleared on the wrapper so persistence
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
	as.asMu.Lock()
	h := as.handle
	as.asMu.Unlock()
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

	// snapshot the args under lock; spawner.Spawn may take seconds
	// and we don't want to hold asMu across it.
	as.asMu.RLock()
	args := append([]string(nil), as.args...)
	as.asMu.RUnlock()
	newHandle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, args, "")
	if err != nil {
		// F-34 Phase 3 review: previously this branch returned
		// without updating status, leaving as.stat=StatusRunning
		// with the OLD PID and as.handle=nil. Subsequent
		// LookupActiveAgentSession would see Running + nil handle
		// and fail every SendBlocks with ErrNotRunning. Mark as
		// Exited so the next user message lazy-spawns a fresh AS.
		as.asMu.Lock()
		as.handle = nil
		as.pid = 0
		as.stat = StatusExited
		as.lastRunAt = time.Now()
		code := -1
		as.exitCode = &code
		as.asMu.Unlock()
		as.SetSessionID("")
		return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
	}
	as.asMu.Lock()
	as.handle = newHandle
	as.pid = newHandle.PID()
	as.stat = StatusRunning
	as.lastRunAt = time.Now()
	as.exitCode = nil
	as.asMu.Unlock()
	// Explicitly clear SessionID so a stale id never gets replayed on
	// the next respawn (the new child will emit its own EventAgentReady,
	// and the runtime's AgentEventBus subscriber will SetSessionID via the normal
	// path).
	as.SetSessionID("")

	// Restart the per-AS readpump on the new handle. The old
	// readpumpLoop saw !ok from the closed handle above and exited;
	// without this restart, no one is draining the new handle's
	// Events() and the chat stalls silently. startReadPump is guarded
	// by readpumpStarted — flip it back to false first so the new
	// handle gets a fresh drainer. Previous readpumpDone is already
	// closed (deferred from the exited goroutine), so waiting on it
	// is a no-op.
	as.asMu.Lock()
	as.readpumpStarted = false
	as.asMu.Unlock()
	as.startReadPump()
	return nil
}

// Close terminates the bridge child (sends shutdown signal to the
// underlying bridge). Idempotent. Marks status=Exited on success.
func (as *AgentSession) Close() error {
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
	if h == nil {
		return nil // not running
	}
	return h.Close()
}

// ObserveClose runs in a goroutine after Spawn to watch the bridge
// events channel. When the channel closes (EventAgentDone / EventAgentError /
// child EOF), the AgentSession transitions to Exited. Returns a
// channel that the caller can wait on for clean shutdown.
//
// Convention: ChatSession starts one ObserveClose per AgentSession
// in its pool.
func (as *AgentSession) ObserveClose() <-chan struct{} {
	done := make(chan struct{})
	as.asMu.RLock()
	ev := as.handle.Events()
	as.asMu.RUnlock()

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