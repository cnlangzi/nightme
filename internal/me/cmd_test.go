package me

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// cmdWorkflow is a workflow whose first step ends the round quietly.
//
// The step that follows it does not run: the round is over.
const cmdWorkflow = `title: cmd
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: skip
        message: "/gtw back"
      - id: commit
        message: "/gtw commit"
`

// TestStepKind_CmdSendsItsMessageAndEndsTheRound — the cmd step sends
// its message to the chat, and the round ends there: the step that
// follows it does not run.
func TestStepKind_CmdSendsItsMessageAndEndsTheRound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "cmd", cmdWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "ok"}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "cmd", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 1 || sent[0] != "/gtw back" {
		t.Fatalf("the chat got %v, want the message of the cmd step and nothing else", sent)
	}
}

// TestStepKind_CmdDoesNotRunWhenItsMessageFails — the message of the
// cmd step is the step's outcome: a message that failed is the step's
// failure, and the command does not run.
func TestStepKind_CmdDoesNotRunWhenItsMessageFails(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "cmd", cmdWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the command failed",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "cmd", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 1 || sent[0] != "/gtw back" {
		t.Fatalf("the chat got %v, want the message of the cmd step", sent)
	}
	// The round was not ended: the card of the round says how it came
	// to.
	if !cardCarries(t, op, "ended:") {
		t.Error("the card of the round does not say how the round came to")
	}
}

// TestTheCardOfARoundThatEndedQuietlyCarriesOnlyItsVerdict — a round
// that ended quietly says nothing else: the card keeps the tree and the
// checklist it opened with.
func TestTheCardOfARoundThatEndedQuietlyCarriesOnlyItsVerdict(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "one", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)

	r.setRoundSession(&session{kind: sessionRound, id: "main · round 1", job: "main", round: 1, anchor: "card-1"})
	r.state.SkippedRounds = []wfe.SkippedRound{{Job: "main", Round: 1}}
	svc.closeRoundSession(context.Background(), r)

	for _, entry := range op.Entries("card-1") {
		if strings.Contains(entry, "ended:") {
			t.Errorf("the card of the round carries %q, want no ending line", entry)
		}
	}
}

// TestTheRecordLeavesOutTheRoundThatEndedQuietly — a round that ended
// quietly leaves nothing behind: no row in the tree, no error, no
// decision and no count.
func TestTheRecordLeavesOutTheRoundThatEndedQuietly(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})

	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow did not load")
	}
	r := &run{
		svc:      svc,
		id:       "one-1",
		chatID:   "obs-1",
		workflow: wf,
		policy:   wfe.NewPolicy(wf, nil),
		state:    newRunState("one-1", wf.Name, "/tmp/work", nil, time.Now()),
		phase:    PhaseStopped,
	}

	at := time.Date(2026, 11, 24, 10, 33, 4, 0, time.UTC)
	r.state.Rounds = map[string]int{"main": 1}
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", outcome.Outcome{Success: true, Status: http.StatusOK})
	r.closeRound("main", 1, at.Add(time.Minute))

	r.state.Rounds = map[string]int{"main": 2}
	r.record(wfe.ActionKindRun, "/gtw fix", "commit", outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the round broke",
	})
	r.state.SkippedRounds = []wfe.SkippedRound{{Job: "main", Round: 2}}
	r.closeRound("main", 2, at.Add(2*time.Minute))

	r.wakes = []wakeRecord{
		{Who: decisionDecider, What: "the first round went on", Job: "main", Round: 1},
		{Who: decisionDecider, What: "the second round went on", Job: "main", Round: 2},
	}

	body := r.recordCard()
	// The tree of the rounds leaves the quiet round out.
	if strings.Contains(body, "[ 2 ]") {
		t.Errorf("the record lists the round that ended quietly:\n%s", body)
	}
	if !strings.Contains(body, "[ 1 ]") {
		t.Errorf("the record does not list the round that worked:\n%s", body)
	}
	// The errors of the quiet round are not errors of the run.
	if strings.Contains(body, "errors:") {
		t.Errorf("the record lists the errors of the quiet round:\n%s", body)
	}
	// The decision of the quiet round is not a decision of the run.
	if strings.Contains(body, "the second round went on") {
		t.Errorf("the record lists the decision of the quiet round:\n%s", body)
	}
	if !strings.Contains(body, "the first round went on") {
		t.Errorf("the record does not list the decision of the round that worked:\n%s", body)
	}
	// The counts do not count the actions of the quiet round.
	if !strings.Contains(body, "actions:  1 (1 ok)") {
		t.Errorf("the record counts the actions of the quiet round:\n%s", body)
	}
	if !strings.Contains(body, "steps:    1 ran · 0 not run") {
		t.Errorf("the record counts the steps of the quiet round:\n%s", body)
	}
}

// waitPending waits until the step of the run waits for an answer.
func waitPending(t *testing.T, svc *Service, chatID, stepID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		r := svc.active[chatID]
		svc.mu.Unlock()
		if r != nil {
			if st, ok := r.stepState(stepID); ok && st.Pending {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the step %q did not wait for an answer", stepID)
}

// cardCarries reports whether the chat saw the given text under any
// card of the run.
func cardCarries(t *testing.T, op *fakeOperator, text string) bool {
	t.Helper()
	for _, card := range op.Cards() {
		for _, entry := range op.Entries(card.ID) {
			if strings.Contains(entry, text) {
				return true
			}
		}
	}
	return false
}

// TestTheCmdStepRunsItsCommandWhenItSettlesLate — a step whose
// message waits for an answer settles later: the command runs then,
// and the round ends quietly.
func TestTheCmdStepRunsItsCommandWhenItSettlesLate(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "cmd", cmdWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("test", "/gtw back", 1, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The chat asks something: the command waits for the answer.
	op.steps = []fakeStep{{open: true, choices: []messages.Choice{{Title: "which one?"}}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "cmd", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw back")
	// The step waits for the answer: the run holds it as pending.
	waitPending(t, svc, "obs-1", "quiet")
	// The operator answers: the command settles.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("answer: %v", err)
	}
	waitRun(t, svc, "obs-1")

	for _, cmd := range op.Sent() {
		if cmd == "/gtw commit" {
			t.Errorf("the chat got %q, want the round to end at the cmd step", cmd)
		}
	}
}
