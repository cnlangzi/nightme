package me

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestTheAnswerComesBeforeTheState — the answer of the operator's
// command is the first entry of the card: the operator reads what their
// command did before the state it left behind.
func TestTheAnswerComesBeforeTheState(t *testing.T) {
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

	if err := svc.Pause(context.Background(), "obs-1", "om_pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The command leaves no card of its own behind.
	if op.Posted() != cards {
		t.Fatalf("the run opened %d cards, want none", op.Posted()-cards)
	}
	entries := op.Entries("om_pause")
	if len(entries) < 3 {
		t.Fatalf("the card carries %d entries, want the answer and the state", len(entries))
	}
	if entries[0] != pauseAnswer {
		t.Errorf("the first entry is %q, want the answer of the pause", entries[0])
	}
	if !isPhaseLine(entries[1]) {
		t.Errorf("the second entry is %q, want the phase line", entries[1])
	}
	if !strings.HasPrefix(entries[2], "rounds:") {
		t.Errorf("the third entry is %q, want the tree of the rounds", entries[2])
	}
}

// TestTheChecklistFollowsTheAnswerOnTheSameCard — the card of the
// command is the live surface of the run: the checklist of the round is
// refreshed on it, and the answer stays the first entry.
func TestTheChecklistFollowsTheAnswerOnTheSameCard(t *testing.T) {
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
	// The step ends: the run pauses, and the checklist of the round is
	// refreshed on the card of the command.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	items := waitForChecklist(t, op, "om_pause", 1)
	if items[0].Subject != "commit" {
		t.Errorf("the row is %q, want the step of the round", items[0].Subject)
	}
	entries := op.Entries("om_pause")
	if len(entries) == 0 || entries[0] != pauseAnswer {
		t.Errorf("the first entry is %v, want the answer of the pause", entries)
	}
}

// TestTheStatusLeavesNoCardOfItsOwn — the state of the run lands on the
// card the chat already holds for the message of the command: the
// command leaves no card of its own behind.
func TestTheStatusLeavesNoCardOfItsOwn(t *testing.T) {
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
	// The state is the answer of the command: it is the first entry of
	// the card.
	entries := op.Entries("om_status")
	if len(entries) == 0 || !isPhaseLine(entries[0]) {
		t.Errorf("the first entry is %v, want the phase line", entries)
	}
}

// TestTheAnswersOfTheCommands — the sentences the operator reads are
// the ones the command used to send.
func TestTheAnswersOfTheCommands(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "the pause",
			got:  pauseAnswer,
			want: "⏸ The run pauses after the current step.",
		},
		{
			name: "the resume",
			got:  resumeAnswer(OperatorDecision{Step: "commit", Act: true}),
			want: "▶ The step runs again: commit.",
		},
		{
			name: "the resume that decided nothing",
			got:  resumeAnswer(OperatorDecision{Step: "commit", Why: reasonSucceeded}),
			want: "▶ The step commit already succeeded — it was not run again.",
		},
		{
			name: "the next",
			got:  nextAnswer(OperatorDecision{Step: "commit", Act: true}),
			want: "⏭ The step is skipped: commit. The run goes on with the workflow.",
		},
		{
			name: "the next that decided nothing",
			got:  nextAnswer(OperatorDecision{Step: "commit", Why: reasonSucceeded}),
			want: "⏭ The step commit already succeeded — there was nothing to skip.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("the answer is %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// TestTheAnswerOfTheDecisionFollowsTheCommand — the answer of `/me
// next` says the step is skipped, and the answer of `/me resume` says
// it runs again.
func TestTheAnswerOfTheDecisionFollowsTheCommand(t *testing.T) {
	d := OperatorDecision{Step: "commit", Act: true}
	if got := decisionAnswer(directiveResume, d); got != resumeAnswer(d) {
		t.Errorf("the answer is %q, want the answer of the resume", got)
	}
	if got := decisionAnswer(directiveNext, d); got != nextAnswer(d) {
		t.Errorf("the answer is %q, want the answer of the next", got)
	}
}

// waitForChecklist waits until the card carries the checklist of the
// round.
func waitForChecklist(t *testing.T, op *fakeOperator, anchor string, n int) []agent.AgentTaskItem {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if items := op.Checklist(anchor); len(items) >= n {
			return items
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the card %q carries no checklist", anchor)
	return nil
}
