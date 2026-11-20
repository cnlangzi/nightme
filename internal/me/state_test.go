package me

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// TestThePhaseLineNamesTheStep — a run that is working says which
// command it waits for.
func TestThePhaseLineNamesTheStep(t *testing.T) {
	r := &run{phase: PhaseRunning, inflight: &inflightState{Command: "/gtw commit"}}
	want := "running · step: /gtw commit"
	if got := r.phaseLine(); got != want {
		t.Errorf("the phase line is %q, want %q", got, want)
	}
}

// TestThePhaseLineWithoutAStep — a run that waits for no command says
// it is running.
func TestThePhaseLineWithoutAStep(t *testing.T) {
	r := &run{phase: PhaseRunning}
	if got := r.phaseLine(); got != "running" {
		t.Errorf("the phase line is %q, want it to say the run is working", got)
	}
}

// TestThePhaseLineNamesThePauseReason — a paused run says why it
// stopped.
func TestThePhaseLineNamesThePauseReason(t *testing.T) {
	r := &run{phase: PhasePaused, paused: true, pauseReason: "the operator paused the run"}
	want := "paused: the operator paused the run"
	if got := r.phaseLine(); got != want {
		t.Errorf("the phase line is %q, want %q", got, want)
	}
}

// TestThePhaseLineWithoutAPauseReason — a paused run that names no
// reason says it is paused.
func TestThePhaseLineWithoutAPauseReason(t *testing.T) {
	r := &run{phase: PhasePaused, paused: true}
	if got := r.phaseLine(); got != "paused" {
		t.Errorf("the phase line is %q, want it to say the run is paused", got)
	}
}

// TestThePhaseLineNamesTheQuestion — a run that waits for an answer
// says which question it waits for.
func TestThePhaseLineNamesTheQuestion(t *testing.T) {
	r := &run{phase: PhaseAsking, question: "Which branch?"}
	want := "asking: Which branch?"
	if got := r.phaseLine(); got != want {
		t.Errorf("the phase line is %q, want %q", got, want)
	}
}

// TestThePhaseLineIsCutShort — the line is read on a phone: it is cut
// to its budget.
func TestThePhaseLineIsCutShort(t *testing.T) {
	r := &run{phase: PhasePaused, paused: true, pauseReason: strings.Repeat("x", 300)}
	got := r.phaseLine()
	if n := len([]rune(got)); n > maxPhaseLine {
		t.Errorf("the phase line is %d runes, want at most %d", n, maxPhaseLine)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("the phase line %q does not say that it was cut", got)
	}
}

// TestThePhaseLineCutsTheQuestion — the question of the run is cut
// before the line is.
func TestThePhaseLineCutsTheQuestion(t *testing.T) {
	r := &run{phase: PhaseAsking, question: strings.Repeat("x", 300)}
	got := r.phaseLine()
	if !strings.HasSuffix(got, "…") {
		t.Errorf("the phase line %q does not say that the question was cut", got)
	}
	if n := len([]rune(got)); n > maxPhaseLine {
		t.Errorf("the phase line is %d runes, want at most %d", n, maxPhaseLine)
	}
}

// TestTheStateCarriesTheStateOfTheRound — the state carries the
// phase line, the tree of the rounds and the checklist of the round.
// It is a snapshot: it carries no heartbeat.
func TestTheStateCarriesTheStateOfTheRound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := activeRun(t, svc, "obs-1")
	started := r.roundStart("main", 1)
	r.mu.Lock()
	spent := r.roundUsage.Total()
	r.mu.Unlock()

	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}

	msgs := stateUnder(op, "om_status")
	if len(msgs) == 0 {
		t.Fatal("the run published no state")
	}
	state := readState(msgs)
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	if !strings.HasPrefix(state.phase, "running") {
		t.Errorf("the phase line is %q, want the phase of the run", state.phase)
	}
	if !state.tree {
		t.Error("the state carries no tree of the rounds")
	}
	if len(state.checklist) != 2 {
		t.Errorf("the checklist carries %d rows, want one per step of the round", len(state.checklist))
	}
	if state.heartbeats != 0 {
		t.Error("the state carries a heartbeat")
	}

	// The round is not started over: its start time and what it spent
	// stay what they were.
	if got := r.roundStart("main", 1); !got.Equal(started) {
		t.Errorf("the round started at %s, want %s", got, started)
	}
	r.mu.Lock()
	now := r.roundUsage.Total()
	r.mu.Unlock()
	if now != spent {
		t.Errorf("the round spent %d tokens, want %d", now, spent)
	}
}

// TestTheStateLandsOnTheMessageThatAskedForIt — the operator asked for
// the state where they are looking: the state lands on the card the
// chat already holds for the message of the command, and the run opens
// no card of its own.
func TestTheStateLandsOnTheMessageThatAskedForIt(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	cards := op.Posted()

	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if op.Posted() != cards {
		t.Fatalf("the run opened %d cards, want none", op.Posted()-cards)
	}
	state := readState(stateUnder(op, "om_status"))
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	if state.phase == "" {
		t.Error("the state carries no phase line")
	}
	if !state.tree {
		t.Error("the state carries no tree of the rounds")
	}
	if state.checklist == nil {
		t.Error("the state carries no checklist of the round")
	}
}

// TestTheStateTakesTheCardOfTheRoundOver — the card of the state is
// the latest card of the round: the run goes on with it, and the card
// it replaces keeps the content it had.
func TestTheStateTakesTheCardOfTheRoundOver(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}
	seen := op.EventsOf(round.anchor)

	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}

	got := r.roundSessionRef()
	if got == nil || got.anchor != "om_status" {
		t.Errorf("the round goes on with %v, want the message the operator asked from", got)
	}
	if got := op.EventsOf(round.anchor); got != seen {
		t.Errorf("the card of the round got %d events, want the %d it had", got, seen)
	}
}

// TestTheChecklistOfTheRoundFollowsTheStep — the checklist of the
// round is refreshed once per step: the card of the round shows what
// the step came to.
func TestTheChecklistOfTheRoundFollowsTheStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}

	// The step settles and the run ends.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitRun(t, svc, "obs-1")

	items := op.Checklist(round.anchor)
	if len(items) != 1 {
		t.Fatalf("the card of the round carries %d rows, want the step of the round", len(items))
	}
	if items[0].Subject != "commit" || items[0].Status != agent.TaskCompleted {
		t.Errorf("the card of the round does not show what the step came to: %+v", items[0])
	}
}

// TestAPausePublishesTheState — a pause stops the run and answers the
// operator on the card of the message they paused from: the answer
// comes first, the state of the round follows.
func TestAPausePublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	before := op.Posted()

	if err := svc.Pause(context.Background(), "obs-1", "om_pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if op.Posted() != before {
		t.Fatalf("the run opened %d cards, want none", op.Posted()-before)
	}
	entries := op.Entries("om_pause")
	if len(entries) == 0 {
		t.Fatal("the run published no answer")
	}
	if entries[0] != "⏸ The run pauses after the current step." {
		t.Errorf("the answer is %q, want the answer of the pause", entries[0])
	}
	if len(entries) < 2 || !strings.HasPrefix(entries[1], "paused: the operator paused the run") {
		t.Fatalf("the phase line is %v, want it to name the pause", entries)
	}
}

// TestAPauseOfAPausedRunPublishesTheState — every explicit pause shows
// the state again.
func TestAPauseOfAPausedRunPublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	if err := svc.Pause(context.Background(), "obs-1", "om_pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
	before := len(op.Emitted())

	if err := svc.Pause(context.Background(), "obs-1", "om_pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := len(op.Emitted()); got <= before {
		t.Fatalf("the run published %d messages, want the state again", got-before)
	}
}

// TestAResumePublishesTheState — the operator hands the run back to
// work: the answer and the state of the round are published on the
// message the operator asked from.
func TestAResumePublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		failedStep("the commit failed"),
		{open: true},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}

	if _, err := svc.Resume(context.Background(), "obs-1", "om_resume", ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	msgs := waitForStateUnder(t, op, "om_resume", 5)
	entries := op.Entries("om_resume")
	if len(entries) == 0 || entries[0] != "▶ The step runs again: commit." {
		t.Fatalf("the answer is %v, want the answer of the resume", entries)
	}
	state := readState(msgs)
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	if !strings.HasPrefix(state.phase, "running") {
		t.Errorf("the phase line is %q, want the phase of the run", state.phase)
	}
	if !state.tree {
		t.Error("the state carries no tree of the rounds")
	}
	if state.checklist == nil {
		t.Error("the state carries no checklist of the round")
	}
	// The card of the state is the card the run works on from now on.
	if got := r.roundSessionRef(); got == nil || got.anchor != "om_resume" {
		t.Errorf("the round goes on with %v, want the message the operator asked from", got)
	}
}

// TestAResumeThatDecidesNothingPublishesTheState — the command had
// nothing to decide: the answer names the step and the reason, and the
// state of the run is published all the same, on the message the
// operator asked from.
func TestAResumeThatDecidesNothingPublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{})
	pausedRun(t, svc, "commit", wfe.Outcome{Status: wfe.OutcomeOK, Summary: "committed"}, true)

	if _, err := svc.Resume(context.Background(), "obs-1", "om_resume", ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	msgs := waitForStateUnder(t, op, "om_resume", 3)
	entries := op.Entries("om_resume")
	if len(entries) == 0 || entries[0] != "▶ The step commit already succeeded — it was not run again." {
		t.Fatalf("the answer is %v, want the reason the step did not run again", entries)
	}
	state := readState(msgs)
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	// The run waits for the operator no more: it goes on.
	if !strings.HasPrefix(state.phase, "running") {
		t.Errorf("the phase line is %q, want the phase of the run", state.phase)
	}
}

// TestANextPublishesTheState — the operator skips the step the run is
// on: the answer and the state of the round are published on the
// message the operator asked from.
func TestANextPublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the commit failed",
			Data:    map[string]string{"error": "the commit failed", "reason": "conflict"},
		}},
		{open: true},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}

	if _, err := svc.Next(context.Background(), "obs-1", "om_next", ""); err != nil {
		t.Fatalf("next: %v", err)
	}
	msgs := waitForStateUnder(t, op, "om_next", 5)
	entries := op.Entries("om_next")
	if len(entries) == 0 || entries[0] != "⏭ The step is skipped: commit. The run goes on with the workflow." {
		t.Fatalf("the answer is %v, want the answer of the skip", entries)
	}
	state := readState(msgs)
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	if !strings.HasPrefix(state.phase, "running") {
		t.Errorf("the phase line is %q, want the phase of the run", state.phase)
	}
	if !state.tree {
		t.Error("the state carries no tree of the rounds")
	}
	if state.checklist == nil {
		t.Error("the state carries no checklist of the round")
	}
	// The card of the state is the card the run works on from now on.
	if got := r.roundSessionRef(); got == nil || got.anchor != "om_next" {
		t.Errorf("the round goes on with %v, want the message the operator asked from", got)
	}
}

// TestALateStateOfAPauseDoesNotTakeTheCardOfTheRound — the state a
// run opens for its pause may reach the chat after the operator has
// let the run go. The card it names is then a snapshot of a moment
// that has passed: it does not take the card of the round, which is
// the card the operator was answered on.
func TestALateStateOfAPauseDoesNotTakeTheCardOfTheRound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the commit failed",
			Data:    map[string]string{"error": "the commit failed", "reason": "conflict"},
		}},
		{open: true},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	// The operator lets the run go: the card of the round is the card
	// of their command.
	if _, err := svc.Next(context.Background(), "obs-1", "om_next", ""); err != nil {
		t.Fatalf("next: %v", err)
	}
	if got := r.roundSessionRef(); got == nil || got.anchor != "om_next" {
		t.Fatalf("the round goes on with %v, want the message the operator asked from", got)
	}

	// The pause publishes its state now: the run opened that card
	// before the operator's command and publishes it after.
	svc.showState(context.Background(), r)

	if got := r.roundSessionRef(); got == nil || got.anchor != "om_next" {
		t.Errorf("the late state took the card of the round: %v, want the message the operator asked from", got)
	}
}

// TestAResumeOfARunWithoutARoundPublishesThePhaseLine — a run that
// has not entered a round has no round state: only the phase line is
// published, on a card of its own.
func TestAResumeOfARunWithoutARoundPublishesThePhaseLine(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	r := testRun(t, svc, "one")
	t.Cleanup(r.cancel)
	svc.mu.Lock()
	svc.active["obs-1"] = r
	svc.mu.Unlock()
	r.pauseNow("the operator paused the run")

	if _, err := svc.Resume(context.Background(), "obs-1", "", ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if op.Posted() != 1 {
		t.Fatalf("the run opened %d cards, want the card of the state", op.Posted())
	}
	card, msgs := stateCard(op, "")
	if len(msgs) == 0 {
		t.Fatal("the run published no state")
	}
	state := readState(msgs)
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	if state.phase == "" {
		t.Error("the state carries no phase line")
	}
	if card.ID == "" {
		t.Error("the run opened no card for the state")
	}
}

// TestATakeoverPublishesTheState — a person who writes in the chat
// takes the chat back: the state of the run is published at once,
// under the card of the round.
func TestATakeoverPublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}
	before := op.Posted()

	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})
	card, _ := waitForStateCardWith(t, op, "paused: you took over", 4)
	if card.ReplyTo != round.anchor {
		t.Errorf("the card of the state hangs under %q, want the card of the round", card.ReplyTo)
	}
	if op.Posted() != before+1 {
		t.Errorf("the run opened %d cards, want the card of the state", op.Posted()-before)
	}
}

// TestATakeoverOfAPausedRunPublishesNoState — the takeover is not new
// when the run already waits for the operator.
func TestATakeoverOfAPausedRunPublishesNoState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	if err := svc.Pause(context.Background(), "obs-1", "om_pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before := len(op.Emitted())

	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})
	time.Sleep(100 * time.Millisecond)
	if got := len(op.Emitted()); got != before {
		t.Fatalf("the run published %d messages, want none", got-before)
	}
}

// TestTheBudgetPausePublishesTheState — the run stops because the step
// used up its budget: the state of the round is published under the
// card of the round.
func TestTheBudgetPausePublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 2})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}

	card, _ := waitForStateCardWith(t, op, `paused: the step "commit" used up its budget`, 4)
	if card.ReplyTo == "" {
		t.Error("the card of the state hangs under nothing")
	}
}

// TestTheRateLimitPausePublishesTheState — the run stops because the
// agent was throttled: the state of the round is published under the
// card of the round, and the chat is told that the run waits.
func TestTheRateLimitPausePublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "429 Too Many Requests",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}

	card, _ := waitForStateCardWith(t, op, "paused: the agent is rate limited (429)", 4)
	if card.ReplyTo == "" {
		t.Error("the card of the state hangs under nothing")
	}

	// The chat is told that the run waits: the state is a snapshot in a
	// card, and the chat does not push it.
	var notice string
	for _, msg := range op.Spoken() {
		if strings.HasPrefix(msg.Text, pauseMark) {
			notice = msg.Text
		}
	}
	if notice == "" {
		t.Fatalf("the chat was not told that the run waits: %v", op.Spoken())
	}
	if !strings.Contains(notice, rateLimitReason) {
		t.Errorf("the notice is %q, want it to name the rate limit", notice)
	}
	for _, want := range []string{"/me resume", "/me next"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice is %q, want it to mention %q", notice, want)
		}
	}
}

// TestARunWithoutARoundShowsThePhaseLine — a run that has not entered
// a round has no round state to show: only the phase line is
// published, on the card the operator asked from.
func TestARunWithoutARoundShowsThePhaseLine(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	r := testRun(t, svc, "one")
	svc.mu.Lock()
	svc.active["obs-1"] = r
	svc.mu.Unlock()

	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if op.Posted() != 0 {
		t.Fatalf("the run opened %d cards, want none", op.Posted())
	}
	msgs := stateUnder(op, "om_status")
	if len(msgs) == 0 {
		t.Fatal("the run published no state")
	}
	state := readState(msgs)
	if !state.identity {
		t.Error("the state carries no identity line")
	}
	if state.phase == "" {
		t.Error("the state carries no phase line")
	}
	if state.checklist != nil {
		t.Error("the state carries a checklist of a round that does not exist")
	}
	if state.tree {
		t.Error("the state carries the tree of the rounds")
	}
}

// TestTheStatusDoesNotReplaceTheCardOfTheRun — the state of a run
// that has not entered a round yet is the phase line alone: the plan
// of the run stays on the card the run started from.
func TestTheStatusDoesNotReplaceTheCardOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := activeRun(t, svc, "obs-1")
	// The run has not entered a round yet.
	r.setRoundSession(nil)
	before := r.runSessionRef()
	cards := op.Posted()

	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if op.Posted() != cards {
		t.Fatalf("the run opened %d cards, want none", op.Posted()-cards)
	}
	if got := r.runSessionRef(); got != before {
		t.Error("the state took the place of the card of the run")
	}
}

// TestTheStatusRequestOfAChatWithoutARunIsRefused — a chat without a
// run has no state to show.
func TestTheStatusRequestOfAChatWithoutARunIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, noModels())
	if err := svc.Status(context.Background(), "obs-1", "om_status"); err == nil {
		t.Fatal("a chat without a run showed a state")
	} else if err != ErrNoRun {
		t.Errorf("the failure is %v, want %v", err, ErrNoRun)
	}
}

// stateCardWith returns the card that carries the phase line that
// starts with the given text.
func stateCardWith(op *fakeOperator, prefix string) (cardRecord, []emittedMessage) {
	for _, c := range op.Cards() {
		msgs := stateUnder(op, c.ID)
		for _, m := range msgs {
			if m.Kind != messages.OutReply {
				continue
			}
			if strings.HasPrefix(m.Text, prefix) {
				return c, msgs
			}
		}
	}
	return cardRecord{}, nil
}

// waitForStateCardWith waits until the run published a card that
// carries the phase line that starts with the given text.
func waitForStateCardWith(t *testing.T, op *fakeOperator, prefix string, n int) (cardRecord, []emittedMessage) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		card, msgs := stateCardWith(op, prefix)
		if len(msgs) >= n {
			return card, msgs
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, c := range op.Cards() {
		t.Logf("CARD %s replyTo=%s body=%q", c.ID, c.ReplyTo, c.Body)
	}
	for _, m := range op.Emitted() {
		t.Logf("MSG kind=%v replyTo=%s sid=%s text=%.40q", m.Kind, m.ReplyTo, m.SessionID, m.Text)
	}
	t.Fatalf("the run published no card with the phase line %q", prefix)
	return cardRecord{}, nil
}

// theState is what the state of the run carries.
//
// The order of the messages is not what the test is about: a refresh
// of the checklist may come in while the state is published.
type theState struct {
	// identity reports the identity line of the session.
	identity bool
	// phase is the phase line of the run.
	phase string
	// tree reports the tree of the rounds.
	tree bool
	// checklist is the checklist of the round.
	checklist []agent.AgentTaskItem
	// heartbeats counts the heartbeats the state carries.
	heartbeats int
}

// readState reads the state of the run off the messages of a card.
func readState(msgs []emittedMessage) theState {
	var out theState
	for _, m := range msgs {
		switch {
		case m.Kind == messages.OutHeartbeat:
			out.heartbeats++
		case m.Kind == messages.OutInit:
			out.identity = true
		case m.Kind == messages.OutTaskUpdate && m.TaskList != nil:
			out.checklist = m.TaskList.Items
		case m.Kind == messages.OutReply && strings.HasPrefix(m.Text, "rounds:"):
			out.tree = true
		case m.Kind == messages.OutReply && isPhaseLine(m.Text):
			out.phase = m.Text
		}
	}
	return out
}

// stateCard returns the card the run opened under the given message
// and the outbound messages it carries.
//
// The card is the one that carries the phase line: the run opens a
// card for the state of the run, and the phase line says so.
func stateCard(op *fakeOperator, parent string) (cardRecord, []emittedMessage) {
	for _, c := range op.Cards() {
		if c.ReplyTo != parent {
			continue
		}
		msgs := stateUnder(op, c.ID)
		if !carriesPhaseLine(msgs) {
			continue
		}
		return c, msgs
	}
	return cardRecord{}, nil
}

// carriesPhaseLine reports whether the messages carry the phase line of
// the run: the state of the run is the only thing that carries it.
func carriesPhaseLine(msgs []emittedMessage) bool {
	for _, m := range msgs {
		if m.Kind != messages.OutReply {
			continue
		}
		if isPhaseLine(m.Text) {
			return true
		}
	}
	return false
}

// isPhaseLine reports whether the text is the phase line of the run.
//
// The answer of the operator's command is an entry of the same card,
// and it is not a phase line.
func isPhaseLine(text string) bool {
	return strings.HasPrefix(text, "running") ||
		strings.HasPrefix(text, "paused") ||
		strings.HasPrefix(text, "asking")
}

// stateUnder returns the outbound messages the run published under the
// given anchor.
func stateUnder(op *fakeOperator, anchor string) []emittedMessage {
	var out []emittedMessage
	for _, m := range op.Emitted() {
		if m.ReplyTo == anchor {
			out = append(out, m)
		}
	}
	return out
}

// waitForStateUnder waits until the run published that many messages
// under the given anchor.
func waitForStateUnder(t *testing.T, op *fakeOperator, anchor string, n int) []emittedMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msgs := stateUnder(op, anchor)
		if len(msgs) >= n {
			return msgs
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run published %d messages under %q, want %d", len(stateUnder(op, anchor)), anchor, n)
	return nil
}

// waitForStepState waits until the run records what came of the step.
func waitForStepState(t *testing.T, r *run, stepID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.stepState(stepID); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the step %q never ran", stepID)
}
