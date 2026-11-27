package me

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// repeatingWorkflow is a workflow whose job runs two rounds.
const repeatingWorkflow = `title: repeat
jobs:
  main:
    iterations: 2
    steps:
      - id: fix
        message: /gtw fix
`

// runRepeating runs a workflow whose job repeats until its rounds are
// spent.
//
// The run ends on its own: no model is asked what comes next.
func runRepeating(t *testing.T) (*Service, *fakeOperator, *run) {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "repeat", repeatingWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repeat", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := chatRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")
	waitDone(t, r)
	return svc, op, r
}

// TestTheRunThatSpentItsRoundsIsOver — a run that runs out of rounds
// ends on its own, and says so: it is over, not complete.
func TestTheRunThatSpentItsRoundsIsOver(t *testing.T) {
	_, op, _ := runRepeating(t)

	verdict := runVerdict(t, op)
	if verdict != "repeat is over" {
		t.Errorf("the verdict of the run is %q, want %q", verdict, "repeat is over")
	}
	for _, m := range op.Emitted() {
		if strings.Contains(m.Text, "is complete") {
			t.Errorf("the run says %q, want no claim that it is complete", m.Text)
		}
	}
}

// TestTheRunOfAJobThatRunsOneRoundIsComplete — a workflow without a
// limit ends the way it always did: it is complete.
func TestTheRunOfAJobThatRunsOneRoundIsComplete(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	verdict := runVerdict(t, op)
	if verdict != "one is complete" {
		t.Errorf("the verdict of the run is %q, want %q", verdict, "one is complete")
	}
}

// runVerdict returns the conclusion the run put in the footer of its
// own card.
func runVerdict(t *testing.T, op *fakeOperator) string {
	t.Helper()
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.ReplyTo == "om_start" {
			return m.Result
		}
	}
	t.Fatalf("the run published no verdict on its own card: %v", op.EmittedOf(messages.OutResult))
	return ""
}

// TestTheRecordOfTheSpentRoundsOpensACardOfItsOwn — the run ended on
// its own: the record of it opens a card of its own, and the operator
// reads it in the chat.
func TestTheRecordOfTheSpentRoundsOpensACardOfItsOwn(t *testing.T) {
	_, op, _ := runRepeating(t)

	record, ok := recordMessage(t, op)
	if !ok {
		t.Fatal("the run published no record")
	}
	if record.ReplyTo != "" {
		t.Errorf("the record hangs under %q, want a card of its own", record.ReplyTo)
	}
	// The card the chat opened for the record carries the identity of
	// the record.
	card := ""
	for _, c := range op.Cards() {
		if strings.HasPrefix(c.Body, "■ [") {
			card = c.ID
		}
	}
	if card == "" {
		t.Fatalf("the chat opened no card for the record: %v", op.Cards())
	}
	// The identity of the record lands on the card the chat opened
	// for it, right after the record itself.
	emitted := op.Emitted()
	at := -1
	for i, m := range emitted {
		if m.Kind == messages.OutReply && strings.HasPrefix(m.Text, "■ [") {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("the run published no record: %v", emitted)
	}
	if at+1 >= len(emitted) {
		t.Fatalf("the record was the last thing the run published: %v", emitted)
	}
	next := emitted[at+1]
	if next.Kind != messages.OutInit || next.ReplyTo != card {
		t.Errorf("the message after the record is %+v, want the identity of the record on the card %q", next, card)
	}
	// The identity lands last on the card, so it carries the verdict:
	// a chat stamps the footer of a card as a whole.
	if next.Result != "repeat is over" {
		t.Errorf("the identity of the record carries %q, want the verdict of the run", next.Result)
	}
}

// TestTheRecordOfTheSpentRoundsNamesTheLoop — the record says which
// loop ran out of rounds, and how many rounds it ran.
func TestTheRecordOfTheSpentRoundsNamesTheLoop(t *testing.T) {
	_, op, _ := runRepeating(t)

	body := recordEntry(t, op)
	lines := strings.Split(body, "\n")
	if lines[0] != "■ [ repeat ] done" {
		t.Errorf("the headline of the record is %q, want the phase of the run", lines[0])
	}
	want := `reason: the loop of "main" is done (2 of 2 rounds)`
	if lines[1] != want {
		t.Errorf("the second line of the record is %q, want %q", lines[1], want)
	}
}

// TestTheSpentRoundsAreNoError — a run that stopped on the limit of
// its rounds did not fail: it has no error.
func TestTheSpentRoundsAreNoError(t *testing.T) {
	_, _, r := runRepeating(t)

	if st := r.snapshot(); st.Error != "" {
		t.Errorf("the run holds the error %q, want none", st.Error)
	}
}

// TestAFailedRunHoldsItsError — a run that failed says why: the error
// is the reason of the failure.
func TestAFailedRunHoldsItsError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{
		Provider: &scriptedProvider{err: errors.New("the chat model must not be called")},
		Decider:  &scriptedDecider{err: errors.New("the decision model must not be called")},
	})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := chatRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")
	waitDone(t, r)

	if st := r.snapshot(); st.Phase != PhaseFailed {
		t.Fatalf("the run is %q, want it failed", st.Phase)
	}
	if st := r.snapshot(); st.Error == "" {
		t.Error("the failed run holds no error")
	}
}

// TestAFailedRunKeepsItsCard — a run that failed leaves its record on
// the card of the run.
func TestAFailedRunKeepsItsCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	record, ok := recordMessage(t, op)
	if !ok {
		t.Fatal("the run published no record")
	}
	if record.ReplyTo != "om_start" {
		t.Errorf("the record hangs under %q, want the card of the start", record.ReplyTo)
	}
	if !strings.HasPrefix(record.Text, "■ [ one ] failed") {
		t.Errorf("the headline of the record is %q, want the phase of the run", record.Text)
	}
}

// recordMessage returns the message the record of the run carries.
func recordMessage(t *testing.T, op *fakeOperator) (emittedMessage, bool) {
	t.Helper()
	for _, m := range op.EmittedOf(messages.OutReply) {
		if strings.HasPrefix(m.Text, "■ [") {
			return m, true
		}
	}
	return emittedMessage{}, false
}
