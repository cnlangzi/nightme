// Tests for the sessions the run speaks in.
//
// Every session owns one card: the placeholder the run posted when the
// session opened. The card's message id is the anchor of every event
// the session sends, so the chat renders the session onto that card
// and patches it in place.

package me

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
)

// sessionService builds a service with the one-step workflow in it.
func sessionService(t *testing.T) (*Service, *fakeOperator) {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	return testService(t, dir, noModels())
}

// TestTheRunCardHangsUnderTheMessageThatStartedTheRun — the run's card
// answers the message the run was started with.
func TestTheRunCardHangsUnderTheMessageThatStartedTheRun(t *testing.T) {
	svc, op := sessionService(t)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run published no card")
	}
	if cards[0].ReplyTo != "om_start" {
		t.Errorf("the run card hangs under %q, want om_start", cards[0].ReplyTo)
	}
}

// TestTheRunCardCarriesTheJobsOfTheWorkflow — the checklist of the run
// is the workflow's jobs, one row per job.
func TestTheRunCardCarriesTheJobsOfTheWorkflow(t *testing.T) {
	svc, op := sessionService(t)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	tasks := op.EmittedOf(messages.OutTaskUpdate)
	if len(tasks) == 0 {
		t.Fatalf("the run published no checklist: %v", op.Emitted())
	}
	items := tasks[0].TaskList.Items
	if len(items) != 1 {
		t.Fatalf("the checklist holds %d rows, want one per job", len(items))
	}
	if items[0].ID != "main" {
		t.Errorf("the row is %q, want the job", items[0].ID)
	}
}

// TestTheRunCardHasNoButtons — the operator drives the run with the
// /me commands, not with buttons on a card.
func TestTheRunCardHasNoButtons(t *testing.T) {
	svc, op := sessionService(t)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	for _, card := range op.Cards() {
		if card.Title != "" {
			t.Errorf("the card %q carries a title, want the session's own header", card.Title)
		}
	}
}

// TestEveryEventOfASessionCarriesItsCard — the card of a session is
// the anchor of its events: the chat renders the session onto that
// card.
func TestEveryEventOfASessionCarriesItsCard(t *testing.T) {
	svc, op := sessionService(t)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := map[string]bool{}
	for _, c := range op.Cards() {
		cards[c.ID] = true
	}
	emitted := op.Emitted()
	if len(emitted) == 0 {
		t.Fatal("the run published no event")
	}
	for _, m := range emitted {
		if !cards[m.ReplyTo] {
			t.Fatalf("the event hangs under %q, want the card of its session", m.ReplyTo)
		}
	}
}

// TestTheRoundCardHangsUnderTheRunCard — the round is the run's work,
// one level below it.
func TestTheRoundCardHangsUnderTheRunCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) < 2 {
		t.Fatalf("the run published %d cards, want the run card and the round card", len(cards))
	}
	if cards[1].ReplyTo != cards[0].ID {
		t.Errorf("the round card hangs under %q, want the run card %q", cards[1].ReplyTo, cards[0].ID)
	}
}

// TestTheChoiceIdsLineUpWithTheAnswers — a click on an option is the
// answer of that option: the ids of the choice and the numbers of a
// typed reply name the same list.
func TestTheChoiceIdsLineUpWithTheAnswers(t *testing.T) {
	ask := &askState{Options: []llm.Option{
		{Value: optionEscalate},
		{Value: "/gtw commit"},
		{Value: "/gtw push"},
	}}
	options := choiceOptions(ask.Options)
	if len(options) != 2 {
		t.Fatalf("the choice holds %d options, want 2", len(options))
	}
	for i, option := range options {
		want := strconv.Itoa(i)
		if option.ID != want {
			t.Errorf("the option %q has the id %q, want %q", option.Label, option.ID, want)
		}
		if got := resolveChoice(ask, want); got != option.Label {
			t.Errorf("the reply %q resolves to %q, want %q", want, got, option.Label)
		}
	}
}

// TestTheRoundCardCarriesTheStepsOfTheRound — the checklist of a round
// is the round's steps.
func TestTheRoundCardCarriesTheStepsOfTheRound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	round := ""
	for _, m := range op.Emitted() {
		if m.Kind == messages.OutTaskUpdate && m.TaskList != nil && len(m.TaskList.Items) > 0 {
			round = m.TaskList.Items[0].Subject
		}
	}
	if !strings.Contains(round, "/gtw commit") {
		t.Errorf("the checklist of the round is %q, want the step of the round", round)
	}
}
