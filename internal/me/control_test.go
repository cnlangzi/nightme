package me

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// waitForStatus waits until the run of a chat stands in the given
// phase.
func waitForStatus(t *testing.T, svc *Service, chatID string, phase Phase) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if svc.Snapshot(chatID).Phase == phase {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run is %q, want %q", svc.Snapshot(chatID).Phase, phase)
}

// TestPause_DoesNotInterruptTheRunningStep — a pause stops the run
// after the step it is on: that step's outcome is recorded.
func TestPause_DoesNotInterruptTheRunningStep(t *testing.T) {
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
	// The step that was running is not interrupted.
	time.Sleep(50 * time.Millisecond)
	if st := svc.Snapshot("obs-1"); st.Paused {
		t.Fatal("the run paused while its step was running")
	}

	// The step ends, and the run pauses.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !st.Paused {
		t.Error("the run does not report that it is paused")
	}
	if st.PauseReason == "" {
		t.Error("the run does not say why it paused")
	}
}

// TestResume_RunsTheStepAgain — the operator runs the step the run is
// on again: its command goes to the chat a second time.
func TestResume_RunsTheStepAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		failedStep("the commit failed"),
		failedStep("the commit failed again"),
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	// The run is taken before the command: a run that is over frees
	// its chat, and the run of a chat that is over is gone.
	r := activeRun(t, svc, "obs-1")

	step, err := svc.Resume(context.Background(), "obs-1", "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if step != "commit" {
		t.Errorf("the command acted on %q, want commit", step)
	}

	// The step's command goes to the chat a second time.
	waitSentCount(t, op, 2)

	waitForAction(t, r, wfe.ActionKindRun, "commit", 2)

	// The budget of the step starts over before the retry.
	history := runHistory(r)
	reset, retry := -1, -1
	for i, rec := range history {
		if rec.StepID != "commit" {
			continue
		}
		switch rec.Kind {
		case wfe.ActionKindReset:
			reset = i
		case wfe.ActionKindRun:
			retry = i
		}
	}
	if reset < 0 {
		t.Fatalf("the history holds no reset of the step: %+v", history)
	}
	if retry < 0 {
		t.Fatalf("the history holds no run of the step: %+v", history)
	}
	if reset > retry {
		t.Errorf("the reset comes after the retry: %+v", history)
	}

	// The retry's verdict is the step's verdict.
	out, ok := stepOutcome(r, "commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if out.Summary != "the commit failed again" {
		t.Errorf("the step ended as %q, want the verdict of the retry", out.Summary)
	}
}

// TestResume_CarriesTheNote — the note is the operator's
// instruction: it joins the conversation the chat model reads.
func TestResume_CarriesTheNote(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{failedStep("the commit failed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	// The run is taken before the command: a run that is over frees
	// its chat, and the run of a chat that is over is gone.
	r := activeRun(t, svc, "obs-1")

	if _, err := svc.Resume(context.Background(), "obs-1", "use the other branch"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	waitForNote(t, r, "use the other branch")
}

// waitForNote waits until the note of the operator joins the
// conversation the chat model reads.
//
// The run applies the note in its own turn: the operator hands the
// note over, and the run joins it to the conversation a moment later.
func waitForNote(t *testing.T, r *run, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conversationCarries(r, text) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the note %q did not join the conversation", text)
}

// TestTheOperatorActionIsInTheConversation — the model sees what the
// operator did: the action is a synthetic assistant tool call with its
// result.
func TestTheOperatorActionIsInTheConversation(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{failedStep("the commit failed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	r := activeRun(t, svc, "obs-1")
	if _, err := svc.Next(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("next: %v", err)
	}
	waitForAction(t, r, wfe.ActionKindRun, "commit", 2)

	// The model sees what the operator did: the action is a synthetic
	// assistant tool call with its result.
	waitForToolCall(t, r, toolNext)
}

// TestNext_SkipsTheStep — the operator skips the step the run is on:
// the plan goes on with the workflow.
func TestNext_SkipsTheStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the commit failed",
			Data:    map[string]string{"error": "the commit failed", "reason": "conflict"},
		}},
		okStep("pushed"),
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	// The run is taken before the command: a run that is over frees
	// its chat, and the run of a chat that is over is gone.
	r := activeRun(t, svc, "obs-1")

	step, err := svc.Next(context.Background(), "obs-1", "")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if step != "commit" {
		t.Errorf("the command acted on %q, want commit", step)
	}

	waitForAction(t, r, wfe.ActionKindRun, "commit", 2)

	// The step is recorded as skipped: the facts it produced are kept
	// and the error is gone.
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	out := st.Outcome
	if out.Status != wfe.OutcomeNoop {
		t.Errorf("the step ended as %q, want the skipped one", out.Status)
	}
	if out.Summary != operatorSkippedSummary {
		t.Errorf("the summary is %q, want %q", out.Summary, operatorSkippedSummary)
	}
	if out.Data["reason"] != "conflict" {
		t.Errorf("the facts are %v, want the ones the step produced", out.Data)
	}
	if _, ok := out.Data[wfe.OutcomeKeyError]; ok {
		t.Errorf("the error is still on the step: %v", out.Data)
	}
	if st.Skipped {
		t.Error("the step is marked as never run: it did run")
	}

	// The history agrees with the run state.
	var skipped bool
	for _, rec := range runHistory(r) {
		if rec.Kind != wfe.ActionKindRun || rec.StepID != "commit" {
			continue
		}
		if rec.Outcome.Status == wfe.OutcomeNoop && rec.Outcome.Summary == operatorSkippedSummary {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("the history holds no skipped run of the step: %+v", runHistory(r))
	}

	// The plan goes on with the workflow.
	waitSent(t, op, "/gtw push")
}

// TestNext_SkipsTheStepTheJudgmentIsAbout — the step the command acts
// on is the subject of the judgment, not the step that comes next.
func TestNext_SkipsTheStepTheJudgmentIsAbout(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		failedStep("the commit failed"),
		okStep("pushed"),
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	// The run is taken before the command: a run that is over frees
	// its chat, and the run of a chat that is over is gone.
	r := activeRun(t, svc, "obs-1")

	if _, err := svc.Next(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("next: %v", err)
	}

	waitSent(t, op, "/gtw push")

	// The step that failed is the one that was skipped.
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if st.Outcome.Status != wfe.OutcomeNoop {
		t.Errorf("the commit step ended as %q, want the skipped one", st.Outcome.Status)
	}
	pushed := waitForStep(t, r, "push")
	if pushed.Outcome.Status == wfe.OutcomeNoop {
		t.Error("the push step was skipped although it ran")
	}
}

// TestNextOnAStepThatSucceededChangesNothing — a step that did its job
// has nothing to skip: the plan goes on and the step keeps its verdict.
func TestNextOnAStepThatSucceededChangesNothing(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "two")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{Status: wfe.OutcomeOK, Summary: "committed"})

	if err := svc.applyNext(context.Background(), r, "commit"); err != nil {
		t.Fatalf("next: %v", err)
	}
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if st.Outcome.Status != wfe.OutcomeOK {
		t.Errorf("the step ended as %q, want the one it had", st.Outcome.Status)
	}
	if st.Outcome.Summary != "committed" {
		t.Errorf("the summary is %q, want the one the step had", st.Outcome.Summary)
	}
}

// TestResume_AfterATakeoverRunsTheBrokenStepAgain — the operator took
// the chat back over a step that broke: the run is on that step, and
// the operator's word runs it again.
func TestResume_AfterATakeoverRunsTheBrokenStepAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		{open: true},
		failedStep("the commit failed again"),
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	// Somebody takes the chat back while the step is running.
	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})

	// The step breaks.
	if err := op.fail("obs-1", "the commit failed"); err != nil {
		t.Fatalf("break the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	// The run is on the step that broke: the operator's word runs it
	// again.
	step, err := svc.Resume(context.Background(), "obs-1", "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if step != "commit" {
		t.Errorf("the command acted on %q, want the step that broke", step)
	}
	waitSentCount(t, op, 2)
}

// TestTheTargetStep_IsTheStepTheRunWorkedOn — a run that has no
// judgment is on the step it ran last, when that step did not succeed.
// A step that succeeded is not the step to act on: the run is on the
// next step of the plan.
func TestTheTargetStep_IsTheStepTheRunWorkedOn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "two")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.recordStep("main", commit, wfe.Outcome{Status: wfe.OutcomeFailed, Summary: "the commit failed"})
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
	})

	step, ok := r.targetStep()
	if !ok {
		t.Fatal("the run is on no step")
	}
	if step.ID != "commit" {
		t.Errorf("the run is on %q, want the step it worked on", step.ID)
	}

	// The judgment wins over the step the run worked on.
	r.setJudgmentStep("push")
	step, ok = r.targetStep()
	if !ok {
		t.Fatal("the run is on no step")
	}
	if step.ID != "push" {
		t.Errorf("the run is on %q, want the step the judgment is about", step.ID)
	}
	r.setJudgmentStep("")

	// A step that succeeded is not the step to act on: the run is on
	// the next step of the plan.
	r.recordStep("main", commit, wfe.Outcome{Status: wfe.OutcomeOK, Summary: "committed"})
	step, ok = r.targetStep()
	if !ok {
		t.Fatal("the run is on no step")
	}
	if step.ID != "push" {
		t.Errorf("the run is on %q, want the next step of the plan", step.ID)
	}
}

// TestTheGate_IgnoresAStaleWake — a wake that came before the run
// waited belongs to a pause the operator already took back: it must
// not swallow the next pause.
func TestTheGate_IgnoresAStaleWake(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	r.wake()
	r.pauseNow("the operator paused the run")

	done := make(chan error, 1)
	go func() { done <- svc.checkGate(context.Background(), r) }()

	select {
	case err := <-done:
		t.Fatalf("the run went on although it is paused: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The operator hands the control back: the run goes on.
	r.wake()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the gate: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not go on")
	}
}

// TestTheBusyStep_IsTheStepACommandIsOn — the run waits for a command:
// the step it is on is busy, and a step cannot be skipped.
func TestTheBusyStep_IsTheStepACommandIsOn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", commitPushWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "two")

	r.setJudgmentStep("commit")
	r.setInflight(&inflightState{MsgID: "msg-1", Command: "/gtw commit"})

	r.mu.Lock()
	busy := r.busyStepIDLocked()
	r.mu.Unlock()
	if busy != "commit" {
		t.Errorf("the busy step is %q, want the step the command is on", busy)
	}
}

// TestTheRoundCardShowsTheErrorOfABrokenStep — a step that broke with
// a noop verdict is not a step that was passed over: its error is on
// the row.
func TestTheRoundCardShowsTheErrorOfABrokenStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeNoop,
		Summary: "nothing to do",
		Data:    map[string]string{"error": "the worktree is gone"},
	})

	items := r.roundTasks("main", 1)
	if len(items) != 1 {
		t.Fatalf("the round holds %d steps, want 1", len(items))
	}
	if !strings.Contains(items[0].ActiveForm, "the worktree is gone") {
		t.Errorf("the row says %q, want the error of the step", items[0].ActiveForm)
	}
}

// TestTheCommandsRefuseARunThatIsNotWaiting — the commands act on a
// paused run and on nothing else.
func TestTheCommandsRefuseARunThatIsNotWaiting(t *testing.T) {
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

	// The run is working: it is not paused.
	_, err := svc.Resume(context.Background(), "obs-1", "")
	if err == nil {
		t.Fatal("the run was resumed although it is working")
	}
	if !strings.Contains(err.Error(), "this run is not paused") {
		t.Errorf("the refusal is %q, want the reason", err)
	}
	if reason := RefusalReason(err); reason != ReasonNotPaused {
		t.Errorf("the reason is %q, want %q", reason, ReasonNotPaused)
	}

	// A chat without a run has nothing to act on.
	_, err = svc.Next(context.Background(), "obs-2", "")
	if err == nil {
		t.Fatal("a chat without a run was skipped")
	}
	if !strings.Contains(err.Error(), "this chat has no run in it") {
		t.Errorf("the refusal is %q, want the reason", err)
	}
	if reason := RefusalReason(err); reason != ReasonNoRun {
		t.Errorf("the reason is %q, want %q", reason, ReasonNoRun)
	}
}

// TestTheCommandsRefuseARunThatIsAsking — a question is answered with
// `/me reply`, not with a decision about the step.
func TestTheCommandsRefuseARunThatIsAsking(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")

	for _, cmd := range []struct {
		name string
		call func() (string, error)
	}{
		{"resume", func() (string, error) { return svc.Resume(context.Background(), "obs-1", "") }},
		{"next", func() (string, error) { return svc.Next(context.Background(), "obs-1", "") }},
	} {
		_, err := cmd.call()
		if err == nil {
			t.Fatalf("the run was %sd although it is asking", cmd.name)
		}
		if !strings.Contains(err.Error(), "waiting for an answer") {
			t.Errorf("the refusal is %q, want the reason", err)
		}
		if reason := RefusalReason(err); reason != ReasonNotPaused {
			t.Errorf("the reason is %q, want %q", reason, ReasonNotPaused)
		}
	}
}

// TestTheDirectiveSurvivesARestart — the operator's decision is in the
// run's state: a daemon that dies in between comes back and applies
// it.
func TestTheDirectiveSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	op := newFakeOperator()
	op.steps = []fakeStep{
		failedStep("the commit failed"),
		failedStep("the commit failed again"),
	}
	cfg := Config{
		Provider:     &scriptedProvider{},
		MaxJudgments: 1,
		Operator:     op,
		WorkflowsDir: dir,
		StateDir:     filepath.Join(dir, "runs"),
	}
	svc := New(cfg)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	// The daemon goes away before the run applies the decision.
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if _, err := svc.Resume(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// The state of the run holds the decision.
	state := waitForDirective(t, filepath.Join(dir, "runs", runID+".json"))
	directive, ok := state["directive"].(map[string]any)
	if !ok {
		t.Fatalf("the state holds no directive: %v", state["directive"])
	}
	if directive["kind"] != "resume" {
		t.Errorf("the state holds the directive %v, want resume", directive["kind"])
	}
	if directive["step"] != "commit" {
		t.Errorf("the state holds the step %v, want commit", directive["step"])
	}

	resumed := New(cfg)
	t.Cleanup(func() { _ = resumed.Shutdown(context.Background()) })
	if err := resumed.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// The restored run applies the decision on its first turn: the
	// step's command goes to the chat a second time.
	waitSentCount(t, op, 2)
}

// TestResume_CancelsAPauseThatHasNotTakenEffect — the operator changed
// their mind before the step ended: the run does not pause.
func TestResume_CancelsAPauseThatHasNotTakenEffect(t *testing.T) {
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
	if _, err := svc.Resume(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	r := activeRun(t, svc, "obs-1")
	r.mu.Lock()
	paused, requested := r.paused, r.pauseRequested
	r.mu.Unlock()
	if paused || requested {
		t.Errorf("the run is still paused (paused %t, requested %t)", paused, requested)
	}

	// The step ends: the run goes on instead of pausing.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if st := svc.Snapshot("obs-1"); st.Paused {
		t.Fatal("the run paused although the operator cancelled the pause")
	}
}

// TestNext_RefusesAStepThatIsStillRunning — the work is in flight: it
// cannot be skipped.
func TestNext_RefusesAStepThatIsStillRunning(t *testing.T) {
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
	_, err := svc.Next(context.Background(), "obs-1", "")
	if err == nil {
		t.Fatal("a step that is still running was skipped")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Errorf("the refusal is %q, want it to name the step", err)
	}
	if reason := RefusalReason(err); reason != ReasonStepRunning {
		t.Errorf("the reason is %q, want %q", reason, ReasonStepRunning)
	}
}

// TestTheRoundCardShowsASkippedStep — a step whose outcome is noop is
// rendered as skipped, and the row says why.
func TestTheRoundCardShowsASkippedStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{failedStep("the commit failed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	r := activeRun(t, svc, "obs-1")
	if _, err := svc.Next(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("next: %v", err)
	}
	waitForAction(t, r, wfe.ActionKindRun, "commit", 2)

	items := r.roundTasks("main", 1)
	if len(items) != 1 {
		t.Fatalf("the round holds %d steps, want 1", len(items))
	}
	if items[0].Status != agent.TaskSkipped {
		t.Errorf("the row is %q, want the skipped one", items[0].Status)
	}
	if !strings.Contains(items[0].ActiveForm, operatorSkippedSummary) {
		t.Errorf("the row says %q, want the summary of the skip", items[0].ActiveForm)
	}
}

// TestTheQuestionIsOfferedAsAChoiceCard — the options the run is
// waiting for become the chat's own choice card, so a click is the
// answer.
func TestTheQuestionIsOfferedAsAChoiceCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	choices := op.EmittedOf(messages.OutChoice)
	if len(choices) == 0 {
		t.Fatalf("the run published no choice: %v", op.Emitted())
	}
	choice := choices[0].Choice
	if choice == nil {
		t.Fatal("the choice carries no payload")
	}
	if choice.RequestID == "" {
		t.Error("the choice has no request id: the answer could not be correlated")
	}
	found := false
	for _, option := range choice.Options {
		if option.Label == "/gtw commit" {
			found = true
		}
		if option.Label == "escalate" {
			t.Errorf("the card offers the operator %q, want the options of the run", option.Label)
		}
	}
	if !found {
		t.Fatalf("the choice has no option of the run: %+v", choice.Options)
	}
}

// TestAClickOnTheChoiceIsTheAnswerOfTheRun — a click on an option of
// the choice card reaches the run.
func TestAClickOnTheChoiceIsTheAnswerOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	choice := waitChoice(t, op)
	if !op.Pick(choice.RequestID, "0") {
		t.Fatal("the click did not reach the run")
	}
	waitRun(t, svc, "obs-1")
}

// TestTheQuestionIsNotDispatched — the question belongs to the run,
// not to the agent: the agent never sees what the run asked.
func TestTheQuestionIsNotDispatched(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")

	asked := op.Asked()
	if len(asked) != 1 {
		t.Fatalf("the run asked %d questions, want 1", len(asked))
	}
	if !strings.Contains(asked[0].Text, "Which branch?") {
		t.Errorf("the question is %q, want the model's question", asked[0].Text)
	}
	for _, m := range op.Spoken() {
		if strings.Contains(m.Text, "Which branch?") {
			t.Fatalf("the question was dispatched to the agent: %+v", m)
		}
	}
}

// TestANumberedAnswerIsTheOption — the card's buttons carry the
// number of the option, because a chat button has room for a few
// bytes only.
func TestANumberedAnswerIsTheOption(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	if err := svc.Reply(context.Background(), "obs-1", "0"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitSent(t, op, "/gtw commit")
}

// TestOneIterationGetsOneCard — a card covers every step of a job in
// one round: the run does not leave a card behind per step.
func TestOneIterationGetsOneCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two steps.
jobs:
  main:
    steps:
      - id: one
        message: /gtw commit
      - id: two
        message: /gtw push
`)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The card of the run and the card of the round: every session
	// owns one card.
	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want one for the run and one for the round", posted)
	}
}

// TestTheRoundCardHangsUnderTheMessageThatStartedTheRun — the round
// is the run's work, one level below the message that started the run.
func TestTheRoundCardHangsUnderTheMessageThatStartedTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	cards := op.Cards()
	if len(cards) != 1 {
		t.Fatalf("the run published %d cards, want the card of the round", len(cards))
	}
	if cards[0].ReplyTo != "om_start" {
		t.Errorf("the round card hangs under %q, want the start message om_start", cards[0].ReplyTo)
	}
}

// TestEveryRoundGetsItsOwnCard — a job that runs several rounds gets
// a card per round, and the card of the round that ended stays in the
// chat with the content it had.
func TestEveryRoundGetsItsOwnCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repeat", `title: repeat.
jobs:
  main:
    iterations: 2
    steps:
      - id: fix
        message: /gtw fix
`)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repeat", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 3 {
		t.Fatalf("the run published %d cards, want the card of the run and one per round", posted)
	}

	// The card of the second round stands in front of the command of
	// that round.
	log := op.Log()
	first := indexOf(log, "speak:/gtw fix")
	second := indexOfAfter(log, "speak:/gtw fix", first+1)
	if first < 0 || second < 0 {
		t.Fatalf("the run did not send the command of both rounds: %v", log)
	}
	published := false
	for _, entry := range log[first+1 : second] {
		if strings.HasPrefix(entry, "card:") {
			published = true
		}
	}
	if !published {
		t.Fatalf("the card of the second round came after its first command: %v", log)
	}

	// The card of the round that ended keeps its content: the round
	// session is closed with its verdict.
	rounds := op.RoundSessions()
	if len(rounds) != 2 {
		t.Fatalf("the run opened %d round sessions, want one per round", len(rounds))
	}
	if rounds[0] != "main · round 1" || rounds[1] != "main · round 2" {
		t.Errorf("the round sessions are %v, want one per round", rounds)
	}
}

// TestAnotherJobGetsACardOfItsOwn — a run that moves to another job
// gets a card of its own, and the card of the job that ended keeps the
// content it had.
func TestAnotherJobGetsACardOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 3 {
		t.Fatalf("the run published %d cards, want the card of the run and one per job", posted)
	}
	rounds := op.RoundSessions()
	if len(rounds) != 2 {
		t.Fatalf("the run opened %d round sessions, want one per job", len(rounds))
	}
	if rounds[0] != "build · round 1" || rounds[1] != "publish · round 1" {
		t.Errorf("the round sessions are %v, want one per job", rounds)
	}
}

// TestTheRunCardCarriesThePhaseInItsFooter — the run's card says how
// the run ended: the verdict rides in the footer of the session.
func TestTheRunCardCarriesThePhaseInItsFooter(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	results := op.EmittedOf(messages.OutResult)
	if len(results) == 0 {
		t.Fatalf("the run published no result: %v", op.Emitted())
	}
	found := false
	for _, m := range results {
		if m.Result == "ship is complete" {
			found = true
		}
	}
	if !found {
		t.Errorf("the run published no verdict of its outcome: %v", results)
	}
}

// TestAQuestionOpensNoCardOfItsOwn — a run that asks a question keeps
// the cards it has: the question is a choice, not a session.
func TestAQuestionOpensNoCardOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	before := op.Posted()

	if err := svc.Reply(context.Background(), "obs-1", "0"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")
	// The question opened no card of its own: the only card the run
	// published after it is the record of the finished run.
	if posted := op.Posted(); posted != before+1 {
		t.Fatalf("the run published %d cards, want the record alone", posted-before)
	}
	card := lastPostedCard(op)
	if body := strings.Join(op.Entries(card.ID), "\n"); !strings.Contains(body, "■ [ one ] done") {
		t.Errorf("the run published a card that is not its record: %+v", card)
	}
}

// TestTheModelPathFollowsTheStepOfTheCommand — the card follows the
// job of the step the model runs.
func TestTheModelPathFollowsTheStepOfTheCommand(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	rounds := op.RoundSessions()
	if len(rounds) < 2 || rounds[len(rounds)-1] != "publish · round 1" {
		t.Errorf("the card does not follow the step of the command: %v", rounds)
	}
}

// TestAnIterationKeepsItsCard — a run that comes back to an iteration
// goes on with the card that iteration has: one card per iteration,
// not one per visit.
func TestAnIterationKeepsItsCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", buildTagWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"args": "push"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "tagged"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The run was in the build job, then in the publish job, and back
	// in the build job: two rounds, two cards. The round the run came
	// back to keeps the card it had.
	rounds := op.RoundSessions()
	if len(rounds) != 2 {
		t.Fatalf("the run opened %d round sessions, want one per round", len(rounds))
	}
	if rounds[0] != "build · round 1" || rounds[1] != "publish · round 1" {
		t.Errorf("the round sessions are %v, want one per round", rounds)
	}
}

// TestACardTheChatRefusedIsPublishedLater — a card the chat refuses
// does not stop the run: the iteration gets its card as soon as the
// chat takes one.
func TestACardTheChatRefusedIsPublishedLater(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", publishTwiceWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	logger := &recordingLogger{}
	svc, op := testService(t, dir, Config{Provider: provider, Logger: slog.New(logger)})
	// The chat refuses the card of the publish iteration — the second
	// attempt — and takes the one that comes after it.
	op.refuseAttempt = 2
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "tagged"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !logged(logger, "the session card could not be posted") {
		t.Errorf("the failure was not logged: %v", logger.Lines())
	}
	if posted := op.Posted(); posted < 2 {
		t.Fatalf("the run published %d cards, want the card of the run and the one the chat took later", posted)
	}
}

// TestTheCardOfAnAnsweredQuestionOffersNoChoice — the card the run
// leaves behind does not offer the answer of a question that is
// answered.
func TestTheCardOfAnAnsweredQuestionOffersNoChoice(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw push", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")
	// The choice follows the question: the run publishes it a moment
	// later.
	waitChoice(t, op)
	choices := op.EmittedOf(messages.OutChoice)
	if len(choices) != 1 {
		t.Fatalf("the run published %d choices, want one", len(choices))
	}

	// The operator picks one: the run goes on in another round.
	if err := svc.Reply(context.Background(), "obs-1", "1"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The question is asked once: the run does not ask it again.
	if again := op.EmittedOf(messages.OutChoice); len(again) != 1 {
		t.Fatalf("the run published %d choices, want one", len(again))
	}
}

// TestACardThatCannotBePublishedDoesNotStopTheRun — the card is for
// people: the work goes on when the chat refuses it.
func TestACardThatCannotBePublishedDoesNotStopTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	logger := &recordingLogger{}
	cfg := noModels()
	cfg.Logger = slog.New(logger)
	svc, op := testService(t, dir, cfg)
	// The chat takes the card the run starts with and refuses the
	// ones that come after it.
	op.failCardAfter = 1

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 2 || sent[1] != "/gtw push" {
		t.Fatalf("the chat got %v, want both steps of the run", sent)
	}
	if posted := op.Posted(); posted != 1 {
		t.Errorf("the run published %d cards, want the one the chat took", posted)
	}
	if !logged(logger, "the session card could not be posted") {
		t.Errorf("the failure was not logged: %v", logger.Lines())
	}
}

// TestResume_KeepsTheCardItOwns — a run that comes back after a
// daemon restart keeps the card it owns: the card's iteration travels
// with the run's state.
func TestResume_KeepsTheCardItOwns(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	op := newFakeOperator()
	op.steps = []fakeStep{{open: true}}
	cfg := Config{
		Provider:     &scriptedProvider{},
		Operator:     op,
		WorkflowsDir: dir,
		StateDir:     filepath.Join(dir, "runs"),
	}
	svc := New(cfg)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	// The state carries the anchors of the open sessions: a run that
	// comes back goes on with the cards it opened.
	state := waitForSession(t, filepath.Join(dir, "runs", runID+".json"))
	runSession, ok := state["run_session"].(map[string]any)
	if !ok || runSession["anchor"] != "om_start" {
		t.Fatalf("the state holds no anchor of the run: %v", state["run_session"])
	}
	if runSession["owns"] == true {
		t.Errorf("the run owns the message that started it: %v", state["run_session"])
	}
	roundSession, ok := state["round_session"].(map[string]any)
	if !ok || roundSession["anchor"] == "" {
		t.Fatalf("the state holds no card of the round: %v", state["round_session"])
	}
	if roundSession["job"] != "main" || roundSession["round"] != float64(1) {
		t.Errorf("the state names the round %v, want main round 1", roundSession)
	}

	// The daemon goes away and comes back.
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	posted := op.Posted()

	resumed := New(cfg)
	t.Cleanup(func() { _ = resumed.Shutdown(context.Background()) })
	if err := resumed.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// The resumed run stays in the same iteration: it publishes no
	// card of its own.
	time.Sleep(100 * time.Millisecond)
	if op.Posted() != posted {
		t.Errorf("the resumed run published %d cards, want none", op.Posted()-posted)
	}

	// The chat has forgotten the cards the run opened: the run tells
	// it again, so the chat patches them instead of sending a second
	// card for the same session.
	adopted := op.Adopted()
	if len(adopted) == 0 {
		t.Fatalf("the resumed run told the chat about no card: %v", adopted)
	}
	want := map[string]bool{}
	for _, card := range op.Cards() {
		want[card.ID] = true
	}
	for _, cardID := range adopted {
		if !want[cardID] {
			t.Errorf("the run told the chat about %q, want a card it owns", cardID)
		}
	}
	// The message that started the run is not a card of the run: the
	// chat must not patch a message the user sent.
	for _, cardID := range adopted {
		if cardID == "om_start" {
			t.Errorf("the run told the chat to adopt the message that started it: %v", adopted)
		}
	}
}

// twoJobWorkflow is a workflow of two jobs: the second one waits for
// the first.
const twoJobWorkflow = `title: two jobs.
jobs:
  build:
    steps:
      - id: commit
        message: /gtw commit
  publish:
    needs: [build]
    steps:
      - id: push
        message: /gtw push
`

// buildTagWorkflow is a workflow whose build job has two steps: the
// run comes back to it after it has worked somewhere else.
const buildTagWorkflow = `title: two jobs.
jobs:
  build:
    steps:
      - id: commit
        message: /gtw commit
      - id: tag
        message: /gtw tag
  publish:
    needs: [build]
    steps:
      - id: push
        message: /gtw push
`

// publishTwiceWorkflow is a workflow whose publish job has two steps:
// the run stays in that job for another turn.
const publishTwiceWorkflow = `title: two jobs.
jobs:
  build:
    steps:
      - id: commit
        message: /gtw commit
  publish:
    needs: [build]
    steps:
      - id: push
        message: /gtw push
      - id: tag
        message: /gtw tag
`

// commitPushWorkflow is a workflow of one job with two steps: the one
// that breaks and the one that comes after it.
const commitPushWorkflow = `title: two steps.
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`

// failedStep is a step that breaks.
func failedStep(summary string) fakeStep {
	return fakeStep{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: summary,
		Data:    map[string]string{"error": summary},
	}}
}

// conversationCarries reports whether the conversation holds the text.
func conversationCarries(r *run, text string) bool {
	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if p.Type == llm.PartText && strings.Contains(p.Text, text) {
				return true
			}
		}
	}
	return false
}

// waitForToolCall waits until the conversation holds a call of the tool
// and its result.
func waitForToolCall(t *testing.T, r *run, tool string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var call, result bool
		for _, m := range r.conversation() {
			for _, p := range m.Parts {
				switch p.Type {
				case llm.PartToolCall:
					if p.ToolName == tool {
						call = true
					}
				case llm.PartToolResult:
					if p.ToolName == tool {
						result = true
					}
				}
			}
		}
		if call && result {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the conversation holds no call of %q with its result", tool)
}

// waitForStep waits until the run state holds a record of the step.
func waitForStep(t *testing.T, r *run, stepID string) wfe.StepState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := r.stepState(stepID); ok {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run state holds no record of the step %q", stepID)
	return wfe.StepState{}
}

// waitForAction waits until the run's history holds that many actions
// of the kind for the step.
func waitForAction(t *testing.T, r *run, kind wfe.ActionKind, stepID string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, rec := range runHistory(r) {
			if rec.Kind == kind && rec.StepID == stepID {
				n++
			}
		}
		if n >= count {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the history holds %d %s records for %q, want %d", count, kind, stepID, count)
}

// waitForDirective waits until the state of a run holds the operator's
// decision.
func waitForDirective(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err == nil {
				if directive, ok := state["directive"].(map[string]any); ok && directive["kind"] != "" {
					return state
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the state of the run never held the operator's decision: %s", path)
	return nil
}

// lastCardBody returns the body that was written to the card with the
// given id last.
func lastCardBody(op *fakeOperator, id string) string {
	cards := op.Cards()
	body := ""
	for _, c := range cards {
		if c.ID == id {
			body = c.Body
		}
	}
	return body
}

// waitForPosted waits until the run published that many cards.
func waitForPosted(t *testing.T, op *fakeOperator, n int) {
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

// lastPostedCard returns the card the run published last.
func lastPostedCard(op *fakeOperator) cardRecord {
	cards := op.Cards()
	for i := len(cards) - 1; i >= 0; i-- {
		if cards[i].Posted {
			return cards[i]
		}
	}
	return cardRecord{}
}

// waitForCardUpdate waits until the run updates the card with the
// given id.
func waitForCardUpdate(t *testing.T, op *fakeOperator, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range op.Cards() {
			if c.ID == id && !c.Posted {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run never updated the card %q: %v", id, op.Cards())
}

// waitForSession waits until the run's state file carries the card of
// the round the run is in.
func waitForSession(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err == nil {
				if sess, ok := state["round_session"].(map[string]any); ok && sess["anchor"] != "" {
					return state
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run state never carried the card of the round: %s", path)
	return nil
}

// indexOf returns the position of the first entry that equals the
// text, or -1.
func indexOf(list []string, text string) int {
	for i, item := range list {
		if item == text {
			return i
		}
	}
	return -1
}

// indexOfAfter returns the position of the first entry after the
// given one that equals the text, or -1.
func indexOfAfter(list []string, text string, from int) int {
	for i := from; i < len(list); i++ {
		if list[i] == text {
			return i
		}
	}
	return -1
}

// TestTheDecisionCarriesTheReasonFirst — the reason the decision was
// taken is the first entry of the decision's card.
func TestTheDecisionCarriesTheReasonFirst(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
		Data:    map[string]string{"error": "the command failed"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	var reason string
	for _, m := range op.EmittedOf(messages.OutReply) {
		if strings.HasPrefix(m.Text, "reason: ") {
			reason = m.Text
		}
	}
	if reason == "" {
		t.Fatalf("the run published no reason for its decision: %v", op.EmittedOf(messages.OutReply))
	}
	if !strings.Contains(reason, "the command failed") {
		t.Errorf("the reason does not carry the error of the step: %q", reason)
	}
}

// TestTakeover_APersonsMessagePausesTheRun — somebody who writes in
// the chat while the run is going takes the chat back: the run stops
// after the step it is on.
func TestTakeover_APersonsMessagePausesTheRun(t *testing.T) {
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

	// Somebody writes in the chat.
	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})

	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !strings.Contains(st.PauseReason, "resume") {
		t.Errorf("the pause reason is %q, want it to name the way back", st.PauseReason)
	}
}

// TestTakeover_AReplyToTheQuestionIsNotATakeover — the answer to the
// run's own question is an answer, not somebody taking the chat back.
func TestTakeover_AReplyToTheQuestionIsNotATakeover(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is gone")
	}
	askID := waitAskID(t, r)

	// The person answers the question.
	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		ReplyTo:   askID,
		Text:      "the feature branch",
		Origin:    messages.OriginHuman,
	})
	if st := svc.Snapshot("obs-1"); st.Paused {
		t.Fatal("the answer to the question paused the run")
	}
}

// TestTakeover_AMessageOfTheRunIsNotATakeover — what the run says is
// not somebody taking the chat back.
func TestTakeover_AMessageOfTheRunIsNotATakeover(t *testing.T) {
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

	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_me",
		Text:      "/gtw commit",
		Origin:    messages.OriginMe,
	})

	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhaseRunning)
}

// TestStop_EndsTheRun — only the run's own command ends it.
func TestStop_EndsTheRun(t *testing.T) {
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

	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
	if st := svc.Snapshot("obs-1"); st.Phase != PhaseIdle {
		t.Errorf("the run is %q, want it gone", st.Phase)
	}
	// The chat is not told to stop working: the stop is the run's
	// stop, and nothing else.
	if containsText(op.Sent(), "/stop") {
		t.Errorf("the chat was told to stop: %v", op.Sent())
	}
}

// containsText reports whether the list holds the text.
func containsText(list []string, text string) bool {
	for _, item := range list {
		if item == text {
			return true
		}
	}
	return false
}
