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
	if decision.Reason == "" {
		t.Error("the judgment point has no reason")
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
	if decision.Reason == "" {
		t.Error("the judgment point has no reason")
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

// guardedFixYAML is the shape of the fix workflow: the steps that
// follow the fix run only when the fix picked an issue.
const guardedFixYAML = `
title: ship
jobs:
  ship:
    iterations: 0
    steps:
      - id: fix
        message: "/gtw fix * -y"
      - id: commit
        message: "/gtw commit"
        if: ${{ steps.fix.outputs.data.issue }}
      - id: pr
        message: "/gtw pr"
        if: ${{ steps.fix.outputs.data.issue }}
      - id: wait
        wait: 5m
        final: true
`

// TestDecide_TheGuardedStepsAreSkippedWhenNoIssueWasPicked pins the
// guard of the fix workflow: a fix that picked nothing leaves the
// steps that follow it skipped, and the wait step is the only one
// left in the round.
func TestDecide_TheGuardedStepsAreSkippedWhenNoIssueWasPicked(t *testing.T) {
	wf := mustParse(t, guardedFixYAML)
	state := newRunState(wf)
	// The empty queue: a normal branch — no issue fact, no error.
	RecordStep(state, "ship", wf.Jobs["ship"].Steps[0], Outcome{
		Status:  OutcomeFailed,
		Summary: "no assigned issue was eligible",
		Data:    map[string]string{"reason": "no_candidate"},
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "wait" {
		t.Errorf("Step.ID = %q, want wait — the guarded steps do not run", decision.Step.ID)
	}
	for _, id := range []string{"commit", "pr"} {
		step, ok := stepState(state, id)
		if !ok {
			t.Fatalf("the step %q was not recorded", id)
		}
		if !step.Skipped {
			t.Errorf("the step %q is not recorded as skipped", id)
		}
	}
}

// TestDecide_TheGuardedStepsRunWhenAnIssueWasPicked pins the other
// side of the guard: a fix that picked an issue lets the steps that
// follow it run.
func TestDecide_TheGuardedStepsRunWhenAnIssueWasPicked(t *testing.T) {
	wf := mustParse(t, guardedFixYAML)
	state := newRunState(wf)
	RecordStep(state, "ship", wf.Jobs["ship"].Steps[0], Outcome{
		Status:  OutcomeOK,
		Summary: "the work is ready",
		Data:    map[string]string{"issue": "42"},
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "commit" {
		t.Errorf("Step.ID = %q, want commit — the guard holds", decision.Step.ID)
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
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status: OutcomeFailed,
		Data:   map[string]string{"error": "the worktree could not be created"},
	}, false)

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

// finalStepYAML is a workflow whose last step cleans up after the
// round.
const finalStepYAML = `
title: with a final step
jobs:
  main:
    steps:
      - id: one
        message: "/one"
      - id: two
        message: "/two"
      - id: cleanup
        message: "/cleanup"
        final: true
`

// TestDecide_AFinalStepRunsInTheOrdinaryOrder pins the final step
// like any other: it runs when the steps before it ran.
func TestDecide_AFinalStepRunsInTheOrdinaryOrder(t *testing.T) {
	wf := mustParse(t, finalStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeOK}, false)
	RecordStep(state, "main", wf.Jobs["main"].Steps[1], Outcome{Status: OutcomeOK}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "cleanup" {
		t.Fatalf("the final step should run now, got %v (%q)", decision.Kind, decision.Reason)
	}
}

// TestEndRound_SkipsTheRemainingStepsAndRunsTheFinalStep pins the
// early end of a round: the steps that did not run are skipped, the
// final step runs in their place, and the round is over.
func TestEndRound_SkipsTheRemainingStepsAndRunsTheFinalStep(t *testing.T) {
	wf := mustParse(t, finalStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{
		Status:  OutcomeFailed,
		Summary: "the worktree has nothing to commit",
	}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	// The step that did not run is recorded as skipped.
	step, ok := stepState(state, "two")
	if !ok {
		t.Fatal("the step that did not run was not recorded")
	}
	if !step.Skipped {
		t.Error("the step that did not run is not recorded as skipped")
	}
	// The final step was left to run.
	if HasRun(state, "cleanup") {
		t.Error("the final step must be left to run")
	}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "cleanup" {
		t.Fatalf("the final step should run now, got %v (%q)", decision.Kind, decision.Reason)
	}
}

// TestEndRound_TheNextRoundBeginsAfterTheFinalStep pins the round
// that follows: the final step ends the round, and the job starts
// the next one.
func TestEndRound_TheNextRoundBeginsAfterTheFinalStep(t *testing.T) {
	wf := mustParse(t, `
title: repeats
jobs:
  main:
    iterations: 0
    steps:
      - id: one
        message: "/one"
      - id: cleanup
        message: "/cleanup"
        final: true
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "cleanup" {
		t.Fatalf("the final step should run now, got %v (%q)", decision.Kind, decision.Reason)
	}
	RecordStep(state, "main", wf.Jobs["main"].Steps[1], Outcome{Status: OutcomeOK}, false)

	decision, err = Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "one" {
		t.Fatalf("the next round should start, got %v (%q)", decision.Kind, decision.Reason)
	}
	if round := state.RoundOf("main"); round != 2 {
		t.Errorf("round = %d, want 2", round)
	}
}

// TestEndRound_AJobWithoutAFinalStepStartsTheNextRound pins the
// round that ends early in a job that names no final step: the
// remaining steps are skipped and the job goes on.
func TestEndRound_AJobWithoutAFinalStepStartsTheNextRound(t *testing.T) {
	wf := mustParse(t, `
title: repeats
jobs:
  main:
    iterations: 0
    steps:
      - id: one
        message: "/one"
      - id: two
        message: "/two"
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "one" {
		t.Fatalf("the next round should start, got %v (%q)", decision.Kind, decision.Reason)
	}
	if round := state.RoundOf("main"); round != 2 {
		t.Errorf("round = %d, want 2", round)
	}
}

// TestEndRound_AFailedFinalStepIsAJudgmentPoint pins the final step
// as a step of its own: when it fails, the agent decides again.
func TestEndRound_AFailedFinalStepIsAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, finalStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "cleanup" {
		t.Fatalf("the final step should run now, got %v (%q)", decision.Kind, decision.Reason)
	}
	RecordStep(state, "main", wf.Jobs["main"].Steps[2], Outcome{Status: OutcomeFailed}, false)

	decision, err = Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point (the final step failed)", decision.Kind)
	}
}

// TestEndRound_TheLastRoundOfAJobExhaustsThePlan pins the job that
// has no round left: the plan is over and the agent is asked again.
func TestEndRound_TheLastRoundOfAJobExhaustsThePlan(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment || !decision.Exhausted {
		t.Fatalf("the plan should be exhausted, got %v (%q)", decision.Kind, decision.Reason)
	}
}

// TestEndRound_AStepThatIsStillRunningHoldsTheRound — a round cannot
// end under a command that has not settled.
func TestEndRound_AStepThatIsStillRunningHoldsTheRound(t *testing.T) {
	wf := mustParse(t, finalStepYAML)
	state := newRunState(wf)
	RecordPending(state, "main", wf.Jobs["main"].Steps[0])

	if err := EndRound(state, wf, "main"); err == nil {
		t.Fatal("the round ended while a step was still running")
	}
	if HasRun(state, "two") {
		t.Error("a step was skipped even though the round did not end")
	}
}

// TestEndRound_AFinalStepThatIsNotLastStillRuns — a final step in the
// middle of a job runs in the ordinary order, and a round that ends
// early runs it too.
func TestEndRound_AFinalStepThatIsNotLastStillRuns(t *testing.T) {
	wf := mustParse(t, `
title: final in the middle
jobs:
  main:
    steps:
      - id: one
        message: "/one"
      - id: cleanup
        message: "/cleanup"
        final: true
      - id: last
        message: "/last"
`)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "cleanup" {
		t.Fatalf("the final step should run now, got %v (%q)", decision.Kind, decision.Reason)
	}
}

// stepState returns the record of the step, and whether it exists.
func stepState(state *RunState, id string) (StepState, bool) {
	for _, st := range state.Steps {
		if st.ID == id {
			return st, true
		}
	}
	return StepState{}, false
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

// TestEndRound_ARoundThatIsAlreadyOverIsRefused — ending a round
// twice at the same point would change nothing, and the plan would
// ask the agent the same question forever.
func TestEndRound_ARoundThatIsAlreadyOverIsRefused(t *testing.T) {
	wf := mustParse(t, finalStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], Outcome{Status: OutcomeFailed}, false)

	if err := EndRound(state, wf, "main"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	if err := EndRound(state, wf, "main"); err == nil {
		t.Fatal("the round was ended twice")
	}
}
