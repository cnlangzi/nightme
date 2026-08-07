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
	// (Status, Model, CumulativeUsage, CompactionCount, ExitCode,
	// Handle, Events, PID) don't contend with each other when no
	// writer is in flight.
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
	// `system/init.session_id`). Captured from EventInit on the
	// first run; persisted via Entry; replayed on the next Spawn as
	// `--resume <id>` (Claude Code currently translates this; other
	// bridges ignore it). Empty when the agent has no resume
	// semantics or has not yet emitted its init event.
	resumeID string

	// handle is the bridge-level live session (returned by
	// agent.Start). nil until Spawn succeeds. Committed to the
	// caller (readPump) only after SetRunning is called.
	handle agent.AgentSession

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
	model string

	// F-45: per-AgentSession running total of token / cost stats.
	// AccumulateUsage adds turn-level counts; ResetCumulative zeroes
	// (called only by /new handler); PersistIfDirty writes the entry
	// to disk when dirty. Persisted via Entry → AgentSessionEntry.
	//CumulativeUsage; restored on restart from FromAgentSessionEntry.
	//
	// F-49: compactionCount sits alongside cumulativeUsage. The
	// RecordCompaction write path increments the count AND zeroes
	// the four token fields (preserving CostUSD); the lock is
	// shared with cumulativeUsage so the two stay consistent.
	cumulativeUsage agent.UsageInfo
	compactionCount int
	cumulativeDirty bool

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
		eventQueue:    make(chan EnrichedEvent, eventQueueCapacity),
	}
	as.pid = 0
	as.isReady.Store(true) // not yet activated; flips false on first Submit success
	// Pre-install a usable opCtx (Background-derived from BG, no
	// cancel — a "do-nothing" ctx). The first Activate(parent) call
	// from the runtime replaces it; in the meantime OpContext()
	// returns this so callers that read it before activation don't
	// observe a nil ctx.
	as.opCtx = context.Background()
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
	// (not by pointer) so the in-memory state is decoupled from the
	// persisted entry.
	if e.CumulativeUsage != nil {
		as.cumulativeUsage = *e.CumulativeUsage
	}
	// F-49: restore compaction count. Zero value on legacy entries
	// (no compactionCount field) is the safe default — "never
	// compacted", consistent with cumulativeUsage being zero.
	as.compactionCount = e.CompactionCount
	// commit fix-6: any persisted "running" agent is actually dead
	// after daemon restart (the process handle is in-memory only).
	// Demote to Detached so the next LookupActiveAgentSession will
	// re-spawn. Persisted PID is also stale; clear it.
	status := e.Status
	if status == StatusRunning {
		status = StatusDetached
	}
	as.stat = status
	as.pid = 0
	// Pre-install a usable opCtx, exactly as NewAgentSession does.
	// Without this, every AgentSession restored from disk carries a
	// nil opCtx until the first Activate(parent), and the default
	// FlushHook hands that nil straight to the bridge
	// (chatsession.go: `as.SendBlocks(as.OpContext(), combined)`).
	// The pi bridge calls ctx.Deadline() on entry, so the FIRST
	// message after any daemon restart panicked the whole daemon with
	// a nil-pointer dereference.
	as.opCtx = context.Background()
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
// running until EventDone/Error/!ok ends it via `endPrompt`.
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

// ResumeID returns the agent's own session id (e.g. Claude Code's
// `system/init.session_id`) captured on the last run. Empty when
// the agent has no resume semantics or has not yet emitted its
// init event.
func (as *AgentSession) ResumeID() string {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.resumeID
}

// SetResumeID records the agent's own session id. Called by the
// runtime's EventHandler when it receives an EventInit with a
// non-empty SessionID. Safe to call concurrently with Spawn /
// SetRunning / SetExited.
func (as *AgentSession) SetResumeID(id string) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.resumeID = id
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
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.model = m
}

// Model returns the captured model name. Empty before the first
// EventInit lands or when the bridge does not report one.
func (as *AgentSession) Model() string {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.model
}

// AccumulateUsage atomically adds u's per-turn counters to this
// AgentSession's cumulative state and marks it dirty so the next
// PersistIfDirty writes the entry. Called by the runtime's
// EventHandler closure on every EventUsage arriving from the
// bridge — typically once per turn. nil u is a no-op (defensive:
// some bridges emit empty EventUsage as "no usage this turn").
func (as *AgentSession) AccumulateUsage(u *agent.UsageEvent) {
	if u == nil {
		return
	}
	as.asMu.Lock()
	defer as.asMu.Unlock()
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
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.cumulativeUsage
}

// ResetCumulative zeroes the cumulative state and marks the entry
// dirty for the next PersistIfDirty. Called only by /new handler
// — /kill, /cwd, /use, daemon restart and process crash all leave
// the cumulative intact (history is valuable to the user).
//
// F-49: also resets compactionCount. /new is the only point that
// zeros the count, mirroring the cumulative-usage semantics —
// user-initiated context reset clears both.
//
// The handle / pool / status are not touched — ResetCumulative is
// purely "clear the counter and mark dirty"; the caller
// (handleNew) is responsible for the surrounding PersistAgentSession.
func (as *AgentSession) ResetCumulative() {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.cumulativeUsage = agent.UsageInfo{}
	as.compactionCount = 0
	as.cumulativeDirty = true
}

// RecordCompaction atomically:
//   1. increments compactionCount;
//   2. zeroes the four token fields of cumulativeUsage, preserving
//      CostUSD;
//   3. marks cumulativeDirty so the next PersistIfDirty flushes
//      both the new count and the post-reset token snapshot.
//
// The token reset makes Footer Line 2 (↓ ↻ ↑ total) reflect
// "since-last-compaction" — i.e. the agent's current context window
// usage — while $cost stays as lifetime spend across the whole
// AgentSession. See docs/feat/F-49-compaction-counter.md §1.2 /
// §1.4 for the user-facing rationale and the cost-vs-token split.
//
// Bridges abstract their own protocol differences and emit exactly
// one EventCompaction per completed compaction cycle (see F-49
// §1.3). The runtime handler calls RecordCompaction unconditionally
// on every EventCompaction — no Subtype branching here, by design.
//
// Safe to call concurrently with AccumulateUsage / CumulativeUsage /
// CompactionCount / PersistIfDirty. Mirrors AccumulateUsage's
// lock pattern (single-writer per EventCompaction).
func (as *AgentSession) RecordCompaction() {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.compactionCount++
	as.cumulativeUsage.InputTokens = 0
	as.cumulativeUsage.CacheCreationInputTokens = 0
	as.cumulativeUsage.CacheReadInputTokens = 0
	as.cumulativeUsage.OutputTokens = 0
	// CostUSD deliberately preserved (lifetime spend).
	as.cumulativeDirty = true
}

// CompactionCount returns the cumulative number of completed
// compaction cycles observed on this AgentSession. 0 when never
// compacted. Snapshot under RLock; safe for concurrent read
// alongside RecordCompaction.
func (as *AgentSession) CompactionCount() int {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.compactionCount
}

// PersistIfDirty writes the AgentSession entry to disk when the
// cumulative stats have changed since the last persist. The
// runtime calls this on EventDone (turn end) so each turn costs
// at most one file write; on clean state it is a no-op.
//
// persist is the registry callback (typically
// Manager.PersistAgentSession). PersistIfDirty snapshots the
// entry itself via as.Entry() — callers should NOT pre-build an
// AgentSessionEntry to pass in, since that would duplicate the
// snapshot work and risk drift between the entry passed to the
// callback and the entry actually persisted.
//
// Returns nil when clean (no I/O) or when persist is nil.
func (as *AgentSession) PersistIfDirty(persist func(*registry.AgentSessionEntry) error) error {
	if persist == nil {
		return nil
	}
	as.asMu.Lock()
	if !as.cumulativeDirty {
		as.asMu.Unlock()
		return nil
	}
	as.cumulativeDirty = false
	as.asMu.Unlock()
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
	resume := as.resumeID
	model := as.model
	handle := as.handle
	var ec *int
	if as.exitCode != nil {
		v := *as.exitCode
		ec = &v
	}
	// F-45: snapshot cumulative stats under the same RLock so we
	// publish a consistent copy. Always emit a non-nil pointer —
	// the legacy "never ran" case is distinguishable only by
	// zero-value counters inside, and omitempty would skip the
	// field for "ran but all zero" which obscures the persisted
	// state.
	//
	// F-49: also snapshot compactionCount under the same RLock so
	// the persisted entry is internally consistent (count + usage
	// refer to the same point in time).
	cum := as.cumulativeUsage
	cc := as.compactionCount
	as.asMu.RUnlock()

	_ = handle // captured but not exported in the persisted entry

	return &registry.AgentSessionEntry{
		ID:              as.ID,
		ChatSessionID:   as.ChatSessionID,
		Agent:           as.Agent,
		Cwd:             as.Cwd,
		PID:             as.PID(),
		Status:          stat,
		Args:            as.Args(),
		ResumeID:        resume,
		CreatedAt:       as.createdAt,
		LastRunAt:       lastRun,
		ExitCode:        ec,
		Model:           model,
		CumulativeUsage: &cum,
		CompactionCount: cc,
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
	resume := as.resumeID
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

	// SendBlocks can block on a hung prompt RPC; do NOT hold asMu.
	err := h.SendBlocks(as.OpContext(), p.Blocks)
	if err != nil {
		return err
	}

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
		as.SetResumeID("")
		return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
	}
	as.asMu.Lock()
	as.handle = newHandle
	as.pid = newHandle.PID()
	as.stat = StatusRunning
	as.lastRunAt = time.Now()
	as.exitCode = nil
	as.asMu.Unlock()
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
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
	if h == nil {
		return nil // not running
	}
	return h.Close()
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