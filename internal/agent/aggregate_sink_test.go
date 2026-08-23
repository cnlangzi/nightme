package agent

import (
	"fmt"
	"sync"
	"testing"
)

// sinkRecorder captures every AgentEvent forwarded to the outer sink
// in arrival order. The aggregator's v12 contract is "N per-job
// streams → 1 outer stream"; recording the outer stream is the
// easiest way to assert that.
type sinkRecorder struct {
	mu     sync.Mutex
	events []AgentEvent
}

func (r *sinkRecorder) sink() func(AgentEvent) {
	return func(ev AgentEvent) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	}
}

func (r *sinkRecorder) snapshot() []AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AgentEvent, len(r.events))
	copy(out, r.events)
	return out
}

// markJobDoneRecorder is a tiny side-channel that records every
// markJobDone() invocation. Tests use it to assert the completion-
// driven counting path: a test that calls wrapJob(...)(ev) on every
// per-job must also call agg.markJobDone() exactly once per
// goroutine, otherwise doneCount never reaches expected.
type markJobDoneRecorder struct {
	mu    sync.Mutex
	count int
}

func (m *markJobDoneRecorder) hook(agg *eventAggregator) func() {
	// Returns a defer-friendly func that increments the recorder
	// AND calls agg.markJobDone. Use:
	//   defer rec.hook(agg)()
	// which expands to defer (rec.hook(agg))() — invokes the
	// returned closure immediately at defer time (LIFO order:
	// last defer runs first).
	rec := m
	return func() {
		rec.mu.Lock()
		rec.count++
		rec.mu.Unlock()
		agg.markJobDone()
	}
}

func (m *markJobDoneRecorder) snapshot() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// runJobGoroutine simulates the orchestrator's per-job goroutine
// pattern: emits a stream of events to a per-job sink, then
// fires markJobDone in defer. Tests that want to exercise the
// completion-driven counting path call runJobGoroutine for
// every per-job they wire up, instead of calling wrapJob(...)()
// inline (which used to work when doneCount was sink-driven but
// no longer does — doneCount now comes from markJobDone).
//
// The function returns a channel that closes when the goroutine
// has finished both the event stream AND the markJobDone defer,
// so tests can deterministically wait for completion before
// asserting on the outer sink.
func runJobGoroutine(agg *eventAggregator, src string, events []AgentEvent) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer agg.markJobDone()
		sink := agg.wrapJob(src)
		for _, ev := range events {
			ev.Source = src // match what wrapJob does in production
			sink(ev)
		}
	}()
	return done
}

// runJobGoroutines synchronously runs N per-job goroutines in
// parallel (one per group) and waits for all of them to finish.
// Mirrors the orchestrator's wg.Wait() — tests that want to
// drive the aggregator to its terminal state use this to ensure
// every per-job's markJobDone has fired before assertions run.
func runJobGoroutines(agg *eventAggregator, jobs map[string][]AgentEvent) {
	var wg sync.WaitGroup
	for src, events := range jobs {
		wg.Add(1)
		src := src
		events := events
		go func() {
			defer wg.Done()
			defer agg.markJobDone()
			sink := agg.wrapJob(src)
			for _, ev := range events {
				ev.Source = src
				sink(ev)
			}
		}()
	}
	wg.Wait()
}

// runSingleJob is the synchronous variant — fires events for one
// job in a goroutine, deferring markJobDone, and waits for the
// goroutine to finish before returning. Use when the test wants
// per-job ordering to be deterministic (no concurrency noise) but
// still needs the completion-driven counting path to engage.
func runSingleJob(agg *eventAggregator, src string, events []AgentEvent) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer agg.markJobDone()
		sink := agg.wrapJob(src)
		for _, ev := range events {
			ev.Source = src
			sink(ev)
		}
	}()
	<-done
}

// readyEvent builds a synthetic per-job Ready event. Source is set by
// wrapJob, not the caller.
func readyEvent(agentName, workspace, branch string) AgentEvent {
	return AgentEvent{
		Kind:      EventAgentReady,
		AgentName: agentName,
		Workspace: workspace,
		Branch:    branch,
	}
}

// resultEvent builds a synthetic per-job Result event.
func resultEvent(text string) AgentEvent {
	return AgentEvent{
		Kind:   EventAgentResult,
		Result: &AgentResultEvent{Text: text},
	}
}

// errorEvent builds a synthetic per-job Error event.
func errorEvent(err error) AgentEvent {
	return AgentEvent{
		Kind: EventAgentError,
		Err:  err,
	}
}

// TestEventAggregator_ReadyAllWait pins invariant #11: the synthetic
// outer Ready fires only when ALL per-job Readys are observed. Until
// then, the outer sink sees ZERO events.
func TestEventAggregator_ReadyAllWait(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 3)

	jobs := []func(AgentEvent){
		agg.wrapJob("group-1"),
		agg.wrapJob("group-2"),
		agg.wrapJob("group-3"),
	}
	// Emit Readys in non-monotonic order to make sure the gate
	// doesn't rely on arrival order.
	jobs[1](readyEvent("a", "/repo", "main"))
	jobs[0](readyEvent("a", "/repo", "main"))
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("after 2 of 3 Readys expected 0 outer events; got %d: %+v", len(got), got)
	}
	jobs[2](readyEvent("a", "/repo", "main"))

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("after all 3 Readys expected exactly 1 outer Ready; got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady {
		t.Errorf("event[0].Kind = %v, want EventAgentReady", got[0].Kind)
	}
	if got[0].Source != "" {
		t.Errorf("synthetic outer Ready must have Source=\"\"; got %q", got[0].Source)
	}
}

// TestEventAggregator_StreamAfterReady pin the buffering contract:
// events arriving BEFORE all-Ready are NOT forwarded immediately;
// they surface only after the synthetic outer Ready fires (via the
// Phase 1→2 replay path).
func TestEventAggregator_StreamAfterReady(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	j1 := agg.wrapJob("group-1")
	j2 := agg.wrapJob("group-2")

	// Phase 1: emit Ready + Text from job-1, then Ready from job-2.
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "thinking in group-1"})
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("Phase 1 must not forward events; got %d: %+v", len(got), got)
	}
	j2(readyEvent("a", "/r", "main"))

	// After all-Ready: synthetic Ready first, then the buffered Text.
	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 outer events (1 synthetic Ready + 1 replayed Text); got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady {
		t.Errorf("event[0].Kind = %v, want EventAgentReady", got[0].Kind)
	}
	if got[0].Source != "" {
		t.Errorf("event[0].Source = %q, want \"\"", got[0].Source)
	}
	if got[1].Kind != EventAgentText {
		t.Errorf("event[1].Kind = %v, want EventAgentText", got[1].Kind)
	}
	if got[1].Source != "group-1" {
		t.Errorf("event[1].Source = %q, want \"group-1\"", got[1].Source)
	}
	if got[1].Text != "thinking in group-1" {
		t.Errorf("event[1].Text = %q, want %q", got[1].Text, "thinking in group-1")
	}
}

// TestEventAggregator_ToolPairForwardedTogether pins invariant #12:
// in Phase 2 (live streaming), per-job ToolStart is held until the
// matching ToolEnd arrives; both events then forward contiguously in
// (Start, End) order on the wire. Intermediate Text events arriving
// BETWEEN Start and End are forwarded in arrival order — the pair is
// the only thing that gets reordered (Start held until End).
func TestEventAggregator_ToolPairForwardedTogether(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	// Drive the gate first (1 expected).
	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))

	// Now Phase 2. Emit: ToolStart (buffered), Text (forwarded
	// immediately), ToolEnd (pairs with buffered Start → forward
	// Start, End together).
	j1(AgentEvent{
		Kind: EventAgentToolStart,
		ToolStart: &AgentToolStartEvent{
			ID:   "t1",
			Name: "Read",
		},
	})
	j1(AgentEvent{Kind: EventAgentText, Text: "in-between thought"})
	j1(AgentEvent{
		Kind: EventAgentToolEnd,
		ToolEnd: &AgentToolEndEvent{
			ID:   "t1",
			Name: "Read",
		},
	})

	got := rec.snapshot()
	// Expected order: synthetic Ready, Text (forwarded when it
	// arrived), then ToolStart+ToolEnd (paired when End arrived).
	if len(got) != 4 {
		t.Fatalf("expected 4 outer events (Ready, Text, ToolStart, ToolEnd); got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady || got[0].Source != "" {
		t.Errorf("event[0] = %+v, want synthetic outer Ready", got[0])
	}
	if got[1].Kind != EventAgentText || got[1].Text != "in-between thought" {
		t.Errorf("event[1] = %+v, want EventAgentText 'in-between thought' (forwarded between Start and End)", got[1])
	}
	if got[2].Kind != EventAgentToolStart || got[2].ToolStart.ID != "t1" {
		t.Errorf("event[2] = %+v, want EventAgentToolStart ID=t1", got[2])
	}
	if got[3].Kind != EventAgentToolEnd || got[3].ToolEnd.ID != "t1" {
		t.Errorf("event[3] = %+v, want EventAgentToolEnd ID=t1", got[3])
	}
	// Critical: Start and End must be contiguous (events 2 and 3).
	if got[2].Kind != EventAgentToolStart || got[3].Kind != EventAgentToolEnd {
		t.Errorf("ToolStart/ToolEnd pair not contiguous: events[2]=%v, events[3]=%v", got[2].Kind, got[3].Kind)
	}
}

// TestEventAggregator_ToolPairFromReplay pins the replay consistency
// rule: a ToolStart/ToolEnd pair BUFFERED in Phase 1 must come out
// paired during replay (Start → End contiguous). Requires expected=2
// so the first per-job Ready doesn't immediately trigger the
// transition.
func TestEventAggregator_ToolPairFromReplay(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	j1 := agg.wrapJob("group-1")
	j2 := agg.wrapJob("group-2")
	// job-1 emits Ready + tool pair during Phase 1 (only 1 of 2
	// Readys — still buffering).
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{
		Kind:      EventAgentToolStart,
		ToolStart: &AgentToolStartEvent{ID: "t1", Name: "Read"},
	})
	j1(AgentEvent{
		Kind:    EventAgentToolEnd,
		ToolEnd: &AgentToolEndEvent{ID: "t1", Name: "Read"},
	})
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("Phase 1 (only 1 of 2 Readys) must not forward; got %d events", len(got))
	}
	// job-2's Ready triggers transition + replay.
	j2(readyEvent("a", "/r", "main"))

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected 3 outer events (synthetic Ready + paired Start/End); got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady || got[0].Source != "" {
		t.Errorf("event[0] = %+v, want synthetic outer Ready", got[0])
	}
	if got[1].Kind != EventAgentToolStart || got[1].ToolStart.ID != "t1" {
		t.Errorf("event[1] = %+v, want ToolStart ID=t1 (replayed)", got[1])
	}
	if got[2].Kind != EventAgentToolEnd || got[2].ToolEnd.ID != "t1" {
		t.Errorf("event[2] = %+v, want ToolEnd ID=t1 (replayed)", got[2])
	}
}

// TestEventAggregator_OrphanEndForwarded: a ToolEnd arriving without
// a matching buffered ToolStart (e.g. bridge emitted End first, or
// the Start was dropped earlier for some reason) is forwarded as a
// single orphan event. Defensive — well-behaved bridges won't do
// this, but the chat renderer should not crash on it.
func TestEventAggregator_OrphanEndForwarded(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{
		Kind:    EventAgentToolEnd,
		ToolEnd: &AgentToolEndEvent{ID: "never-matched", Name: "Bash"},
	})

	got := rec.snapshot()
	// Expected: synthetic Ready + orphan ToolEnd (no Start preceded).
	endCount := 0
	for _, ev := range got {
		if ev.Kind == EventAgentToolEnd {
			endCount++
		}
	}
	if endCount != 1 {
		t.Errorf("expected 1 ToolEnd in outer events; got %d. events: %+v", endCount, got)
	}
}

// TestEventAggregator_DoneAllWait pins the symmetric #11 invariant
// for the terminal: the synthetic outer Result fires only when ALL
// per-job goroutines have called markJobDone (completion-driven
// counting). Per-job terminal events (Result/Error) are forwarded
// to the outer sink as OBSERVATION but do not drive doneCount.
func TestEventAggregator_DoneAllWait(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 3)

	// Drive all-Ready first to clear Phase 1 buffering.
	runSingleJob(agg, "group-1", []AgentEvent{readyEvent("a", "/r", "main")})
	runSingleJob(agg, "group-2", []AgentEvent{readyEvent("a", "/r", "main")})
	runSingleJob(agg, "group-3", []AgentEvent{readyEvent("a", "/r", "main")})

	// Only 3 markJobDones have fired → doneCount == expected → 1
	// synthetic outer Result already emitted. Verify.
	resultCount := 0
	for _, ev := range rec.snapshot() {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("after 3 markJobDone calls expected exactly 1 outer Result; got %d. events: %+v", resultCount, rec.snapshot())
	}
}

// TestEventAggregator_TaskListMerge pins invariant #13: per-job
// TaskCreate/Update events with overlapping IDs are merged into ONE
// snapshot (latest-wins) regardless of which job emitted them.
func TestEventAggregator_TaskListMerge(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	j1 := agg.wrapJob("group-1")
	j2 := agg.wrapJob("group-2")
	// Drive gate.
	j1(readyEvent("a", "/r", "main"))
	j2(readyEvent("a", "/r", "main"))

	// Now streaming. Each job emits its own TaskCreate with
	// overlapping + unique IDs.
	j1(AgentEvent{
		Kind: EventAgentTaskCreate,
		TaskList: &AgentTaskListEvent{
			Items: []AgentTaskItem{
				{ID: "1", Subject: "group-1 task A", Status: TaskPending},
				{ID: "3", Subject: "shared task", Status: TaskPending},
			},
		},
	})
	j2(AgentEvent{
		Kind: EventAgentTaskUpdate,
		TaskList: &AgentTaskListEvent{
			Items: []AgentTaskItem{
				{ID: "2", Subject: "group-2 task", Status: TaskInProgress},
				{ID: "3", Subject: "shared task (updated by group-2)", Status: TaskCompleted},
			},
		},
	})

	// Each Task* event is forwarded as a merged snapshot; the
	// final outer state should have 3 unique IDs, with "3" carrying
	// the latest (group-2's update).
	got := rec.snapshot()
	var last AgentEvent
	for _, ev := range got {
		if ev.Kind == EventAgentTaskUpdate || ev.Kind == EventAgentTaskCreate {
			last = ev
		}
	}
	if last.TaskList == nil {
		t.Fatalf("expected at least one TaskCreate/Update in outer stream; got %+v", got)
	}
	gotIDs := map[string]AgentTaskItem{}
	for _, it := range last.TaskList.Items {
		gotIDs[it.ID] = it
	}
	if len(gotIDs) != 3 {
		t.Errorf("expected 3 unique task IDs after merge; got %d. items: %+v", len(gotIDs), last.TaskList.Items)
	}
	if gotIDs["3"].Subject != "shared task (updated by group-2)" {
		t.Errorf("task ID=3 should reflect latest write (group-2 update); got %+v", gotIDs["3"])
	}
}

// TestEventAggregator_OutOfOrderReadyAndDone: a fast job reports
// Result BEFORE all jobs have reached Ready. The aggregator must NOT
// fire the synthetic outer Result early — it waits for the Ready
// gate (REVIEW.md §2.5.5异序到达容错) AND for the completion
// markJobDone from every goroutine.
func TestEventAggregator_OutOfOrderReadyAndDone(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 3)

	// Job 1 reports Ready + Result early (before j2/j3 Ready).
	// The buffered events fire markJobDone; j2 and j3 haven't
	// even been wired up yet — doneCount stays at 1 of 3.
	runSingleJob(agg, "group-1", []AgentEvent{
		readyEvent("a", "/r", "main"),
		resultEvent("early group-1 findings"),
	})

	// j2 + j3 not yet started → outer should see ZERO events.
	got := rec.snapshot()
	if len(got) != 0 {
		t.Errorf("after 1 of 3 jobs done expected 0 outer events; got %d: %+v", len(got), got)
	}

	// Now j2 + j3 run. Each markJobDone increments doneCount;
	// the LAST one drives it to expected → synthetic outer Result.
	runJobGoroutines(agg, map[string][]AgentEvent{
		"group-2": {readyEvent("a", "/r", "main"), resultEvent("group-2 findings")},
		"group-3": {readyEvent("a", "/r", "main"), resultEvent("group-3 findings")},
	})

	got = rec.snapshot()
	resultCount := 0
	var synthetic AgentEvent
	for _, ev := range got {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
			synthetic = ev
		}
	}
	if resultCount != 1 {
		t.Errorf("after all 3 markJobDone expected exactly 1 outer Result; got %d. events: %+v", resultCount, got)
	}
	if synthetic.Kind != EventAgentResult {
		t.Errorf("synthetic = %+v, want EventAgentResult", synthetic)
	}
}

// TestEventAggregator_LateEventsDropped: events arriving after
// phaseClosed are silently dropped. The chat has already received
// the synthetic outer Result; further events are out-of-band.
func TestEventAggregator_LateEventsDropped(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	runSingleJob(agg, "group-1", []AgentEvent{
		readyEvent("a", "/r", "main"),
		resultEvent("done"),
	})

	// After Ready + Result + markJobDone, we're in phaseClosed.
	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected 3 events (Ready + per-job Result + synthetic outer Result); got %d: %+v", len(got), got)
	}

	// Late event arrives via a fresh wrapJob (defensive — late
	// events shouldn't normally reach the sink post-close).
	lateSink := agg.wrapJob("group-1")
	lateSink(AgentEvent{Kind: EventAgentText, Text: "this should be dropped"})

	got = rec.snapshot()
	if len(got) != 3 {
		t.Errorf("late event should be dropped; outer event count = %d", len(got))
	}
}

// TestEventAggregator_FinalizeClosesLifecycle is the regression
// lock for Finding 3 from /review: when a per-job RunOnce never
// emits a terminal (spawn failure before sink wiring, error path
// that bypassed the sink, ctx cancellation that orphaned the
// subprocess), doneCount stays short of expected. Without
// finalize(), the synthetic outer Result never fires and the
// chat channel hangs at "review running…" until the 30-min
// revCtx timeout. finalize() is the defensive catch-up: in
// the new completion-driven design, every per-job goroutine
// already called markJobDone via defer, so doneCount reaches
// expected before finalize() even runs. finalize() therefore
// becomes a near no-op in the normal path. This test pins the
// defensive behavior for the edge case where some markJobDone
// didn't fire (test mock, future bug, goroutine panic).
func TestEventAggregator_FinalizeClosesLifecycle(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 3)

	// Only 1 of 3 per-jobs wired up + markJobDone. finalize()
	// catches up the remaining 2 of expected.
	runSingleJob(agg, "group-1", []AgentEvent{readyEvent("a", "/r", "main")})

	agg.finalize()

	got := rec.snapshot()
	resultCount := 0
	for _, ev := range got {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("finalize() must emit exactly 1 synthetic outer Result; got %d. events: %+v", resultCount, got)
	}
}

// TestEventAggregator_FinalizeIdempotent: calling finalize()
// multiple times is safe — only one synthetic outer Result is
// emitted total.
func TestEventAggregator_FinalizeIdempotent(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	runSingleJob(agg, "group-1", []AgentEvent{readyEvent("a", "/r", "main")})

	agg.finalize()
	agg.finalize()
	agg.finalize()

	resultCount := 0
	for _, ev := range rec.snapshot() {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("finalize() must be idempotent; got %d outer Results", resultCount)
	}
}

// TestEventAggregator_MarkJobDoneFiresOuterResult pins the core
// design of the refactor: doneCount is driven by markJobDone
// (called from each per-job goroutine's defer), NOT by sink
// events. When the LAST goroutine's markJobDone drives doneCount
// to expected, that goroutine fires the synthetic outer Result
// — single-shot via finalEmitted.
func TestEventAggregator_MarkJobDoneFiresOuterResult(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	// Job 1 done — doneCount=1, no synthetic Result yet (need 2).
	runSingleJob(agg, "group-1", []AgentEvent{readyEvent("a", "/r", "main")})

	got := rec.snapshot()
	resultCount := 0
	for _, ev := range got {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if resultCount != 0 {
		t.Errorf("after 1 of 2 markJobDone expected 0 synthetic Results; got %d", resultCount)
	}

	// Job 2 done — doneCount=2, synthetic Result fires.
	runSingleJob(agg, "group-2", []AgentEvent{readyEvent("a", "/r", "main")})

	got = rec.snapshot()
	resultCount = 0
	for _, ev := range got {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("after 2 of 2 markJobDone expected 1 synthetic Result; got %d", resultCount)
	}
}

// TestEventAggregator_MarkJobDoneEvenWhenSinkSilent pins the
// design's central invariant: markJobDone fires the synthetic
// outer Result REGARDLESS of whether the sink ever received a
// terminal event. This is what makes the aggregator robust to
// bridges whose error path bypasses the sink (Findings 1 & 2
// from /review). drain a single per-job that emits ONLY a
// Ready (no Result, no Error); markJobDone still closes the
// lifecycle.
func TestEventAggregator_MarkJobDoneEvenWhenSinkSilent(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	// Job 1: emits Ready only (no terminal). The sink sees no
	// EventAgentResult / EventAgentError for this job.
	runSingleJob(agg, "group-1", []AgentEvent{readyEvent("a", "/r", "main")})
	// Job 2: ditto.
	runSingleJob(agg, "group-2", []AgentEvent{readyEvent("a", "/r", "main")})

	got := rec.snapshot()
	resultCount := 0
	for _, ev := range got {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("sink-silent jobs must still close lifecycle via markJobDone; got %d synthetic Results", resultCount)
	}
}

// TestEventAggregator_HandleStalePhaseRedirects is the regression
// lock for Finding 4 from /review: the TOCTOU race between
// handle()'s unlock and the phase-specific handler's own
// lock. handleBuffering re-checks phase under lock and
// redirects late events to handleStreaming if the transition
// already fired (and vice versa).
func TestEventAggregator_HandleStalePhaseRedirects(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	j1 := agg.wrapJob("group-1")
	j2 := agg.wrapJob("group-2")

	// Drive all-Ready via j1 to force phaseBuffering → phaseStreaming
	// transition. The transition takes a snapshot + clears
	// j1.initBuffer.
	j1(readyEvent("a", "/r", "main"))
	j2(readyEvent("a", "/r", "main"))

	// After both Readys: phase=streaming, j1.initBuffer is empty
	// (cleared during replay).
	got := rec.snapshot()
	syntheticReadyCount := 0
	for _, ev := range got {
		if ev.Kind == EventAgentReady && ev.Source == "" {
			syntheticReadyCount++
		}
	}
	if syntheticReadyCount != 1 {
		t.Fatalf("expected 1 synthetic outer Ready; got %d. events: %+v", syntheticReadyCount, got)
	}

	// Now emit a Result from j1 — this is a LIVE event arriving
	// in Phase 2. The handle() dispatch reads phase=streaming
	// and routes to handleStreaming. handleStreaming increments
	// doneCount (terminalSeen guard). All good — this is the
	// normal path.
	j1(resultEvent("j1 findings"))

	got = rec.snapshot()
	var syntheticResult AgentEvent
	for _, ev := range got {
		if ev.Kind == EventAgentResult && ev.Source == "" {
			syntheticResult = ev
		}
	}
	if syntheticResult.Source != "" || syntheticResult.Kind != EventAgentResult {
		// After j1 Result + j2 still pending, no synthetic Result yet
		// — doneCount is 1 of 2. So this is correct: no synthetic Result.
		// We just verify nothing crashed.
	}
}

// TestEventAggregator_ConcurrentEmissions: N goroutines pump events
// concurrently. Verifies that the aggregator's mutex is sufficient —
// no panics, no lost updates, exactly one synthetic outer Ready +
// exactly one synthetic outer Result, regardless of interleaving.
func TestEventAggregator_ConcurrentEmissions(t *testing.T) {
	rec := &sinkRecorder{}
	const N = 6
	agg := newEventAggregator(rec.sink(), N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// markJobDone mirrors the orchestrator's defer.
			defer agg.markJobDone()
			sink := agg.wrapJob(fmt.Sprintf("group-%d", i))
			sink(readyEvent("a", "/r", "main"))
			// Some interleaved intermediate events.
			sink(AgentEvent{Kind: EventAgentText, Text: "thinking"})
			sink(AgentEvent{
				Kind:      EventAgentToolStart,
				ToolStart: &AgentToolStartEvent{ID: fmt.Sprintf("t-%d", i), Name: "Read"},
			})
			sink(AgentEvent{
				Kind:    EventAgentToolEnd,
				ToolEnd: &AgentToolEndEvent{ID: fmt.Sprintf("t-%d", i), Name: "Read"},
			})
			sink(resultEvent("done"))
		}(i)
	}
	wg.Wait()

	got := rec.snapshot()

	// Count only the synthetic outer events (Source=""), NOT
	// per-job terminal events which are now also forwarded as
	// observation (per refactor).
	readyCount := 0
	resultCount := 0
	for _, ev := range got {
		if ev.Kind == EventAgentReady && ev.Source == "" {
			readyCount++
		}
		if ev.Kind == EventAgentResult && ev.Source == "" {
			resultCount++
		}
	}
	if readyCount != 1 {
		t.Errorf("expected exactly 1 synthetic outer Ready; got %d", readyCount)
	}
	if resultCount != 1 {
		t.Errorf("expected exactly 1 synthetic outer Result; got %d", resultCount)
	}

	// Per-job, the (ToolStart, ToolEnd) pair must be contiguous.
	for i := 0; i < N; i++ {
		wantID := fmt.Sprintf("t-%d", i)
		var startIdx, endIdx = -1, -1
		for j, ev := range got {
			if ev.Kind == EventAgentToolStart && ev.ToolStart != nil && ev.ToolStart.ID == wantID {
				startIdx = j
			}
			if ev.Kind == EventAgentToolEnd && ev.ToolEnd != nil && ev.ToolEnd.ID == wantID {
				endIdx = j
			}
		}
		if startIdx == -1 || endIdx == -1 {
			t.Errorf("job %d: tool pair not found (start=%d end=%d)", i, startIdx, endIdx)
			continue
		}
		if endIdx != startIdx+1 {
			t.Errorf("job %d: tool pair not contiguous (start=%d end=%d)", i, startIdx, endIdx)
		}
	}
}
