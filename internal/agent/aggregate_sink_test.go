package agent

import (
	"sync"
	"testing"
)

// sinkRecorder captures every AgentEvent passed to the outer sink in
// order. Used as the test outer sink for eventAggregator tests — the
// aggregator's contract is "N per-job streams → 1 outer stream", and
// recording the outer stream is the easiest way to assert that.
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

// TestEventAggregator_FirstReadyForwarded pins the #1 invariant: only
// the FIRST per-job EventAgentReady is forwarded as the outer Ready
// (with Source reset to ""). Subsequent per-job Ready events are
// suppressed to avoid N redundant "🤖: agent X ready" header flips in
// the chat channel's StatusBar.
func TestEventAggregator_FirstReadyForwarded(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 3)

	// Job 1: up-front Ready — this is the one that becomes the outer
	// Ready. Source is reset to "".
	agg.wrapJob("group-1")(AgentEvent{
		Kind:      EventAgentReady,
		AgentName: "agent-x",
		Workspace: "/repo",
	})

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("after first Ready expected 1 outer event, got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady {
		t.Errorf("event[0].Kind = %v, want EventAgentReady", got[0].Kind)
	}
	if got[0].Source != "" {
		t.Errorf("event[0].Source = %q, want \"\" (Ready must reset Source to outer)", got[0].Source)
	}
	if got[0].AgentName != "agent-x" {
		t.Errorf("event[0].AgentName = %q, want %q (forwarded from first job)", got[0].AgentName, "agent-x")
	}
}

// TestEventAggregator_SubsequentReadyDropped: jobs 2/3/4's Ready are
// dropped (the first Ready already signaled outer readiness).
func TestEventAggregator_SubsequentReadyDropped(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 4)

	for i := 1; i <= 4; i++ {
		agg.wrapJob("group-N")(AgentEvent{
			Kind:      EventAgentReady,
			AgentName: "x",
		})
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Errorf("expected exactly 1 outer Ready (first only), got %d", len(got))
	}
}

// TestEventAggregator_IntermediateDropped pins the #2 invariant: per-
// job intermediate events (Text / ToolStart / ToolEnd / Permission /
// Task*) are dropped at the aggregator. The chat channel only sees
// lifecycle markers; shipping N interleaved intermediate streams
// would clutter the chat.
//
// Scenario: 1 of 2 expected jobs reaches its terminal. We expect the
// outer sink to see ONLY the first Ready (1 event). The intermediate
// Text/ToolStart/ToolEnd events are dropped, AND the per-job Result
// is also dropped at the aggregator level (it counts toward done but
// doesn't get forwarded — done < expected so no synthetic yet).
func TestEventAggregator_IntermediateDropped(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	// Per-job: Ready → Text → ToolStart → ToolEnd → Result.
	// expected=2, only job1 reaches terminal → no synthetic yet.
	job1 := agg.wrapJob("group-1")
	job1(AgentEvent{Kind: EventAgentReady})
	job1(AgentEvent{Kind: EventAgentText, Text: "thinking..."})
	job1(AgentEvent{Kind: EventAgentToolStart, ToolStart: &AgentToolStartEvent{Name: "grep"}})
	job1(AgentEvent{Kind: EventAgentToolEnd, ToolEnd: &AgentToolEndEvent{Name: "grep"}})
	job1(AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "findings"}})

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 outer event (Ready only — intermediate dropped, no synthetic yet), got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady {
		t.Errorf("event[0].Kind = %v, want EventAgentReady", got[0].Kind)
	}
}

// TestEventAggregator_TerminalIncrementsDone: each per-job terminal
// increments the done counter; the synthetic outer Result fires only
// when done == expected.
func TestEventAggregator_TerminalIncrementsDone(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 3)

	jobs := []func(AgentEvent){
		agg.wrapJob("g-1"),
		agg.wrapJob("g-2"),
		agg.wrapJob("g-3"),
	}
	// 3 Ready → 1 outer Ready (rest dropped).
	for _, j := range jobs {
		j(AgentEvent{Kind: EventAgentReady})
	}
	// 1 terminal of 3 → no synthetic yet.
	jobs[0](AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}})

	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("after 1 terminal of 3 expected no synthetic Result yet; got %d events: %+v", len(got), got)
	}

	// 2nd terminal → still no synthetic.
	jobs[1](AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}})

	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("after 2 terminals of 3 still no synthetic; got %d events: %+v", len(got), got)
	}

	// 3rd terminal → synthetic outer Result fires.
	jobs[2](AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}})

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("after 3rd terminal expected synthetic outer Result; got %d events: %+v", len(got), got)
	}
	if got[1].Kind != EventAgentResult {
		t.Errorf("event[1].Kind = %v, want EventAgentResult (synthetic outer)", got[1].Kind)
	}
	if got[1].Source != "" {
		t.Errorf("event[1].Source = %q, want \"\" (synthetic outer marker)", got[1].Source)
	}
}

// TestEventAggregator_ErrorTerminalCounts: EventAgentError also
// counts as terminal (per-job crash should not strand the outer
// lifecycle).
func TestEventAggregator_ErrorTerminalCounts(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	j1 := agg.wrapJob("g-1")
	j2 := agg.wrapJob("g-2")
	j1(AgentEvent{Kind: EventAgentReady})
	j2(AgentEvent{Kind: EventAgentReady})
	j1(AgentEvent{Kind: EventAgentError, Err: errSentinel("job-1 crashed")})

	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("after 1 terminal of 2 (Error counts) no synthetic; got %d: %+v", len(got), got)
	}

	j2(AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}})
	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("after 2nd terminal expected synthetic; got %d events", len(got))
	}
	if got[1].Kind != EventAgentResult {
		t.Errorf("synthetic outer Kind = %v, want EventAgentResult", got[1].Kind)
	}
}

// errSentinel is a minimal sentinel-error helper for tests.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// TestEventAggregator_ConcurrentEmissions: N goroutines pump events
// concurrently. Verifies that the aggregator's internal mutex is
// sufficient — no panics, no lost updates, exactly one outer Ready +
// one synthetic outer Result at the end.
func TestEventAggregator_ConcurrentEmissions(t *testing.T) {
	rec := &sinkRecorder{}
	const N = 8
	agg := newEventAggregator(rec.sink(), N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sink := agg.wrapJob("g")
			sink(AgentEvent{Kind: EventAgentReady})
			// Some interleaved intermediate events (must be dropped).
			sink(AgentEvent{Kind: EventAgentText, Text: "thinking"})
			sink(AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}})
		}(i)
	}
	wg.Wait()

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 outer events (1 Ready + 1 synthetic Result), got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentReady || got[1].Kind != EventAgentResult {
		t.Errorf("event order = [%v, %v], want [Ready, Result]", got[0].Kind, got[1].Kind)
	}
}

// TestEventAggregator_NoReadyNoSynthetic: a defensive check — if no
// per-job Ready was emitted (extreme edge case: bridge crashed before
// emitting Ready and then surfaced a terminal), the synthetic outer
// Result still fires. This guards the "balanced lifecycle" invariant
// even at the edge.
func TestEventAggregator_NoReadyTerminalStillEmits(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	// Terminal WITHOUT preceding Ready (degenerate path).
	agg.wrapJob("g-1")(AgentEvent{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}})

	got := rec.snapshot()
	// No outer Ready was emitted (firstReady stays false), but the
	// synthetic Result still fires because done == expected.
	if len(got) != 1 {
		t.Fatalf("expected 1 event (synthetic Result); got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventAgentResult {
		t.Errorf("Kind = %v, want EventAgentResult", got[0].Kind)
	}
}
