package wfe

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// TestSetOutput_WritesAFact — a fact of a step is written under its
// own name.
func TestSetOutput_WritesAFact(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: "the commit ran",
		Data:       map[string]string{"commits": "3"},
	}, false)

	if err := SetOutput(state, wf, "one", "commits", "5"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	st, ok := stepRecord(state, "one")
	if !ok {
		t.Fatal("the step has no record")
	}
	if st.Outcome.Data["commits"] != "5" {
		t.Errorf("the fact is %q, want 5", st.Outcome.Data["commits"])
	}
	if st.Outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want the one of the step", st.Outcome.Status)
	}
}

// TestSetOutput_WritesTheStatusAndTheSuccess — the verdict follows
// from the status: a 2xx is a success, anything else is not.
func TestSetOutput_WritesTheStatusAndTheSuccess(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)

	if err := SetOutput(state, wf, "one", "status", "200"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	st, _ := stepRecord(state, "one")
	if st.Outcome.Status != http.StatusOK {
		t.Errorf("the status is %d, want 200", st.Outcome.Status)
	}
	if !st.Outcome.Success {
		t.Error("the step did not succeed, want the success of a 2xx")
	}

	if err := SetOutput(state, wf, "one", "status", "500"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	st, _ = stepRecord(state, "one")
	if st.Outcome.Status != http.StatusInternalServerError {
		t.Errorf("the status is %d, want 500", st.Outcome.Status)
	}
	if st.Outcome.Success {
		t.Error("the step succeeded, want the failure of a 5xx")
	}
}

// TestSetOutput_WritesTheStatusText — the line of words of a step is
// replaced.
func TestSetOutput_WritesTheStatusText(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)

	if err := SetOutput(state, wf, "one", "statusText", "the commit worked after all"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	st, _ := stepRecord(state, "one")
	if st.Outcome.StatusText != "the commit worked after all" {
		t.Errorf("the words are %q, want the new ones", st.Outcome.StatusText)
	}
}

// TestSetOutput_RefusesAStepWithoutARecord — a step that has not run
// has no output to write.
func TestSetOutput_RefusesAStepWithoutARecord(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)

	err := SetOutput(state, wf, "one", "commits", "5")
	if err == nil {
		t.Fatal("the output of a step that has not run was written")
	}
	if !strings.Contains(err.Error(), "no record") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestSetOutput_RefusesAStatusThatIsNoNumber — the status is a status
// code.
func TestSetOutput_RefusesAStatusThatIsNoNumber(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{Success: true, Status: http.StatusOK}, false)

	if err := SetOutput(state, wf, "one", "status", "all good"); err == nil {
		t.Fatal("a status that is no number was written")
	}
}

// gatedYAML is a workflow whose second step waits for the verdict of
// the first one.
const gatedYAML = `
title: gated
jobs:
  main:
    steps:
      - id: commit
        message: "/commit"
      - id: push
        message: "/push"
        if: ${{ steps.commit.outputs.success == "true" }}
`

// TestSetOutput_UnblocksTheStepsWhoseConditionHoldsNow — a write that
// changes what a condition reads puts the step back into the plan.
func TestSetOutput_UnblocksTheStepsWhoseConditionHoldsNow(t *testing.T) {
	wf := mustParse(t, gatedYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)

	// The plan looks at the gated step: its condition does not hold,
	// so the step is passed over.
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("the engine decided %v, want a judgment point", decision.Kind)
	}
	if _, ok := stepRecord(state, "push"); !ok {
		t.Fatal("the gated step has no record")
	}

	// The verdict of the first step is fixed.
	if err := SetOutput(state, wf, "commit", "status", "200"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}

	decision, err = Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("the engine decided %v (%q), want the gated step to run", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "push" {
		t.Errorf("the engine runs %q, want push", decision.Step.ID)
	}
}

// TestSetOutput_RefusesAStepThatIsStillRunning — a step whose work is
// in flight has no verdict to write: the verdict that comes back
// replaces it.
func TestSetOutput_RefusesAStepThatIsStillRunning(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordPending(state, "main", wf.Jobs["main"].Steps[0])

	err := SetOutput(state, wf, "one", "status", "200")
	if err == nil {
		t.Fatal("the output of a step that is still running was written")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestSetOutput_DoesNotChangeTheFactsOfTheRecordInPlace — the facts
// are copied: a reader that holds the map of the record keeps what it
// read.
func TestSetOutput_DoesNotChangeTheFactsOfTheRecordInPlace(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: "committed",
		Data:       map[string]string{"commits": "3"},
	}, false)
	before, _ := stepRecord(state, "one")

	if err := SetOutput(state, wf, "one", "commits", "5"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if before.Outcome.Data["commits"] != "3" {
		t.Errorf("the facts of the earlier record changed: %v", before.Outcome.Data)
	}
}

// TestSetOutput_UnblocksTheStepsOfAJobWhoseConditionHoldsNow — a job
// whose condition did not hold runs again when the condition holds
// now.
func TestSetOutput_UnblocksTheStepsOfAJobWhoseConditionHoldsNow(t *testing.T) {
	wf := mustParse(t, `
title: gated job
jobs:
  main:
    steps:
      - id: commit
        message: "/commit"
  side:
    if: ${{ steps.commit.outputs.success == "true" }}
    steps:
      - id: push
        message: "/push"
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("the engine decided %v, want a judgment point", decision.Kind)
	}
	if _, ok := stepRecord(state, "push"); !ok {
		t.Fatal("the step of the job has no record")
	}

	if err := SetOutput(state, wf, "commit", "status", "200"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}

	decision, err = Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("the engine decided %v (%q), want the step of the job to run", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "push" {
		t.Errorf("the engine runs %q, want push", decision.Step.ID)
	}
}

// TestSetOutput_KeepsTheStepsOfAJobWhoseConditionStillFails — the
// job's condition governs its steps: a step of a job that does not run
// stays out of the plan.
func TestSetOutput_KeepsTheStepsOfAJobWhoseConditionStillFails(t *testing.T) {
	wf := mustParse(t, `
title: gated job
jobs:
  main:
    steps:
      - id: commit
        message: "/commit"
  side:
    if: ${{ steps.commit.outputs.success == "false" }}
    steps:
      - id: push
        message: "/push"
        if: ${{ steps.commit.outputs.success == "true" }}
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)

	if _, err := Decide(state, wf); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, ok := stepRecord(state, "push"); !ok {
		t.Fatal("the step of the job has no record")
	}

	// The step of the job waits for a verdict the job does not wait
	// for: the job does not run.
	if err := SetOutput(state, wf, "commit", "status", "200"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if _, ok := stepRecord(state, "push"); !ok {
		t.Fatal("the step of the job has no record")
	}
}

// TestSetOutput_RefusesAStepThatDidNotRun — a step whose condition did
// not hold has no output of its own.
func TestSetOutput_RefusesAStepThatDidNotRun(t *testing.T) {
	wf := mustParse(t, `
title: gated
jobs:
  main:
    steps:
      - id: commit
        message: "/commit"
      - id: push
        message: "/push"
        if: ${{ steps.commit.outputs.success == "true" }}
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)
	if _, err := Decide(state, wf); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	err := SetOutput(state, wf, "push", "status", "200")
	if err == nil {
		t.Fatal("the output of a step that did not run was written")
	}
	if !strings.Contains(err.Error(), "did not run") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestSetOutput_RefusesTheSuccess — the verdict of a step follows from
// its status: there is no separate way to write it.
func TestSetOutput_RefusesTheSuccess(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the commit failed",
	}, false)

	err := SetOutput(state, wf, "one", "success", "true")
	if err == nil {
		t.Fatal("the success of a step was written")
	}
	if !strings.Contains(err.Error(), "follows from its status") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	st, _ := stepRecord(state, "one")
	if st.Outcome.Success {
		t.Error("the step succeeded although the success was refused")
	}
}

// TestTheOutputCorrectionCountsAgainstTheBudget — every action of the
// agent costs a chance of the step the run is on.
func TestTheOutputCorrectionCountsAgainstTheBudget(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	policy := NewPolicy(wf, nil)
	policy.Budget = 3

	before := policy.Remaining().Steps[0]
	policy.Record(ActionRecord{Kind: ActionKindOutput, StepID: "one"})
	after := policy.Remaining().Steps[0]
	if before-after != 1 {
		t.Fatalf("the step spent %d chances, want 1", before-after)
	}
}
