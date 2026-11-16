package agent

// The delivery surface of a review: the findings of one group reach
// the chat as one reply, the plan the fan-out reports before it starts,
// and the parsed report every group carries out.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// groupTexts returns the outer stream's group texts — the thinking
// events that carry a group's output, without the start hints.
func groupTexts(events []AgentEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind != EventAgentThinking {
			continue
		}
		if strings.HasPrefix(ev.Text, "▶ **") {
			continue
		}
		out = append(out, ev.Text)
	}
	return out
}

// startHints returns the outer stream's start hints — the thinking
// events that announce a group.
func startHints(events []AgentEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == EventAgentThinking && strings.HasPrefix(ev.Text, "▶ **") {
			out = append(out, ev.Text)
		}
	}
	return out
}

// stripNumber removes the "[N] " prefix a delivered group text carries.
func stripNumber(text string) string {
	if i := strings.Index(text, "] "); i >= 0 && strings.HasPrefix(text, "[") {
		return text[i+2:]
	}
	return text
}

// TestEventAggregator_HandsTheFindingsOverAsOneReply: a group's
// findings do not reach the chat as a stream of fragments. The group
// collects them and hands them over when it ends — one reply, no
// matter how many chunks the group produced.
func TestEventAggregator_HandsTheFindingsOverAsOneReply(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))

	// 40 chunks of a paragraph — far below the size trigger, so the
	// group never has to split its findings.
	var raw strings.Builder
	for i := 0; i < 40; i++ {
		chunk := fmt.Sprintf("chunk %d of the review", i)
		if i > 0 {
			raw.WriteString("\n")
		}
		raw.WriteString(chunk)
		j1(AgentEvent{Kind: EventAgentText, Text: chunk})
	}

	// Nothing is forwarded while the group is still working.
	if got := groupTexts(rec.snapshot()); len(got) != 0 {
		t.Fatalf("the group's findings were forwarded before the group ended: %+v", got)
	}

	// The group ends: its findings go out as one reply.
	j1(resultEvent(""))

	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("the group produced %d replies, want 1: %+v", len(bodies), bodies)
	}
	if !strings.HasPrefix(bodies[0], "[1] ") {
		t.Errorf("the reply does not carry the group's number: %q", bodies[0])
	}
	if got := stripNumber(bodies[0]); got != raw.String() {
		t.Errorf("the reply does not reproduce the group's raw text:\ngot:  %q\nwant: %q", got, raw.String())
	}
}

// TestEventAggregator_FindingsRideTheThinkingKind: the findings ride
// the kind the channels render in the thread — the thinking kind. The
// card keeps the plan and the verdict; the findings do not land on it.
func TestEventAggregator_FindingsRideTheThinkingKind(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "the findings"})
	j1(resultEvent(""))

	for _, ev := range rec.snapshot() {
		if ev.Kind == EventAgentText {
			t.Fatalf("a group's findings reached the chat as a text event: %+v", ev)
		}
	}
	if got := groupTexts(rec.snapshot()); len(got) != 1 {
		t.Fatalf("the group produced %d replies, want 1: %+v", len(got), got)
	}
}

// TestEventAggregator_AnnouncesTheGroup: the group announces itself
// when its first output arrives — the hint names the group and how
// many files it covers.
func TestEventAggregator_AnnouncesTheGroup(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "the findings"})
	j1(resultEvent(""))

	hints := startHints(rec.snapshot())
	if len(hints) != 1 {
		t.Fatalf("the group produced %d start hints, want 1: %+v", len(hints), hints)
	}
	for _, want := range []string{"[1]", "group 1", "1 file"} {
		if !strings.Contains(hints[0], want) {
			t.Errorf("the start hint does not carry %q: %q", want, hints[0])
		}
	}
}

// TestEventAggregator_AnnouncesTheGroupOnce: the group announces
// itself once, not once per chunk.
func TestEventAggregator_AnnouncesTheGroupOnce(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	for i := 0; i < 5; i++ {
		j1(AgentEvent{Kind: EventAgentText, Text: fmt.Sprintf("chunk %d", i)})
	}
	j1(resultEvent(""))

	if got := startHints(rec.snapshot()); len(got) != 1 {
		t.Fatalf("the group produced %d start hints, want 1: %+v", len(got), got)
	}
}

// TestEventAggregator_SplitsTheFindingsIntoConsecutiveReplies: a
// group whose findings exceed one message produces consecutive
// replies — every one of them starts with the group's number, and
// together they carry the group's whole text.
func TestEventAggregator_SplitsTheFindingsIntoConsecutiveReplies(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))

	// One long review — the findings cross the 2 KiB trigger in the
	// middle of the text, and the cut lands on the last paragraph
	// break before it.
	raw := strings.Repeat("a paragraph of review text that goes on for a while\n\n", 39) +
		"a paragraph of review text that goes on for a while"
	j1(AgentEvent{Kind: EventAgentText, Text: raw})
	j1(resultEvent(""))

	bodies := groupTexts(rec.snapshot())
	if len(bodies) < 2 {
		t.Fatalf("expected the oversized findings to be split; got %d replies: %+v", len(bodies), bodies)
	}
	for i, body := range bodies {
		if !strings.HasPrefix(body, "[1] ") {
			t.Errorf("reply %d does not carry the group's number: %q", i, body)
		}
	}
	// Every reply but the last ends on a paragraph break — the cut
	// never splits a markdown block.
	for i, body := range bodies[:len(bodies)-1] {
		if !strings.HasSuffix(stripNumber(body), "\n\n") {
			t.Errorf("reply %d does not end on a paragraph break: %q", i, body)
		}
	}
	// Nothing is dropped: the replies concatenate back into the
	// group's raw text.
	var joined strings.Builder
	for _, body := range bodies {
		joined.WriteString(stripNumber(body))
	}
	if joined.String() != raw {
		t.Errorf("the replies do not reproduce the group's raw text:\ngot:  %q\nwant: %q",
			joined.String(), raw)
	}
}

// TestEventAggregator_StreamsTheReasoning: the group's reasoning keeps
// streaming while the group works — it is the process, not the
// outcome.
func TestEventAggregator_StreamsTheReasoning(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
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
	if len(think) != 2 {
		t.Fatalf("the group produced %d thinking messages, want the start hint and the reasoning: %+v", len(think), think)
	}
	if !strings.HasPrefix(think[1].Text, "[1] ") {
		t.Errorf("the reasoning does not carry the group's number: %q", think[1].Text)
	}
	if got := stripNumber(think[1].Text); got != "thinking about \nthe diff" {
		t.Errorf("the flushed reasoning = %q, want the group's reasoning", got)
	}
}

// TestEventAggregator_FlushesWithoutATerminal: a group that never
// emits a terminal event still does not lose its findings — the
// aggregator's finalization hands them over.
func TestEventAggregator_FlushesWithoutATerminal(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "orphan text"})

	agg.finalize()

	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected the orphaned findings to be handed over; got %d: %+v", len(bodies), bodies)
	}
	if got := stripNumber(bodies[0]); got != "orphan text" {
		t.Errorf("the reply = %q, want %q", got, "orphan text")
	}
}

// TestEventAggregator_FlushesTheTailOfTheReply: a bridge may hold
// the tail of the group's reply back until the turn ends (pi / omp /
// dsh put the last segment on the result event). That tail belongs to
// the group — it reaches the chat with the rest of it.
func TestEventAggregator_FlushesTheTailOfTheReply(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "the streamed part"})
	j1(resultEvent(" and the tail"))

	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected one reply, got %d: %+v", len(bodies), bodies)
	}
	if got := stripNumber(bodies[0]); got != "the streamed part\n and the tail" {
		t.Errorf("the reply = %q, want the group's whole reply", got)
	}
}

// TestEventAggregator_AnnouncesTheGroupBeforeItsTail: a bridge that
// streams nothing and puts the whole reply on the terminal event still
// gets its group announced — the findings never arrive without a hint.
func TestEventAggregator_AnnouncesTheGroupBeforeItsTail(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(resultEvent("the whole reply"))

	hints := startHints(rec.snapshot())
	if len(hints) != 1 {
		t.Fatalf("the group produced %d start hints, want 1: %+v", len(hints), hints)
	}
	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("the group produced %d replies, want 1: %+v", len(bodies), bodies)
	}
	if got := stripNumber(bodies[0]); got != "the whole reply" {
		t.Errorf("the reply = %q, want the group's reply", got)
	}
}

// TestEventAggregator_KeepsToolEventsUnchanged: the tool events are
// not coalesced — they reach the outer sink as they are, with the
// start of a call immediately followed by its end.
func TestEventAggregator_KeepsToolEventsUnchanged(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
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
		if plan[i].Label == "" {
			t.Errorf("plan[%d] carries no label", i)
		}
		if len(plan[i].Files) != len(g.Files) {
			t.Errorf("plan[%d] carries %d files, want %d", i, len(plan[i].Files), len(g.Files))
		}
	}
}

// TestRunReviewGroups_PlanObserverNamesTheNightmeGroups: the two
// nightme-owned groups get a name the user can read — the sentinel
// itself is an internal marker — and the name carries the summary of
// the files the group covers.
func TestRunReviewGroups_PlanObserverNamesTheNightmeGroups(t *testing.T) {
	groups := []reviewGroup{
		{Pattern: PatternBuiltin, Files: []string{"a.go"}},
		{Pattern: PatternSimplify, Files: []string{"a.go"}},
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
	if plan[0].Label != "nightme code reviewer · Go" {
		t.Errorf("the builtin group's label = %q, want %q", plan[0].Label, "nightme code reviewer · Go")
	}
	if plan[1].Label != "Simplify review lens · Go" {
		t.Errorf("the simplify group's label = %q, want %q", plan[1].Label, "Simplify review lens · Go")
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
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "the findings"})
	j1(resultEvent("the findings"))

	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected one reply, got %d: %+v", len(bodies), bodies)
	}
	if got := stripNumber(bodies[0]); got != "the findings" {
		t.Errorf("the reply = %q, want the group's text once", got)
	}
}

// TestEventAggregator_ReplaysTheBuffersWhenAGroupNeverReportsReady:
// a group that fails before its Ready event never reaches the Ready
// gate — the aggregator would stay in phaseBuffering and every
// group's output would sit in its buffer. The fan-out's end replays
// them, so the surviving groups' findings still reach the chat.
func TestEventAggregator_ReplaysTheBuffersWhenAGroupNeverReportsReady(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(2))

	j1 := agg.wrapJob(0)
	_ = agg.wrapJob(1)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "group-1 findings"})

	// group-2 never reports Ready; the fan-out ends.
	agg.finalize()

	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected the stranded findings to be replayed, got %d: %+v", len(bodies), bodies)
	}
	if got := stripNumber(bodies[0]); got != "group-1 findings" {
		t.Errorf("the replayed reply = %q, want the group's findings", got)
	}
}

// TestEventAggregator_JoinsThePiecesWithANewline: the pieces of a
// reply are complete blocks — two sentences must not run together in
// the reply that carries them.
func TestEventAggregator_JoinsThePiecesWithANewline(t *testing.T) {
	rec := &sinkRecorder{}
	agg := newEventAggregator(rec.sink(), testPlan(1))

	j1 := agg.wrapJob(0)
	j1(readyEvent("a", "/r", "main"))
	j1(AgentEvent{Kind: EventAgentText, Text: "first sentence."})
	j1(AgentEvent{Kind: EventAgentText, Text: "second sentence."})
	j1(resultEvent(""))

	bodies := groupTexts(rec.snapshot())
	if len(bodies) != 1 {
		t.Fatalf("expected one reply, got %d: %+v", len(bodies), bodies)
	}
	if got := stripNumber(bodies[0]); got != "first sentence.\nsecond sentence." {
		t.Errorf("the reply = %q, want the pieces joined by a newline", got)
	}
}
