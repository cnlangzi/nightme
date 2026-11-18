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

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
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
// The run is over when its goroutine is: the record is posted before
// that, so a test that waits here sees the whole card.
func waitDone(t *testing.T, r *run) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish")
	}
}

// recordCardOf returns the card the record was posted as.
func recordCardOf(t *testing.T, op *fakeOperator) cardRecord {
	t.Helper()
	cards := op.Cards()
	for i := len(cards) - 1; i >= 0; i-- {
		if cards[i].Posted {
			return cards[i]
		}
	}
	t.Fatalf("the run posted no record card: %+v", cards)
	return cardRecord{}
}

// recordEntries returns the entries the record card carries.
//
// The record is one entry: the whole body of the card is one reply,
// not one reply per line.
func recordEntries(t *testing.T, op *fakeOperator) []string {
	t.Helper()
	card := recordCardOf(t, op)
	out := make([]string, 0, 4)
	for _, m := range op.EmittedOf(messages.OutReply) {
		if m.ReplyTo == card.ID {
			out = append(out, m.Text)
		}
	}
	return out
}

// recordBody renders the entries of the record card as one body.
func recordBody(t *testing.T, op *fakeOperator) string {
	t.Helper()
	return strings.Join(recordEntries(t, op), "\n")
}

// TestStop_PostsTheRecordOfTheSession — the operator stops the run:
// the chat gets the record of the session.
func TestStop_PostsTheRecordOfTheSession(t *testing.T) {
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
	waitAsked(t, op, "Which step should run next?")

	before := svc.Status("obs-1")
	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !r.wasStoppedByOperator() {
		t.Error("the run does not know that the operator stopped it")
	}
	waitDone(t, r)

	body := recordBody(t, op)
	card := recordCardOf(t, op)
	// The card says what it is: the run is finishing.
	if card.Body != "the run is finishing…" {
		t.Errorf("the record card says %q, want the placeholder of the record", card.Body)
	}
	// The footer of the card carries the phase the run ended in.
	var phase bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.ReplyTo == card.ID && m.Result == string(PhaseStopped) {
			phase = true
		}
	}
	if !phase {
		t.Errorf("the record card carries no phase in its footer: %v", op.EmittedOf(messages.OutResult))
	}
	for _, want := range []string{
		"■ [ one ] stopped",
		"duration: ",
		"jobs:     main × 1",
		"actions:  1 (1 failed)",
		"steps:    1 ran · 0 not run",
		"errors:",
		"└─ [ ! ] /gtw commit — failed: the issue is claimed by another run (reason: issue_claimed)",
		"decisions:",
		"└─ the decision model — escalate (p=0.10)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the record does not carry %q:\n%s", want, body)
		}
	}
	// The record is one entry: one reply per line would render the
	// card as a paragraph per line.
	if entries := recordEntries(t, op); len(entries) != 1 {
		t.Errorf("the record card carries %d entries, want the whole body in one: %v", len(entries), entries)
	}
	// The counters of the run ride in the tree of the rounds: a card
	// that counts them a second time says two things about one number.
	for _, unwanted := range []string{"wake-ups:", "system-1:", "system-2:", "tokens:"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the record carries a %q of its own:\n%s", unwanted, body)
		}
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

// TestTheRecordIsPostedWhenTheRunEndsOnItsOwn — a run that finishes
// its workflow leaves a record too: the session is worth recording
// however it ended.
func TestTheRecordIsPostedWhenTheRunEndsOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{okStep("the commit landed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	body := recordBody(t, op)
	for _, want := range []string{
		"■ [ one ] done",
		"actions:  1 (1 ok)",
		"steps:    1 ran · 0 not run",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the record does not carry %q:\n%s", want, body)
		}
	}
	// A run that went well has nothing to report about errors and
	// decisions.
	if strings.Contains(body, "\nerrors:") || strings.Contains(body, "\ndecisions:") {
		t.Errorf("the record lists what did not happen:\n%s", body)
	}
}

// TestStop_EndsTheRunSessionWithItsPhase — the run's card says how
// the run ended.
func TestStop_EndsTheRunSessionWithItsPhase(t *testing.T) {
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
	waitAsked(t, op, "Which step should run next?")

	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitDone(t, r)

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

// TestTheRecordComesAfterTheRunUnwinds — the step that was in flight
// when the operator stopped the run counts as a step that did not run.
func TestTheRecordComesAfterTheRunUnwinds(t *testing.T) {
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

	body := recordBody(t, op)
	for _, want := range []string{
		"actions:  1 (1 ok)",
		"steps:    1 ran · 1 not run",
		"stopped while: /gtw commit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the record does not carry %q:\n%s", want, body)
		}
	}
}

// TestTheRecordListsOnlyTheActionsThatWentWrong — the actions that
// went well are counted, not listed.
func TestTheRecordListsOnlyTheActionsThatWentWrong(t *testing.T) {
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
	waitAsked(t, op, "Which step should run next?")

	r := chatRun(t, svc, "obs-1")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitDone(t, r)

	body := recordBody(t, op)
	if !strings.Contains(body, "actions:  2 (1 ok · 1 failed)") {
		t.Errorf("the record does not count the actions:\n%s", body)
	}
	if strings.Contains(body, "/gtw fix 1 -y —") {
		t.Errorf("the record lists an action that went well:\n%s", body)
	}
	if !strings.Contains(body, "└─ [ ! ] /gtw commit — failed: the commit failed") {
		t.Errorf("the record does not list the action that failed:\n%s", body)
	}
}

// TestTheRecordCapsTheErrorsAndTheDecisions — a long session keeps
// the card readable; the totals stay in the facts.
func TestTheRecordCapsTheErrorsAndTheDecisions(t *testing.T) {
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

	body := r.recordCard()
	for _, want := range []string{
		"errors (last 5 of 6):",
		"decisions (last 10 of 23):",
		"actions:  6 (6 failed)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the record does not carry %q:\n%s", want, body)
		}
	}
	// Five errors and ten decisions: four rows and nine rows carry the
	// branch prefix, the last row of each block the last prefix.
	if got := countLines(body, "├─ "); got != 13 {
		t.Errorf("the record lists %d branch rows, want 13", got)
	}
	if got := countLines(body, "└─ "); got != 2 {
		t.Errorf("the record lists %d last rows, want 2", got)
	}

	// A short session carries the plain headings.
	r.policy.History = r.policy.History[:2]
	r.wakes = r.wakes[:3]
	body = r.recordCard()
	for _, want := range []string{"\nerrors:\n", "\ndecisions:\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("the record does not carry %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "(last") {
		t.Errorf("the record counts a short session:\n%s", body)
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

// TestTheRecordClipsItsRows — a card that carries a whole build log
// is unreadable, and a card that is too long is refused.
func TestTheRecordClipsItsRows(t *testing.T) {
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

	body := r.recordCard()
	for _, line := range strings.Split(body, "\n") {
		if n := len([]rune(line)); n > maxSummaryLine {
			t.Errorf("a line of the record is %d runes long, want at most %d: %q", n, maxSummaryLine, line)
		}
	}
	// The reason code is the machine-readable part of the row: the cut
	// must not swallow it.
	if !strings.Contains(body, "reason: agent_error") {
		t.Errorf("the record lost the reason code:\n%s", body)
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
	waitAsked(t, op, "Which branch?")

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
	waitAsked(t, op, "Which step should run next?")
	waitStateFile(t, dir)

	r := chatRun(t, svc, "obs-1")
	raw := readStateFile(t, filepath.Join(dir, "runs", r.id+".json"))
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

// TestTheDaemonShutdownPostsNoRecord — the daemon is going away, not
// the run: the run keeps its state and resumes when it comes back.
func TestTheDaemonShutdownPostsNoRecord(t *testing.T) {
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
}
