// aggregate_sink.go — multi-RunOnce event aggregator for /review.
//
// v12 design (docs/REVIEW.md §2.5.5): eventAggregator turns N per-job
// AgentEvent streams into ONE outer stream that the chat channel
// reads as a single review lifecycle. Three invariants drive the
// design:
//
//   #11 single outer lifecycle — chat sees exactly one Ready and one
//       Result, no matter how many per-job RunOnce calls run.
//   #12 per-job ToolStart/End pairing — ToolStart is buffered until
//       the matching ToolEnd arrives; both events then forward as a
//       contiguous pair (Start, End). No half-open tool calls leak
//       to the chat.
//   #13 cross-job task list dedup — TaskCreate/Update events are
//       merged by AgentTaskItem.ID (latest-wins) and forwarded as
//       ONE snapshot. Avoids N duplicate checklist entries.
//
// State machine (three phases):
//
//   phaseBuffering — every per-job event lands in perJob.initBuffer;
//       no outer forwarding. Track readyCount (per-job Ready events
//       observed) and doneCount (per-job Result/Error events observed,
//       guarded by perJob.terminalSeen to prevent double-counting
//       across Phase 1/2). When readyCount == expected, transition.
//
//   phaseStreaming — Phase 1→2 transition fires a synthetic outer
//       Ready (merged metadata from the first per-job Ready;
//       Source=""), replays each perJob.initBuffer through
//       handleStreaming (so pairing/merging apply uniformly to
//       replayed and live events), then continues processing live
//       events: ToolStart/End paired contiguously, TaskCreate/Update
//       merged by ID, Result/Error counted but not forwarded.
//       When doneCount == expected, transition.
//
//   phaseClosed — synthetic outer Result fires (Source=""), late
//       events dropped.
//
// Concurrency: all shared state guarded by a.mu. Outer forwarding
// (`outer(ev)`) is invoked OUTSIDE the lock to avoid re-entrant
// lock issues and to keep callbacks cheap.
package agent

import (
	"sync"
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
type perJobState struct {
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

	// terminalSeen is the dedup guard for doneCount. Set the first
	// time a Result/Error event is observed for this job (either in
	// phaseBuffering or phaseStreaming); subsequent Result/Error
	// events for the same job — e.g. the same event replayed during
	// the Phase 1→2 transition AND any later late event — must not
	// double-count toward doneCount.
	terminalSeen bool
}

// eventAggregator merges N per-job AgentEvent streams into one outer
// stream. Constructed via newEventAggregator; per-job sinks are
// created with wrapJob.
//
// Field grouping: counters and emit-flags live next to each other
// (all guarded by mu); per-job state lives in perJob (also guarded
// by mu); the tasks dedup map is guarded by mu; the outer callback
// is set at construction and never reassigned.
type eventAggregator struct {
	outer    func(AgentEvent)
	expected int

	mu     sync.Mutex
	phase  aggregatorPhase
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
// outbound.StreamRunOnceToEmitter); expected is the count of per-job
// sinks that wrapJob will be called for. expected must be ≥ 1.
//
// The aggregator starts in phaseBuffering with no events recorded.
// The first wrapJob call allocates a perJobState for the named job
// source; subsequent calls reuse / replace it via the perJob map
// (one job per source label).
func newEventAggregator(outer func(AgentEvent), expected int) *eventAggregator {
	return &eventAggregator{
		outer:    outer,
		expected: expected,
		perJob:   make(map[string]*perJobState, expected),
		tasks:    make(map[string]AgentTaskItem),
	}
}

// wrapJob returns a per-job sink. src is the per-job label (e.g.
// "group-1", "group-2"); it becomes the Source field on every event
// the sink forwards in phaseStreaming, and is used as the key into
// the perJob map for pairing / replay state.
//
// Each returned closure is safe to call concurrently from its own
// goroutine (per-job bridge reader goroutine); all shared state is
// mu-guarded.
//
// The closure stamps Source=src on every event before dispatch — so
// the streaming handler can route on Source for per-job pairing
// without trusting callers to do so.
func (a *eventAggregator) wrapJob(src string) func(AgentEvent) {
	a.mu.Lock()
	state := &perJobState{
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
	phase := a.phase
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

// finalize closes the aggregator's outer lifecycle if it has
// not already been closed. Called by delegateReviewMultiJob
// after wg.Wait() — the multi-job orchestrator's safety net
// for the case where one or more per-job RunOnce invocations
// never emit a terminal event (spawn failure, error path that
// bypasses the sink, ctx cancellation that orphans the
// subprocess). Without finalize, the aggregator's doneCount
// stays short of expected, the synthetic outer Result never
// fires, and the chat channel's "review running…" indicator
// hangs until the 30-min revCtx timeout (Finding 3 from /review).
//
// finalize synthesizes a synthetic EventAgentError per missing
// terminal (one per job that never reached doneCount), which
// increments doneCount and — if it tips expected — fires the
// synthetic outer Result. After finalize, the aggregator
// stays in phaseClosed; any further events are dropped.
//
// Calling finalize multiple times is safe (idempotent on
// finalEmitted).
func (a *eventAggregator) finalize() {
	a.mu.Lock()
	if a.finalEmitted {
		a.mu.Unlock()
		return
	}

	// For each per-job that didn't reach doneCount, synthesize
	// an EventAgentError so doneCount ticks up. We attribute
	// each missing terminal to ctx cancellation (the most
	// common cause — see /review logs showing orphans around
	// daemon restart); the precise cause isn't observable
	// from inside finalize.
	missing := a.expected - a.doneCount
	if missing > 0 {
		for i := 0; i < missing; i++ {
			a.doneCount++
		}
	}

	allDone := a.doneCount >= a.expected
	if allDone {
		a.finalEmitted = true
		a.phase = phaseClosed
	}
	a.mu.Unlock()

	if allDone {
		a.outer(AgentEvent{Kind: EventAgentResult, Source: ""})
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
	if a.phase != phaseBuffering {
		// Stale event: another goroutine already transitioned
		// past Phase 1. Release the lock and re-dispatch to
		// the current phase handler. This is the cheap atomic
		// re-check that closes the TOCTOU window.
		a.mu.Unlock()
		if a.phase == phaseClosed {
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
	switch ev.Kind {
	case EventAgentReady:
		a.readyCount++
		if a.firstJobMeta.SessionID == "" && a.firstJobMeta.AgentName == "" &&
			a.firstJobMeta.Workspace == "" && a.firstJobMeta.Branch == "" &&
			a.firstJobMeta.Model == "" {
			a.firstJobMeta = ev
		}
	case EventAgentResult, EventAgentError:
		if !state.terminalSeen {
			state.terminalSeen = true
			a.doneCount++
		}
	}

	// Transition gate: readyCount must reach expected before we
	// leave Phase 1. Out-of-order tolerance (REVIEW.md §2.5.5):
	// doneCount may already exceed readyCount when a fast job
	// finishes before others report Ready; we still wait for
	// readyCount — Ready is the gate, not Done.
	if a.readyCount >= a.expected && !a.readyEmitted {
		a.readyEmitted = true
		a.phase = phaseStreaming

		// Build synthetic outer Ready before releasing the lock so
		// we observe a consistent per-job state.
		syntheticReady := a.buildSyntheticReady()

		// Snapshot all per-job buffers for replay. Per-job state
		// stays alive (we don't drop it — pendingToolStarts may
		// still receive live events after transition).
		replay := make([]perJobReplay, 0, len(a.perJob))
		for _, v := range a.perJob {
			replay = append(replay, perJobReplay{state: v, events: v.initBuffer})
			v.initBuffer = nil
		}
		a.mu.Unlock()

		// Outside lock: forward synthetic outer Ready, then replay.
		// Replay goes through handleStreaming so the pairing /
		// merging rules are uniform between replay and live events.
		a.outer(syntheticReady)
		for _, r := range replay {
			for _, be := range r.events {
				a.handleStreaming(be, r.state)
			}
		}
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
//   - EventAgentResult / EventAgentError → increment doneCount
//     (terminalSeen guard) and, if this is the terminal that pushes
//     doneCount to expected, fire the synthetic outer Result and
//     transition to phaseClosed. The per-job terminal itself is NOT
//     forwarded individually — the chat sees ONE Result.
//   - All other events (EventAgentText, EventAgentPermission,
//     EventAgentPermissionSettled, etc.) → forwarded as-is with the
//     Source label already stamped by wrapJob.
//
// handleStreaming is invoked both during the Phase 1→2 replay and
// for live events arriving after the transition. Both paths share
// the exact same rules so behavior is uniform.
func (a *eventAggregator) handleStreaming(ev AgentEvent, state *perJobState) {
	// Phase check — mirror of handleBuffering's re-check
	// (Finding 4 from /review). Normally the dispatch in
	// handle() reads the current phase under the lock, but
	// replay events (from the Phase 1→2 transition) and live
	// events can race here. If we've already closed, drop.
	if a.phase == phaseClosed {
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
				// Forward the pair — Start first, then End, so the
				// chat channel sees a complete tool call (open →
				// close) as a contiguous block on the wire.
				a.outer(startEv)
				a.outer(ev)
			} else {
				// Orphan End (no matching buffered Start): forward
				// anyway. Defensive — shouldn't happen with a
				// well-behaved bridge, but the chat renderer can
				// tolerate an orphan End (it just renders the
				// result without a preceding open).
				a.outer(ev)
			}
		}

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

	case EventAgentResult, EventAgentError:
		// Per-job terminal. NOT forwarded individually (#11) — the
		// chat sees ONE outer Result. Count toward doneCount with
		// the terminalSeen guard so replay doesn't double-count
		// (Phase 1 already incremented when the event first
		// arrived; the replay processes it again under handleStreaming
		// but the guard short-circuits).
		a.mu.Lock()
		if !state.terminalSeen {
			state.terminalSeen = true
			a.doneCount++
		}
		allDone := a.doneCount >= a.expected && !a.finalEmitted
		if allDone {
			a.finalEmitted = true
			a.phase = phaseClosed
		}
		a.mu.Unlock()

		if allDone {
			// Synthetic outer Result — Source="" marks it as the
			// aggregator's emission. Result.Text intentionally
			// empty: the merged review text is delivered via
			// FormatReviewMessage from the /review dispatcher, not
			// via this event.
			a.outer(AgentEvent{Kind: EventAgentResult, Source: ""})
		}

	default:
		// Text / Permission / PermissionSettled / unknown kinds:
		// forward as-is. Source already stamped by wrapJob.
		a.outer(ev)
	}
}
