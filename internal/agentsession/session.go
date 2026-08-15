// Package agentsession — AgentSession (v1.2 per-CLI-process handle).
//
// See docs/SPEC.md v1.2 §1.1 and docs/feat/F-29-agent-session-pool.md
// for the full model. In v1.2 the AgentSession replaces v1.1's
// Session type for process ownership; the per-chat ChatSession owns
// the pool of AgentSessions.
package agentsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/command/services"
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
// handle (agent.Agent) is stored in `handle` and is the
// source of Events / SendBlocks / Close. Lifecycle
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
	// Spawner.Spawn). nil until Spawn succeeds. Committed to the
	// caller (readPump) only after SetRunning is called. Type is
	// *agent.Agent (the shared runtime struct); per-bridge
	// state lives in the unexported driver. Tests that need to
	// reach bridge-specific state use Handle().Driver().
	handle *agent.Agent

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

	// inFlightMessages mirrors AgentSessionEntry.InFlightMessages.
	// Set by Submit on successful SendBlocks, cleared by endPrompt.
	// Strictly co-lives with currentPrompt: when currentPrompt is
	// non-nil, inFlightMessages holds refs derived from p.Messages;
	// when currentPrompt is nil, inFlightMessages is nil. This
	// invariant is what makes restart-replay unambiguous.
	inFlightMessages []registry.InFlightMessageRef

	// F-61: watchdog suspect state (mirrors Entry fields).
	// SetSuspect/ClearSuscept maintain the invariant "SuspectSince
	// is non-nil iff SuspectReason != ''". Read by the prober to
	// gate proactive respawn under the 5min cooldown window.
	suspectReason string
	suspectSince  *time.Time

	// F-61: closedByUser is set by Close() to tell the immediate
	// respawn path "this death was intentional, don't respawn".
	// Stays sticky across the lifetime of the AgentSession;
	// cleared by respawn so a future crash IS recoverable. Without
	// this flag, the F-61 §3.2 Lifecycle handler would race with
	// /close and respawn a bridge the user just asked to close.
	closedByUser bool

	// persist is the registry callback used to write the current
	// entry back to disk. Wired by attachAgentSessionLocked in the
	// chat layer (it has access to asFile). nil is allowed — Submit
	// and endPrompt silently skip persistence when unset (e.g.
	// pre-attachment or test contexts).
	persist func(*registry.AgentSessionEntry) error

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
	// eventQueueCapacity). Push by readpump; drained by the
	// dispatcher goroutine which publishes onto EventBus. Created at
	// construction time, persists for AS lifetime. Closed by
	// `Shutdown()` after readpump exits.
	eventQueue chan EnrichedEvent

	// EventBus is the per-AS pub/sub for EnrichedEvent. The
	// dispatcher goroutine publishes onto it; ChatSession subscribes
	// when it attaches the AS. Owned by the AgentSession, persists
	// for the AS lifetime (survives bridge respawns). Closed by
	// `Shutdown()` after the dispatcher exits.
	//
	// Exposed as a field so callers can use the full EventBus API
	// (Subscribe / Unsubscribe / Len). Publish / Close remain the
	// AgentSession's responsibility; ChatSession should not call
	// them directly.
	EventBus *services.EventBus[EnrichedEvent]

	// readpumpStarted guards single-launch of the readpump goroutine.
	// Set true on first Activate. Re-A activate is a no-op.
	readpumpStarted bool

	// readpumpStop / readpumpDone coordinate orderly shutdown of
	// the readpump goroutine. Stop is closed by Shutdown; Done is
	// closed by the readpump on exit.
	readpumpStop chan struct{}
	readpumpDone chan struct{}

	// dispatchStarted guards single-launch of the dispatcher
	// goroutine that drains eventQueue and publishes onto EventBus.
	dispatchStarted bool

	// dispatchStop / dispatchDone coordinate orderly shutdown of
	// the dispatcher goroutine. Stop is closed by Shutdown after
	// readpumpStop; Done is closed by the dispatcher on exit.
	dispatchStop chan struct{}
	dispatchDone chan struct{}

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
func newAgentSessionRuntime(id, chatSessionID, agentName, cwd string, opts []AgentSessionOption) *AgentSession {
	var o agentSessionOptions
	for _, opt := range opts {
		opt(&o)
	}
	queueCap := eventQueueCapacity
	if o.eventQueueCapacity > 0 {
		queueCap = o.eventQueueCapacity
	}
	as := &AgentSession{
		ID:            id,
		ChatSessionID: chatSessionID,
		Agent:         agentName,
		Cwd:           cwd,
		stat:          StatusDetached,
		eventQueue:    make(chan EnrichedEvent, queueCap),
		EventBus:      services.NewEventBus[EnrichedEvent](),
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
//
// opts lets test/edge-case code override fields that the production
// default would otherwise pin. Currently only the eventQueue
// capacity is exposed (via WithEventQueueCapacity); production
// callers should not pass any options.
func NewAgentSession(id, chatSessionID, agent, cwd string, args []string, opts ...AgentSessionOption) *AgentSession {
	as := newAgentSessionRuntime(id, chatSessionID, agent, cwd, opts)
	as.args = append([]string(nil), args...)
	as.createdAt = time.Now()
	as.lastRunAt = time.Now()
	return as
}

// AgentSessionOption overrides a default field at construction
// time. Used by tests that need a different eventQueue cap (or
// future fields) without mutating shared package state.
type AgentSessionOption func(*agentSessionOptions)

type agentSessionOptions struct {
	// eventQueueCapacity, if > 0, overrides the package default
	// for this AgentSession's eventQueue. The queue is created
	// in newAgentSessionRuntime; passing 0 (or not setting it)
	// uses eventQueueCapacity (the const default).
	eventQueueCapacity int
}

// WithEventQueueCapacity returns an option that sets the
// AgentSession's eventQueue capacity to n. Use this in tests
// to exercise the buffer-cap backpressure path with a small
// queue; production code should not override the default
// (see eventQueueCapacity doc for the worst-case-AS-backlog
// rationale).
func WithEventQueueCapacity(n int) AgentSessionOption {
	return func(o *agentSessionOptions) {
		o.eventQueueCapacity = n
	}
}

// FromAgentSessionEntry reconstructs an AgentSession from persisted
// data. Process is not running on restart — the in-memory handle
// is lost (we don't persist it), so we mark anything persisted as
// StatusRunning as StatusDetached to force a re-spawn on next
// LookupSelectedAgentSession. This prevents the "spawned but
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
	as := newAgentSessionRuntime(e.ID, e.ChatSessionID, e.Agent, e.Cwd, nil)
	as.args = append([]string(nil), e.Args...)
	as.createdAt = e.CreatedAt
	as.lastRunAt = e.LastRunAt
	as.sessionID = e.SessionID
	as.model = e.Model
	if len(e.InFlightMessages) > 0 {
		as.inFlightMessages = append([]registry.InFlightMessageRef(nil), e.InFlightMessages...)
	}
	// F-61: restore suspect state verbatim. Cooldown window is
	// measured from SuspectSince; on restart, the prober picks up
	// from there so a suspect-then-crashed AS doesn't immediately
	// re-trigger a respawn.
	as.suspectReason = e.SuspectReason
	if e.SuspectSince != nil {
		t := *e.SuspectSince
		as.suspectSince = &t
	}
	// commit fix-6: any persisted "running" agent is actually dead
	// after daemon restart (the process handle is in-memory only).
	// Demote to Detached so the next LookupSelectedAgentSession will
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

// Shutdown (multi-as Phase 1) is the orderly AS lifecycle end. It:
//
//  1. Cancels opCtx (cascades to any bridge callCtx derived from
//     it; an in-flight SendBlocks waiting on a hung prompt RPC
//     wakes up with ctx.Canceled).
//  2. Signals the readpump goroutine to exit via readpumpStop,
//     then waits on readpumpDone.
//  3. Closes the eventQueue channel so the dispatcher sees `!ok`.
//  4. Signals the dispatcher goroutine to exit via dispatchStop,
//     then waits on dispatchDone (so it can publish remaining
//     events onto EventBus before closing it).
//  5. Closes the EventBus so subscribers see Publish as a no-op.
//
// Called by /close, ChatSession shutdown, and AS reaping. NOT
// called by /use (which only changes `cs.selectedAS` — see
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
	// has exited and won't push more. After this, the dispatcher's
	// `for ev := range eventQueue` returns `!ok` and exits.
	//
	// Note: we close but do NOT nil-out eventQueue. Callers that
	// captured a reference to the channel before Shutdown
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

	// Step 4: signal dispatcher to stop and wait for it to drain
	// remaining events onto EventBus. dispatcher is lazily started
	// by ensureDispatcher(); on never-published ASes dispatchStop
	// /dispatchDone are nil and we skip the close + wait.
	as.asMu.RLock()
	dStop := as.dispatchStop
	dDone := as.dispatchDone
	as.asMu.RUnlock()
	if dStop != nil {
		select {
		case <-dStop:
			// already closed
		default:
			close(dStop)
		}
	}
	if dDone != nil {
		<-dDone
	}

	// Step 5: close the EventBus so any future Subscribe/Publish
	// calls become no-ops. Existing subscribers that captured the
	// bus before Shutdown retain their unsubscribe funcs (they
	// remain valid even after Close; Close just sets the closed
	// flag).
	if as.EventBus != nil {
		as.EventBus.Close()
	}
}

// IsActivated reports whether Activate has installed a live per-AS
// ctx (i.e. one derived from the ChatSession, with a working cancel).
// A pre-installed Background ctx from either constructor does NOT
// count — it has no cancel, so Background() would be a silent no-op.
//
// Used by ChatSession.selectAgentSessionLocked to make activation
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
// The ONLY caller is ChatSession.selectAgentSessionLocked, which runs on
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

// Args returns a defensive copy of the spawn arguments. as.args is
// immutable post-construction today, but Args takes asMu.RLock to
// keep the lock discipline consistent with other readers (e.g.
// SessionID, Model) — protects against any future SetArgs-style
// mutation racing a respawn-time read.
func (as *AgentSession) Args() []string {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
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
// SetPersist wires the registry callback used to flush state
// transitions (notably InFlightMessages after Submit / endPrompt).
// Wired by the chat layer at attach time; nil (no persistence or
// pre-attachment) means Submit / endPrompt silently skip the write.
func (as *AgentSession) SetPersist(persist func(*registry.AgentSessionEntry) error) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.persist = persist
}

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
	as.pid = 0
	as.stat = StatusExited
	as.lastRunAt = time.Now()
	as.exitCode = &code
	// F-61: clear suspect state on terminal transition — a dead AS
	// has nothing left to probe. The cooldown window is implicit
	// (next Spawn will overwrite SuspectReason anyway).
	as.suspectReason = ""
	as.suspectSince = nil
	persist := as.persist
	as.asMu.Unlock()

	// Best-effort persistence. Failures fall through — the next
	// status transition will retry. Symmetric with endPrompt's
	// persist hook (readpump.go:231).
	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after SetExited failed; JSON may be stale",
				"as_id", as.ID, "err", err)
		}
	}
}

// SetSuspect (F-61) marks the AS as suspect for the given reason.
// Persists the new state so a daemon restart can honor the
// cooldown window. Cooldown window is enforced by the prober
// (5min per AS) — not here. Setting the same reason twice is a
// no-op (only the first timestamp sticks) so spam from a flapping
// bridge doesn't reset the cooldown.
func (as *AgentSession) SetSuspect(reason string) {
	if reason == "" {
		return
	}
	as.asMu.Lock()
	if as.suspectReason == reason {
		as.asMu.Unlock()
		return
	}
	now := time.Now()
	as.suspectReason = reason
	as.suspectSince = &now
	persist := as.persist
	as.asMu.Unlock()

	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after SetSuspect failed; JSON may be stale",
				"as_id", as.ID, "reason", reason, "err", err)
		}
	}
}

// ClearSuspect (F-61) removes the suspect marking. Called when a
// prompt ends cleanly (KindPromptEnded in pump_events) so the
// prober stops considering this AS for proactive respawn.
func (as *AgentSession) ClearSuspect() {
	as.asMu.Lock()
	if as.suspectReason == "" {
		as.asMu.Unlock()
		return
	}
	as.suspectReason = ""
	as.suspectSince = nil
	persist := as.persist
	as.asMu.Unlock()

	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after ClearSuspect failed; JSON may be stale",
				"as_id", as.ID, "err", err)
		}
	}
}

// ClearInFlight (F-62) drops the in-flight message mirror without
// firing the Prompt end lifecycle. Used at chat-session "new
// session" boundaries — /cwd switch (SetSelectedCwd) and the
// hadPrior branch of LookupSelectedAgentSession — to declare the
// previous (agent, cwd) focus lost. Idempotent (no-op on empty
// slice). Persists the empty state so the next daemon restart
// does not re-push the abandoned messages.
//
// Differs from endPrompt(reason) in two ways:
//   - Does not emit KindPromptEnded (no receipt card transition).
//   - Does not touch currentPrompt / isReady — the AS is
//     detached here, so the readPump's subscribers are already
//     gone.
//
// See docs/feat/F-62-inflight-cwd-home.md §3.3.4.
func (as *AgentSession) ClearInFlight() {
	as.asMu.Lock()
	if len(as.inFlightMessages) == 0 {
		as.asMu.Unlock()
		return
	}
	as.inFlightMessages = nil
	persist := as.persist
	as.asMu.Unlock()

	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after ClearInFlight failed; JSON may be stale",
				"as_id", as.ID, "err", err)
		}
	}
}

// Suspect (F-61) returns the current suspect reason and since
// timestamp. Used by the prober to decide whether to probe + respawn.
// Both are zero-valued when not suspect.
func (as *AgentSession) Suspect() (reason string, since time.Time) {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	if as.suspectSince == nil {
		return as.suspectReason, time.Time{}
	}
	return as.suspectReason, *as.suspectSince
}

// SetSuspectAt (F-61, test-only) sets the suspect state with an
// explicit timestamp. Exported because tests need to backdate
// SuspectSince to exercise the cooldown gate. Production code
// uses SetSuspect (no timestamp arg).
func (as *AgentSession) SetSuspectAt(reason string, at time.Time) {
	if reason == "" {
		return
	}
	as.asMu.Lock()
	as.suspectReason = reason
	as.suspectSince = &at
	persist := as.persist
	as.asMu.Unlock()
	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after SetSuspectAt failed",
				"as_id", as.ID, "err", err)
		}
	}
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
	msgs := as.inFlightMessages
	sr := as.suspectReason
	ss := as.suspectSince
	var ec *int
	if as.exitCode != nil {
		v := *as.exitCode
		ec = &v
	}
	as.asMu.RUnlock()

	return &registry.AgentSessionEntry{
		ID:               as.ID,
		ChatSessionID:    as.ChatSessionID,
		Agent:            as.Agent,
		Cwd:              as.Cwd,
		PID:              as.PID(),
		Status:           stat,
		Args:             as.Args(),
		SessionID:        resume,
		CreatedAt:        as.createdAt,
		LastRunAt:        lastRun,
		ExitCode:         ec,
		Model:            model,
		InFlightMessages: msgs,
		SuspectReason:    sr,
		SuspectSince:     ss,
	}
}

// ErrNotRunning is returned by SendBlocks/Close when called
// before Spawn() succeeds.
var ErrNotRunning = errors.New("chatsession: AgentSession not running (Spawn not called or failed)")

// ErrAgentNotFound is returned by pool-lookup helpers when the
// (agent, cwd) key has no AgentSession. Re-exported from chatsession
// as chatsession.ErrAgentNotFound for back-compat.
var ErrAgentNotFound = errors.New("agentsession: agent not in pool")

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
	// F-61: explicit user-driven spawn overrides any prior /close
	// intent. Without this, after /close the next user message
	// would still see closedByUser=true and... actually respawn's
	// internal rollback handles the race. But clearing here makes
	// the intent explicit: user said "spawn" so spawn we will.
	as.closedByUser = false
	_ = as.pid // respawn will read it under its own RLock
	as.asMu.Unlock()

	// Reap + spawn are owned by respawn (single source of truth);
	// Spawn just delegates. respawn will reap oldPID before launching
	// the new child.
	return as.respawn(ctx, spawner, args, resume)
}

// respawn is the single source of truth for "fork a fresh child and
// wire it into this AgentSession". Both Spawn (cold start / crash
// recovery) and AgentSession.New's PTY fallback (ErrRestartRequired)
// route through here, so reap + spawn + state-update + readpump-start
// live in exactly one place.
//
// Preconditions / contract:
//   - Caller must NOT hold as.asMu (this function takes and releases it).
//   - Caller is responsible for closing any previous handle BEFORE
//     calling. respawn does NOT close the old handle — it only
//     reaps the orphan PID as defense-in-depth (in case Close failed
//     or the runtime crashed and left a child behind).
//   - respawn does NOT touch as.sessionID. The caller decides
//     whether to clear it (New's fallback does; Spawn preserves it
//     so a future Spawn with cfg.SessionID can resume).
//
// On failure respawn marks the session Exited with exitCode=-1 so
// the next caller sees a consistent state instead of a stale
// closed handle left dangling.
func (as *AgentSession) respawn(
	ctx context.Context,
	spawner Spawner,
	args []string,
	sessionID string,
) error {
	// Snapshot the orphan PID + previous stat under lock (no writer in
	// flight yet). prevStat drives the failure-path semantics: only
	// demote Running → Exited; preserve Detached (never ran) and
	// Exited (already exited) so callers can distinguish them.
	as.asMu.RLock()
	oldPID := as.pid
	prevStat := as.stat
	as.asMu.RUnlock()

	// Reap any orphan child from a previous (crashed) runtime before
	// launching. Even when the caller has already Close()'d the
	// previous handle, this catches the case where Close silently
	// failed or the runtime died leaving a child behind.
	if err := reapOrphan(oldPID); err != nil {
		return fmt.Errorf("chatsession: reap orphan pid %d: %w", oldPID, err)
	}

	// Fork+exec+handshake may take seconds; don't hold asMu across it
	// — EventHandler / readPump / footer renderers would block.
	handle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, args, sessionID)
	if err != nil {
		as.asMu.Lock()
		as.handle = nil
		as.pid = 0
		// Only demote Running → Exited. A fresh AS (Detached) that
		// fails to spawn should stay Detached — the test
		// TestAgentSession_SpawnFailureLeavesDetached pins this so
		// callers can tell "we tried and failed" (Running → Exited)
		// apart from "we never even tried successfully" (Detached).
		if prevStat == StatusRunning {
			as.stat = StatusExited
			code := -1
			as.exitCode = &code
		}
		as.lastRunAt = time.Now()
		as.asMu.Unlock()

		// fix-stop (2026-08-15): if the bridge rejected the saved
		// sessionID with agent.ErrResumeUnhealthy, clear the saved
		// sessionID and persist so the chat layer's
		// LookupSelectedAgentSession retry path (which catches
		// this same error and re-Spawns) lands on a clean fresh
		// session. Without this clear the retry would re-pass the
		// same stale sessionID and re-fail identically — the user
		// would see "Failed to spawn agent" on every message until
		// they edit agent_sessions.json manually.
		//
		// Cases this catches:
		//   - daemon restart with an AS restored from disk whose
		//     sessionID the upstream CLI no longer recognizes
		//     (e.g. nightly cleanup of stale threads, or a
		//     different host).
		//   - claudecode SIGINT-fallback path (the stdin pipe was
		//     broken, so SIGINT terminated the CLI; the chat layer
		//     respawns with --resume, hits the stale-id branch).
		//     With the post-fix-stop control_request path this is
		//     the exception rather than the rule — see
		//     internal/bridge/claudecode/claudecode.go::Stop for
		//     the primary happy path.
		//
		// Keeping the SessionID clear here does NOT throw away
		// the bridge's loud failure signal: we still return the
		// wrapped error so callers that want to surface it (e.g.
		// dispatcher in non-recovery mode) can.
		if errors.Is(err, agent.ErrResumeUnhealthy) {
			as.asMu.Lock()
			cleared := as.sessionID != ""
			as.sessionID = ""
			persist := as.persist
			as.asMu.Unlock()
			slog.Warn("agentsession: resume rejected, cleared sessionID for fresh retry",
				"as_id", as.ID, "agent", as.Agent)
			if cleared && persist != nil {
				if perr := persist(as.Entry()); perr != nil {
					slog.Warn("agentsession: persist after clearing sessionID failed; next spawn may re-use stale id",
						"as_id", as.ID, "err", perr)
				}
			}
		}

		return fmt.Errorf("chatsession: respawn %s at %s: %w", as.Agent, as.Cwd, err)
	}

	as.asMu.Lock()
	as.handle = handle
	as.pid = handle.PID()
	as.stat = StatusRunning
	as.lastRunAt = time.Now()
	as.exitCode = nil
	// F-61: race-detect against Close() during the spawn. fork+
	// exec+handshake is multi-second; Close() can flip
	// closedByUser=true between our snapshot and here. If so,
	// roll back: close the freshly-spawned bridge, mark Exited.
	if as.closedByUser {
		h := as.handle
		as.handle = nil
		as.pid = 0
		as.stat = StatusExited
		code := -1
		as.exitCode = &code
		as.lastRunAt = time.Now()
		as.asMu.Unlock()
		if h != nil {
			_ = h.Close()
		}
		if persist := as.persist; persist != nil {
			if err := persist(as.Entry()); err != nil {
				slog.Warn("agentsession: persist after rollback respawn failed",
					"as_id", as.ID, "err", err)
			}
		}
		return nil
	}
	persist := as.persist
	as.asMu.Unlock()

	// F-61: persist on success so agent_sessions.json reflects
	// the new PID/status immediately (otherwise the previous
	// SetExited writeback sits on disk until the next transition).
	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after respawn success failed; JSON may be stale",
				"as_id", as.ID, "err", err)
		}
	}

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

// Handle returns the bridge-level agent.Agent (nil if not yet
// spawned). Exposed for callers that need direct access (e.g., the
// Handle returns the bridge-level live handle (nil if not yet
// spawned). Production code uses the typed methods on *Agent
// (PID/Events/Send*/Close). Tests that need bridge-specific state
// use Handle().Driver() and type-assert to the bridge's driver
// type.
func (as *AgentSession) Handle() *agent.Agent {
	as.asMu.RLock()
	defer as.asMu.RUnlock()
	return as.handle
}

// Events (multi-as Phase 1) returns the per-AS enriched event channel
// for ChatSession. Reads from this channel receive EnrichedEvent
// values (KindAgentEvent / KindPromptEnded / KindLifecycle) instead
// of raw bridge events, with Prompt anchoring already applied.
//
// Implementation: events flow through `as.eventQueue` (filled by the
// read pump, drained by the dispatcher which publishes onto
// `as.EventBus`). `as.Events()` returns `eventQueue` directly so
// callers that read it observe events as soon as the read pump
// pushes them — before the dispatcher publishes them onto the bus.
//
// Subscribers that prefer the bus interface should use
// `as.EventBus.Subscribe(...)` directly. ChatSession's PumpEvents
// keeps using `as.Events()` until S2 replaces it with a bus-based
// subscription on attach.
//
// The returned channel is closed by Shutdown after the readpump
// goroutine exits. Returns nil before the AgentSession is fully
// constructed (defensive — should not happen in production).
func (as *AgentSession) Events() <-chan EnrichedEvent {
	as.asMu.RLock()
	eq := as.eventQueue
	as.asMu.RUnlock()
	return eq
}

// IsReady (CS-AS 边界重构 Phase 1) reports whether the AS can
// accept a new Submit. True when no Prompt is currently in flight
// (currentPrompt == nil). The flag flips false on Submit and true
// again at endPrompt (Spawn also re-arms it).
//
// IsReady does NOT directly track process-alive status — it
// reflects the prompt lifecycle, not the bridge process. If the
// bridge dies between Submit and endPrompt, IsReady stays false
// until the readpump observes the closed events channel and calls
// endPrompt; callers in that small window may observe IsReady=false
// alongside a dead process. ChatSession.TryFlush polls this before
// calling Submit. Atomic load — safe to call concurrently with
// Submit / endPrompt.
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
// Concurrency: asMu briefly during commit, but SendBlocks runs
// outside the lock (it can block on a hung prompt RPC). Two
// concurrent Submits are serialized by the bridge's turnMu
// (pi) or single-threaded access (claudecode / pty).
//
// Ordering — set currentPrompt BEFORE SendBlocks so that any
// response events (assistant / result) observed by the readpump
// after SendBlocks returns are anchored to the user message.
// The previous ordering (SendBlocks → currentPrompt) created a
// race: a sub-millisecond-responding child (the integration
// test's bash mock, or a CLI that hot-caches its prior turn)
// could emit the full assistant + result envelope before
// currentPrompt was committed, in which case the readpump
// stamps UserMsgID="" on those events. The persisted
// InFlightMessages mirror is committed in the same atomic
// step as currentPrompt so Entry() always observes a
// consistent pair. If SendBlocks fails after the commit, the
// commit is rolled back (currentPrompt + inFlightMessages
// cleared, isReady flipped back to true) so the queue's
// Retry/Rewind path on the next TryFlush sees a clean AS —
// matching the pre-fix contract that a failed Submit leaves
// the AS in the same state as before the call (verified by
// TestSubmit_FailureLeavesInFlightEmpty).
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

	// Commit: install currentPrompt and flip isReady BEFORE the
	// bridge call. See the function-level ordering comment for
	// the rationale (anchor race fix).
	//
	// Blocks is defensively copied: Message.Blocks is a slice
	// header that aliases the queue's storage, and the persisted
	// InFlightMessageRef may later be re-read by RestoreFromRegistry
	// and pushed into a fresh queue (see Manager.RestoreFromRegistry).
	// Copying here keeps the in-memory mirror independent of the
	// prompt's Messages and safe to round-trip through the registry
	// without aliasing any other slice.
	as.asMu.Lock()
	as.currentPrompt = p
	as.inFlightMessages = make([]registry.InFlightMessageRef, len(p.Messages))
	for i, m := range p.Messages {
		as.inFlightMessages[i] = registry.InFlightMessageRef{
			ID:         m.ID,
			Blocks:     append([]agent.ContentBlock(nil), m.Blocks...),
			ReceivedAt: m.ReceivedAt,
		}
	}
	as.asMu.Unlock()
	as.isReady.Store(false)

	// SendBlocks can block on a hung prompt RPC; do NOT hold asMu.
	err := h.SendBlocks(as.OpContext(), p.Blocks)
	if err != nil {
		slog.Warn("chatsession: Submit SendBlocks FAILED",
			"chat_id", as.ChatSessionID,
			"as_id", as.ID,
			"err", err)
		// Roll back the commit. Without this, a failed Submit
		// would leave currentPrompt + isReady=false set, and the
		// next TryFlush would skip (as_not_ready) until something
		// else (e.g. an endPrompt from a stale event) cleared
		// them. Compare with the pre-fix contract verified by
		// TestSubmit_FailureLeavesInFlightEmpty.
		as.asMu.Lock()
		if as.currentPrompt == p {
			as.currentPrompt = nil
			as.inFlightMessages = nil
		}
		as.asMu.Unlock()
		as.isReady.Store(true)
		return err
	}
	slog.Debug("chatsession: Submit SendBlocks ok",
		"chat_id", as.ChatSessionID,
		"as_id", as.ID,
		"prompt_id", p.ID)

	// Best-effort persistence — failures must NOT roll back the
	// commit, since SendBlocks already accepted the prompt and the
	// bridge is now expecting a reply. The next status change
	// (endPrompt) will retry the write.
	//
	// Concurrency note: endPrompt may fire between our Unlock and
	// the persist call below. as.Entry() takes asMu.RLock so the
	// snapshot it returns reflects the latest in-memory state —
	// if endPrompt's clear has run, Entry() sees a nil
	// InFlightMessages and we persist that. The disk always
	// converges to the in-memory state; we never observe a stale
	// non-empty InFlightMessages after the prompt actually ended.
	if as.persist != nil {
		if err := as.persist(as.Entry()); err != nil {
			slog.Warn("chatsession: persist after Submit failed; entry may be stale on restart",
				"as_id", as.ID, "err", err)
		}
	}
	return nil
}

// SendBlocks delivers structured content blocks. Returns ErrNotRunning
// if Spawn has not been called.
//
// The ctx passed to SendBlocks flows to the bridge's SendBlocks, which
// derives its own per-call callCtx. For AS-owned cancellation semantics
// (i.e. wake any in-flight send when the AS is deactivated), callers
// should pass as.OpContext() — the AS-owned ctx installed by
// Activate(parent).
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
// Returns one of:
//
//   - nil: bridge handled in-place reset (claudecode's writeLine("/clear"),
//     pi's new_session RPC, acp's session/new, codex's thread reset).
//     AgentSession.ID / Cwd / pool membership are preserved; only
//     the bridge's internal conversation state is cleared. The
//     bridge is expected to emit a fresh EventAgentReady carrying
//     the new SessionID; the runtime's AgentEventBus subscriber
//     captures it via SetSessionID and persists.
//
//   - - agent.ErrRestartRequired: bridge cannot do in-place reset
//     (raw PTY bridge). The CALLER is responsible for the
//     kill-and-respawn fallback — AgentSession.New is deliberately
//     not coupled to a Spawner, because 4/5 bridges never need one.
//     ChatSession.NewActiveAgentSessions handles this case at the
//     chat layer.
//
//   - other errors: bridge tried but failed (transient). Propagated
//     to the caller; InputBuffer is NOT cleared by the wrapper.
func (as *AgentSession) New(ctx context.Context) error {
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
	if h == nil {
		return ErrNotRunning
	}
	return h.New(ctx)
}

// HandlePTYRestart implements the PTY fallback for in-place reset.
// Called by the chat layer when as.New(ctx) returns
// agent.ErrRestartRequired. AS owns the full kill + respawn
// lifecycle so callers don't need to reach into internals
// (asMu, readpumpStarted, respawn).
//
// Preconditions:
//   - as.handle is non-nil (as was previously spawned successfully).
//   - launcher is non-nil (caller's responsibility to inject).
//
// Returns:
//   - nil on success; handle has been swapped and readpump reset.
//   - agent.ErrRestartRequired if launcher is nil (caller's bug).
//   - any error from launcher.Launch (after respawn marks Exited).
func (as *AgentSession) HandlePTYRestart(ctx context.Context, launcher Spawner) error {
	if launcher == nil {
		return agent.ErrRestartRequired
	}
	// Close the old handle. Swallow the close error — respawn is
	// the source of truth for "did reset succeed".
	if h := as.handle; h != nil {
		_ = h.Close()
	}
	// Reset the readpump so a fresh drainer starts on the new handle.
	as.asMu.Lock()
	as.readpumpStarted = false
	as.asMu.Unlock()
	// respawn with empty sessionID — we deliberately want a fresh
	// conversation, not a resume of the previous one.
	if err := as.respawn(ctx, launcher, as.args, ""); err != nil {
		as.SetSessionID("")
		return err
	}
	as.SetSessionID("")
	return nil
}

// Close terminates the bridge child (sends shutdown signal to the
// underlying bridge). Idempotent. Marks status=Exited on success.
func (as *AgentSession) Close() error {
	as.asMu.Lock()
	// F-61: mark this death as user-initiated so the Lifecycle
	// handler's immediate-respawn path skips this AS. Without
	// this flag, /close would race with the respawn and leave
	// a freshly-spawned bridge that the user didn't ask for.
	as.closedByUser = true
	h := as.handle
	as.asMu.Unlock()
	if h == nil {
		return nil // not running
	}
	return h.Close()
}

// RestartFromDeath (F-61) is the synchronous recovery path called
// from chatsession.routeEvent's KindLifecycle handler. It forks a
// fresh bridge with the SAME sessionID (so --resume picks up the
// user's in-flight message from the bridge's JSONL history) and
// re-arms the readpump.
//
// Returns nil on a clean respawn; the AS is then StatusRunning
// and the caller's TryFlush will drain any queued messages. On
// error the AS stays StatusExited and the watchdog/prober takes
// over with its 5min cooldown.
//
// ClosedByUser skips the respawn entirely — the /close path goes
// through here when the bridge actually exits AFTER Close was
// called, and we don't want to undo the user's intent.
func (as *AgentSession) RestartFromDeath(ctx context.Context, launcher Spawner) error {
	if launcher == nil {
		return ErrSpawnerNotSet
	}
	as.asMu.Lock()
	if as.closedByUser {
		as.asMu.Unlock()
		return nil
	}
	resume := as.sessionID
	args := append([]string(nil), as.args...)
	as.asMu.Unlock()

	if err := as.respawn(ctx, launcher, args, resume); err != nil {
		return err
	}

	// F-61: respawn returned nil. Check whether closedByUser got
	// re-set by a racing Close() during the spawn. If so, respawn
	// already rolled back the bridge and AS is back to Exited —
	// do NOT clear closedByUser (user wants it closed).
	as.asMu.Lock()
	if as.closedByUser {
		// respawn's internal rollback already handled cleanup.
		as.asMu.Unlock()
		return nil
	}
	persist := as.persist
	as.asMu.Unlock()

	if persist != nil {
		if err := persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after RestartFromDeath failed",
				"as_id", as.ID, "err", err)
		}
	}
	return nil
}

// Stop halts execution of the in-flight turn on the bridge without
// tearing down the AgentSession. After Stop returns, the bridge's
// responsibility for the old turn is over: the bridge may emit a
// terminal event (EventAgentDone / EventAgentError), exit the child
// process, or neither — the chat layer's TryFlush loop watches
// IsReady() and reschedules the next queued prompt automatically
// once the bridge settles.
//
// Stop is fire-and-forget: it does NOT block waiting for IsReady()
// to flip back. Callers should treat the call as fire-and-forget;
// the chat layer coordinates the turn-end → next-submit transition
// via its existing KindPromptEnded handler.
//
// Bridges that cannot honor Stop return agent.ErrNotSupported; the
// caller can detect with errors.Is and fall back to Close() (full
// /close semantics for this AgentSession).
//
// Returns ErrNotRunning if Spawn has not been called (handle is
// nil). Distinct from Close(): Stop does not change the
// AgentSession's lifecycle status and does not drain the events
// channel — it only signals the bridge to halt its current work.
func (as *AgentSession) Stop(ctx context.Context) error {
	as.asMu.RLock()
	h := as.handle
	as.asMu.RUnlock()
	if h == nil {
		return ErrNotRunning
	}
	return h.Stop(ctx)
}


// newAgentSessionID returns a unique ID for an AgentSession. v1.2
// commit 6 uses a simple counter-based scheme for testability;
// commit 7 may swap to UUID v7.
var agentSessionCounter atomic.Uint64

func newAgentSessionID() string {
	n := agentSessionCounter.Add(1)
	return fmt.Sprintf("as_%d_%d", time.Now().UnixNano(), n)
}

// --- Test-only API --------------------------------------------------
//
// These methods exist so cross-package tests (chatsession package
// in particular) can build up AgentSession state without going
// through Spawn / Spawn-then-kill cycles. Production code MUST NOT
// use them — use Spawn / SetRunning / SetDetached / SetExited /
// SetSessionID / SetModel instead.
//
// They live in production code (not _test.go) so cross-package tests
// can call them. The "ForTest" suffix is the convention.

// SetHandleForTest injects a bridge handle. Caller is responsible
// for keeping the handle alive for the duration of any usage.
func (as *AgentSession) SetHandleForTest(h *agent.Agent) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.handle = h
}

// SetStatusForTest sets the runtime status directly.
func (as *AgentSession) SetStatusForTest(s Status) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.stat = s
}

// SetPIDForTest sets the OS PID directly.
func (as *AgentSession) SetPIDForTest(pid int) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.pid = pid
}

// SetCurrentPromptForTest installs the in-flight Prompt directly.
// Used by tests that bypass Submit.
func (as *AgentSession) SetCurrentPromptForTest(p *Prompt) {
	as.asMu.Lock()
	defer as.asMu.Unlock()
	as.currentPrompt = p
}

// SetIsReadyForTest toggles the isReady atomic flag.
func (as *AgentSession) SetIsReadyForTest(v bool) {
	as.isReady.Store(v)
}
