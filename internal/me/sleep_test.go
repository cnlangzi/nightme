package me

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// sleepingWorkflow is a workflow whose job waits between its rounds.
const sleepingWorkflow = `title: Sleeps between the rounds.
jobs:
  main:
    iterations: 2
    sleep: 200ms
    steps:
      - id: fix
        message: /gtw fix
`

// TestTheRunWaitsTheGapBetweenTwoRounds — the run waits the gap the
// job names before the next round runs.
func TestTheRunWaitsTheGapBetweenTwoRounds(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sleep", sleepingWorkflow)

	svc, _ := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sleep", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := chatRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	history := runHistory(r)
	if len(history) != 2 {
		t.Fatalf("the run holds %d actions, want one per round: %+v", len(history), history)
	}
	gap := history[1].At.Sub(history[0].At)
	if gap < 200*time.Millisecond {
		t.Errorf("the second round ran %v after the first, want the gap of 200ms", gap)
	}
}

// TestStopEndsTheGapAtOnce — the gap belongs to the run: `/me stop`
// ends it at once.
func TestStopEndsTheGapAtOnce(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sleep", `title: Sleeps between the rounds.
jobs:
  main:
    iterations: 2
    sleep: 1h
    steps:
      - id: fix
        message: /gtw fix
`)

	svc, op := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sleep", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw fix")

	// The run waits the gap: the next round does not start.
	time.Sleep(100 * time.Millisecond)
	if sent := op.Sent(); len(sent) != 1 {
		t.Fatalf("the chat received %v, want only the command of the first round", sent)
	}

	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var found bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.Result == string(PhaseStopped) {
			found = true
		}
	}
	if !found {
		t.Errorf("the run published no verdict of its phase: %v", op.EmittedOf(messages.OutResult))
	}
}

// TestTheDaemonShutdownEndsTheGapAtOnce — the daemon is going away:
// the wait ends at once and the run stops where it stands.
func TestTheDaemonShutdownEndsTheGapAtOnce(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sleep", `title: Sleeps between the rounds.
jobs:
  main:
    iterations: 2
    sleep: 1h
    steps:
      - id: fix
        message: /gtw fix
`)

	svc, op := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sleep", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw fix")
	r := chatRun(t, svc, "obs-1")

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitDone(t, r)

	if sent := op.Sent(); len(sent) != 1 {
		t.Errorf("the chat received %v, want only the command of the first round", sent)
	}
}

// TestTheGapIsSilent — the gap is not a step: it carries no row on the
// card of the round, and the chat hears nothing about it.
func TestTheGapIsSilent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sleep", sleepingWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sleep", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The chat heard the two rounds and nothing else.
	for _, m := range op.Spoken() {
		if m.Text != "/gtw fix" {
			t.Errorf("the chat heard %q, want only the commands of the rounds", m.Text)
		}
	}
	// The card of the round carries the steps of the round and nothing
	// else.
	for _, card := range op.Cards() {
		for _, item := range op.Checklist(card.ID) {
			if item.Subject != "fix" {
				t.Errorf("the card of the round carries %q, want only the steps of the round", item.Subject)
			}
		}
	}
}

// TestTheRunEndsWhenTheRoundsAreSpent — a job that repeats knows how
// many rounds it runs: when they are spent, the run ends on its own
// and no model is asked what comes next.
func TestTheRunEndsWhenTheRoundsAreSpent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repeat", `title: Repeats.
jobs:
  main:
    iterations: 2
    steps:
      - id: fix
        message: /gtw fix
`)

	// The models must not be called: the run ends on its own.
	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{
		{outcome: outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "there was nothing to fix"}},
		{outcome: outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "there was nothing to fix"}},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repeat", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var found bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.Result == "repeat is complete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the run published no verdict of its outcome: %v", op.EmittedOf(messages.OutResult))
	}
}
