package wfe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sleepingYAML is a workflow whose job waits between its rounds.
const sleepingYAML = `
title: sleeps
jobs:
  ship:
    iterations: 2
    sleep: 5m
    steps:
      - id: fix
        message: "/fix"
`

// TestLoad_AJobCarriesItsGap — the gap is read with the workflow.
func TestLoad_AJobCarriesItsGap(t *testing.T) {
	wf := mustParse(t, sleepingYAML)
	job := wf.Jobs["ship"]
	if job.Sleep != "5m" {
		t.Errorf("Sleep = %q, want 5m", job.Sleep)
	}
	if gap := job.Gap(); gap != 5*time.Minute {
		t.Errorf("Gap = %v, want 5m", gap)
	}
}

// TestLoad_ASleepThatIsNotADurationIsRefused — the gap is a duration,
// the way the wait of a step is one.
func TestLoad_ASleepThatIsNotADurationIsRefused(t *testing.T) {
	raw := []byte(`
title: sleeps
jobs:
  ship:
    sleep: soon
    steps: [{id: fix, message: "/fix"}]
`)
	_, err := Parse(raw)
	if err == nil {
		t.Fatal("the workflow loaded")
	}
	if !strings.Contains(err.Error(), "sleep") {
		t.Errorf("error = %v, want it to name the setting", err)
	}

	diags := Validate(raw)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if !strings.Contains(diags[0].Message, "sleep") {
		t.Errorf("the diagnostic = %q, want it to name the setting", diags[0].Message)
	}
}

// TestBind_TheGapSurvivesTheBinding — a bound workflow keeps the gap
// of every job.
func TestBind_TheGapSurvivesTheBinding(t *testing.T) {
	wf := mustParse(t, sleepingYAML)

	bound, err := Bind(wf, nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if gap := bound.Jobs["ship"].Gap(); gap != 5*time.Minute {
		t.Errorf("Gap = %v, want 5m", gap)
	}
}

// TestTheGapSurvivesTheJSONRoundTrip — the workflow goes to the disk
// with the run: the gap comes back with it.
func TestTheGapSurvivesTheJSONRoundTrip(t *testing.T) {
	wf := mustParse(t, sleepingYAML)

	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Workflow
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if gap := back.Jobs["ship"].Gap(); gap != 5*time.Minute {
		t.Errorf("Gap = %v, want 5m", gap)
	}
}

// TestDecide_TheFirstRoundWaitsNoGap — the first round of a job
// starts as soon as the jobs it needs have finished.
func TestDecide_TheFirstRoundWaitsNoGap(t *testing.T) {
	wf := mustParse(t, sleepingYAML)
	state := newRunState(wf)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Gap != 0 {
		t.Errorf("Gap = %v, want no gap before the first round", decision.Gap)
	}
}

// TestDecide_TheNextRoundCarriesTheGap — the decision that starts the
// next round of a job carries the gap.
func TestDecide_TheNextRoundCarriesTheGap(t *testing.T) {
	wf := mustParse(t, sleepingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Gap != 5*time.Minute {
		t.Errorf("Gap = %v, want 5m", decision.Gap)
	}
}

// TestDecide_ARoundTheAgentEndedEarlyCarriesTheGap — the back-off
// keeps the round the agent ended early.
func TestDecide_ARoundTheAgentEndedEarlyCarriesTheGap(t *testing.T) {
	wf := mustParse(t, `
title: sleeps
jobs:
  ship:
    iterations: 0
    sleep: 5m
    steps:
      - id: fix
        message: "/fix"
`)
	state := newRunState(wf)
	RecordStep(state, "ship", wf.Jobs["ship"].Steps[0], Outcome{Status: OutcomeOK}, false)
	if err := EndRound(state, wf, "ship"); err != nil {
		t.Fatalf("EndRound: %v", err)
	}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Gap != 5*time.Minute {
		t.Errorf("Gap = %v, want 5m", decision.Gap)
	}
}

// TestDecide_TheLastRoundCarriesNoGap — a job with no next round has
// no gap: the run ends when its last round ends.
func TestDecide_TheLastRoundCarriesNoGap(t *testing.T) {
	wf := mustParse(t, sleepingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")
	if _, err := Decide(state, wf); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	// The second and last round of the job.
	runWholeJob(state, wf, "ship")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !decision.Exhausted {
		t.Fatalf("the plan should be exhausted (the reason was %q)", decision.Reason)
	}
	if decision.Gap != 0 {
		t.Errorf("Gap = %v, want no gap after the last round", decision.Gap)
	}
}

// TestDecide_AJobWithoutASleepCarriesNoGap — a workflow that names no
// gap behaves the way it always did: the next round starts at once.
func TestDecide_AJobWithoutASleepCarriesNoGap(t *testing.T) {
	wf := mustParse(t, repeatingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Gap != 0 {
		t.Errorf("Gap = %v, want no gap", decision.Gap)
	}
}

// TestGap_EveryJobKeepsItsOwnGap — a workflow with several jobs gives
// each of them its own gap.
func TestGap_EveryJobKeepsItsOwnGap(t *testing.T) {
	wf := mustParse(t, `
title: sleeps
jobs:
  a:
    iterations: 2
    sleep: 1m
    steps: [{id: a1, message: "/a"}]
  b:
    iterations: 3
    sleep: 30s
    steps: [{id: b1, message: "/b"}]
`)
	if gap := wf.Jobs["a"].Gap(); gap != time.Minute {
		t.Errorf("the gap of a = %v, want 1m", gap)
	}
	if gap := wf.Jobs["b"].Gap(); gap != 30*time.Second {
		t.Errorf("the gap of b = %v, want 30s", gap)
	}
}

// TestDecide_ASleepOfZeroIsNoGap — a gap of zero is no gap.
func TestDecide_ASleepOfZeroIsNoGap(t *testing.T) {
	wf := mustParse(t, `
title: sleeps
jobs:
  ship:
    iterations: 2
    sleep: 0s
    steps: [{id: fix, message: "/fix"}]
`)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Gap != 0 {
		t.Errorf("Gap = %v, want no gap", decision.Gap)
	}
}

// TestDecide_TheSpentRoundsMakeTheWorkflowOver — a job that repeats
// knows how many rounds it runs: when they are spent, the workflow is
// over and no model is asked what comes next.
func TestDecide_TheSpentRoundsMakeTheWorkflowOver(t *testing.T) {
	wf := mustParse(t, sleepingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")
	if _, err := Decide(state, wf); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	runWholeJob(state, wf, "ship")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !decision.Exhausted {
		t.Fatalf("the plan should be exhausted (the reason was %q)", decision.Reason)
	}
	if !decision.Over {
		t.Error("Over = false, want the spent rounds to make the workflow over")
	}
}

// TestDecide_AJobThatRunsOneRoundIsNotOver — a job that runs one round
// has no rounds to spend: the agent still has a say about the plan
// that is exhausted.
func TestDecide_AJobThatRunsOneRoundIsNotOver(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "main")

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !decision.Exhausted {
		t.Fatalf("the plan should be exhausted (the reason was %q)", decision.Reason)
	}
	if decision.Over {
		t.Error("Over = true, want the agent to decide about a job that ran one round")
	}
}

// TestDecide_ABrokenStepIsNotOver — a step that broke is a judgment
// point: the agent decides what to do about it.
func TestDecide_ABrokenStepIsNotOver(t *testing.T) {
	wf := mustParse(t, sleepingYAML)
	state := newRunState(wf)
	runWholeJob(state, wf, "ship")
	if _, err := Decide(state, wf); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	runWholeJob(state, wf, "ship")
	// The last step of the last round broke.
	state.Steps[len(state.Steps)-1].Outcome = Outcome{
		Status: OutcomeFailed,
		Data:   map[string]string{"error": "the worktree could not be created"},
	}

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !decision.Exhausted {
		t.Fatalf("the plan should be exhausted (the reason was %q)", decision.Reason)
	}
	if decision.Over {
		t.Error("Over = true, want the agent to decide about the step that broke")
	}
}
