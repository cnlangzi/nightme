// Tests for the sessions the run speaks in.
//
// Every session owns one card: the placeholder the run posted when the
// session opened. The card's message id is the anchor of every event
// the session sends, so the chat renders the session onto that card
// and patches it in place.

package me

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// sessionService builds a service with the one-step workflow in it.
func sessionService(t *testing.T) (*Service, *fakeOperator) {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	return testService(t, dir, noModels())
}

// TestTheRunStartLeavesOneCard — the run writes into the card the
// chat already holds for the message that started it: the plan is a
// reply under that message, and the verdict lands on the same card.
func TestTheRunStartLeavesOneCard(t *testing.T) {
	svc, op := sessionService(t)

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The plan is a reply under the message that started the run.
	var plan bool
	for _, m := range op.EmittedOf(messages.OutReply) {
		if m.ReplyTo == "om_start" && strings.Contains(m.Text, "▶ [ one ] started") {
			plan = true
		}
	}
	if !plan {
		t.Errorf("the plan does not hang under the message that started the run: %v", op.EmittedOf(messages.OutReply))
	}
	// The verdict of the run lands on the same card.
	var verdict bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.ReplyTo == "om_start" {
			verdict = true
		}
	}
	if !verdict {
		t.Errorf("the verdict of the run does not land on the card of the start message: %v",
			op.EmittedOf(messages.OutResult))
	}
	// The run posts no card of its own for the plan.
	for _, c := range op.Cards() {
		for _, entry := range op.Entries(c.ID) {
			if strings.Contains(entry, "▶ [ one ] started") {
				t.Errorf("the run posted the plan as a card of its own: %+v", c)
			}
		}
	}
}

// TestARunWithoutAMessagePublishesThePlanOfItsOwn — a run that no
// message started has no card of the chat to write into: it posts no
// message of its own, and the chat shows the plan the way it shows
// every message of the run.
func TestARunWithoutAMessagePublishesThePlanOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("committed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var plan bool
	for _, m := range op.EmittedOf(messages.OutReply) {
		if m.ReplyTo == "" && strings.Contains(m.Text, "▶ [ one ] started") {
			plan = true
		}
	}
	if !plan {
		t.Errorf("the run published no plan of its own: %v", op.EmittedOf(messages.OutReply))
	}
	// The verdict of the run lands on the same card.
	var verdict bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.ReplyTo == "" {
			verdict = true
		}
	}
	if !verdict {
		t.Errorf("the verdict of the run does not land on the card of the run: %v",
			op.EmittedOf(messages.OutResult))
	}
}

// TestTheRunCardCarriesThePlanOfTheRun — the card of the run is the
// plan of the run: its headline, its facts and its steps. It is
// published once and never changed.
func TestTheRunCardCarriesThePlanOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", cliWorkflow)
	svc, op := testService(t, dir, noModels())

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work", Args: []string{"issue=*"},
		MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The job of the workflow repeats until the run ends: the plan is
	// published when the run opens, and the run never ends.
	waitEntry(t, op, "▶ [ fix ] started")

	entries := op.Entries("om_start")
	if len(entries) != 1 {
		t.Fatalf("the card of the start message carries %d entries, want the plan alone: %v", len(entries), entries)
	}
	card := entries[0]
	for _, want := range []string{
		"▶ [ fix ] started",
		"jobs:     1",
		"steps:    8",
		"started: ",
		"args:     issue=*",
		"ship ⟳ until the run ends",
		"├─ [ 1 ]: fix             /gtw fix * -y",
		"└─ [ 8 ]: wait            (wait) 5m",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the card of the run does not carry %q: %q", want, card)
		}
	}
	// The card carries no identifier of its own: the status bar names
	// the run.
	if runID := runIDOf(svc, "obs-1"); runID != "" && strings.Contains(card, runID) {
		t.Errorf("the card of the run carries the id of the run: %q", card)
	}
	// The run publishes no checklist of its own.
	if tasks := op.Checklist("om_start"); len(tasks) != 0 {
		t.Errorf("the run published a checklist of its own: %+v", tasks)
	}
	// The counters of the run are on the card of the round: the card
	// of the run is static.
	for _, m := range op.EmittedOf(messages.OutHeartbeat) {
		if m.ReplyTo == "om_start" {
			t.Errorf("the run card carries a heartbeat: %+v", m.Heartbeat)
		}
	}
}

// runIDOf returns the id of the run of a chat.
func runIDOf(svc *Service, chatID string) string {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	r, ok := svc.active[chatID]
	if !ok {
		return ""
	}
	return r.id
}

// TestTheRoundCardCarriesTheTreeOfTheRounds — the card of a round
// carries the tree of the rounds of the run.
func TestTheRoundCardCarriesTheTreeOfTheRounds(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("committed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) < 1 {
		t.Fatalf("the run published %d cards, want the card of the round", len(cards))
	}
	entries := op.Entries(cards[0].ID)
	if len(entries) == 0 {
		t.Fatal("the card of the round carries no entry")
	}
	if !strings.HasPrefix(entries[0], "rounds: ") {
		t.Errorf("the card of the round does not open with the tree of the rounds: %q", entries[0])
	}
	// The card of the round carries no identity of its own: the status
	// bar names the run and the round.
	for _, entry := range entries {
		if strings.Contains(entry, "main · round 1") {
			t.Errorf("the card of the round carries the identity of the round: %q", entry)
		}
	}
}

// TestThePlaceholderSaysWhatIsComing — the placeholder is a message
// of the chat: it says in one line what the card below it is about.
func TestThePlaceholderSaysWhatIsComing(t *testing.T) {
	svc, op := sessionService(t)

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run opened no card")
	}
	if cards[0].Body != "main · round 1 is starting…" {
		t.Errorf("the placeholder says %q, want the round that starts", cards[0].Body)
	}
}

// TestEveryEventOfASessionCarriesItsCard — the card of a session is
// the anchor of its events: the chat renders the session onto that
// card. The run's own events hang under the message that started the
// run: the chat folds them into the card it already holds.
func TestEveryEventOfASessionCarriesItsCard(t *testing.T) {
	svc, op := sessionService(t)

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
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
		if m.ReplyTo == "om_start" {
			continue
		}
		if !cards[m.ReplyTo] {
			t.Fatalf("the event hangs under %q, want the card of its session", m.ReplyTo)
		}
	}
}

// TestTheRoundCardHangsUnderTheStartMessage — the round is the run's
// work, one level below the message that started the run.
func TestTheRoundCardHangsUnderTheStartMessage(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("committed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) < 1 {
		t.Fatalf("the run published %d cards, want the card of the round", len(cards))
	}
	if cards[0].ReplyTo != "om_start" {
		t.Errorf("the round card hangs under %q, want the start message om_start", cards[0].ReplyTo)
	}
}

// TestTheRunCardCarriesTheTokensOfTheRun — the run's card shows what
// the run spent in its footer.
func TestTheRunCardCarriesTheTokensOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{}),
			Usage: llm.Usage{InputTokens: 120, OutputTokens: 30}},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	runID, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var found bool
	for _, m := range op.Emitted() {
		if m.SessionID != runID || m.Usage == nil {
			continue
		}
		if m.Usage.InputTokens == 120 && m.Usage.OutputTokens == 30 {
			found = true
		}
	}
	if !found {
		t.Errorf("the run's card carries no usage: %v", op.Emitted())
	}
}

// TestThePlaceholderStartsNoTurnOfTheAgent — the placeholder is a
// message of the chat, not a turn of the agent: the daemon does not
// take it as one.
func TestThePlaceholderStartsNoTurnOfTheAgent(t *testing.T) {
	svc, op := sessionService(t)

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run opened no card")
	}
	if _, ok := op.Get(cards[0].ID); ok {
		t.Errorf("the placeholder %q started a turn of the agent", cards[0].ID)
	}
}

// TestTheChecklistOfAnOlderCardIsFrozen — the card the round had keeps
// the checklist of the moment it was left: the checklist of the round
// follows the steps on the latest card only.
func TestTheChecklistOfAnOlderCardIsFrozen(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{okStep("the commit landed"), {open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw push")

	r := activeRun(t, svc, "obs-1")
	round := r.roundSessionRef()
	if round == nil {
		t.Fatal("the run has no card of the round")
	}
	frozen := op.Checklist(round.anchor)
	if len(frozen) != 2 {
		t.Fatalf("the card of the round carries %d rows, want one per step", len(frozen))
	}
	if frozen[0].Status != agent.TaskCompleted {
		t.Errorf("the card of the round does not show the step that ran: %+v", frozen[0])
	}

	// The operator asks for the state: the card of the state is the
	// latest card of the round.
	if err := svc.Status(context.Background(), "obs-1", "om_status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	card, _ := stateCard(op, "om_status")
	if card.ID == round.anchor {
		t.Fatal("the state did not open a card of its own")
	}

	// The card the round had keeps the checklist it had.
	if got := op.Checklist(round.anchor); len(got) != len(frozen) {
		t.Errorf("the card of the round carries %d rows, want the %d it had", len(got), len(frozen))
	}
	// The latest card carries the checklist of the round.
	latest := op.Checklist(card.ID)
	if len(latest) != 2 {
		t.Fatalf("the card of the state carries %d rows, want one per step", len(latest))
	}
	if latest[0].Status != agent.TaskCompleted {
		t.Errorf("the card of the state does not show the step that ran: %+v", latest[0])
	}
}

// TestACardTheChatRefusesGoesToTheOrphanPath — a chat that refuses the
// placeholder leaves the session without an anchor: the run goes on and
// the content of the session goes to the chat's orphan handling.
func TestACardTheChatRefusesGoesToTheOrphanPath(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("committed")}
	op.failCard = true

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if op.Posted() != 0 {
		t.Errorf("the run opened %d cards, want none", op.Posted())
	}
	// The tree of the round hangs under no message at all: the chat
	// shows it the way it shows every message of a run.
	var orphan bool
	for _, m := range op.Emitted() {
		if m.Kind == messages.OutReply && m.ReplyTo == "" && strings.HasPrefix(m.Text, "rounds: ") {
			orphan = true
		}
	}
	if !orphan {
		t.Errorf("the content of the round does not go to the orphan path: %v", op.Emitted())
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
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
	if round != "commit" {
		t.Errorf("the checklist of the round is %q, want the id of the step", round)
	}
}

// TestTheRoundsTreeCarriesTheFactsOfEveryRound — the tree reads as a
// ledger: every finished round carries its number, the icon of its
// outcome, when it started, how long it took and the numbers of the run
// so far.
func TestTheRoundsTreeCarriesTheFactsOfEveryRound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{
		{
			Round:     1,
			Job:       "main",
			StartedAt: start,
			EndedAt:   start.Add(5*time.Minute + 25*time.Second),
			Outcome:   wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the fix landed"},
			Decisions: roundDecisions{System1: 1, Operator: 1},
			Usage:     llm.Usage{InputTokens: 32400, OutputTokens: 8100},
		},
	}

	tree := r.roundsTree(iteration{job: "main", round: 2})
	want := []string{
		"rounds: 1",
		"├─ [ 1 ] v 10:33:04 · 5m25s decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k",
		"└─ [ 2 ] > ",
	}
	got := strings.Split(tree, "\n")
	if len(got) != len(want) {
		t.Fatalf("the tree holds %d lines, want %d: %q", len(got), len(want), tree)
	}
	for i, line := range got {
		if !strings.HasPrefix(line, want[i]) {
			t.Errorf("line %d is %q, want %q", i, line, want[i])
		}
	}
}

// TestTheRoundsTreeNamesTheCountOfTheJob — the heading of the tree
// says how many rounds the run has: the count of the job the round
// belongs to.
func TestTheRoundsTreeNamesTheCountOfTheJob(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "rounds", roundWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("rounds")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "rounds", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)

	for _, c := range []struct {
		job  string
		want string
	}{
		{job: "ship", want: "rounds: 2"},
		{job: "verify", want: "rounds: 3"},
	} {
		tree := r.roundsTree(iteration{job: c.job, round: 1})
		if !strings.HasPrefix(tree, c.want+"\n") {
			t.Errorf("the tree of %s opens with %q, want %q", c.job, tree, c.want)
		}
	}
}

// TestTheRoundsTreeNamesTheCountOfTheRun — the count the run was
// started with is the count the heading names.
func TestTheRoundsTreeNamesTheCountOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    iterations: 3
    steps:
      - id: rest
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	rounds := 5
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work", Iterations: &rounds,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		waitRun(t, svc, "obs-1")
	}()

	r := activeRun(t, svc, "obs-1")
	tree := r.roundsTree(iteration{job: "main", round: 1})
	if !strings.HasPrefix(tree, "rounds: 5\n") {
		t.Errorf("the tree opens with %q, want the count of the run", tree)
	}
}

// TestTheRoundsTreeOfAJobThatRepeatsUntilTheRunEnds — a job with no
// count has no maximum: the heading says so.
func TestTheRoundsTreeOfAJobThatRepeatsUntilTheRunEnds(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "cli", cliWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("cli")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "cli", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)

	tree := r.roundsTree(iteration{job: "ship", round: 1})
	if !strings.HasPrefix(tree, "rounds: ⟳ until the run ends\n") {
		t.Errorf("the tree opens with %q, want it to say the job repeats", tree)
	}
}

// TestTheIconOfAnOutcome — the cards of a run and the checklist of a
// step share one vocabulary of icons.
func TestTheIconOfAnOutcome(t *testing.T) {
	for _, c := range []struct {
		status wfe.OutcomeStatus
		want   string
	}{
		{status: wfe.OutcomeOK, want: "v"},
		{status: wfe.OutcomeNoop, want: "v"},
		{status: wfe.OutcomeFailed, want: "!"},
		{status: wfe.OutcomeIncomplete, want: "!"},
		{status: wfe.OutcomeTimeout, want: "!"},
		{status: wfe.OutcomeCancelled, want: "x"},
		{status: wfe.OutcomeUnknown, want: "?"},
		{status: "", want: "?"},
	} {
		got := outcomeIcon(c.status)
		if got != c.want {
			t.Errorf("the icon of %q is %q, want %q", c.status, got, c.want)
		}
		if n := len([]rune(got)); n != 1 {
			t.Errorf("the icon of %q is %d characters wide, want one", c.status, n)
		}
	}
}

// TestTheRoundsTreeFoldsTheOldRounds — the tree shows the last rounds
// of the run: the earlier ones fold into one node.
func TestTheRoundsTreeFoldsTheOldRounds(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	for i := 1; i <= 8; i++ {
		r.roundRecords = append(r.roundRecords, roundRecord{
			Round:     i,
			Job:       "main",
			StartedAt: start.Add(time.Duration(i) * time.Minute),
			EndedAt:   start.Add(time.Duration(i)*time.Minute + time.Second),
			Outcome:   wfe.Outcome{Status: wfe.OutcomeOK},
		})
	}

	tree := r.roundsTree(iteration{job: "main", round: 9})
	if !strings.Contains(tree, "├─ … +3") {
		t.Errorf("the tree does not fold the old rounds: %q", tree)
	}
	for _, want := range []string{"[ 4 ]", "[ 5 ]", "[ 6 ]", "[ 7 ]", "[ 8 ]"} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree does not list round %s: %q", want, tree)
		}
	}
	if strings.Contains(tree, "[ 1 ]") {
		t.Errorf("the tree lists a folded round: %q", tree)
	}
}

// TestTheRoundsTreeKeepsTheIconsInOneColumn — the round number is as
// wide as the largest one the tree shows, so the icons of the rows
// start at the same column.
func TestTheRoundsTreeKeepsTheIconsInOneColumn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{
		{
			Round:     9,
			Job:       "main",
			StartedAt: start,
			EndedAt:   start.Add(time.Minute),
			Outcome:   wfe.Outcome{Status: wfe.OutcomeOK},
		},
		{
			Round:     10,
			Job:       "main",
			StartedAt: start.Add(time.Minute),
			EndedAt:   start.Add(2 * time.Minute),
			Outcome:   wfe.Outcome{Status: wfe.OutcomeFailed},
		},
	}

	tree := r.roundsTree(iteration{job: "main", round: 11})
	for _, want := range []string{
		"├─ [  9 ] v ",
		"├─ [ 10 ] ! ",
		"└─ [ 11 ] > ",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree does not carry %q: %q", want, tree)
		}
	}
}

// TestTheRoundThatFailedSaysSo — the icon of a round tells how it
// came to.
func TestTheRoundThatFailedSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{{
		Round:     1,
		Job:       "main",
		StartedAt: start,
		EndedAt:   start.Add(time.Second),
		Outcome:   wfe.Outcome{Status: wfe.OutcomeFailed, Summary: "the remote refused"},
	}}

	tree := r.roundsTree(iteration{job: "main", round: 2})
	if !strings.Contains(tree, "[ 1 ] ! ") {
		t.Errorf("the tree does not mark the round that failed: %q", tree)
	}
	rec := r.roundRecords[0]
	if got := roundEndedLine(rec); !strings.Contains(got, "[ ! ] the remote refused") {
		t.Errorf("the closing line does not name the failure: %q", got)
	}
}

// TestTheClosingBlockOfARoundCarriesItsNumbers — the closing block
// carries the numbers of the run through that round.
func TestTheClosingBlockOfARoundCarriesItsNumbers(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	rec := roundRecord{
		Round:     1,
		Job:       "main",
		StartedAt: start,
		EndedAt:   start.Add(5*time.Minute + 14*time.Second),
		Outcome:   wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the fix landed"},
		Decisions: roundDecisions{System1: 1, Operator: 1},
		Usage:     llm.Usage{InputTokens: 32400, OutputTokens: 8100},
	}

	if got := roundEndedLine(rec); got != "ended: [ v ] the fix landed · 5m14s" {
		t.Errorf("the closing line is %q", got)
	}
	want := "decisions:  system-1 ×1 · operator ×1 ·  tokens - 32.4k / 8.1k"
	if got := decisionsText(rec); got != want {
		t.Errorf("the decisions line is %q, want %q", got, want)
	}
	// A round in which nothing was decided says so.
	empty := roundRecord{Outcome: wfe.Outcome{Status: wfe.OutcomeOK}}
	if got := decisionsText(empty); !strings.HasPrefix(got, "decisions:  none") {
		t.Errorf("the decisions line is %q, want it to say none", got)
	}
}

// TestTheRoundCardGivesEveryStepItsOwnState — a step that failed is
// not a step that has not started: every step of the round carries
// the state it is in, and the detail of that state.
func TestTheRoundCardGivesEveryStepItsOwnState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "three", threeStepWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("three")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "three", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)
	r.state.Steps = []wfe.StepState{
		{ID: "commit", Job: "main", Round: 1, Outcome: wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the commit landed"}},
		{ID: "push", Job: "main", Round: 1, Outcome: wfe.Outcome{
			Status:  wfe.OutcomeFailed,
			Summary: "the remote refused",
			Data:    map[string]string{wfe.OutcomeKeyError: "no_remote"},
		}},
		// The record of another round is not the state of this round.
		{ID: "pr", Job: "main", Round: 2, Outcome: wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the PR is up"}},
	}

	items := r.roundTasks("main", 1)
	if len(items) != 3 {
		t.Fatalf("the checklist holds %d rows, want one per step of the round", len(items))
	}
	if items[0].Status != agent.TaskCompleted {
		t.Errorf("the step that landed is %q, want it completed", items[0].Status)
	}
	if items[0].ActiveForm != "the commit landed" {
		t.Errorf("the detail of the step that landed is %q", items[0].ActiveForm)
	}
	if items[1].Status != agent.TaskFailed {
		t.Errorf("the step that failed is %q, want it failed", items[1].Status)
	}
	for _, want := range []string{"the remote refused", "no_remote"} {
		if !strings.Contains(items[1].ActiveForm, want) {
			t.Errorf("the detail of the step that failed is %q, want it to name %q", items[1].ActiveForm, want)
		}
	}
	if items[2].Status != agent.TaskPending {
		t.Errorf("the step that did not run is %q, want it pending", items[2].Status)
	}
	if items[2].ActiveForm != "" {
		t.Errorf("the step that did not run carries the detail %q, want none", items[2].ActiveForm)
	}
}

// TestTheDetailOfAStepIsCutShort — a step that reports a whole build
// log as its summary must not make the row unreadable.
func TestTheDetailOfAStepIsCutShort(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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
	r.state.Steps = []wfe.StepState{
		{ID: "commit", Job: "main", Outcome: wfe.Outcome{Status: wfe.OutcomeFailed, Summary: strings.Repeat("x", 4000)}},
	}

	items := r.roundTasks("main", 1)
	if len(items) != 1 {
		t.Fatalf("the checklist holds %d rows, want one per step of the round", len(items))
	}
	detail := items[0].ActiveForm
	if n := len([]rune(detail)); n > maxStepDetail {
		t.Errorf("the detail is %d runes, want at most %d", n, maxStepDetail)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Errorf("the detail is %q, want it cut short", detail)
	}
}

// TestTheRunCardIsRenderedAsACommandLine — the body of the run card
// reads like the output of a command: a headline, the facts of the run
// and the tree of its plan.
func TestTheRunCardIsRenderedAsACommandLine(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", flowWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("two")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "two", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, map[string]string{"id": "42"})
	t.Cleanup(r.cancel)

	want := []string{
		"▶ [ two ] started",
		"Two jobs.",
		"",
		"jobs:     2",
		"steps:    3",
		"started:  " + clockTime(r.state.StartedAt),
		"args:     id=42",
		"",
		"ship",
		"├─ [ 1 ]: fix      /gtw fix",
		"└─ [ 2 ]: commit   /gtw commit",
		"",
		"verify",
		"└─ [ 3 ]: review   /review",
	}
	got := strings.Split(r.runCard(), "\n")
	if len(got) != len(want) {
		t.Fatalf("the card holds %d lines, want %d: %q", len(got), len(want), r.runCard())
	}
	for i, line := range got {
		if line != want[i] {
			t.Errorf("line %d is %q, want %q", i, line, want[i])
		}
	}
}

// TestTheRunCardNamesTheRoundsOfEveryJob — every job says how many
// rounds it runs.
func TestTheRunCardNamesTheRoundsOfEveryJob(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "rounds", roundWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("rounds")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "rounds", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)

	card := r.runCard()
	for _, want := range []string{
		"ship ⟳ 2 rounds",
		"verify ⟳ 3 rounds",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the card does not carry %q: %q", want, card)
		}
	}
	// A run without arguments carries no args row.
	if strings.Contains(card, "args:") {
		t.Errorf("the card carries an args row: %q", card)
	}
}

// TestTheDetailOfAStepDropsTheBranch — the branch is in the status
// bar of the card: the detail of a row does not repeat it.
func TestTheDetailOfAStepDropsTheBranch(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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
	r.state.Steps = []wfe.StepState{
		{ID: "commit", Job: "main", Outcome: wfe.Outcome{
			Status:  wfe.OutcomeOK,
			Summary: "the agent produced 12 change(s) on 12-fix-client-refuse",
			Data:    map[string]string{outcomeKeyBranch: "12-fix-client-refuse"},
		}},
		{ID: "push", Job: "main", Outcome: wfe.Outcome{
			Status:  wfe.OutcomeOK,
			Summary: "the work landed on 12-fix-client-refuse already",
			Data:    map[string]string{outcomeKeyBranch: "12-fix-client-refuse"},
		}},
	}

	items := r.roundTasks("main", 1)
	if len(items) != 1 {
		t.Fatalf("the checklist holds %d rows, want one per step of the round", len(items))
	}
	if items[0].ActiveForm != "the agent produced 12 change(s)" {
		t.Errorf("the detail is %q, want it without the branch", items[0].ActiveForm)
	}
}

// TestTheDetailOfAStepKeepsABranchItDoesNotEndWith — only a trailing
// " on <branch>" is dropped: the rest of the summary stays.
func TestTheDetailOfAStepKeepsABranchItDoesNotEndWith(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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
	r.state.Steps = []wfe.StepState{
		{ID: "commit", Job: "main", Outcome: wfe.Outcome{
			Status:  wfe.OutcomeOK,
			Summary: "the work landed on 12-fix-client-refuse already",
			Data:    map[string]string{outcomeKeyBranch: "12-fix-client-refuse"},
		}},
	}

	items := r.roundTasks("main", 1)
	if len(items) != 1 {
		t.Fatalf("the checklist holds %d rows, want one per step of the round", len(items))
	}
	if items[0].ActiveForm != "the work landed on 12-fix-client-refuse already" {
		t.Errorf("the detail is %q, want the summary unchanged", items[0].ActiveForm)
	}
}

// TestTheDetailOfAStepIsCutShortAfterTheBranchIsDropped — the branch
// is dropped before the detail is cut short, so it cannot eat the
// budget of the row.
func TestTheDetailOfAStepIsCutShortAfterTheBranchIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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
	branch := "12-fix-client-refuse"
	r.state.Steps = []wfe.StepState{
		{ID: "commit", Job: "main", Outcome: wfe.Outcome{
			Status:  wfe.OutcomeFailed,
			Summary: strings.Repeat("x", 4000) + " on " + branch,
			Data:    map[string]string{outcomeKeyBranch: branch},
		}},
	}

	items := r.roundTasks("main", 1)
	if len(items) != 1 {
		t.Fatalf("the checklist holds %d rows, want one per step of the round", len(items))
	}
	detail := items[0].ActiveForm
	if n := len([]rune(detail)); n > maxStepDetail {
		t.Errorf("the detail is %d runes, want at most %d", n, maxStepDetail)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Errorf("the detail is %q, want it cut short", detail)
	}
	if strings.Contains(detail, branch) {
		t.Errorf("the detail carries the branch: %q", detail)
	}
}

// TestTheCurrentRoundSaysWhenItStarted — the node of the round that
// is going says when the round started.
func TestTheCurrentRoundSaysWhenItStarted(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.setRoundStarted(iteration{job: "main", round: 2}, start.Add(time.Minute))

	tree := r.roundsTree(iteration{job: "main", round: 2})
	want := "└─ [ 2 ] > " + clockTime(start.Add(time.Minute))
	if !strings.Contains(tree, want) {
		t.Errorf("the tree does not say when the round started: %q", tree)
	}
}

// TestTheTotalsOfTheRunSurviveARestart — the numbers of the run are
// the numbers of its last round: a run that comes back keeps counting
// up, and the tree never runs backwards.
func TestTheTotalsOfTheRunSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	r.roundRecords = []roundRecord{{
		Round:     1,
		Job:       "main",
		Decisions: roundDecisions{System1: 6, Operator: 2},
		Usage:     llm.Usage{InputTokens: 6000, OutputTokens: 1000},
	}}
	r.system1Decisions = 6
	r.system2Decisions = 3
	r.operatorDecisions = 2
	r.roundAnswers = 1
	r.roundUsage = llm.Usage{InputTokens: 100, OutputTokens: 10}

	restored := svc.restoreRun(persistedRun{
		RunID:             "one-1",
		ChatID:            "obs-1",
		Cwd:               "/tmp/work",
		Workflow:          r.workflow,
		State:             r.state,
		RoundRecords:      r.roundRecords,
		System1Decisions:  r.system1Decisions,
		System2Decisions:  r.system2Decisions,
		OperatorDecisions: r.operatorDecisions,
		RoundAnswers:      r.roundAnswers,
		RoundUsage:        r.roundUsage,
	})
	if restored.system1Decisions != 6 || restored.system2Decisions != 3 {
		t.Errorf("the restored decisions are %d and %d, want the numbers of the run", restored.system1Decisions, restored.system2Decisions)
	}
	if restored.operatorDecisions != 2 {
		t.Errorf("the restored run counts %d answers of the operator, want 2", restored.operatorDecisions)
	}
	if restored.cumUsage.InputTokens != 6000 || restored.cumUsage.OutputTokens != 1000 {
		t.Errorf("the restored totals are %+v, want the numbers of the last round", restored.cumUsage)
	}
	if restored.roundAnswers != 1 {
		t.Errorf("the restored round counts %d answers of the operator, want 1", restored.roundAnswers)
	}
	if restored.roundUsage.InputTokens != 100 {
		t.Errorf("the restored round spent %+v, want what it spent", restored.roundUsage)
	}
}

// TestTheStateOfTheRunDoesNotNameTheRoundThatEnded — the state on
// disk must not name a round that has ended as the current one: a
// daemon that comes back would close that round a second time.
func TestTheStateOfTheRunDoesNotNameTheRoundThatEnded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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
	svc.closeRoundSession(context.Background(), r)
	if r.roundSessionRef() != nil {
		t.Error("the run still holds the round that ended")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runs", r.id+".json"))
	if err != nil {
		t.Fatalf("read the state: %v", err)
	}
	var state persistedRun
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	if state.RoundSession != nil {
		t.Errorf("the state names the round that ended: %+v", state.RoundSession)
	}
	if len(state.RoundRecords) != 1 {
		t.Errorf("the state holds %d records, want the round that ended", len(state.RoundRecords))
	}
}

// TestTheTokensOfTheCardKeepTheirUnit — a round that spent exactly a
// thousand tokens says "1k", not "1": the unit is part of the number.
func TestTheTokensOfTheCardKeepTheirUnit(t *testing.T) {
	rec := roundRecord{Usage: llm.Usage{InputTokens: 1000, OutputTokens: 1000000}}
	got := decisionsText(rec)
	if !strings.Contains(got, "tokens - 1k / 1M") {
		t.Errorf("the decisions line is %q, want the units of the tokens", got)
	}
}

// TestTheTreeNamesTheJobOfARound — the rounds of a job are numbered
// from one, so a run with several jobs names the job in its tree.
func TestTheTreeNamesTheJobOfARound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", flowWorkflow)
	svc, _ := testService(t, dir, noModels())
	wf, ok := svc.catalog.Lookup("two")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "two", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	t.Cleanup(r.cancel)

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{
		{Round: 1, Job: "ship", StartedAt: start, EndedAt: start.Add(time.Minute), Outcome: wfe.Outcome{Status: wfe.OutcomeOK}},
		{Round: 1, Job: "verify", StartedAt: start.Add(time.Minute), EndedAt: start.Add(2 * time.Minute), Outcome: wfe.Outcome{Status: wfe.OutcomeOK}},
	}

	tree := r.roundsTree(iteration{job: "ship", round: 2})
	for _, want := range []string{"[ 1 ] v ship · ", "[ 1 ] v verify · ", "[ 2 ] > ship · "} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree does not carry %q: %q", want, tree)
		}
	}
}

// TestTheRoundThatEndsIsRecorded — a round that ends leaves its facts
// with the run: the tree of the rounds reads them back.
func TestTheRoundThatEndsIsRecorded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	at := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.policy.History = []wfe.ActionRecord{{
		Kind:    wfe.ActionKindRun,
		StepID:  "commit",
		Round:   1,
		Outcome: wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the commit landed"},
		At:      at,
	}}
	r.system1Decisions = 1
	r.operatorDecisions = 2
	r.roundUsage = llm.Usage{InputTokens: 100, OutputTokens: 20}

	rec := r.closeRound("main", 1, at.Add(time.Minute))
	if rec.Decisions.System1 != 1 || rec.Decisions.Operator != 2 {
		t.Errorf("the record counts %+v, want the decisions of the run", rec.Decisions)
	}
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 20 {
		t.Errorf("the record carries %+v, want what the round spent", rec.Usage)
	}
	if rec.StartedAt != at {
		t.Errorf("the round started at %v, want %v", rec.StartedAt, at)
	}
	if rec.EndedAt != at.Add(time.Minute) {
		t.Errorf("the round ended at %v, want %v", rec.EndedAt, at.Add(time.Minute))
	}
	if len(r.roundRecords) != 1 || r.roundRecords[0].Round != 1 {
		t.Errorf("the run holds %+v, want the record of the round", r.roundRecords)
	}
	if rec.Outcome.Summary != "the commit landed" {
		t.Errorf("the record carries %q, want what the round came to", rec.Outcome.Summary)
	}
}

// TestTheRoundsSurviveARestart — the facts of the rounds a run
// finished are persisted with the run: the tree of the rounds looks the
// same after a restart of the daemon.
func TestTheRoundsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	start := time.Date(2026, 11, 18, 10, 33, 4, 0, time.UTC)
	r.roundRecords = []roundRecord{
		{
			Round:     1,
			Job:       "main",
			StartedAt: start,
			EndedAt:   start.Add(time.Minute),
			Outcome:   wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the fix landed"},
			Decisions: roundDecisions{System1: 1},
			Usage:     llm.Usage{InputTokens: 100, OutputTokens: 20},
		},
		{
			Round:     2,
			Job:       "main",
			StartedAt: start.Add(time.Minute),
			EndedAt:   start.Add(2 * time.Minute),
			Outcome:   wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the fix landed again"},
			Decisions: roundDecisions{System1: 1, Operator: 1},
			Usage:     llm.Usage{InputTokens: 200, OutputTokens: 40},
		},
	}

	restored := svc.restoreRun(persistedRun{
		RunID:        "one-1",
		ChatID:       "obs-1",
		Cwd:          "/tmp/work",
		Workflow:     r.workflow,
		State:        r.state,
		RoundRecords: r.roundRecords,
	})
	if len(restored.roundRecords) != 2 {
		t.Fatalf("the restored run holds %d records, want 2", len(restored.roundRecords))
	}
	tree := restored.roundsTree(iteration{job: "main", round: 3})
	for _, want := range []string{
		"[ 1 ] v 10:33:04 · 1m00s decisions:  system-1 ×1 ·  tokens - 100 / 20",
		"[ 2 ] v 10:34:04 · 1m00s decisions:  system-1 ×1 · operator ×1 ·  tokens - 200 / 40",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree does not carry %q: %q", want, tree)
		}
	}
}

// TestTheDecisionsOfTheRoundAreCounted — the round counts what each
// capability decided.
func TestTheDecisionsOfTheRoundAreCounted(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	r.recordWake(wakeRecord{Who: decisionDecider})
	r.recordWake(wakeRecord{Who: decisionChat})
	r.recordWake(wakeRecord{Who: decisionChat})

	if len(r.wakeRecords()) != 3 {
		t.Errorf("the run holds %d decisions, want 3", len(r.wakeRecords()))
	}
}

// TestTheAnswerOfTheOperatorCountsOnce — the answers of the operator
// are counted once: a click on an option is the same answer as a typed
// reply.
func TestTheAnswerOfTheOperatorCountsOnce(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, noModels())
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

	r.mu.Lock()
	r.ask = &askState{Question: "which one?", Options: []llm.Option{{Value: "/gtw commit"}}, RequestID: "req-1"}
	r.mu.Unlock()

	if err := r.deliver("/gtw commit"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if r.roundAnswers != 1 {
		t.Errorf("the round counts %d answers of the operator, want 1", r.roundAnswers)
	}
	if r.operatorDecisions != 1 {
		t.Errorf("the run counts %d answers of the operator, want 1", r.operatorDecisions)
	}
	// The run is no longer waiting: a second answer is not an answer.
	if err := r.deliver("/gtw commit"); err == nil {
		t.Error("the run took a second answer")
	}
	if r.roundAnswers != 1 {
		t.Errorf("the round counts %d answers of the operator, want 1", r.roundAnswers)
	}
	if r.operatorDecisions != 1 {
		t.Errorf("the run counts %d answers of the operator, want 1", r.operatorDecisions)
	}
}

// flowWorkflow is a workflow of two jobs: the first one holds two
// steps, the second one holds a single step.
const flowWorkflow = `title: Two jobs.
args:
  - name: id
    default: "1"
jobs:
  ship:
    steps:
      - id: fix
        message: /gtw fix
      - id: commit
        message: /gtw commit
  verify:
    steps:
      - id: review
        message: /review
`

// roundWorkflow is a workflow whose jobs declare different round
// counts.
const roundWorkflow = `title: Different rounds.
jobs:
  ship:
    iterations: 2
    steps:
      - id: fix
        message: /gtw fix
  verify:
    iterations: 3
    steps:
      - id: review
        message: /review
`

// threeStepWorkflow is a workflow with three steps in one job.
const threeStepWorkflow = `title: Three things.
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
      - id: pr
        message: /gtw pr
`

// cliWorkflow is a workflow that reads like a command line: one job
// that repeats until the run ends, with eight steps.
const cliWorkflow = `title: 自动挑选处于 open 状态的 nightme/assigned 的 issue,执行修复,review,并提交 PR
args:
  - name: issue
    default: "*"
jobs:
  ship:
    iterations: 0
    steps:
      - id: fix
        message: /gtw fix * -y
      - id: commit
        message: /gtw commit
      - id: review
        message: /review
      - id: commit-revise
        message: /gtw commit
      - id: push
        message: /gtw push
      - id: pr
        message: /gtw pr
      - id: back
        message: /gtw back
      - id: wait
        wait: 5m
`
