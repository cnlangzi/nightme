package me

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// twoStepWorkflow is a workflow with two steps.
const twoStepWorkflow = `title: Do the two things.
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix 1 -y
      - id: commit
        message: /gtw commit
`

// chatRun returns the run of a chat.
func chatRun(t *testing.T, svc *Service, chatID string) *run {
	t.Helper()
	svc.mu.Lock()
	r := svc.active[chatID]
	svc.mu.Unlock()
	if r == nil {
		t.Fatalf("the chat %q has no run", chatID)
	}
	return r
}

// waitDone waits until the run is over.
//
// The run is over when its goroutine is: the summary is posted before
// that, so a test that waits here sees the whole card.
func waitDone(t *testing.T, r *run) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish")
	}
}

// summaryCard returns the card the summary was posted as, and its
// buttons.
func summaryCard(t *testing.T, op *fakeOperator) (cardRecord, []channel.CardButton) {
	t.Helper()
	cards := op.Cards()
	buttons := op.CardButtons()
	for i, c := range cards {
		if c.Posted && strings.HasSuffix(c.Title, " · stopped") {
			return c, buttons[i]
		}
	}
	t.Fatalf("the run posted no summary card: %+v", cards)
	return cardRecord{}, nil
}

// TestStop_PostsTheSummaryOfTheSession — the operator stops the run:
// the chat gets the record of the session.
func TestStop_PostsTheSummaryOfTheSession(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the issue is claimed by another run",
		Data:    map[string]string{"reason": "issue_claimed"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	before := svc.Status("obs-1")
	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !r.wasStoppedByOperator() {
		t.Error("the run does not know that the operator stopped it")
	}
	waitDone(t, r)

	card, buttons := summaryCard(t, op)
	if card.Title != "one · stopped" {
		t.Errorf("the title = %q, want %q", card.Title, "one · stopped")
	}
	for _, want := range []string{
		"run: ",
		"duration: ",
		"actions: 1 (1 failed)",
		"wake-ups: 1 (1 by the decision model)",
		"steps: 1 ran · 0 not run",
		"system-1: 1 decisions, 0 tokens",
		"system-2: 0 decisions, 0 tokens",
		"errors\n",
		"· /gtw commit — failed: the issue is claimed by another run (reason: issue_claimed)",
		"decisions\n",
		"· the decision model — escalate (p=0.10)",
	} {
		if !strings.Contains(card.Body, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, card.Body)
		}
	}
	if len(buttons) != 0 {
		t.Errorf("the summary carries %d buttons, want none", len(buttons))
	}
	// The summary is rendered from the run's own record: stopping a run
	// asks no model, so the counters do not move.
	if !strings.Contains(card.Body, "system-1: 1 decisions, 0 tokens") ||
		!strings.Contains(card.Body, "system-2: 0 decisions, 0 tokens") {
		t.Errorf("the summary does not carry the counters of the run:\n%s", card.Body)
	}
	if before.System1Decisions != 1 || before.System2Decisions != 0 {
		t.Errorf("the run decided %d times with the decision model and %d times with the chat model, want 1 and 0",
			before.System1Decisions, before.System2Decisions)
	}
	// The state of a run that ended goes with it: a file that outlived
	// the run would be resumed by the next daemon.
	if _, err := os.Stat(filepath.Join(dir, "runs", r.id+".json")); !os.IsNotExist(err) {
		t.Errorf("the state of the stopped run is still on disk: %v", err)
	}
}

// TestStop_UpdatesTheLastCardToTheFinalPhase — the card of the last
// iteration shows where the run ended.
func TestStop_UpdatesTheLastCardToTheFinalPhase(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the step failed",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitDone(t, r)

	cards := op.Cards()
	last := cards[len(cards)-1]
	if !strings.HasSuffix(last.Title, " · stopped") {
		t.Errorf("the last card is %q, want the stopped phase", last.Title)
	}
}

// TestTheSummaryComesAfterTheRunUnwinds — the step that was in flight
// when the operator stopped the run counts as a step that did not run.
func TestTheSummaryComesAfterTheRunUnwinds(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", twoStepWorkflow)

	svc, op := testService(t, dir, Config{Decider: &scriptedDecider{}})
	op.steps = []fakeStep{
		okStep("the fix landed"),
		{open: true},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitDone(t, r)

	card, _ := summaryCard(t, op)
	for _, want := range []string{
		"actions: 1 (1 ok)",
		"steps: 1 ran · 1 not run",
		"stopped while: /gtw commit",
	} {
		if !strings.Contains(card.Body, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, card.Body)
		}
	}
}

// TestTheSummaryListsOnlyTheActionsThatWentWrong — the actions that
// went well are counted, not listed.
func TestTheSummaryListsOnlyTheActionsThatWentWrong(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", twoStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{
		okStep("the fix landed"),
		{outcome: messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the commit failed",
		}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitDone(t, r)

	card, _ := summaryCard(t, op)
	if !strings.Contains(card.Body, "actions: 2 (1 ok · 1 failed)") {
		t.Errorf("the summary does not count the actions:\n%s", card.Body)
	}
	if strings.Contains(card.Body, "/gtw fix 1 -y —") {
		t.Errorf("the summary lists an action that went well:\n%s", card.Body)
	}
	if !strings.Contains(card.Body, "· /gtw commit — failed: the commit failed") {
		t.Errorf("the summary does not list the action that failed:\n%s", card.Body)
	}
}

// TestTheSummaryCapsTheErrorsAndTheDecisions — a long session keeps
// the card readable; the totals stay in the facts.
func TestTheSummaryCapsTheErrorsAndTheDecisions(t *testing.T) {
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
		state:    newRunState("one-1", wf.Name, "/tmp/work", time.Now()),
		phase:    PhaseStopped,
	}
	for i := 0; i < 6; i++ {
		r.policy.History = append(r.policy.History, wfe.ActionRecord{
			Kind:    wfe.ActionKindRun,
			Command: "/gtw commit",
			StepID:  "commit",
			Outcome: wfe.Outcome{Status: wfe.OutcomeFailed, Summary: "it failed"},
		})
	}
	for i := 0; i < 23; i++ {
		r.wakes = append(r.wakes, wakeRecord{
			Who:   decisionDecider,
			What:  "escalate (p=0.10)",
			Round: 1,
		})
	}

	body := svc.summaryView(r).Body
	for _, want := range []string{
		"errors (last 5 of 6)",
		"decisions (last 10 of 23)",
		"actions: 6 (6 failed)",
		"wake-ups: 23 (23 by the decision model)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, body)
		}
	}
	if got := countLines(body, "· "); got != 15 {
		t.Errorf("the summary lists %d lines, want 15", got)
	}

	// A short session carries the plain headings.
	r.policy.History = r.policy.History[:2]
	r.wakes = r.wakes[:3]
	body = svc.summaryView(r).Body
	for _, want := range []string{"\nerrors\n", "\ndecisions\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "(last") {
		t.Errorf("the summary counts a short session:\n%s", body)
	}
}

// countLines returns the number of lines that start with the given
// text.
func countLines(body, prefix string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestTheSummaryClipsItsRows — a card that carries a whole build log
// is unreadable, and a card that is too long is refused.
func TestTheSummaryClipsItsRows(t *testing.T) {
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
		state:    newRunState("one-1", wf.Name, "/tmp/work", time.Now()),
		phase:    PhaseStopped,
	}
	r.policy.History = []wfe.ActionRecord{{
		Kind:    wfe.ActionKindRun,
		Command: "/gtw build",
		StepID:  "commit",
		Outcome: wfe.Outcome{
			Status:  wfe.OutcomeFailed,
			Summary: strings.Repeat("build log ", 100),
			Data:    map[string]string{"reason": "agent_error"},
		},
	}}
	r.wakes = []wakeRecord{{
		Who:  decisionChat,
		What: "run_step(step_id=" + strings.Repeat("x", 400) + ")",
		Why:  strings.Repeat("because ", 100),
	}}

	body := svc.summaryView(r).Body
	for _, line := range strings.Split(body, "\n") {
		if n := len([]rune(line)); n > maxSummaryLine {
			t.Errorf("a line of the summary is %d runes long, want at most %d: %q", n, maxSummaryLine, line)
		}
	}
	// The reason code is the machine-readable part of the row: the cut
	// must not swallow it.
	if !strings.Contains(body, "reason: agent_error") {
		t.Errorf("the summary lost the reason code:\n%s", body)
	}
}

// TestTheFastPathHoldsNoWakeRecords — the workflow's own plan is not a
// decision a model made.
func TestTheFastPathHoldsNoWakeRecords(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := chatRun(t, svc, "obs-1")
	if wakes := r.wakeRecords(); len(wakes) != 0 {
		t.Errorf("the run holds %d wake records, want none: %+v", len(wakes), wakes)
	}
}

// TestTheWakeLogHoldsOneRecordPerModelCall — the decision model's
// answer and the chat model's turn are two records.
func TestTheWakeLogHoldsOneRecordPerModelCall(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Decider: decider, Provider: provider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the step failed",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "question: Which branch?")

	r := chatRun(t, svc, "obs-1")
	wakes := r.wakeRecords()
	if len(wakes) != 2 {
		t.Fatalf("the run holds %d wake records, want 2: %+v", len(wakes), wakes)
	}
	if wakes[0].Who != decisionDecider {
		t.Errorf("the first record is %q, want %q", wakes[0].Who, decisionDecider)
	}
	if wakes[1].Who != decisionChat {
		t.Errorf("the second record is %q, want %q", wakes[1].Who, decisionChat)
	}
	if !strings.Contains(wakes[1].What, "ask_human") {
		t.Errorf("the second record does not name the tool call: %q", wakes[1].What)
	}
	if wakes[0].Why == "" {
		t.Error("the first record does not say why the run woke the model")
	}
	for i, rec := range wakes {
		if rec.Round != 1 {
			t.Errorf("the record %d is from round %d, want the round of the job the run is on", i, rec.Round)
		}
	}
}

// TestTheWakeLogSurvivesTheDaemon — the wake log is persisted with the
// rest of the run.
func TestTheWakeLogSurvivesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the step failed",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")
	waitStateFile(t, dir)

	r := chatRun(t, svc, "obs-1")
	raw, err := os.ReadFile(filepath.Join(dir, "runs", r.id+".json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state persistedRun
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if len(state.Wakes) != 1 {
		t.Fatalf("the state file holds %d wake records, want 1", len(state.Wakes))
	}
	if state.Wakes[0].Who != decisionDecider {
		t.Errorf("the persisted record is %q, want %q", state.Wakes[0].Who, decisionDecider)
	}

	restored := svc.restoreRun(state)
	if wakes := restored.wakeRecords(); len(wakes) != 1 {
		t.Errorf("the restored run holds %d wake records, want 1", len(wakes))
	}
}

// TestTheDaemonShutdownPostsNoSummary — the daemon is going away, not
// the run: the run keeps its state and resumes when it comes back.
func TestTheDaemonShutdownPostsNoSummary(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Decider: &scriptedDecider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := chatRun(t, svc, "obs-1")
	before := op.Posted()
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitDone(t, r)

	if op.Posted() != before {
		t.Errorf("the run posted %d cards after the shutdown, want none", op.Posted()-before)
	}
	for _, c := range op.Cards() {
		if strings.HasSuffix(c.Title, " · stopped") {
			t.Errorf("the run posted a summary card: %+v", c)
		}
	}
}
