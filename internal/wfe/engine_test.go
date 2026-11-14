package wfe

import (
	"testing"
	"time"
)

// newRunState builds a fresh run state for a workflow.
func newRunState(wf *Workflow) *RunState {
	now := time.Now()
	return &RunState{
		RunID:        "test-run",
		WorkflowName: wf.Name,
		Workspace:    "/tmp",
		Status:       StatusRunning,
		StartedAt:    now,
		UpdatedAt:    now,
	}
}

// threeStepYAML is a workflow whose steps run in order.
const threeStepYAML = `
title: three
jobs:
  main:
    steps:
      - id: one
        message: "/one"
      - id: two
        message: "/two"
      - id: three
        message: "/three"
`

func TestDecide_FirstStepRunsOnTheFastPath(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "one" {
		t.Errorf("Step.ID = %q, want one", decision.Step.ID)
	}
}

func TestDecide_NextStepFollowsTheOkOutcome(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeOK}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "two" {
		t.Errorf("Step.ID = %q, want two", decision.Step.ID)
	}
}

func TestDecide_ANonOkOutcomeIsAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeIncomplete}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if decision.Reason == "" {
		t.Error("the judgment point has no reason")
	}
	if len(decision.Ready) != 1 || decision.Ready[0].ID != "two" {
		t.Errorf("Ready = %v, want the second step", decision.Ready)
	}
}

func TestDecide_ANoopOutcomeKeepsTheFastPath(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeNoop}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
}

func TestDecide_ThePlanIsExhausted(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	job := wf.Jobs["main"]
	for _, step := range job.Steps {
		RecordStep(state, "main", step, Outcome{Status: OutcomeOK}, false)
	}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if !decision.Exhausted {
		t.Error("Exhausted = false, want true")
	}
}

func TestDecide_AForkIsAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, `
title: fork
jobs:
  a:
    steps: [{id: a1, message: "/a"}]
  b:
    steps: [{id: b1, message: "/b"}]
`)
	state := newRunState(wf)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if len(decision.Ready) != 2 {
		t.Errorf("Ready = %v, want both steps", decision.Ready)
	}
}

func TestDecide_JobNeedsHoldTheNextJobBack(t *testing.T) {
	wf := mustParse(t, `
title: two-jobs
jobs:
  build:
    steps: [{id: build1, message: "/build"}]
  ship:
    needs: [build]
    steps: [{id: ship1, message: "/ship"}]
`)
	state := newRunState(wf)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "build1" {
		t.Fatalf("the first job should run first, got %v (%q)", decision.Kind, decision.Reason)
	}
	RecordStep(state, "build", wf.Jobs["build"].Steps[0], Outcome{Status: OutcomeOK}, false)

	decision, err = Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "ship1" {
		t.Fatalf("the second job should run now, got %v (%q)", decision.Kind, decision.Reason)
	}
}

func TestDecide_OnlyTheFirstReadyStepOfAJobCounts(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(decision.Ready) != 1 {
		t.Fatalf("Ready = %v, want one step", decision.Ready)
	}
	if decision.Ready[0].ID != "one" {
		t.Errorf("Ready[0].ID = %q, want one", decision.Ready[0].ID)
	}
}

func TestDecide_AFalseConditionSkipsTheStep(t *testing.T) {
	wf := mustParse(t, `
title: conditional
jobs:
  main:
    steps:
      - id: skip
        message: "/skip"
        if: "false"
      - id: run
        message: "/run"
`)
	state := newRunState(wf)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "run" {
		t.Errorf("Step.ID = %q, want run — the first step's condition does not hold", decision.Step.ID)
	}
	if !HasRun(state, "skip") {
		t.Error("the skipped step was not recorded")
	}
}

func TestDecide_AnEmptyConditionHolds(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeOK}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Step.ID != "two" {
		t.Errorf("Step.ID = %q, want two", decision.Step.ID)
	}
}

func TestDecide_ContinueOnErrorKeepsTheChainGoing(t *testing.T) {
	wf := mustParse(t, `
title: resilient
jobs:
  main:
    steps:
      - id: flaky
        message: "/flaky"
        continue-on-error: true
      - id: next
        message: "/next"
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "next" {
		t.Errorf("Step.ID = %q, want next", decision.Step.ID)
	}
}

func TestExprContext_CarriesTheStepOutcomes(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status:  OutcomeOK,
		Summary: "done",
		Data:    map[string]string{"issue": "42"},
	}, false)

	ec := ExprContext(state)
	got := EvalString("${{ steps.one.outputs.status }}", ec)
	if got != "ok" {
		t.Errorf("status = %q, want ok", got)
	}
	got = EvalString("${{ steps.one.outputs.data.issue }}", ec)
	if got != "42" {
		t.Errorf("data.issue = %q, want 42", got)
	}
	got = EvalString("${{ needs.main.outputs.summary }}", ec)
	if got != "done" {
		t.Errorf("needs.main summary = %q, want done", got)
	}
}
