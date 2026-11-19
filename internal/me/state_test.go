package me

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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

	state := stateUnder(op, "om_status")
	if len(state) != 4 {
		t.Fatalf("the state carries %d messages, want the identity line, the phase line, the tree and the checklist", len(state))
	}
	if state[0].Kind != messages.OutInit {
		t.Errorf("the first message is %q, want the identity line", state[0].Kind)
	}
	if state[1].Kind != messages.OutReply || !strings.HasPrefix(state[1].Text, "running") {
		t.Errorf("the second message is %q (%q), want the phase line", state[1].Kind, state[1].Text)
	}
	if state[2].Kind != messages.OutReply || !strings.HasPrefix(state[2].Text, "rounds:") {
		t.Errorf("the third message is %q (%q), want the tree of the rounds", state[2].Kind, state[2].Text)
	}
	if state[3].Kind != messages.OutTaskUpdate || state[3].TaskList == nil {
		t.Fatalf("the fourth message is %q, want the checklist of the round", state[3].Kind)
	}
	if items := state[3].TaskList.Items; len(items) != 2 {
		t.Errorf("the checklist carries %d rows, want one per step of the round", len(items))
	}
	for _, m := range state {
		if m.Kind == messages.OutHeartbeat {
			t.Error("the state carries a heartbeat")
		}
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

// TestTheStateHangsUnderTheMessageThatAskedForIt — the operator asked
// for the state where they are looking: the state hangs under the
// message of the command.
func TestTheStateHangsUnderTheMessageThatAskedForIt(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	cards := op.Posted()

	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	state := stateUnder(op, "om_status")
	if len(state) == 0 {
		t.Fatal("the run published no state")
	}
	for _, m := range state {
		if m.ReplyTo != "om_status" {
			t.Errorf("the message hangs under %q, want om_status", m.ReplyTo)
		}
	}
	// The state posts no card of its own.
	if op.Posted() != cards {
		t.Errorf("the run published %d cards, want none", op.Posted()-cards)
	}
}

// TestTheStateTakesNoCardOver — the state is a snapshot: the round
// keeps the card it has, and that card receives nothing more.
func TestTheStateTakesNoCardOver(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
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

	if got := r.roundSessionRef(); got != round {
		t.Error("the state took the place of the card of the round")
	}
	if got := op.EventsOf(round.anchor); got != seen {
		t.Errorf("the card of the round got %d events, want the %d it had", got, seen)
	}
}

// TestTheStateIsNotPartOfTheRun — the state is a snapshot: the run does
// not keep it, and the state file of the run does not hold it.
func TestTheStateIsNotPartOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := activeRun(t, svc, "obs-1")
	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, sess := range r.ownedSessions() {
		if sess.anchor == "om_status" {
			t.Error("the run owns the session of the state")
		}
	}
	svc.persist(r)
	raw, err := os.ReadFile(filepath.Join(dir, "runs", runID+".json"))
	if err != nil {
		t.Fatalf("the state of the run is gone: %v", err)
	}
	if strings.Contains(string(raw), "om_status") {
		t.Errorf("the state of the run holds the session of the state: %s", raw)
	}
}

// TestTheRoundCardIsASnapshot — the card of the round is a snapshot: the
// checklist it carries is the one the round started with, and the card
// is not written again while the round runs.
func TestTheRoundCardIsASnapshot(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
	if items[0].Subject != "commit" || items[0].Status != agent.TaskPending {
		t.Errorf("the card of the round was written again: %+v", items[0])
	}
}

// TestAPausePublishesTheState — a pause stops the run and shows the
// state of the round under the message the operator paused from.
func TestAPausePublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
		t.Fatalf("the run published %d cards, want none", op.Posted()-before)
	}
	entries := op.Entries("om_pause")
	if len(entries) == 0 || !strings.HasPrefix(entries[0], "paused: the operator paused the run") {
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
// work: the state of the round is published under the message the
// operator asked from.
func TestAResumePublishesTheState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		failedStep("the commit failed"),
		{open: true},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
	state := waitForStateUnder(t, op, "om_resume", 4)
	if len(state) < 4 {
		t.Fatalf("the state carries %d messages, want the identity line, the phase line, the tree and the checklist", len(state))
	}
	if state[0].Kind != messages.OutInit {
		t.Errorf("the first message is %q, want the identity line", state[0].Kind)
	}
	if state[1].Kind != messages.OutReply || !strings.HasPrefix(state[1].Text, "running") {
		t.Errorf("the phase line is %q (%q), want the phase of the run", state[1].Kind, state[1].Text)
	}
	if state[2].Kind != messages.OutReply || !strings.HasPrefix(state[2].Text, "rounds:") {
		t.Errorf("the tree is %q (%q), want the tree of the rounds", state[2].Kind, state[2].Text)
	}
	// The card of the round is the card the run works on.
	if got := r.roundSessionRef(); got != round {
		t.Error("the state took the place of the card of the round")
	}
}

// TestANextPublishesTheState — the operator skips the step the run is
// on: the state of the round is published under the message the
// operator asked from.
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
	state := waitForStateUnder(t, op, "om_next", 4)
	if len(state) < 4 {
		t.Fatalf("the state carries %d messages, want the identity line, the phase line, the tree and the checklist", len(state))
	}
	if state[0].Kind != messages.OutInit {
		t.Errorf("the first message is %q, want the identity line", state[0].Kind)
	}
	if state[1].Kind != messages.OutReply || !strings.HasPrefix(state[1].Text, "running") {
		t.Errorf("the phase line is %q (%q), want the phase of the run", state[1].Kind, state[1].Text)
	}
	if state[2].Kind != messages.OutReply || !strings.HasPrefix(state[2].Text, "rounds:") {
		t.Errorf("the tree is %q (%q), want the tree of the rounds", state[2].Kind, state[2].Text)
	}
	// The card of the round keeps the checklist it started with.
	if got := r.roundSessionRef(); got != round {
		t.Error("the state took the place of the card of the round")
	}
}

// TestAResumeOfARunWithoutARoundPublishesThePhaseLine — a run that
// has not entered a round has no round state: only the phase line is
// published, and no card of its own.
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
	if op.Posted() != 0 {
		t.Fatalf("the run published %d cards, want none", op.Posted())
	}
	state := stateUnder(op, "")
	if len(state) != 2 {
		t.Fatalf("the state carries %d messages, want the identity line and the phase line", len(state))
	}
	if state[0].Kind != messages.OutInit {
		t.Errorf("the first message is %q, want the identity line", state[0].Kind)
	}
	if state[1].Kind != messages.OutReply {
		t.Errorf("the second message is %q, want the phase line", state[1].Kind)
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
	waitForStateUnder(t, op, round.anchor, 4)

	var takeover bool
	for _, entry := range op.Entries(round.anchor) {
		if strings.HasPrefix(entry, "paused: you took over") {
			takeover = true
		}
	}
	if !takeover {
		t.Fatalf("the phase line is %v, want it to name the takeover", op.Entries(round.anchor))
	}
	if op.Posted() != before {
		t.Errorf("the run published %d cards, want none", op.Posted()-before)
	}
}

// TestATakeoverOfAPausedRunPublishesNoState — the takeover is not new
// when the run already waits for the operator.
func TestATakeoverOfAPausedRunPublishesNoState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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

	waitForStateUnder(t, op, round.anchor, 4)
	var budget bool
	for _, entry := range op.Entries(round.anchor) {
		if strings.HasPrefix(entry, `paused: the step "commit" used up its budget`) {
			budget = true
		}
	}
	if !budget {
		t.Fatalf("the phase line is %v, want it to name the budget", op.Entries(round.anchor))
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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

	waitForStateUnder(t, op, round.anchor, 4)
	var limit bool
	for _, entry := range op.Entries(round.anchor) {
		if strings.HasPrefix(entry, "paused: the agent is rate limited (429)") {
			limit = true
		}
	}
	if !limit {
		t.Fatalf("the phase line is %v, want it to name the rate limit", op.Entries(round.anchor))
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
// published, and no card of its own.
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
		t.Fatalf("the run published %d cards, want none", op.Posted())
	}
	state := stateUnder(op, "om_status")
	if len(state) != 2 {
		t.Fatalf("the state carries %d messages, want the identity line and the phase line", len(state))
	}
	if state[0].Kind != messages.OutInit {
		t.Errorf("the first message is %q, want the identity line", state[0].Kind)
	}
	if state[1].Kind != messages.OutReply {
		t.Errorf("the second message is %q, want the phase line", state[1].Kind)
	}
	for _, m := range state {
		if m.Kind == messages.OutTaskUpdate {
			t.Error("the state carries a checklist of a round that does not exist")
		}
		if strings.HasPrefix(m.Text, "rounds:") {
			t.Error("the state carries the tree of the rounds")
		}
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

	if _, err := svc.StartWorkflow(context.Background(), Request{
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
		t.Fatalf("the run published %d cards, want none", op.Posted()-cards)
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
