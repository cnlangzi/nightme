package agent

// The delivery surface of a review: per-group coalescing of the
// streamed text, the plan the fan-out reports before it starts, and
// the parsed report every group carries out.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// textBodies returns the bodies of the outer stream's text events —
// the label stripped off, so the concatenation can be compared with
// the group's raw text.
func textBodies(events []AgentEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind != EventAgentText {
			continue
		}
		out = append(out, ev.Text)
	}
	return out
}

// stripLabel removes the "[group-N]\n\n" prefix a flushed batch
// carries.
func stripLabel(text string) string {
	if i := strings.Index(text, "\n\n"); i >= 0 && strings.HasPrefix(text, "[") {
		return text[i+2:]
	}
	return text
}

// TestEventAggregator_CoalescesTextIntoLabeledBatches: a group's
// text chunks do not become one outbound message each. They are
// coalesced into batches, each batch labeled with its group, and the
// bodies of the batches concatenate back into the group's raw text —
// nothing is dropped.
func TestEventAggregator_CoalescesTextIntoLabeledBatches(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))

	// 40 chunks of a paragraph — far below the size trigger, so the
	// batch stays open until the group ends.
	var raw strings.Builder
	for i := 0; i < 40; i++ {
		chunk := fmt.Sprintf("chunk %d of the review", i)
		if i > 0 {
			raw.WriteString("\n")
		}
		raw.WriteString(chunk)
		j1(AgentEvent{Kind: EventAgentText, Text: chunk})
	}

	// Nothing is forwarded while the batch is pending.
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("expected only the synthetic Ready while the batch is pending; got %d: %+v", len(got), got)
	}

	// The group ends: its batch is flushed.
	j1(resultEvent(""))

	bodies := textBodies(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("the group produced %d text messages, want 1 coalesced batch: %+v", len(bodies), bodies)
	}
	if !strings.HasPrefix(bodies[0], "[group-1]\n\n") {
		t.Errorf("the batch does not carry its group label: %q", bodies[0])
	}
	if got := stripLabel(bodies[0]); got != raw.String() {
		t.Errorf("the flushed body does not reproduce the group's raw text:\ngot:  %q\nwant: %q", got, raw.String())
	}
}

// TestEventAggregator_CoalescesThinking: the group's thinking follows
// the same rules as its text.
func TestEventAggregator_CoalescesThinking(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentThinking, Text: "thinking about "})
	j1(AgentEvent{Kind: EventAgentThinking, Text: "the diff"})
	j1(resultEvent(""))

	var think []AgentEvent
	for _, ev := range rec.snapshot() {
		if ev.Kind == EventAgentThinking {
			think = append(think, ev)
		}
	}
	if len(think) != 1 {
		t.Fatalf("the group produced %d thinking messages, want 1 coalesced batch: %+v", len(think), think)
	}
	if !strings.HasPrefix(think[0].Text, "[group-1]\n\n") {
		t.Errorf("the thinking batch does not carry its group label: %q", think[0].Text)
	}
	if got := stripLabel(think[0].Text); got != "thinking about \nthe diff" {
		t.Errorf("the flushed thinking = %q, want the group's thinking", got)
	}
}

// TestEventAggregator_FlushesOnSize: a batch that grows past the size
// trigger is flushed — cut at its last blank line, so a markdown
// block is not split in half.
func TestEventAggregator_FlushesOnSize(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))

	// One long review — the batch crosses the 2 KiB trigger in the
	// middle of the text, and the cut lands on the last paragraph
	// break before it.
	raw := strings.Repeat("a paragraph of review text that goes on for a while\n\n", 39) +
		"a paragraph of review text that goes on for a while"
	j1(AgentEvent{Kind: EventAgentText, Text: raw})
	j1(resultEvent(""))

	bodies := textBodies(rec.snapshot())
	if len(bodies) < 2 {
		t.Fatalf("expected the oversized batch to be flushed before the group ended; got %d batches: %+v",
			len(bodies), bodies)
	}
	for i, body := range bodies {
		if !strings.HasPrefix(body, "[group-1]\n\n") {
			t.Errorf("batch %d does not carry its group label: %q", i, body)
		}
	}
	// Every batch but the last ends on a paragraph break — the cut
	// never splits a markdown block.
	for i, body := range bodies[:len(bodies)-1] {
		if !strings.HasSuffix(stripLabel(body), "\n\n") {
			t.Errorf("batch %d does not end on a paragraph break: %q", i, body)
		}
	}
	// Nothing is dropped: the batches concatenate back into the
	// group's raw text.
	var joined strings.Builder
	for _, body := range bodies {
		joined.WriteString(stripLabel(body))
	}
	if joined.String() != raw {
		t.Errorf("the flushed batches do not reproduce the group's raw text:\ngot:  %q\nwant: %q",
			joined.String(), raw)
	}
}

// TestEventAggregator_FlushesWithoutATerminal: a group that never
// emits a terminal event still does not lose its text — the
// aggregator's finalization flushes it.
func TestEventAggregator_FlushesWithoutATerminal(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "orphan text"})

	agg.finalize()

	bodies := textBodies(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected the orphaned batch to be flushed; got %d: %+v", len(bodies), bodies)
	}
	if got := stripLabel(bodies[0]); got != "orphan text" {
		t.Errorf("the flushed body = %q, want %q", got, "orphan text")
	}
}

// TestEventAggregator_FlushesTheTailOfTheReply: a bridge may hold
// the tail of the group's reply back until the turn ends (pi / omp /
// dsh put the last segment on the result event). That tail belongs to
// the group — it reaches the channel with the rest of it.
func TestEventAggregator_FlushesTheTailOfTheReply(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "the streamed part"})
	j1(resultEvent(" and the tail"))

	bodies := textBodies(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected one coalesced batch, got %d: %+v", len(bodies), bodies)
	}
	if got := stripLabel(bodies[0]); got != "the streamed part\n and the tail" {
		t.Errorf("the flushed body = %q, want the group's whole reply", got)
	}
}

// TestEventAggregator_KeepsToolEventsUnchanged: the tool events are
// not coalesced — they reach the outer sink as they are, with the
// start of a call immediately followed by its end.
func TestEventAggregator_KeepsToolEventsUnchanged(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{
		Kind:      EventAgentToolStart,
		ToolStart: &AgentToolStartEvent{ID: "t1", Name: "Read"},
	})
	j1(AgentEvent{
		Kind:    EventAgentToolEnd,
		ToolEnd: &AgentToolEndEvent{ID: "t1", Name: "Read"},
	})

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected 3 outer events (Ready, ToolStart, ToolEnd); got %d: %+v", len(got), got)
	}
	if got[1].Kind != EventAgentToolStart || got[1].ToolStart.ID != "t1" {
		t.Errorf("event[1] = %+v, want the ToolStart unchanged", got[1])
	}
	if got[2].Kind != EventAgentToolEnd || got[2].ToolEnd.ID != "t1" {
		t.Errorf("event[2] = %+v, want the ToolEnd unchanged", got[2])
	}
}

// TestRunReviewGroups_PlanObserver: the plan fires once, before the
// first group produces output, and carries one entry per group in
// fan-out order.
func TestRunReviewGroups_PlanObserver(t *testing.T) {
	groups := twoGroups()
	pre := reviewContext{workspace: "/nonexistent-repo", ocrGroups: groups}
	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				return groupReply("ok")
			})
		},
	}

	var mu sync.Mutex
	var order []string
	var plans [][]ReviewPlanGroup
	observer := func(p []ReviewPlanGroup) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "plan")
		plans = append(plans, p)
	}
	sink := func(ev AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "event")
	}

	if _, err := delegateReviewMultiJob(context.Background(), fs, StartConfig{Workspace: pre.workspace},
		pre, pre.ocrGroups, WithEventSink(sink), WithReviewPlanObserver(observer)); err != nil {
		t.Fatalf("delegateReviewMultiJob: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(plans) != 1 {
		t.Fatalf("the plan observer fired %d times, want 1: %+v", len(plans), plans)
	}
	if len(order) == 0 || order[0] != "plan" {
		t.Errorf("the plan did not fire before the first event: %+v", order)
	}
	plan := plans[0]
	if len(plan) != len(groups) {
		t.Fatalf("the plan carries %d groups, want %d: %+v", len(plan), len(groups), plan)
	}
	for i, g := range groups {
		if plan[i].Pattern != g.Pattern {
			t.Errorf("plan[%d].Pattern = %q, want %q", i, plan[i].Pattern, g.Pattern)
		}
		if plan[i].Label != g.Pattern {
			t.Errorf("plan[%d].Label = %q, want the group's pattern", i, plan[i].Label)
		}
		if len(plan[i].Files) != len(g.Files) {
			t.Errorf("plan[%d] carries %d files, want %d", i, len(plan[i].Files), len(g.Files))
		}
	}
}

// TestRunReviewGroups_PlanObserverNamesTheNightmeGroups: the two
// nightme-owned sentinels get a name the user can read — the
// sentinel itself is an internal marker.
func TestRunReviewGroups_PlanObserverNamesTheNightmeGroups(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: patternBuiltin, Files: []string{"a.go"}},
		{Pattern: patternSimplify, Files: []string{"a.go"}},
	}
	pre := reviewContext{workspace: "/nonexistent-repo", ocrGroups: groups}
	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				return groupReply("ok")
			})
		},
	}

	var mu sync.Mutex
	var plan []ReviewPlanGroup
	observer := func(p []ReviewPlanGroup) {
		mu.Lock()
		defer mu.Unlock()
		plan = p
	}
	if _, err := delegateReviewMultiJob(context.Background(), fs, StartConfig{Workspace: pre.workspace},
		pre, pre.ocrGroups, WithReviewPlanObserver(observer)); err != nil {
		t.Fatalf("delegateReviewMultiJob: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(plan) != 2 {
		t.Fatalf("the plan carries %d groups, want 2: %+v", len(plan), plan)
	}
	if plan[0].Label != "builtin rubric" {
		t.Errorf("the builtin group's label = %q, want %q", plan[0].Label, "builtin rubric")
	}
	if plan[1].Label != "simplify lens" {
		t.Errorf("the simplify group's label = %q, want %q", plan[1].Label, "simplify lens")
	}
}

// TestRunReviewGroups_ReportCarriesTheGroupsFindings: the observer
// receives the group's parsed report — the group's own output is
// parsed, per group.
func TestRunReviewGroups_ReportCarriesTheGroupsFindings(t *testing.T) {
	groups := twoGroups()
	pre := reviewContext{workspace: "/nonexistent-repo", ocrGroups: groups}
	review := "- path: internal/foo.go\n- start_line: 3\n- end_line: 3\n- category: bug\n- severity: high\n- content: it breaks\n"
	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				return []AgentEvent{
					sessionReadyEvent("sess-group", "model"),
					{Kind: EventAgentResult, Result: &AgentResultEvent{Text: review}},
				}
			})
		},
	}

	var mu sync.Mutex
	var reports []ReviewJob
	observer := func(job ReviewJob, err error) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, job)
	}
	if _, err := delegateReviewMultiJob(context.Background(), fs, StartConfig{Workspace: pre.workspace},
		pre, pre.ocrGroups, WithReviewJobObserver(observer)); err != nil {
		t.Fatalf("delegateReviewMultiJob: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != len(groups) {
		t.Fatalf("the observer saw %d reports, want %d", len(reports), len(groups))
	}
	for _, r := range reports {
		if !r.Report.Parsed {
			t.Errorf("the report of %q was not parsed: %+v", r.Pattern, r.Report)
			continue
		}
		if len(r.Report.Findings) != 1 {
			t.Errorf("the report of %q carries %d findings, want 1: %+v", r.Pattern, len(r.Report.Findings), r.Report.Findings)
			continue
		}
		if r.Report.Findings[0].Severity != SeverityHigh {
			t.Errorf("the finding of %q has severity %q, want %q",
				r.Pattern, r.Report.Findings[0].Severity, SeverityHigh)
		}
	}
}

// TestEventAggregator_SkipsTheTailTheGroupAlreadyDelivered: a
// bridge that streams the reply AND repeats it on the terminal event
// (dsh carries its last message on both) must not deliver that
// message twice.
func TestEventAggregator_SkipsTheTailTheGroupAlreadyDelivered(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "the findings"})
	j1(resultEvent("the findings"))

	bodies := textBodies(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected one batch, got %d: %+v", len(bodies), bodies)
	}
	if got := stripLabel(bodies[0]); got != "the findings" {
		t.Errorf("the flushed body = %q, want the group's text once", got)
	}
}

// TestEventAggregator_ReplaysTheBuffersWhenAGroupNeverReportsReady:
// a group that fails before its Ready event never reaches the Ready
// gate — the aggregator would stay in phaseBuffering and every
// group's output would sit in its buffer. The fan-out's end replays
// them, so the surviving groups' findings still reach the chat.
func TestEventAggregator_ReplaysTheBuffersWhenAGroupNeverReportsReady(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 2)

	j1 := agg.wrapJob("group-1")
	_ = agg.wrapJob("group-2")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "group-1 findings"})

	// group-2 never reports Ready; the fan-out ends.
	agg.finalize()

	bodies := textBodies(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected the stranded batch to be replayed, got %d: %+v", len(bodies), bodies)
	}
	if got := stripLabel(bodies[0]); got != "group-1 findings" {
		t.Errorf("the replayed body = %q, want the group's findings", got)
	}
}

// TestEventAggregator_JoinsThePiecesWithANewline: the pieces of a
// reply are complete blocks — two sentences must not run together in
// the batch that carries them.
func TestEventAggregator_JoinsThePiecesWithANewline(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), 1)

	j1 := agg.wrapJob("group-1")
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "first sentence."})
	j1(AgentEvent{Kind: EventAgentText, Text: "second sentence."})
	j1(resultEvent(""))

	bodies := textBodies(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected one batch, got %d: %+v", len(bodies), bodies)
	}
	if got := stripLabel(bodies[0]); got != "first sentence.\nsecond sentence." {
		t.Errorf("the flushed body = %q, want the pieces joined by a newline", got)
	}
}
