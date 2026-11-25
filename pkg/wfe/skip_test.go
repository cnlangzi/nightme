package wfe

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// twoJobYAML is a workflow with two jobs: the steps of a job run in
// order, and a job waits for the one before it.
const twoJobYAML = `
title: two
jobs:
  main:
    steps:
      - id: fix
        message: "/fix"
      - id: commit
        message: "/commit"
      - id: review
        message: "/review"
      - id: push
        message: "/push"
  side:
    needs: [main]
    steps:
      - id: notify
        message: "/notify"
`

// stepIDs returns the ids of the steps a range covers.
func stepIDs(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.ID)
	}
	return out
}

// TestSkipRange_CoversTheStepsUpToTheTarget — the range runs from the
// step the run is on up to and including the target.
func TestSkipRange_CoversTheStepsUpToTheTarget(t *testing.T) {
	wf := mustParse(t, twoJobYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Success: true, Status: http.StatusOK}, false)
	RecordStep(state, "main", wf.Jobs["main"].Steps[1], outcome.Outcome{Success: false, Status: http.StatusInternalServerError}, false)

	steps, err := SkipRange(state, wf, "commit", "push")
	if err != nil {
		t.Fatalf("SkipRange: %v", err)
	}
	got := stepIDs(steps)
	if len(got) != 3 || got[0] != "commit" || got[1] != "review" || got[2] != "push" {
		t.Fatalf("the range is %v, want commit, review and push", got)
	}
}

// TestSkipRange_AstepThatDidItsJobIsNotInTheRange — a step that
// succeeded keeps its verdict: it is not passed over again.
func TestSkipRange_AstepThatDidItsJobIsNotInTheRange(t *testing.T) {
	wf := mustParse(t, twoJobYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Success: true, Status: http.StatusOK}, false)

	steps, err := SkipRange(state, wf, "fix", "review")
	if err != nil {
		t.Fatalf("SkipRange: %v", err)
	}
	got := stepIDs(steps)
	if len(got) != 2 || got[0] != "commit" || got[1] != "review" {
		t.Fatalf("the range is %v, want commit and review", got)
	}
}

// TestSkipRange_RefusesATargetOfAnotherJob — a step of another job is
// not a step of this round.
func TestSkipRange_RefusesATargetOfAnotherJob(t *testing.T) {
	wf := mustParse(t, twoJobYAML)
	state := newRunState(wf)

	_, err := SkipRange(state, wf, "commit", "notify")
	if err == nil {
		t.Fatal("the target of another job was accepted")
	}
	var skipErr *SkipError
	if !errors.As(err, &skipErr) {
		t.Fatalf("err = %v, want a skip error", err)
	}
	if skipErr.Reason != targetOtherJob {
		t.Errorf("the reason is %q, want %q", skipErr.Reason, targetOtherJob)
	}
}

// TestSkipRange_RefusesATargetThatHasPassed — the plan of a run only
// goes forward.
func TestSkipRange_RefusesATargetThatHasPassed(t *testing.T) {
	wf := mustParse(t, twoJobYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Success: false, Status: http.StatusInternalServerError}, false)
	RecordStep(state, "main", wf.Jobs["main"].Steps[1], outcome.Outcome{Success: false, Status: http.StatusInternalServerError}, false)

	_, err := SkipRange(state, wf, "review", "fix")
	if err == nil {
		t.Fatal("the target that has passed was accepted")
	}
	var skipErr *SkipError
	if !errors.As(err, &skipErr) {
		t.Fatalf("err = %v, want a skip error", err)
	}
	if skipErr.Reason != targetPassed {
		t.Errorf("the reason is %q, want %q", skipErr.Reason, targetPassed)
	}
}

// TestSkipRange_RefusesATargetThatSucceeded — a step that did its job
// has nothing to skip.
func TestSkipRange_RefusesATargetThatSucceeded(t *testing.T) {
	wf := mustParse(t, twoJobYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Success: true, Status: http.StatusOK}, false)

	_, err := SkipRange(state, wf, "fix", "fix")
	if err == nil {
		t.Fatal("the target that succeeded was accepted")
	}
	var skipErr *SkipError
	if !errors.As(err, &skipErr) {
		t.Fatalf("err = %v, want a skip error", err)
	}
	if skipErr.Reason != targetSucceeded {
		t.Errorf("the reason is %q, want %q", skipErr.Reason, targetSucceeded)
	}
}

// TestSkipThrough_RecordsTheStepsAsSkipped — the steps of the range
// are skipped, and the facts they produced are kept.
func TestSkipThrough_RecordsTheStepsAsSkipped(t *testing.T) {
	wf := mustParse(t, twoJobYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Success: true, Status: http.StatusOK}, false)
	RecordStep(state, "main", wf.Jobs["main"].Steps[1], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
		Data:       map[string]string{"commits": "3"},
	}, false)

	states, err := SkipThrough(state, wf, "commit", "review", "the operator skipped this step")
	if err != nil {
		t.Fatalf("SkipThrough: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("the range holds %d steps, want 2", len(states))
	}
	for _, st := range states {
		if !st.Outcome.Succeeded() {
			t.Errorf("the step %q ended as %d, want a success", st.ID, st.Outcome.Status)
		}
		if st.Outcome.StatusText != "the operator skipped this step" {
			t.Errorf("the step %q says %q, want the summary of the skip", st.ID, st.Outcome.StatusText)
		}
	}
	commit, ok := stepRecord(state, "commit")
	if !ok {
		t.Fatal("the commit step has no record")
	}
	if commit.Outcome.Data["commits"] != "3" {
		t.Errorf("the facts of the step are %v, want the ones it produced", commit.Outcome.Data)
	}

	// The plan goes on with the step after the range.
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "push" {
		t.Fatalf("the engine decided %v on %q, want push to run", decision.Kind, decision.Step.ID)
	}
}
