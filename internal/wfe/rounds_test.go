package wfe

import (
	"strings"
	"testing"
)

// repeatingYAML is a workflow whose job runs two rounds.
const repeatingYAML = `
title: repeat
jobs:
  ship:
    iterations: 2
    steps:
      - id: fix
        message: "/fix"
      - id: ship
        message: "/ship"
`

// runWholeJob records every step of the job as done.
func runWholeJob(state *RunState, wf *Workflow, job string) {
	for _, step := range wf.Jobs[job].Steps {
		RecordStep(state, job, step, Outcome{Status: OutcomeOK}, false)
	}
}

func TestDecide_AJobWithIterationsStartsTheNextRound(t *testing.T) {
	wf := mustParse(t, repeatingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "fix" {
		t.Errorf("Step.ID = %q, want fix — the round starts over", decision.Step.ID)
	}
	if state.Rounds["ship"] != 2 {
		t.Errorf("Rounds = %v, want the second round", state.Rounds)
	}
	if HasRun(state, "fix") {
		t.Error("the round's outputs were not cleared")
	}
}

func TestDecide_TheNextRoundReadsItsOwnOutputs(t *testing.T) {
	wf := mustParse(t, repeatingYAML)
	state := newRunState(wf)
	fix, ship := wf.Jobs["ship"].Steps[0], wf.Jobs["ship"].Steps[1]

	// Round 1: fix picks an issue, the job's other step runs, the
	// round ends.
	RecordStep(state, "ship", fix, Outcome{
		Status: OutcomeOK,
		Data:   map[string]string{"issue": "42"},
	}, false)
	RecordStep(state, "ship", ship, Outcome{Status: OutcomeOK}, false)
	if _, err := Decide(state, wf); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if got := EvalString("${{ steps.fix.outputs.data.issue }}", ExprContext(state)); got != "" {
		t.Errorf("the first round's issue = %q, want it cleared", got)
	}

	// Round 2: fix picks the next issue.
	RecordStep(state, "ship", fix, Outcome{
		Status: OutcomeOK,
		Data:   map[string]string{"issue": "43"},
	}, false)
	if got := EvalString("${{ steps.fix.outputs.data.issue }}", ExprContext(state)); got != "43" {
		t.Errorf("the second round's issue = %q, want 43", got)
	}
}

func TestDecide_ZeroIterationsRepeatsUntilTheRunEnds(t *testing.T) {
	wf := mustParse(t, `
title: forever
jobs:
  ship:
    iterations: 0
    steps: [{id: fix, message: "/fix"}]
`)
	state := newRunState(wf)

	for round := 1; round <= 3; round++ {
		runWholeJob(state, wf, "ship")
		decision, err := Decide(state, wf)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if decision.Kind != DecisionRun || decision.Step.ID != "fix" {
			t.Fatalf("round %d: Kind = %v (%q), want the job to repeat", round, decision.Kind, decision.Reason)
		}
	}
}

func TestDecide_AJobWithoutIterationsRunsOneRound(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "main")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !decision.Exhausted {
		t.Errorf("Exhausted = false, want the plan to be exhausted (the reason was %q)", decision.Reason)
	}
}

func TestDecide_TwoJobsThatRepeatAreAJudgmentPoint(t *testing.T) {
	wf := mustParse(t, `
title: two
jobs:
  a:
    iterations: 0
    steps: [{id: a1, message: "/a"}]
  b:
    iterations: 0
    steps: [{id: b1, message: "/b"}]
`)
	state := newRunState(wf)
	runWholeJob(state, wf, "a")
	runWholeJob(state, wf, "b")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if !strings.Contains(decision.Reason, "another round") {
		t.Errorf("the reason = %q, want it to name the rounds", decision.Reason)
	}
}

func TestDecide_AFailedStepIsNotSwallowedByTheNextRound(t *testing.T) {
	wf := mustParse(t, repeatingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")
	// The last step of the round did not end ok.
	state.Steps[len(state.Steps)-1].Outcome = Outcome{Status: OutcomeFailed}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if state.Rounds["ship"] != 1 {
		t.Errorf("Rounds = %v, want the round to stay where it is", state.Rounds)
	}
}

func TestDecide_ContinueOnErrorLetsTheRoundAdvance(t *testing.T) {
	wf := mustParse(t, `
title: repeat
jobs:
  ship:
    iterations: 2
    steps:
      - id: fix
        message: "/fix"
        continue-on-error: true
`)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")
	state.Steps[len(state.Steps)-1].Outcome = Outcome{Status: OutcomeFailed}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "fix" {
		t.Errorf("Step.ID = %q, want fix — the round starts over", decision.Step.ID)
	}
}

func TestDecide_ARoundThatDidNothingDoesNotRepeat(t *testing.T) {
	wf := mustParse(t, `
title: repeat
jobs:
  ship:
    iterations: 0
    steps:
      - id: fix
        message: "/fix"
        if: "false"
`)
	state := newRunState(wf)
	// The step's condition does not hold, so the plan skips it and
	// the round does nothing.
	RecordStep(state, "ship", wf.Jobs["ship"].Steps[0], Outcome{
		Status:  OutcomeNoop,
		Summary: "the step's condition did not hold",
	}, true)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment point", decision.Kind)
	}
	if !decision.Exhausted {
		t.Errorf("Exhausted = false, want the plan to be exhausted (the reason was %q)", decision.Reason)
	}
	if state.Rounds["ship"] != 1 {
		t.Errorf("Rounds = %v, want the round to stay where it is", state.Rounds)
	}
}

func TestPolicy_TheStepBudgetCountsTheCurrentRoundOnly(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    iterations: 2
    steps:
      - id: one
        message: echo
        attempts: 1
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, Command: "echo", StepID: "one", Round: 1},
	})

	pol.SetRounds(map[string]int{"main": 2})
	if _, err := pol.CheckRun("echo"); err != nil {
		t.Fatalf("the attempt of the previous round still counts: %v", err)
	}

	pol.SetRounds(map[string]int{"main": 1})
	_, err := pol.CheckRun("echo")
	if err == nil {
		t.Fatal("the attempt of the current round does not count")
	}
	if !strings.Contains(err.Error(), "limit is 1") {
		t.Errorf("error = %v, want it to name the limit", err)
	}
}

func TestPolicy_TheStepBudgetRefusesTheNextAttempt(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: echo
        attempts: 2
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, Command: "echo", StepID: "one"},
		{Kind: ActionKindRun, Command: "echo", StepID: "one"},
	})

	_, err := pol.CheckRun("echo")
	if err == nil {
		t.Fatal("the third attempt was accepted")
	}
	if !strings.Contains(err.Error(), "one") || !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("error = %v, want it to name the step and the limit", err)
	}
}
