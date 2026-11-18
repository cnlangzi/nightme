package me

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestTheStateCardCarriesTheStateOfTheRound — the state card carries
// the phase line, the tree of the rounds, the checklist of the round
// and the heartbeat.
func TestTheStateCardCarriesTheStateOfTheRound(t *testing.T) {
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

	if err := svc.Status(context.Background(), "obs-1"); err != nil {
		t.Fatalf("status: %v", err)
	}

	state := waitForStateCard(t, op)
	entries := op.Entries(state.ID)
	if len(entries) < 2 {
		t.Fatalf("the state card carries %d entries, want the phase line and the tree", len(entries))
	}
	if !strings.HasPrefix(entries[0], "running") {
		t.Errorf("the first entry is %q, want the phase line", entries[0])
	}
	if !strings.HasPrefix(entries[1], "rounds:") {
		t.Errorf("the second entry is %q, want the tree of the rounds", entries[1])
	}
	items := op.Checklist(state.ID)
	if len(items) != 2 {
		t.Fatalf("the state card carries %d rows, want one per step of the round", len(items))
	}
	if !heartbeatOn(op, state.ID) {
		t.Error("the state card carries no heartbeat")
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

// TestTheStateCardHangsAtTheTopOfTheChat — the operator asked for the
// state where they are looking.
func TestTheStateCardHangsAtTheTopOfTheChat(t *testing.T) {
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

	if err := svc.Status(context.Background(), "obs-1"); err != nil {
		t.Fatalf("status: %v", err)
	}
	state := waitForStateCard(t, op)
	if state.ReplyTo != "" {
		t.Errorf("the state card hangs under %q, want the top of the chat", state.ReplyTo)
	}
}

// TestTheStateCardBecomesTheLiveCardOfTheRound — the events of the run
// are anchored to the state card from then on, and the card the round
// had receives nothing more.
func TestTheStateCardBecomesTheLiveCardOfTheRound(t *testing.T) {
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

	round := lastPostedCard(op)

	if err := svc.Status(context.Background(), "obs-1"); err != nil {
		t.Fatalf("status: %v", err)
	}
	state := waitForStateCard(t, op)
	// The state card is the live card of the round from here on: the
	// card the round had receives nothing more.
	seen := op.EventsOf(round.ID)
	before := op.EventsOf(state.ID)

	// The step settles and the run goes on: the state card is the card
	// the run works on.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForCardEvent(t, op, state.ID, before)

	if got := op.EventsOf(round.ID); got != seen {
		t.Errorf("the card of the round got %d events, want the %d it had", got, seen)
	}
}

// TestAPausePostsTheStateCard — a pause stops the run and shows the
// state of the round.
func TestAPausePostsTheStateCard(t *testing.T) {
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

	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if op.Posted() != before+1 {
		t.Fatalf("the run published %d cards, want one more", op.Posted())
	}
	state := waitForStateCard(t, op)
	entries := op.Entries(state.ID)
	if len(entries) == 0 || !strings.HasPrefix(entries[0], "paused: the operator paused the run") {
		t.Fatalf("the phase line is %v, want it to name the pause", entries)
	}
}

// TestAPauseOfAPausedRunPostsTheStateCard — every explicit pause shows
// the state again.
func TestAPauseOfAPausedRunPostsTheStateCard(t *testing.T) {
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
	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
	before := op.Posted()

	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if op.Posted() != before+1 {
		t.Fatalf("the run published %d cards, want one more", op.Posted())
	}
}

// TestATakeoverPostsTheStateCard — a person who writes in the chat
// takes the chat back: the state of the run is posted at once.
func TestATakeoverPostsTheStateCard(t *testing.T) {
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

	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})
	waitForCards(t, op, before+1)

	state := waitForStateCard(t, op)
	entries := op.Entries(state.ID)
	if len(entries) == 0 || !strings.HasPrefix(entries[0], "paused: you took over") {
		t.Fatalf("the phase line is %v, want it to name the takeover", entries)
	}
}

// TestATakeoverOfAPausedRunPostsNoCard — the takeover is not new when
// the run already waits for the operator.
func TestATakeoverOfAPausedRunPostsNoCard(t *testing.T) {
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
	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before := op.Posted()

	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})
	time.Sleep(100 * time.Millisecond)
	if op.Posted() != before {
		t.Fatalf("the run published %d cards, want none", op.Posted()-before)
	}
}

// TestTheBudgetPausePostsTheStateCard — the run stops because the step
// used up its budget: the state of the round is posted.
func TestTheBudgetPausePostsTheStateCard(t *testing.T) {
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

	state := waitForStateCard(t, op)
	entries := op.Entries(state.ID)
	if len(entries) == 0 || !strings.HasPrefix(entries[0], "paused: the step \"commit\" used up its budget") {
		t.Fatalf("the phase line is %v, want it to name the budget", entries)
	}
}

// TestTheRateLimitPausePostsTheStateCard — the run stops because the
// agent was throttled: the state of the round is posted.
func TestTheRateLimitPausePostsTheStateCard(t *testing.T) {
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

	state := waitForStateCard(t, op)
	entries := op.Entries(state.ID)
	if len(entries) == 0 || !strings.HasPrefix(entries[0], "paused: the agent is rate limited (429)") {
		t.Fatalf("the phase line is %v, want it to name the rate limit", entries)
	}
}

// TestARunWithoutARoundShowsTheOverview — a run that has not entered a
// round has no round state to show: the overview of the run is posted
// instead.
func TestARunWithoutARoundShowsTheOverview(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	r := testRun(t, svc, "one")

	if !svc.showState(context.Background(), r) {
		t.Fatal("the run published no card")
	}
	cards := op.Cards()
	if len(cards) != 1 {
		t.Fatalf("the run published %d cards, want the overview alone", len(cards))
	}
	entries := op.Entries(cards[0].ID)
	if len(entries) != 1 || !strings.Contains(entries[0], "started") {
		t.Fatalf("the card carries %v, want the overview of the run", entries)
	}
}

// TestTheStateCardDoesNotTakeOverARoundTheRunLeft — the run moves
// between rounds on its own goroutine: a card posted for a round the
// run has left does not become the card the run works on.
func TestTheStateCardDoesNotTakeOverARoundTheRunLeft(t *testing.T) {
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
	// The run leaves the round while the card is posted.
	op.onCard = func() {
		r.setRoundSession(&session{kind: sessionRound, id: "main · round 2", job: "main", round: 2})
	}

	if err := svc.Status(context.Background(), "obs-1"); err != nil {
		t.Fatalf("status: %v", err)
	}

	sess := r.roundSessionRef()
	if sess == nil || sess.job != "main" || sess.round != 2 {
		t.Fatalf("the live card is %v, want the card of the round the run is in", sess)
	}
}

// TestTheOverviewDoesNotReplaceTheCardOfTheRun — a run that has not
// entered a round yet shows the overview of the run as a card of its
// own: the card the run started from keeps the verdict of the run.
func TestTheOverviewDoesNotReplaceTheCardOfTheRun(t *testing.T) {
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

	if err := svc.Status(context.Background(), "obs-1"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if op.Posted() != cards+1 {
		t.Fatalf("the run published %d cards, want one more", op.Posted())
	}
	if got := r.runSessionRef(); got != before {
		t.Error("the overview took the place of the card of the run")
	}
}

// TestTheStatusRequestOfAChatWithoutARunIsRefused — a chat without a
// run has no state to show.
func TestTheStatusRequestOfAChatWithoutARunIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, noModels())
	if err := svc.Status(context.Background(), "obs-1"); err == nil {
		t.Fatal("a chat without a run showed a state")
	} else if err != ErrNoRun {
		t.Errorf("the failure is %v, want %v", err, ErrNoRun)
	}
}

// heartbeatOn reports whether the run published a heartbeat for the
// card with the given id.
func heartbeatOn(op *fakeOperator, id string) bool {
	for _, m := range op.EmittedOf(messages.OutHeartbeat) {
		if m.ReplyTo == id {
			return true
		}
	}
	return false
}

// waitForCards waits until the run published that many cards.
func waitForCards(t *testing.T, op *fakeOperator, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op.Posted() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run published %d cards, want %d", op.Posted(), n)
}

// waitForStateCard waits until the run posts the state card of the
// round and returns it.
//
// The state card is the one whose body names the state of the run, and
// it is returned once its phase line has landed.
func waitForStateCard(t *testing.T, op *fakeOperator) cardRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cards := op.Cards()
		for i := len(cards) - 1; i >= 0; i-- {
			if !strings.Contains(cards[i].Body, "the state of the run") {
				continue
			}
			if len(op.Entries(cards[i].ID)) > 0 {
				return cards[i]
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run posted no state card: %v", op.Cards())
	return cardRecord{}
}

// waitForCardEvent waits until the run publishes a new event under the
// card with the given id.
func waitForCardEvent(t *testing.T, op *fakeOperator, id string, seen int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op.EventsOf(id) > seen {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run never published an event under the card %q", id)
}
