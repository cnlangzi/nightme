package me

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/pkg/outcome"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// manyStepWorkflow is a workflow whose job holds a step for every part
// of the work: the operator can name any of them.
const manyStepWorkflow = `title: many steps.
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix
      - id: commit
        message: /gtw commit
      - id: review
        message: /review
      - id: push
        message: /gtw push
      - id: pr
        message: /gtw pr
`

// waitForRound waits until the job is in the given round.
func waitForRound(t *testing.T, r *run, job string, round int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := r.state.Rounds[job]
		r.mu.Unlock()
		if got == round {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.mu.Lock()
	got := r.state.Rounds[job]
	r.mu.Unlock()
	t.Fatalf("the job %q is in round %d, want %d", job, got, round)
}

// TestNextWithATarget_SkipsTheStepsUpToIt — the operator names the
// step the plan lands on: the steps of the round up to it are passed
// over.
func TestNextWithATarget_SkipsTheStepsUpToIt(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
		okStep("pushed"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	d, err := svc.Next(context.Background(), "obs-1", "", "review")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if d.Step != "review" {
		t.Errorf("the command landed on %q, want review", d.Step)
	}
	if !d.Act {
		t.Error("the command decided nothing, want the steps to be skipped")
	}

	// The steps of the range are skipped.
	for _, id := range []string{"commit", "review"} {
		waitForStepSummary(t, r, id, operatorSkippedSummary)
	}
	// The step after the range is the one the plan runs.
	waitSent(t, op, "/gtw push")
}

// TestNextWithATarget_SkipsTheRoundAway — the operator names the last
// step of the round: the round is over and the job starts its next
// one.
func TestNextWithATarget_SkipsTheRoundAway(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", `title: many steps.
jobs:
  main:
    iterations: 2
    steps:
      - id: fix
        message: /gtw fix
      - id: commit
        message: /gtw commit
`)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
		okStep("fixed again"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	if _, err := svc.Next(context.Background(), "obs-1", "", "commit"); err != nil {
		t.Fatalf("next: %v", err)
	}

	// The round is over: the job starts its next one.
	waitForRound(t, r, "main", 2)
}

// TestNextWithATargetOfAnotherJobIsRefused — the target is not a step
// of the round the run is on: the command says why and does nothing.
func TestNextWithATargetOfAnotherJobIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", twoJobWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{failedStep("the commit failed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	_, err := svc.Next(context.Background(), "obs-1", "", "push")
	if err == nil {
		t.Fatal("the target of another job was accepted")
	}
	if RefusalReason(err) != ReasonBadTarget {
		t.Errorf("the refusal is %q, want %q", RefusalReason(err), ReasonBadTarget)
	}
	if !strings.Contains(err.Error(), "another job") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	// The run state did not move.
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if isSkippedOutcome(st.Outcome) {
		t.Error("the commit step was skipped although the command was refused")
	}
}

// TestNextWithATargetThatHasPassedIsRefused — the plan of a run only
// goes forward: a step that has passed cannot be the target.
func TestNextWithATargetThatHasPassedIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	// The fix step did its job: it is behind the run.
	_, err := svc.Next(context.Background(), "obs-1", "", "fix")
	if err == nil {
		t.Fatal("the target that has passed was accepted")
	}
	if RefusalReason(err) != ReasonBadTarget {
		t.Errorf("the refusal is %q, want %q", RefusalReason(err), ReasonBadTarget)
	}
	if !strings.Contains(err.Error(), "fix") {
		t.Errorf("the refusal does not name the step: %v", err)
	}
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if isSkippedOutcome(st.Outcome) {
		t.Error("the commit step was skipped although the command was refused")
	}
}

// TestResumeWithATarget_RunsTheStepAgain — the operator names the step
// that runs again.
func TestResumeWithATarget_RunsTheStepAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
		failedStep("the commit failed again"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	d, err := svc.Resume(context.Background(), "obs-1", "", "commit")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if d.Step != "commit" {
		t.Errorf("the command acted on %q, want commit", d.Step)
	}
	if !d.Act {
		t.Error("the command decided nothing, want the step to run again")
	}

	// The step runs again.
	waitForAction(t, r, wfe.ActionKindRun, "commit", 2)
}

// TestResumeWithATargetThatSucceededIsRefused — a step that did its
// job has nothing to repair.
func TestResumeWithATargetThatSucceededIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	_, err := svc.Resume(context.Background(), "obs-1", "", "fix")
	if err == nil {
		t.Fatal("the target that succeeded was accepted")
	}
	if RefusalReason(err) != ReasonBadTarget {
		t.Errorf("the refusal is %q, want %q", RefusalReason(err), ReasonBadTarget)
	}
}

// TestResumeWithATargetThatHasNotRunIsRefused — a step that has not
// run has nothing to run again.
func TestResumeWithATargetThatHasNotRunIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	_, err := svc.Resume(context.Background(), "obs-1", "", "review")
	if err == nil {
		t.Fatal("the target that has not run was accepted")
	}
	if RefusalReason(err) != ReasonBadTarget {
		t.Errorf("the refusal is %q, want %q", RefusalReason(err), ReasonBadTarget)
	}
	if !strings.Contains(err.Error(), "has not run") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestTheTargetIsTheFirstTokenAndTheNoteIsTheRest — the target is a
// positional argument: the first token of the text names the step when
// the workflow has one, and the rest of the text is the note.
func TestTheTargetIsTheFirstTokenAndTheNoteIsTheRest(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
		okStep("pushed"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	if _, err := svc.Next(context.Background(), "obs-1", "", "push 剩下的别管"); err != nil {
		t.Fatalf("next: %v", err)
	}

	// The note is the text after the target.
	waitForNote(t, r, "剩下的别管")
	// The target is the step the plan landed on.
	waitForStepSummary(t, r, "review", operatorSkippedSummary)
}

// TestATextThatNamesNoStepIsTheNote — a first token the workflow does
// not know makes the whole text the note.
func TestATextThatNamesNoStepIsTheNote(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{
		okStep("fixed"),
		failedStep("the commit failed"),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	d, err := svc.Next(context.Background(), "obs-1", "", "修一下这个")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	// The command acts on the step the run is on.
	if d.Step != "commit" {
		t.Errorf("the command acted on %q, want commit", d.Step)
	}
	waitForNote(t, r, "修一下这个")
}

// TestTheNextToolWithATarget_SkipsTheStepsUpToIt — the agent names the
// step the plan lands on.
func TestTheNextToolWithATarget_SkipsTheStepsUpToIt(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	call := pendingCall{Name: toolNext, Args: []byte(`{"step_id":"review"}`)}
	if _, err := svc.toolNext(context.Background(), r, call); err != nil {
		t.Fatalf("next: %v", err)
	}

	for _, id := range []string{"commit", "review"} {
		st, ok := r.stepState(id)
		if !ok {
			t.Fatalf("the step %q has no state", id)
		}
		if !isSkippedOutcome(st.Outcome) {
			t.Errorf("the step %q ended as %d, want the skipped one", id, st.Outcome.Status)
		}
	}
}

// TestTheNextToolWithATargetThatCannotBeMovedIsRefused — the tool
// error tells the agent what it cannot do.
func TestTheNextToolWithATargetThatCannotBeMovedIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", twoJobWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "two")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	call := pendingCall{Name: toolNext, Args: []byte(`{"step_id":"push"}`)}
	_, err := svc.toolNext(context.Background(), r, call)
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "another job") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestTheNextToolWithATargetRunsWhileTheRunMoves — the tool reads the
// state of the run while the run goes on: the race detector watches.
func TestTheNextToolWithATargetRunsWhileTheRunMoves(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	review, ok := r.stepByID("review")
	if !ok {
		t.Fatal("the workflow has no review step")
	}
	r.setJudgmentStep("commit")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.setRunningStep("commit")
			r.recordStep("main", commit, outcome.Outcome{
				Success:    false,
				Status:     http.StatusInternalServerError,
				StatusText: "the commit failed",
			})
			r.recordStep("main", review, outcome.Outcome{
				Success:    false,
				Status:     http.StatusInternalServerError,
				StatusText: "the review failed",
			})
		}
	}()

	for i := 0; i < 200; i++ {
		call := pendingCall{Name: toolNext, Args: []byte(`{"step_id":"review"}`)}
		_, _ = svc.toolNext(context.Background(), r, call)
	}
	close(stop)
	<-done
}

// TestSetOutput_WritesAFactOfAStep — the agent corrects a fact: the
// step's output holds the new value.
func TestSetOutput_WritesAFactOfAStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
		Data:       map[string]string{"commits": "3"},
	})

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"commits","value":"5"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if st.Outcome.Data["commits"] != "5" {
		t.Errorf("the fact is %q, want 5", st.Outcome.Data["commits"])
	}
}

// TestSetOutput_WritesTheVerdictOfAStep — the status carries the
// verdict: a 2xx is a success, anything else is not.
func TestSetOutput_WritesTheVerdictOfAStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}
	st, _ := r.stepState("commit")
	if st.Outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want 200", st.Outcome.Status)
	}
	if !st.Outcome.Success {
		t.Error("the step did not succeed, want the success of a 2xx")
	}

	// The expression of the plan reads the new verdict.
	got := wfe.EvalString("${{ steps.commit.outputs.success }}", wfe.ExprContext(r.state))
	if got != "true" {
		t.Errorf("the expression reads %q, want true", got)
	}

	call = pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"500"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}
	st, _ = r.stepState("commit")
	if st.Outcome.Success {
		t.Error("the step succeeded, want the failure of a 5xx")
	}
	got = wfe.EvalString("${{ steps.commit.outputs.success }}", wfe.ExprContext(r.state))
	if got != "false" {
		t.Errorf("the expression reads %q, want false", got)
	}
}

// TestSetOutput_WritesTheWordsOfAStep — the status text replaces the
// line of words of the step.
func TestSetOutput_WritesTheWordsOfAStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"statusText","value":"the commit worked after all"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if st.Outcome.StatusText != "the commit worked after all" {
		t.Errorf("the words are %q, want the new ones", st.Outcome.StatusText)
	}
}

// TestSetOutput_WritesAnEmptyFact — an empty value is a value: a fact
// may be empty.
func TestSetOutput_WritesAnEmptyFact(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
		Data:       map[string]string{"commits": "3"},
	})

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"commits","value":""}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}
	st, _ := r.stepState("commit")
	value, ok := st.Outcome.Data["commits"]
	if !ok {
		t.Fatal("the fact is gone")
	}
	if value != "" {
		t.Errorf("the fact is %q, want the empty one", value)
	}
}

// TestSetOutput_RefusesAStepThatHasNotRun — a step that has no record
// has no output to write: the tool says so.
func TestSetOutput_RefusesAStepThatHasNotRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	_, err := svc.toolSetOutput(context.Background(), r, call)
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "no record") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestSetOutput_RefusesAStepThatIsStillRunning — a step whose work is
// in flight has no verdict to write: the tool says so.
func TestSetOutput_RefusesAStepThatIsStillRunning(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordPending("main", commit)

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	_, err := svc.toolSetOutput(context.Background(), r, call)
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestSetOutput_IsInTheHistoryAndCostsAChance — the correction is work
// the agent causes: it is in the history and it costs a chance.
func TestSetOutput_IsInTheHistoryAndCostsAChance(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})
	before, limit := r.stepUsage("commit")

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}

	var found bool
	for _, rec := range runHistory(r) {
		if rec.Kind == wfe.ActionKindOutput {
			found = true
		}
	}
	if !found {
		t.Errorf("the history holds no correction: %+v", runHistory(r))
	}
	after, _ := r.stepUsage("commit")
	if after-before != 1 {
		t.Errorf("the step spent %d chances, want 1 (the limit is %d)", after-before, limit)
	}
}

// TestTheCorrectedVerdictReachesTheFinishCheck — the history carries
// the corrected verdict: the run may end as done.
func TestTheCorrectedVerdictReachesTheFinishCheck(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	failed := outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}
	r.recordStep("main", commit, failed)
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", failed)

	if err := r.checkFinish("done"); err == nil {
		t.Fatal("the run may end as done although the step broke")
	}

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}
	if err := r.checkFinish("done"); err != nil {
		t.Errorf("the run may not end as done: %v", err)
	}
}

// TestSetOutput_RefusesAStepThatWasPassedOver — a step that was
// passed over keeps its record: it has no output to write.
func TestSetOutput_RefusesAStepThatWasPassedOver(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})
	res, err := svc.skipStep(context.Background(), r, commit, operatorSkippedSummary)
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("the step ended as %d, want the skipped one", res.Status)
	}

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"500"}`)}
	_, err = svc.toolSetOutput(context.Background(), r, call)
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "passed over") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestTheNextToolWithABlankStepIdActsOnTheCurrentStep — a step id
// that is blank names no step.
func TestTheNextToolWithABlankStepIdActsOnTheCurrentStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	call := pendingCall{Name: toolNext, Args: []byte(`{"step_id":"   "}`)}
	if _, err := svc.toolNext(context.Background(), r, call); err != nil {
		t.Fatalf("next: %v", err)
	}
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if !isSkippedOutcome(st.Outcome) {
		t.Errorf("the step ended as %d, want the skipped one", st.Outcome.Status)
	}
}

// TestTheCorrectedFactIsReadByTheExpressions — the fact the agent
// wrote is the one the expressions of the steps after it read.
func TestTheCorrectedFactIsReadByTheExpressions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
		Data:       map[string]string{"commits": "3"},
	})

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"commits","value":"5"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}

	got := wfe.EvalString("${{ steps.commit.outputs.data.commits }}", wfe.ExprContext(r.state))
	if got != "5" {
		t.Errorf("the expression reads %q, want 5", got)
	}
}

// TestTheCorrectedStepIsNoLongerAnException — the plan goes on: the
// step is not a judgment point any more.
func TestTheCorrectedStepIsNoLongerAnException(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	fix, ok := r.stepByID("fix")
	if !ok {
		t.Fatal("the workflow has no fix step")
	}
	r.recordStep("main", fix, outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "fixed"})
	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	decision, err := wfe.Decide(r.state, r.workflow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != wfe.DecisionJudgment {
		t.Fatalf("the engine decided %v, want a judgment point", decision.Kind)
	}

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}

	decision, err = wfe.Decide(r.state, r.workflow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != wfe.DecisionRun {
		t.Fatalf("the engine decided %v (%q), want the plan to go on", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "review" {
		t.Errorf("the engine runs %q, want review", decision.Step.ID)
	}

	// The card of the round shows the new verdict.
	tasks := r.roundTasks("main", 1)
	for _, item := range tasks {
		if item.ID != "commit" {
			continue
		}
		if item.Status != agent.TaskCompleted {
			t.Errorf("the row of the step is %q, want the one of a step that did its job", item.Status)
		}
	}
}

// TestTheConditionOfALaterStepReadsTheNewVerdict — a step that waits
// for the verdict of the corrected step runs once the verdict is
// fixed.
func TestTheConditionOfALaterStepReadsTheNewVerdict(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", `title: many steps.
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
        if: ${{ steps.commit.outputs.success == "true" }}
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	})

	decision, err := wfe.Decide(r.state, r.workflow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != wfe.DecisionJudgment {
		t.Fatalf("the engine decided %v, want a judgment point", decision.Kind)
	}

	call := pendingCall{Name: toolSetOutput, Args: []byte(`{"step_id":"commit","key":"status","value":"200"}`)}
	if _, err := svc.toolSetOutput(context.Background(), r, call); err != nil {
		t.Fatalf("set_output: %v", err)
	}

	decision, err = wfe.Decide(r.state, r.workflow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != wfe.DecisionRun {
		t.Fatalf("the engine decided %v (%q), want the plan to go on", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "push" {
		t.Errorf("the engine runs %q, want push", decision.Step.ID)
	}
}

// TestNextWithATargetWhileTheRunIsWorkingDecidesNothing — a run that
// is working decides for itself: the command says so and decides
// nothing.
func TestNextWithATargetWhileTheRunIsWorkingDecidesNothing(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	d, _, err := r.prepareDecision(directiveNext, "review")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if d.Act {
		t.Error("the command decided something, want nothing")
	}
	if d.Why != reasonWorking {
		t.Errorf("the command says %q, want %q", d.Why, reasonWorking)
	}
}

// TestTheSkipOfARangeRefusesARunningShellStep — the run cannot go on
// while a shell step runs: the command says why.
func TestTheSkipOfARangeRefusesARunningShellStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", `title: many steps.
jobs:
  main:
    steps:
      - id: build
        shell: make
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	build, ok := r.stepByID("build")
	if !ok {
		t.Fatal("the workflow has no build step")
	}
	r.setJudgmentStep("build")
	r.setRunningStep("build")
	r.recordPending("main", build)

	_, _, err := r.prepareDecision(directiveNext, "push")
	if err == nil {
		t.Fatal("the skip of a running shell step was accepted")
	}
	if RefusalReason(err) != ReasonBadTarget {
		t.Errorf("the refusal is %q, want %q", RefusalReason(err), ReasonBadTarget)
	}
	if !strings.Contains(err.Error(), "not a chat command") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestTheSkipOfARangeAbandonsTheRunningCommand — a chat command in
// flight is abandoned: the range is passed over at once.
func TestTheSkipOfARangeAbandonsTheRunningCommand(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "many", manyStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "many")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.setRunningStep("commit")
	r.setInflight(&inflightState{MsgID: "msg-1", Command: "/gtw commit"})
	r.recordPending("main", commit)

	d, _, err := r.prepareDecision(directiveNext, "review")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !d.Act {
		t.Error("the command decided nothing, want the steps to be skipped")
	}
	if !d.skippedNow {
		t.Error("the command waits for the run's next turn, want the skip at once")
	}
}

// TestTheRefusedTargetIsNotANote — a target the command cannot move is
// refused: its text does not become the note of the command.
func TestTheRefusedTargetIsNotANote(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", twoJobWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}, MaxJudgments: 1})
	op.steps = []fakeStep{failedStep("the commit failed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")
	r := activeRun(t, svc, "obs-1")

	if _, err := svc.Next(context.Background(), "obs-1", "", "push hurry up"); err == nil {
		t.Fatal("the target of another job was accepted")
	}
	if conversationCarries(r, "hurry up") {
		t.Error("the text of a refused target joined the conversation")
	}
}

// TestTheNextToolIsInTheCatalogue — the model reads the tools it may
// call.
func TestTheNextToolIsInTheCatalogue(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	if _, ok := toolNamed(svc.toolCatalogue(r), toolSetOutput); !ok {
		t.Error("the catalogue holds no set_output tool")
	}
	next, ok := toolNamed(svc.toolCatalogue(r), toolNext)
	if !ok {
		t.Fatal("the catalogue holds no next tool")
	}
	if !strings.Contains(string(next.Schema), "step_id") {
		t.Errorf("the next tool takes no step: %s", next.Schema)
	}
}

// TestTheServiceHasNoRun — a command in a chat without a run says so.
func TestTheServiceHasNoRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})

	_, err := svc.Next(context.Background(), "obs-none", "", "commit")
	if err == nil {
		t.Fatal("the command acted in a chat without a run")
	}
	if RefusalReason(err) != ReasonNoRun {
		t.Errorf("the refusal is %q, want %q", RefusalReason(err), ReasonNoRun)
	}
}
