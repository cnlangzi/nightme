// aggregate_sink.go — multi-RunOnce event aggregator for /review.
//
// v12 design (docs/REVIEW.md §2.5.5): eventAggregator turns N per-job
// AgentEvent streams into ONE outer stream that the chat channel
// reads as a single review lifecycle. Three invariants drive the
// design:
//
//	#11 single outer lifecycle — chat sees exactly one Ready and one
//	    Result, no matter how many per-job RunOnce calls run.
//	#12 per-job ToolStart/End pairing — ToolStart is buffered until
//	    the matching ToolEnd arrives; both events then forward as a
//	    contiguous pair (Start, End). No half-open tool calls leak
//	    to the chat.
//	#13 cross-job task list dedup — TaskCreate/Update events are
//	    merged by AgentTaskItem.ID (latest-wins) and forwarded as
//	    ONE snapshot. Avoids N duplicate checklist entries.
//
// The fourth rule is the delivery surface: a per-job chunk is not an
// outbound message of its own. The findings — the group's text — are
// collected and handed over as one reply when the group ends, so one
// group reads as one piece instead of as fragments mixed with the
// other groups; a group whose findings exceed one message produces
// consecutive replies that carry the same group's number. The
// reasoning — the group's thinking — keeps streaming while the group
// works, coalesced into batches of roughly 2 seconds or 2 KiB. Every
// delivered batch carries its group's number ("[1] ") inline, and the
// findings ride the thinking kind, so they are rendered in the
// thread. A batch is lossless — the bodies of a group's delivered
// replies concatenate back into the group's raw text.
//
// State machine (three phases):
//
//	phaseBuffering — every per-job event lands in perJob.initBuffer;
//	    no outer forwarding. Track readyCount (per-job Ready events
//	    observed) and doneCount (per-job Result/Error events observed,
//	    guarded by perJob.terminalSeen to prevent double-counting
//	    across Phase 1/2). When readyCount == expected, transition.
//
//	phaseStreaming — Phase 1→2 transition fires a synthetic outer
//	    Ready (merged metadata from the first per-job Ready;
//	    Source=""), replays each perJob.initBuffer through
//	    handleStreaming (so pairing/merging apply uniformly to
//	    replayed and live events), then continues processing live
//	    events: ToolStart/End paired contiguously, TaskCreate/Update
//	    merged by ID, text/thinking coalesced per job, Result/Error
//	    counted but not forwarded.
//	    When doneCount == expected, transition.
//
//	phaseClosed — synthetic outer Result fires (Source=""), late
//	    events dropped.
//
// Concurrency: all shared state guarded by a.mu. Outer forwarding
// (`outer(ev)`) is invoked OUTSIDE the lock to avoid re-entrant
// lock issues and to keep callbacks cheap.
package agent

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxConcurrentReviewJobs caps how many parallel RunOnce jobs a single
// /review may spawn. 4 is the documented ceiling (REVIEW.md §2.5.3):
// too high risks token / API-rate blow-up, too low starves large
// changesets. The cap is enforced by a semaphore inside DelegateReview
// — this package only does the event-side aggregation.
const maxConcurrentReviewJobs = 4

// aggregatorPhase tracks the lifecycle of an /review multi-job
// session. Transitions are guarded by `phase` updates under the
// aggregator mutex; readers (the dispatch in handle) read a snapshot
// of the current phase before routing.
type aggregatorPhase int

const (
	// phaseBuffering is the initial state. All per-job events go
	// into perJob.initBuffer; the outer sink receives nothing.
	// Transition fires when readyCount reaches the expected job
	// count.
	phaseBuffering aggregatorPhase = iota

	// phaseStreaming is the live streaming state entered after all
	// per-job Readys are observed. Process events are forwarded
	// immediately (paired / merged / source-tagged per the v12
	// rules); per-job terminals increment doneCount; transition
	// fires when doneCount reaches the expected job count.
	phaseStreaming

	// phaseClosed is the terminal state. The synthetic outer Result
	// has been forwarded; late events are dropped. entered after
	// doneCount == expected.
	phaseClosed
)

// perJobState is per-source state held by the aggregator. One
// perJobState is allocated per wrapJob call (i.e. one per parallel
// RunOnce job).
//
// Note: doneCount is NOT tracked per-job in this struct. It's
// driven from the orchestrator (delegateReviewMultiJob calls
// agg.markJobDone() in each per-job goroutine's defer block —
// see markJobDone's doc for why sink-event-driven counting was
// replaced with completion-driven counting).
type perJobState struct {
	// index is the group's position in the fan-out order — the number
	// the reader sees in front of the group's findings. Set once, when
	// the job is wrapped; never mutated.
	index int

	// pendingToolStarts is the per-job ToolStart buffer keyed by
	// AgentToolStartEvent.ID. When a ToolEnd with a matching ID
	// arrives, both events are forwarded contiguously (Start first,
	// then End). Map access is guarded by the aggregator's mutex;
	// the field is intentionally package-private so the closure
	// inside wrapJob can capture it.
	pendingToolStarts map[string]AgentEvent

	// initBuffer accumulates every per-job event received during
	// phaseBuffering. Replayed through handleStreaming at the
	// phaseBuffering → phaseStreaming transition so pairing / task
	// merging apply uniformly (the same rules govern replay and live
	// events). Cleared after replay to release memory.
	initBuffer []AgentEvent

	// pendingText is the group's review text that has not been cut
	// yet, and pendingThinking is its thinking. Both are coalescing
	// batches — see pendingBatch.
	pendingText     pendingBatch
	pendingThinking pendingBatch

	// textChunks holds the group's findings, already cut into the
	// pieces they are delivered in. The group hands them over when it
	// ends: one reply per piece, so a group whose findings fit in one
	// message produces one reply.
	textChunks []flushedBatch

	// announced records that the group's start hint has been emitted.
	// The hint goes out once, when the group's first output arrives.
	announced bool

	// deliveredText holds every text piece the group delivered, in
	// order. It is the text the user has already seen — the tail a
	// terminal event carries is checked against it, so a bridge
	// that repeats the reply on the terminal does not deliver it
	// twice.
	deliveredText []string
}

// pendingBatch is one coalescing batch: the pieces collected so far,
// the event that opened it (its metadata rides the flushed event),
// and the time the batch was opened — the age the time trigger
// measures.
//
// The pieces are kept apart because the reply of a turn is assembled
// from its pieces joined by a newline (see sessionText) — a batch
// that concatenated them without the separator would run two
// sentences of the reply together.
type pendingBatch struct {
	pieces  []string
	size    int
	started time.Time
	ev      AgentEvent
	kind    EventKind
}

// body returns the batch's text: its pieces joined by a newline.
func (b *pendingBatch) body() string {
	return strings.Join(b.pieces, "\n")
}

// append adds a piece to the batch and returns the updated size.
func (b *pendingBatch) append(piece string) int {
	if len(b.pieces) > 0 {
		b.size++ // the newline between the pieces
	}
	b.pieces = append(b.pieces, piece)
	b.size += len(piece)
	return b.size
}

// reset empties the batch.
func (b *pendingBatch) reset() {
	b.pieces = nil
	b.size = 0
	b.started = time.Time{}
	b.ev = AgentEvent{}
	b.kind = 0
}

// The coalescing triggers. The findings are cut when a batch reaches
// coalesceBytes of body; the reasoning is cut when it reaches
// coalesceBytes of body or when it has been open for coalesceInterval
// — whichever comes first.
//
// The size keeps a batch from growing without end; the interval
// keeps the user watching the review being written instead of
// staring at a card that has not moved for a minute. Neither is a
// hard bound: a single event larger than the size trigger is
// delivered as it is.
//
// The clock does not cut the findings: they are handed over when the
// group ends, and a clock-cut batch would split one group's findings
// over several replies.
const (
	// coalesceBytes is the body size that flushes a pending batch.
	coalesceBytes = 2048
	// coalesceInterval is how long a pending batch may stay open
	// before the next event flushes it.
	coalesceInterval = 2 * time.Second
)

// eventAggregator merges N per-job AgentEvent streams into one outer
// stream. Constructed via newEventAggregator; per-job sinks are
// created with wrapJob.
//
// Field grouping: counters and emit-flags live next to each other
// (all guarded by mu); per-job state lives in perJob (also guarded
// by mu); the tasks dedup map is guarded by mu; the outer callback
// is set at construction and never reassigned.
type eventAggregator struct {
	outer func(AgentEvent)

	// plan is the fan-out's plan — one entry per group, in fan-out
	// order. The aggregator needs it to name the groups: the start
	// hint carries the group's name and its file count, and the
	// group's number is its position in the plan. Never mutated
	// after construction.
	plan []ReviewPlanGroup

	mu     sync.Mutex
	emitMu sync.Mutex   // guards pairs of a.outer() calls (invariant #12)
	phase  atomic.Int32 // Finding 3 from /review: atomic load/store (data race per Go mem model)
	perJob map[string]*perJobState

	// Counters incremented under mu. readyCount tracks per-job
	// Ready emissions; doneCount tracks per-job Result/Error
	// emissions (with perJob.terminalSeen dedup).
	readyCount int
	doneCount  int

	// tasks is the cross-job AgentTaskList dedup table, keyed by
	// AgentTaskItem.ID. Latest write wins (so an Update overwrites
	// an earlier Create with the same ID). Guarded by mu.
	tasks map[string]AgentTaskItem

	// firstJobMeta caches the metadata (SessionID / Model /
	// AgentName / Workspace / Branch) from the first per-job Ready
	// observed. Used to populate synthetic TaskCreate/Update
	// events so they carry the same context as the synthetic
	// outer Ready — parity with buildSyntheticReady's contract
	// (Finding 3 from /review). Source field is preserved
	// separately (per-job label). Guarded by mu.
	firstJobMeta AgentEvent

	// readyEmitted and finalEmitted are one-shot guards so the
	// synthetic outer Ready / Result fires exactly once even under
	// racy transitions. Guarded by mu.
	readyEmitted bool
	finalEmitted bool
}

// newEventAggregator constructs an aggregator. outer is the outer
// sink callback (typically the one returned by
// outbound.StreamRunOnceToEmitter); plan is the fan-out's plan — one
// entry per group, in fan-out order.
//
// The aggregator starts in phaseBuffering with no events recorded.
// The first wrapJob call allocates a perJobState for the named job
// source; subsequent calls reuse / replace it via the perJob map
// (one job per source label).
func newEventAggregator(outer func(AgentEvent), plan []ReviewPlanGroup) *eventAggregator {
	return &eventAggregator{
		outer:  outer,
		plan:   plan,
		perJob: make(map[string]*perJobState, len(plan)),
		tasks:  make(map[string]AgentTaskItem),
	}
}

// groupSource is the source label of the group at the given index —
// the fan-out position, one-based.
func groupSource(index int) string {
	return "group-" + strconv.Itoa(index+1)
}

// wrapJob returns a per-job sink for the group at the given index.
// The index is the group's position in the fan-out order — the number
// the reader sees in front of the group's findings.
//
// The group's source label ("group-1", "group-2") becomes the Source
// field on every event the sink forwards in phaseStreaming, and is
// the key into the perJob map for pairing / replay state.
//
// Each returned closure is safe to call concurrently from its own
// goroutine (per-job bridge reader goroutine); all shared state is
// mu-guarded.
//
// The closure stamps Source=src on every event before dispatch — so
// the streaming handler can route on Source for per-job pairing
// without trusting callers to do so.
func (a *eventAggregator) wrapJob(groupIdx int) func(AgentEvent) {
	src := groupSource(groupIdx)
	a.mu.Lock()
	state := &perJobState{
		index:             groupIdx,
		pendingToolStarts: make(map[string]AgentEvent),
	}
	a.perJob[src] = state
	a.mu.Unlock()

	return func(ev AgentEvent) {
		ev.Source = src
		a.handle(ev, state)
	}
}

// handle is the per-event entry. It reads the current phase under
// the lock, releases it, then dispatches by phase. We snapshot
// (rather than dispatching under the lock) so the per-phase handlers
// can call a.outer without re-entering the lock.
//
// Phase read + dispatch are NOT atomic — Finding 4 from /review:
// between unlock and the handler's own lock, a concurrent
// transition (Phase 1 → Phase 2 in handleBuffering) can run,
// leaving a stale event to enter the wrong phase. handle()'s
// callers are per-job goroutines that are themselves serial
// (one event at a time), so the TOCTOU window is narrow but
// real. We don't fix it by collapsing the read+dispatch into a
// single locked section (that would force a.outer under the
// lock). Instead, handleBuffering and handleStreaming both
// RE-CHECK phase under their own lock and redirect to the
// current phase if it has changed. The re-check is a cheap
// atomic read.
func (a *eventAggregator) handle(ev AgentEvent, state *perJobState) {
	a.mu.Lock()
	phase := aggregatorPhase(a.phase.Load())
	a.mu.Unlock()

	switch phase {
	case phaseBuffering:
		a.handleBuffering(ev, state)
	case phaseStreaming:
		a.handleStreaming(ev, state)
	case phaseClosed:
		// Late event after the synthetic outer Result fired:
		// drop silently. Logging at this volume would be noisy —
		// the bridge layer is responsible for surfacing its own
		// errors via its sink BEFORE returning, so anything
		// arriving here is genuinely out-of-band.
	}
}

// markJobDone is called by the orchestrator's per-job goroutine
// in a defer (delegateReviewMultiJob). It is the canonical
// "this job's RunOnce returned" signal — fired once per
// goroutine regardless of success or failure, and INDEPENDENT
// of whether the sink ever received a terminal event.
//
// Why this design (refactor from /review): previously doneCount
// was sink-driven (incremented when EventAgentResult/Error
// arrived at the aggregator). That worked for happy paths but
// silently failed when a bridge's error path bypassed the sink
// (Findings 1 & 2 from /review): doneCount stayed short of
// expected and the aggregator hung in phaseStreaming until the
// 30-min revCtx timeout. Completion-driven counting sidesteps
// the entire failure mode:
//
//   - spawn fail before sink wiring → defer still fires markJobDone
//   - error path bypasses sink → defer still fires markJobDone
//   - ctx cancel orphans subprocess → goroutine returns → defer fires
//   - happy path → defer fires alongside the sink terminal
//
// In every case doneCount increments exactly once per goroutine.
// When doneCount hits expected, the synthetic outer Result fires
// (single-shot via finalEmitted). Idempotent: a second call is
// a no-op (early-out under the lock).
//
// The aggregator's outer sink callback runs OUTSIDE the lock so
// the callback can't re-enter the aggregator under a held mutex
// (mirrors handleBuffering's transition path — see the comment
// at the top of handle()).
//
// A group that failed before its Ready event never reaches the
// Ready gate: the aggregator would stay in phaseBuffering and every
// group's output would sit in its buffer. When the fan-out ends, the
// last markJobDone transitions and replays those buffers — the
// surviving groups' findings reach the chat even when a sibling
// failed.
func (a *eventAggregator) markJobDone() {
	a.mu.Lock()
	if a.finalEmitted {
		a.mu.Unlock()
		return
	}
	a.doneCount++
	allDone := a.doneCount >= len(a.plan)
	// The transition must happen while the phase is still
	// phaseBuffering — the replay runs through handleStreaming,
	// which drops everything once the phase is closed.
	synthetic, replay, transition := a.beginTransition(allDone)
	a.mu.Unlock()

	if transition {
		a.replay(synthetic, replay)
	}

	a.mu.Lock()
	if a.finalEmitted {
		a.mu.Unlock()
		return
	}
	if allDone {
		a.finalEmitted = true
		a.phase.Store(int32(phaseClosed))
	}
	a.mu.Unlock()

	if allDone {
		// Every group is done: whatever a group left pending goes
		// out before the synthetic outer Result closes the
		// lifecycle.
		a.flushAll()

		// Synthetic outer Result — Source="" marks it as the
		// aggregator's emission. Result.Text intentionally
		// empty: the merged review text is delivered via
		// FormatReviewMessage from the /review dispatcher, not
		// via this event. Result is non-nil (an empty struct)
		// so the upstream outbound.Translate doesn't drop it
		// on its `if ev.Result == nil return false` guard
		// — without non-nil Result, the chat channel's
		// "review running…" indicator would never flip
		// (Finding 2 from /review). Source="" marks the event
		// as outer-emitted (vs per-job forwarded events).
		// emitMu covers the synthetic Result so it doesn't
		// interleave with a concurrent per-job pair emit.
		a.emitMu.Lock()
		a.outer(AgentEvent{
			Kind:   EventAgentResult,
			Result: &AgentResultEvent{},
			Source: "",
		})
		a.emitMu.Unlock()
	}
}

// beginTransition moves the aggregator out of phaseBuffering when the
// fan-out ends before every group reported Ready. It returns the
// synthetic outer Ready and the per-job buffers to replay.
//
// The caller holds a.mu.
func (a *eventAggregator) beginTransition(allDone bool) (AgentEvent, []perJobReplay, bool) {
	if !allDone || a.readyEmitted {
		return AgentEvent{}, nil, false
	}
	if aggregatorPhase(a.phase.Load()) != phaseBuffering {
		return AgentEvent{}, nil, false
	}
	a.readyEmitted = true
	a.phase.Store(int32(phaseStreaming))
	synthetic := a.buildSyntheticReady()
	replay := make([]perJobReplay, 0, len(a.plan))
	for i := range a.plan {
		state := a.perJob[groupSource(i)]
		if state == nil {
			continue
		}
		replay = append(replay, perJobReplay{state: state, events: state.initBuffer})
		state.initBuffer = nil
	}
	return synthetic, replay, true
}

// replay forwards the synthetic outer Ready and replays the per-job
// buffers through handleStreaming. The caller holds no lock.
func (a *eventAggregator) replay(synthetic AgentEvent, replay []perJobReplay) {
	a.emitMu.Lock()
	a.outer(synthetic)
	a.emitMu.Unlock()
	for _, r := range replay {
		for _, be := range r.events {
			a.handleStreaming(be, r.state)
		}
	}
}

// finalize is the post-wg.Wait() safety net called by
// delegateReviewMultiJob. With completion-driven counting
// (markJobDone in each per-job goroutine's defer), every
// running goroutine is guaranteed to call markJobDone when it
// returns — so after wg.Wait() doneCount is guaranteed to
// equal expected and the outer Result has already fired from
// the goroutine that drove doneCount over the threshold.
//
// finalize() therefore becomes a near no-op — it only fires
// the outer Result if for some reason markJobDone was never
// called for a job (e.g., a future test mock without a defer,
// or a goroutine that exits via os.Exit). Idempotent.
//
// Kept as a public API entry point so the orchestrator's
// post-wg.Wait() call site stays simple — one finalize call
// at the bottom, no "did we miss any?" arithmetic at the
// call site.
func (a *eventAggregator) finalize() {
	// A group that never produced a terminal event still has text
	// pending — flush it before the lifecycle closes. A group that
	// never produced a Ready event has its whole output in its
	// buffer: the transition replays it first.
	a.mu.Lock()
	synthetic, replay, transition := a.beginTransition(true)
	a.mu.Unlock()
	if transition {
		a.replay(synthetic, replay)
	}
	a.flushAll()

	a.mu.Lock()
	if a.finalEmitted {
		a.mu.Unlock()
		return
	}

	// Compute how many per-job slots never got a markJobDone
	// (defensive — under normal operation this is 0 because
	// every goroutine's defer fires).
	missing := len(a.plan) - a.doneCount
	if missing > 0 {
		a.doneCount = len(a.plan)
	}

	allDone := a.doneCount >= len(a.plan)
	if allDone {
		a.finalEmitted = true
		a.phase.Store(int32(phaseClosed))
	}
	a.mu.Unlock()

	if allDone {
		// emitMu covers the synthetic Result so it doesn't
		// interleave with a concurrent per-job pair emit
		// (invariant #12).
		a.emitMu.Lock()
		a.outer(AgentEvent{
			Kind:   EventAgentResult,
			Result: &AgentResultEvent{},
			Source: "",
		})
		a.emitMu.Unlock()
	}
}

// handleBuffering is the Phase 1 path. Every per-job event is
// appended to perJob.initBuffer (so the Phase 1→2 transition can
// replay it through the streaming handler with pairing / merging
// applied). Counters are updated under the lock with the
// terminalSeen guard so that:
//
//   - A Ready event observed here increments readyCount exactly once.
//   - A Result/Error event observed here sets state.terminalSeen and
//     increments doneCount exactly once; if the same event is
//     replayed at transition time, handleStreaming's terminalSeen
//     check prevents double-counting.
//
// Transition fires when readyCount reaches expected. The transition
// path: snapshot each perJob.initBuffer, clear them, build the
// synthetic outer Ready, set phase=streaming, release the lock, then
// forward the synthetic Ready and replay each per-job's snapshot
// through handleStreaming. Replay order is per-job (jobs replayed
// sequentially; per-job events replayed in arrival order).
func (a *eventAggregator) handleBuffering(ev AgentEvent, state *perJobState) {
	// Re-check phase under lock — Finding 4 from /review:
	// between handle()'s unlock and this function's lock, the
	// Phase 1→2 transition could have fired and cleared
	// initBuffer. Without this re-check, a late-arriving
	// event would silently append to a freshly-nil'd
	// initBuffer and be lost on replay.
	a.mu.Lock()
	if aggregatorPhase(a.phase.Load()) != phaseBuffering {
		// Stale event: another goroutine already transitioned
		// past Phase 1. Release the lock and re-dispatch to
		// the current phase handler. This is the cheap atomic
		// re-check that closes the TOCTOU window.
		a.mu.Unlock()
		if aggregatorPhase(a.phase.Load()) == phaseClosed {
			return
		}
		a.handleStreaming(ev, state)
		return
	}
	state.initBuffer = append(state.initBuffer, ev)

	// Counters (terminalSeen guard for Result/Error — see comment
	// above). Also caches the first per-job Ready's metadata for
	// the synthetic outer Ready + for parity on synthetic Task
	// events (Finding 3 from /review — see firstJobMeta doc).
	//
	// ToolStart in Phase 1: ALSO store in pendingToolStarts so
	// a ToolEnd arriving AFTER the Phase 1→2 snapshot (which
	// fires from a concurrent goroutine hitting expected) can
	// still find the Start. Without this, the End falls into
	// handleStreaming's orphan path and the Start is dropped
	// from the initBuffer replay (which only sees Ready/Text
	// for that job's slice) — invariant #12 broken. The pair
	// is emitted in Phase 1 below if the End is also in Phase
	// 1; the End in Phase 2 picks up the Start from
	// pendingToolStarts and emits the pair atomically.
	switch ev.Kind {
	case EventAgentReady:
		a.readyCount++
		if a.firstJobMeta.SessionID == "" && a.firstJobMeta.AgentName == "" &&
			a.firstJobMeta.Workspace == "" && a.firstJobMeta.Branch == "" &&
			a.firstJobMeta.Model == "" {
			a.firstJobMeta = ev
		}
	case EventAgentToolStart:
		if ev.ToolStart != nil && ev.ToolStart.ID != "" {
			state.pendingToolStarts[ev.ToolStart.ID] = ev
		}
	case EventAgentToolEnd:
		// Phase 1 End — try to pair with a buffered Start.
		// If Start was buffered in Phase 1 (same goroutine sent
		// them in sequence), pair emits here, atomically,
		// BEFORE the initBuffer append so the pair is contiguous
		// in outer regardless of phase transitions. If the
		// lookup misses (Start hasn't arrived yet — out-of-
		// order within Phase 1 is impossible by construction
		// because a.mu serializes handleBuffering invocations),
		// forward as orphan.
		if ev.ToolEnd != nil && ev.ToolEnd.ID != "" {
			startEv, ok := state.pendingToolStarts[ev.ToolEnd.ID]
			if ok {
				delete(state.pendingToolStarts, ev.ToolEnd.ID)
				// Pop the just-appended End from initBuffer so
				// the Phase 1→2 transition's replay doesn't re-emit
				// the same pair (the default case appended End
				// before this case ran; we just emitted it; pop it
				// back off so replay skips it). Done under a.mu so
				// the mutation has a happens-before with the
				// transition's snapshot at handleBuffering
				// — without this the race detector flags a read
				// of state.initBuffer concurrent with this write
				// (Ubuntu CI, fix-git-lock-file).
				if n := len(state.initBuffer); n > 0 && state.initBuffer[n-1].Kind == EventAgentToolEnd {
					state.initBuffer = state.initBuffer[:n-1]
				}
			}
			a.mu.Unlock()
			if ok {
				a.emitMu.Lock()
				a.outer(startEv)
				a.outer(ev)
				a.emitMu.Unlock()
			} else {
				a.emitMu.Lock()
				a.outer(ev)
				a.emitMu.Unlock()
			}
			a.mu.Lock()
			// Re-check phase in case the transition fired
			// between our unlock and relock. We need to bail
			// out of handleBuffering's transition gate logic
			// below — if we're past Phase 1, the transition
			// handler already ran and our per-job state is in
			// Phase 2 mode. Returning early is safe; the
			// event has already been fully processed.
			if aggregatorPhase(a.phase.Load()) != phaseBuffering {
				a.mu.Unlock()
				return
			}
		}
	}

	// Transition gate: readyCount must reach expected before we
	// leave Phase 1. Out-of-order tolerance (REVIEW.md §2.5.5):
	// doneCount may already exceed readyCount when a fast job
	// finishes before others report Ready; we still wait for
	// readyCount — Ready is the gate, not Done.
	//
	// A group that never reports Ready at all (it failed before its
	// session came up) would strand every group's output in its
	// buffer — the fan-out's end transitions and replays them
	// instead (see markJobDone).
	if a.readyCount >= len(a.plan) && !a.readyEmitted {
		a.readyEmitted = true
		a.phase.Store(int32(phaseStreaming))

		// Build synthetic outer Ready before releasing the lock so
		// we observe a consistent per-job state.
		syntheticReady := a.buildSyntheticReady()

		// Snapshot all per-job buffers for replay. Per-job state
		// stays alive (we don't drop it — pendingToolStarts may
		// still receive live events after transition).
		replay := make([]perJobReplay, 0, len(a.plan))
		for i := range a.plan {
			v := a.perJob[groupSource(i)]
			if v == nil {
				continue
			}
			replay = append(replay, perJobReplay{state: v, events: v.initBuffer})
			v.initBuffer = nil
		}
		a.mu.Unlock()

		// Outside lock: forward synthetic outer Ready, then replay.
		// Replay goes through handleStreaming so the pairing /
		// merging rules are uniform between replay and live events.
		// emitMu covers the synthetic Ready so it doesn't
		// interleave with a concurrent per-job pair emit.
		a.replay(syntheticReady, replay)
		return
	}
	a.mu.Unlock()
}

// perJobReplay is a small helper struct for the snapshot taken at
// Phase 1→2 transition. Source label is unused by handleStreaming
// (events already have Source stamped), so we only carry the per-job
// state pointer and the event slice.
type perJobReplay struct {
	state  *perJobState
	events []AgentEvent
}

// buildSyntheticReady returns the outer AgentReady event fired at
// the Phase 1→2 transition. Source is reset to "" to mark it as the
// aggregator's own emission. Metadata (SessionID / Model / AgentName
// / Workspace / Branch) is taken from the FIRST per-job Ready
// observed — per-job Readys are expected to carry identical metadata
// (same review, same agent bridge) so the choice is cosmetic; any
// per-job Ready is representative.
func (a *eventAggregator) buildSyntheticReady() AgentEvent {
	for _, state := range a.perJob {
		for _, ev := range state.initBuffer {
			if ev.Kind == EventAgentReady {
				return AgentEvent{
					Kind:      EventAgentReady,
					SessionID: ev.SessionID,
					Model:     ev.Model,
					AgentName: ev.AgentName,
					Workspace: ev.Workspace,
					Branch:    ev.Branch,
					Source:    "",
				}
			}
		}
	}
	// Defensive fallback: no per-job Ready observed (shouldn't
	// happen — readyCount >= expected implies at least one Ready).
	return AgentEvent{Kind: EventAgentReady, Source: ""}
}

// handleStreaming is the Phase 2 path. Applies the v12 rules per
// event kind:
//
//   - EventAgentToolStart  → buffered in perJob.pendingToolStarts by
//     ID, NOT forwarded. A matching ToolEnd will pick it up and
//     forward both events contiguously.
//   - EventAgentToolEnd    → look up the matching buffered Start by
//     ID. If found, forward (Start, End) as a contiguous pair;
//     otherwise forward the End alone as an orphan (defensive — the
//     chat renderer tolerates orphan Ends).
//   - EventAgentTaskCreate / EventAgentTaskUpdate → merge
//     ev.TaskList.Items into the cross-job tasks map (latest-wins
//     by AgentTaskItem.ID), then forward ONE snapshot event with
//     the merged items.
//   - EventAgentResult / EventAgentError → SUPPRESSED (the chat sees
//     one outer Result, not one per group). The group is over, so
//     its pending batches are flushed; a terminal that carries the
//     tail of the reply joins them first. NOT counted here —
//     doneCount is driven by the orchestrator's per-job goroutine via
//     markJobDone (see "Completion-driven counting" rationale
//     below).
//   - EventAgentText / EventAgentThinking → coalesced per job into
//     batches (see coalesce); the flushed batch carries the job's
//     label.
//   - All other events (EventAgentPermission,
//     EventAgentPermissionSettled, etc.) → forwarded as-is with the
//     Source label already stamped by wrapJob.
//
// handleStreaming is invoked both during the Phase 1→2 replay and
// for live events arriving after the transition. Both paths share
// the exact same rules so behavior is uniform.
//
// **Completion-driven counting** (refactor from /review):
// doneCount was previously incremented here when a per-job
// terminal arrived. That worked for happy paths but failed
// silently on the broken paths: bridges whose error-path
// bypassed the sink left doneCount short of expected, stranding
// the aggregator in phaseStreaming. The fix: each per-job
// goroutine in delegateReviewMultiJob now calls markJobDone()
// in its defer block — once per goroutine, regardless of
// RunOnce outcome or sink behavior. This guarantees doneCount
// reaches expected after wg.Wait(). The synthetic outer Result
// is fired by the goroutine that drove doneCount to expected
// (single-shot via finalEmitted).
func (a *eventAggregator) handleStreaming(ev AgentEvent, state *perJobState) {
	// Phase check — mirror of handleBuffering's re-check
	// (Finding 4 from /review). Normally the dispatch in
	// handle() reads the current phase under the lock, but
	// replay events (from the Phase 1→2 transition) and live
	// events can race here. If we've already closed, drop.
	if aggregatorPhase(a.phase.Load()) == phaseClosed {
		return
	}
	switch ev.Kind {
	case EventAgentReady:
		// Per-job Ready is NEVER forwarded individually — the
		// synthetic outer Ready, fired at the Phase 1→2 transition
		// (or counted in Phase 1 if a per-job Ready somehow
		// surfaces here post-transition — defensive no-op), is the
		// only Ready the chat sees. Drop silently.
		//
		// Why we don't increment readyCount here: handleBuffering
		// is the canonical counter site (Phase 1 catches every
		// per-job Ready before transition). A per-job Ready
		// arriving in Phase 2 would be a bridge bug (bridges emit
		// Ready once, at subprocess start); dropping it without
		// counting is the safe path.
	case EventAgentToolStart:
		if ev.ToolStart != nil && ev.ToolStart.ID != "" {
			a.mu.Lock()
			state.pendingToolStarts[ev.ToolStart.ID] = ev
			a.mu.Unlock()
			// No forward — pair-up happens on the matching ToolEnd.
		}
		// No-ID ToolStart: dropped (can't pair). Bridges populate ID.

	case EventAgentToolEnd:
		if ev.ToolEnd != nil && ev.ToolEnd.ID != "" {
			a.mu.Lock()
			startEv, ok := state.pendingToolStarts[ev.ToolEnd.ID]
			if ok {
				delete(state.pendingToolStarts, ev.ToolEnd.ID)
			}
			a.mu.Unlock()

			if ok {
				// Forward the pair ATOMICALLY — Start first, then
				// End, with no other goroutine's a.outer() call
				// allowed in between. Otherwise concurrent per-job
				// goroutines can interleave their events between
				// Start and End on the outer wire, breaking
				// invariant #12 ("Start, End strictly contiguous
				// per-job"). Finding 1 from /review: the previous
				// code called a.outer() twice without locking,
				// and -race testing reproduced the pair-loss
				// failure mode ~16% of the time under load.
				//
				// emitMu is a SEPARATE mutex from a.mu so the
				// emit critical section is short (two sends) and
				// doesn't extend the time other goroutines spend
				// waiting on the data-mutation lock. Inner lock
				// taken here; outer outer() calls never nest.
				a.emitMu.Lock()
				a.outer(startEv)
				a.outer(ev)
				a.emitMu.Unlock()
			} else {
				// Orphan End (no matching buffered Start): forward
				// anyway. Defensive — shouldn't happen with a
				// well-behaved bridge, but the chat renderer can
				// tolerate an orphan End (it just renders the
				// result without a preceding open). emitMu still
				// taken so the orphan's emit doesn't interleave
				// with a paired emit on another goroutine.
				a.emitMu.Lock()
				a.outer(ev)
				a.emitMu.Unlock()
			}
		}

	case EventAgentText:
		// The group's findings are collected, not streamed: the group
		// hands them over as one reply when it ends, so a reader can
		// follow one group instead of fragments mixed with the other
		// groups. The chunk that opens the group announces it first.
		a.announceGroup(state, ev)
		a.coalesce(state, &state.pendingText, EventAgentText, ev.Text, ev)

	case EventAgentThinking:
		// The group's reasoning keeps streaming while the group
		// works — it is the process, not the outcome.
		a.announceGroup(state, ev)
		a.coalesce(state, &state.pendingThinking, EventAgentThinking, ev.Text, ev)

	case EventAgentTaskCreate, EventAgentTaskUpdate:
		// Cross-job task list dedup (#13). Merge incoming items
		// into the cross-job tasks map (latest-wins by ID) under
		// the lock, then forward ONE synthetic snapshot with the
		// merged items. Source carries the originating job label
		// for downstream debugging.
		a.mu.Lock()
		if ev.TaskList != nil {
			for _, item := range ev.TaskList.Items {
				a.tasks[item.ID] = item
			}
		}
		items := make([]AgentTaskItem, 0, len(a.tasks))
		for _, item := range a.tasks {
			items = append(items, item)
		}
		a.mu.Unlock()

		a.emitMu.Lock()
		a.outer(AgentEvent{
			Kind:      ev.Kind,
			TaskList:  &AgentTaskListEvent{Items: items},
			SessionID: a.firstJobMeta.SessionID,
			Model:     a.firstJobMeta.Model,
			AgentName: a.firstJobMeta.AgentName,
			Workspace: a.firstJobMeta.Workspace,
			Branch:    a.firstJobMeta.Branch,
			Source:    ev.Source,
		})
		a.emitMu.Unlock()

	case EventAgentResult, EventAgentError:
		// Per-job terminal — SUPPRESSED at the aggregator
		// layer. REVIEW.md §2.5.5 specifies "Result/Error → 不
		// 外发" (don't forward); invariant #11 requires the
		// chat to see exactly ONE outer Result. Forwarding
		// per-job terminals here would render N "📝 result"
		// cards during the streaming phase — every group
		// review would surface as its own card with the
		// group's text — on top of the final merged-text
		// reply via emitter.Send. The synthetic outer Result
		// fired by markJobDone (or finalize) is the only
		// Result the chat should observe. (Finding 2 from
		// /review.)
		//
		// Per-job completion is tracked at the orchestrator
		// level (delegateReviewMultiJob's defer markJobDone),
		// NOT here. The sink's per-job terminal is purely a
		// "this job returned" signal that the orchestrator
		// already has via the goroutine's defer.
		//
		// The terminal does carry one piece of content: the tail
		// of the group's reply, which a bridge holds back until
		// the turn ends (pi / omp put the last segment on the
		// result event; claudecode / codex leave it empty and
		// stream everything). That tail belongs to the group, so
		// it joins the coalesced stream before the group is
		// flushed — otherwise the channel would never see it.
		//
		// A bridge that repeats the reply it already streamed
		// (dsh carries the last message on the result event as
		// well) must not deliver it twice: the tail is skipped
		// when the group already delivered it.
		if ev.Kind == EventAgentResult && ev.Err == nil && ev.Result != nil && ev.Result.Text != "" {
			if !a.tailAlreadyDelivered(state, ev.Result.Text) {
				// The tail is the group's first output when the bridge
				// streamed nothing — the group announces itself here
				// too, so its findings never arrive without a hint.
				a.announceGroup(state, ev)
				a.coalesce(state, &state.pendingText, EventAgentText, ev.Result.Text, ev)
			}
		}
		// The group is over: whatever it left open goes out now.
		a.flushJob(state)

	default:
		// Permission / PermissionSettled / unknown kinds:
		// forward as-is. Source already stamped by wrapJob.
		//
		// emitMu serializes ALL outer() calls so a Text from one
		// per-job cannot interleave between a sibling per-job's
		// ToolStart / ToolEnd pair (invariant #12 — Finding 1
		// from /review). Without this, the chat renderer can
		// observe [Start_X, Text_Y, End_X] instead of the
		// required [Start_X, End_X, ...]. The hot path is
		// bounded: per-event emit is O(1) and chat channels
		// already serialize inbound events on their own
		// delivery queue, so emitMu contention is dominated by
		// the upstream consumer.
		a.emitMu.Lock()
		a.outer(ev)
		a.emitMu.Unlock()
	}
}

// ─── per-group coalescing ─────────────────────────────────────────

// flushedBatch is one cut batch: the body and the event that opened
// it — the delivered event carries the opening event's metadata.
type flushedBatch struct {
	ev   AgentEvent
	kind EventKind
	body string
}

// coalesce appends text to the group's pending batch of the given
// kind.
//
// The two kinds are handled differently. The findings (the text) are
// cut by size only and kept: the group hands them over when it ends,
// so one group reads as one reply — a batch cut by the clock would
// split one group's findings over several replies. The reasoning (the
// thinking) is cut by size and by the clock and goes out at once: it
// is the process, and it keeps streaming while the group works.
//
// The clock is checked when the next event arrives, not by a timer: a
// group that stops producing reasoning has nothing to flush until it
// ends.
//
// A size-triggered cut splits at the batch's last blank line so a
// markdown block is not split in half; a batch without a blank line
// is cut at its end — a single event larger than the trigger is
// never split.
//
// ev carries the metadata of the flushed event (session, model,
// agent, workspace) and the group's label.
func (a *eventAggregator) coalesce(state *perJobState, batch *pendingBatch, kind EventKind, text string, ev AgentEvent) {
	if text == "" {
		return
	}
	now := time.Now()
	var out []flushedBatch
	isText := kind == EventAgentText

	a.mu.Lock()
	if isText {
		// The group's delivered text — what the user has already
		// seen. The tail its terminal event carries is checked
		// against it.
		state.deliveredText = append(state.deliveredText, text)
	}
	if !isText && len(batch.pieces) > 0 && now.Sub(batch.started) >= coalesceInterval {
		out = append(out, flushedBatch{ev: batch.ev, kind: batch.kind, body: batch.body()})
		batch.reset()
	}
	if len(batch.pieces) == 0 {
		batch.started = now
		batch.ev = ev
		batch.kind = kind
	}
	if batch.append(text) >= coalesceBytes {
		body := batch.body()
		cut := len(body)
		// A blank line at the very start would flush an empty
		// batch — cut at the end instead.
		if i := strings.LastIndex(body, "\n\n"); i > 0 {
			cut = i + 2
		}
		out = append(out, flushedBatch{ev: batch.ev, kind: batch.kind, body: body[:cut]})
		rest := body[cut:]
		batch.reset()
		if rest != "" {
			batch.append(rest)
			batch.started = now
			batch.ev = ev
			batch.kind = kind
		}
	}
	if isText {
		// The findings wait for the group to end.
		state.textChunks = append(state.textChunks, out...)
		out = nil
	}
	a.mu.Unlock()

	// The reasoning goes out as it is cut.
	for _, fb := range out {
		a.emitOne(groupEvent(fb.ev, fb.kind, state.index+1, fb.body))
	}
}

// announceGroup emits the group's start hint: it names the group and
// how many files it covers, so the reader knows what the findings that
// follow are about.
//
// The hint goes out once per group, when the group's first output
// arrives, and rides the thinking kind — it belongs with the findings
// in the thread, and it follows the chat's /think setting like they do.
func (a *eventAggregator) announceGroup(state *perJobState, ev AgentEvent) {
	a.mu.Lock()
	if state.announced {
		a.mu.Unlock()
		return
	}
	state.announced = true
	a.mu.Unlock()

	g := a.planGroup(state.index)
	if g == nil {
		return
	}
	a.emitOne(AgentEvent{
		Kind:      EventAgentThinking,
		Text:      fmt.Sprintf("▶ **[%d] %s** — %s", state.index+1, CodeSpan(g.Label), pluralFiles(len(g.Files))),
		SessionID: ev.SessionID,
		Model:     ev.Model,
		AgentName: ev.AgentName,
		Workspace: ev.Workspace,
		Branch:    ev.Branch,
		Source:    ev.Source,
	})
}

// planGroup returns the plan entry of the group at the given index —
// nil when the index is outside the plan.
func (a *eventAggregator) planGroup(index int) *ReviewPlanGroup {
	if index < 0 || index >= len(a.plan) {
		return nil
	}
	return &a.plan[index]
}

// flushJob hands the group's output over — the group is over.
//
// The findings are handed over as one reply: the group's whole text
// goes out now, cut into as many replies as it needs, so a group
// whose findings fit in one message produces one reply. Every reply
// starts with the group's number, so the reader knows which group it
// belongs to.
//
// The reasoning goes out as it is: it was streamed while the group
// worked.
func (a *eventAggregator) flushJob(state *perJobState) {
	a.mu.Lock()
	if body := state.pendingText.body(); body != "" {
		state.textChunks = append(state.textChunks, flushedBatch{
			ev:   state.pendingText.ev,
			kind: state.pendingText.kind,
			body: body,
		})
	}
	state.pendingText.reset()
	chunks := state.textChunks
	state.textChunks = nil
	reasoning := takePending(state.index+1, &state.pendingThinking)
	a.mu.Unlock()

	for _, ev := range reasoning {
		a.emitOne(ev)
	}
	for _, c := range chunks {
		a.emitOne(groupEvent(c.ev, EventAgentThinking, state.index+1, c.body))
	}
}

// tailAlreadyDelivered reports whether the group already delivered
// the text its terminal event carries.
//
// The reply of a turn reaches the user in two pieces: the pieces the
// bridge streamed as text events, and the tail it held back until the
// turn ended. A bridge that streams the whole reply AND repeats it on
// the terminal event (dsh carries its last message on both) would
// deliver that message twice — the check keeps the tail out of the
// stream when the group already said it.
//
// The comparison is a suffix check: the tail is the end of the
// group's text. A tail that merely repeats the end of the text is
// skipped too — the user has already read it.
func (a *eventAggregator) tailAlreadyDelivered(state *perJobState, tail string) bool {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delivered := strings.Join(state.deliveredText, "\n")
	return strings.HasSuffix(delivered, tail)
}

// flushAll hands over the output of the groups that are still holding
// it — the fan-out is over, no group keeps its text back.
//
// A group normally hands its findings over when it ends, so this is
// the mop-up for the groups that never reached their terminal. The
// groups are walked in fan-out order, so what it hands over arrives
// in the order the plan named the groups.
func (a *eventAggregator) flushAll() {
	for i := range a.plan {
		a.mu.Lock()
		state := a.perJob[groupSource(i)]
		a.mu.Unlock()
		if state == nil {
			continue
		}
		a.flushJob(state)
	}
}

// emitOne forwards one event to the outer sink.
func (a *eventAggregator) emitOne(ev AgentEvent) {
	a.emitMu.Lock()
	a.outer(ev)
	a.emitMu.Unlock()
}

// takePending takes the bodies of the given batches and resets them.
// The caller holds the aggregator's lock.
func takePending(groupNum int, batches ...*pendingBatch) []AgentEvent {
	var out []AgentEvent
	for _, b := range batches {
		if len(b.pieces) == 0 {
			continue
		}
		out = append(out, groupEvent(b.ev, b.kind, groupNum, b.body()))
		b.reset()
	}
	return out
}

// groupEvent builds the event that carries a group's batch: the
// group's number inline, then the batch body.
//
// The number travels inside the text on purpose — the outbound layer
// carries no group field, and the number has to travel with the text
// it names. It is the group's position in the fan-out order, the same
// number the plan numbers it with.
func groupEvent(ev AgentEvent, kind EventKind, groupNum int, body string) AgentEvent {
	out := ev
	out.Kind = kind
	out.Text = "[" + strconv.Itoa(groupNum) + "] " + body
	// A flushed batch is a plain text (or thinking) event — the
	// terminal that opened it carried a payload of its own.
	out.Result = nil
	out.Err = nil
	return out
}

// pluralFiles renders a file count with its noun: "1 file", "12 files".
func pluralFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}
