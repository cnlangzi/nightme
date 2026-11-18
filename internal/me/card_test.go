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

// TestTheRunCardCarriesTheOverviewOfTheRun — the card of the run is
// the overview of the run: the workflow it works through, the jobs it
// has and the steps of those jobs. It is published once and never
// changed.
func TestTheRunCardCarriesTheOverviewOfTheRun(t *testing.T) {
	svc, op := sessionService(t)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run published no card")
	}
	entries := op.Entries(cards[0].ID)
	if len(entries) != 1 {
		t.Fatalf("the card of the run carries %d entries, want the overview alone: %v", len(entries), entries)
	}
	overview := entries[0]
	for _, want := range []string{
		"**one** — Do the one thing.",
		"→ rounds:  1 round",
		"→ jobs:    1 · steps: 1",
		"→ started: ",
		"→ main:    commit",
	} {
		if !strings.Contains(overview, want) {
			t.Errorf("the overview does not carry %q: %q", want, overview)
		}
	}
	// The jobs are a flow row each, not a nested list.
	for _, line := range strings.Split(overview, "\n") {
		if strings.HasPrefix(line, "  - ") || strings.HasPrefix(line, "     - ") {
			t.Errorf("the overview carries a nested list item: %q", overview)
		}
		if line == "jobs:" {
			t.Errorf("the overview carries a jobs list: %q", overview)
		}
	}
	// The first line names the run.
	var runID string
	for _, m := range op.EmittedOf(messages.OutReply) {
		if m.ReplyTo == cards[0].ID {
			runID = m.SessionID
		}
	}
	if !strings.HasPrefix(overview, BotMark+runID) {
		t.Errorf("the overview does not start with the run id %q: %q", runID, overview)
	}
	// The run publishes no checklist of its own.
	if tasks := op.Checklist(cards[0].ID); len(tasks) != 0 {
		t.Errorf("the run published a checklist of its own: %+v", tasks)
	}
	// The counters of the run are on the card of the round: the card
	// of the run is static.
	for _, m := range op.EmittedOf(messages.OutHeartbeat) {
		if m.ReplyTo == cards[0].ID {
			t.Errorf("the run card carries a heartbeat: %+v", m.Heartbeat)
		}
	}
}

// TestTheRoundCardNamesTheRunItBelongsTo — the card of a round names
// the run it belongs to, and the round it is.
func TestTheRoundCardNamesTheRunItBelongsTo(t *testing.T) {
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
	entries := op.Entries(cards[1].ID)
	if len(entries) == 0 {
		t.Fatal("the card of the round carries no entry")
	}
	if !strings.HasPrefix(entries[0], "→ run: one-") {
		t.Errorf("the block does not name the run: %q", entries[0])
	}
	var named bool
	for _, entry := range entries {
		if strings.Contains(entry, "main · round 1") {
			named = true
		}
	}
	if !named {
		t.Errorf("the card of the round does not name its round: %v", entries)
	}
	// The round's outcome rides on the card of the round: the card of
	// the run stays the overview it was.
	for _, entry := range op.Entries(cards[0].ID) {
		if strings.Contains(entry, "round 1 —") {
			t.Errorf("the card of the run names the outcome of a round: %q", entry)
		}
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

	runID, err := svc.StartWorkflow(context.Background(), Request{
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
	if round != "commit" {
		t.Errorf("the checklist of the round is %q, want the id of the step", round)
	}
}

// TestTheRunBlockListsTheRoundsThatHaveFinished — the block that
// names the run lists the rounds that have finished, with the time
// each of them ended. The round the run is in is not among them.
func TestTheRunBlockListsTheRoundsThatHaveFinished(t *testing.T) {
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

	ended := time.Date(2026, 11, 18, 14, 7, 41, 0, time.UTC)
	r.state.Rounds = map[string]int{"main": 3}
	r.policy.History = []wfe.ActionRecord{
		{Kind: wfe.ActionKindRun, StepID: "commit", Round: 1, At: ended},
		{Kind: wfe.ActionKindRun, StepID: "commit", Round: 2, At: ended.Add(time.Minute)},
	}

	block := r.runBlock()
	if !strings.HasPrefix(block, "→ run:     one-") {
		t.Errorf("the block does not name the run: %q", block)
	}
	for _, want := range []string{
		"→ round 1: main · 14:07:41",
		"→ round 2: main · 14:08:41",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block does not list %q: %q", want, block)
		}
	}
	if strings.Contains(block, "round 3") {
		t.Errorf("the block lists the round the run is in: %q", block)
	}
}

// TestTheRunBlockListsTheLastRoundOfAJobThatIsDone — a job whose
// steps all ran has finished its round: the block lists it.
func TestTheRunBlockListsTheLastRoundOfAJobThatIsDone(t *testing.T) {
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

	ended := time.Date(2026, 11, 18, 14, 7, 41, 0, time.UTC)
	r.state.Rounds = map[string]int{"main": 1}
	r.state.Steps = []wfe.StepState{{ID: "commit", Job: "main"}}
	r.policy.History = []wfe.ActionRecord{
		{Kind: wfe.ActionKindRun, StepID: "commit", Round: 1, At: ended},
	}

	block := r.runBlock()
	if !strings.Contains(block, "→ round 1: main · 14:07:41") {
		t.Errorf("the block does not list the finished round: %q", block)
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

// TestTheRunCardIsRenderedAsArrowRows — the body of the run card is a
// flat flow: the facts of the run are arrow rows and every job is a
// row of its own. The values of the rows start at the same column.
func TestTheRunCardIsRenderedAsArrowRows(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", twoJobWorkflow)
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
		BotMark + r.id,
		"**two** — Two jobs.",
		"→ rounds:  1 round",
		"→ jobs:    2 · steps: 3",
		"→ started: " + clockTime(r.state.StartedAt),
		"→ args:    id=42",
		"→ ship:    fix → commit",
		"→ verify:  review",
	}
	got := strings.Split(r.runOverview(), "\n")
	if len(got) != len(want) {
		t.Fatalf("the overview holds %d lines, want %d: %q", len(got), len(want), r.runOverview())
	}
	for i, line := range got {
		if line != want[i] {
			t.Errorf("line %d is %q, want %q", i, line, want[i])
		}
	}
}

// TestTheRunCardOmitsTheRoundsRowWhenTheJobsDiffer — the round count
// is the count every job shares: a workflow whose jobs declare
// different counts carries the count of each job on its own row.
func TestTheRunCardOmitsTheRoundsRowWhenTheJobsDiffer(t *testing.T) {
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

	overview := r.runOverview()
	if strings.Contains(overview, "rounds:") && !strings.Contains(overview, "· 2 rounds") {
		t.Errorf("the overview carries a shared rounds row: %q", overview)
	}
	for _, want := range []string{
		"→ ship:    fix · 2 rounds",
		"→ verify:  review · 3 rounds",
	} {
		if !strings.Contains(overview, want) {
			t.Errorf("the overview does not carry %q: %q", want, overview)
		}
	}
	for _, line := range strings.Split(overview, "\n") {
		if strings.HasPrefix(line, "→ rounds:") {
			t.Errorf("the overview carries a shared rounds row: %q", overview)
		}
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

// twoJobWorkflow is a workflow of two jobs: the first one holds two
// steps, the second one holds a single step.
const twoJobWorkflow = `title: Two jobs.
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
