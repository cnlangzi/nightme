package wfe

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// cmdWorkflowYAML is a polling job: it asks for an issue, and it ends
// the round quietly when there is none.
const cmdWorkflowYAML = `
title: poll
jobs:
  main:
    iterations: 0
    steps:
      - id: poll
        message: "/gtw fix * -y"
      - id: quiet
        cmd:
          name: skip
      - id: commit
        message: "/gtw commit"
`

// TestTheCmdStepIsAKindOfItsOwn — a cmd step is a step of its own: it
// may carry a message, and it is exclusive with the other kinds.
func TestTheCmdStepIsAKindOfItsOwn(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "a cmd step alone",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: skip
`,
		},
		{
			name: "a cmd step with a message",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: skip
        message: "/gtw back"
`,
		},
		{
			name: "a cmd step with a message and no name",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd: {}
        message: "/gtw back"
`,
		},
		{
			name: "a cmd step with a shell",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: skip
        shell: "echo hi"
`,
			want: "a cmd step does not take shell / use / wait",
		},
		{
			name: "a cmd step with agents",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: skip
        message: "/gtw back"
        agents:
          - mode: use
            names: [omp]
`,
			want: "the agents are for a message step",
		},
		{
			name: "a cmd step that does nothing",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd: {}
`,
			want: "a cmd step needs a name or a message",
		},
		{
			name: "an unknown builtin",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: nope
`,
			want: "unknown builtin command",
		},
		{
			name: "an argument the builtin does not take",
			yaml: `
title: t
jobs:
  main:
    steps:
      - id: quiet
        cmd:
          name: skip
          args: [now]
`,
			want: "the builtin \"skip\" takes no arguments",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := collect([]byte(tt.yaml), true)
			if tt.want == "" {
				if len(diags) > 0 {
					t.Fatalf("the workflow was refused: %v", diags)
				}
				return
			}
			if len(diags) == 0 {
				t.Fatalf("the workflow was accepted, want the error %q", tt.want)
			}
			if !strings.Contains(diags[0].Message, tt.want) {
				t.Errorf("the diagnostic says %q, want it to mention %q", diags[0].Message, tt.want)
			}
		})
	}
}

// TestTheSkipCommandEndsTheRoundQuietly — the round ends, the steps
// that did not run are recorded as skipped, and the round leaves
// nothing behind in the report.
func TestTheSkipCommandEndsTheRoundQuietly(t *testing.T) {
	wf := mustParse(t, cmdWorkflowYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success: true,
		Status:  http.StatusOK,
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Step.ID != "quiet" {
		t.Fatalf("Step.ID = %q, want quiet", decision.Step.ID)
	}

	RecordStep(state, "main", decision.Step, outcome.Outcome{Success: true, Status: http.StatusOK}, false)
	if err := RunBuiltin(state, wf, "main", decision.Step); err != nil {
		t.Fatalf("RunBuiltin: %v", err)
	}

	// The step that did not run is recorded as skipped.
	commit, ok := stepStateOf(state, "commit")
	if !ok {
		t.Fatal("the commit step was not recorded")
	}
	if !commit.Skipped {
		t.Error("the commit step was not skipped")
	}
	if commit.Outcome.StatusText != "the round was ended before this step ran" {
		t.Errorf("the commit step says %q, want the words of the engine", commit.Outcome.StatusText)
	}
	// The step that ended the round is the cmd step itself.
	ended, ok := state.EndedRounds["main"]
	if !ok {
		t.Fatal("the round was not marked as ended")
	}
	if ended.Step != "quiet" {
		t.Errorf("the round ended at %q, want quiet", ended.Step)
	}
	// The round is one of the rounds that leave nothing behind.
	if len(state.SkippedRounds) != 1 {
		t.Fatalf("the run holds %d skipped rounds, want 1", len(state.SkippedRounds))
	}
	if state.SkippedRounds[0].Job != "main" || state.SkippedRounds[0].Round != 1 {
		t.Errorf("the skipped round is %+v, want main round 1", state.SkippedRounds[0])
	}

	policy := NewPolicy(wf, nil)
	policy.SetSkippedRounds(state.SkippedRounds)
	if got := policy.RoundStatus("main", 1); got != RoundStatusSilent {
		t.Errorf("the round is %q, want %q", got, RoundStatusSilent)
	}
}

// TestThePlanGoesOnWithTheNextRoundAfterASkip — the round the skip
// ended is over: the plan starts the next one.
func TestThePlanGoesOnWithTheNextRoundAfterASkip(t *testing.T) {
	wf := mustParse(t, cmdWorkflowYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success: true,
		Status:  http.StatusOK,
	}, false)
	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	RecordStep(state, "main", decision.Step, outcome.Outcome{Success: true, Status: http.StatusOK}, false)
	if err := RunBuiltin(state, wf, "main", decision.Step); err != nil {
		t.Fatalf("RunBuiltin: %v", err)
	}

	decision, err = Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun {
		t.Fatalf("Kind = %v, want a run (the reason was %q)", decision.Kind, decision.Reason)
	}
	if decision.Step.ID != "poll" {
		t.Errorf("Step.ID = %q, want poll — the job starts its next round", decision.Step.ID)
	}
	if state.RoundOf("main") != 2 {
		t.Errorf("the job is in round %d, want 2", state.RoundOf("main"))
	}
}

// TestTheCmdStepIsAnOrdinaryStep — the cmd step is a step of the plan:
// it has its place in the list and it takes a chance of the budget.
func TestTheCmdStepIsAnOrdinaryStep(t *testing.T) {
	wf := mustParse(t, cmdWorkflowYAML)
	state := newRunState(wf)

	ready, err := ReadySet(state, wf)
	if err != nil {
		t.Fatalf("ReadySet: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != "poll" {
		t.Fatalf("the ready set holds %v, want the first step", ready)
	}

	policy := NewPolicy(wf, nil)
	policy.SetRounds(state.Rounds)
	if err := policy.CheckStep(1); err != nil {
		t.Errorf("the cmd step may not run: %v", err)
	}
}

// stepStateOf returns the state of a step.
func stepStateOf(state *RunState, stepID string) (StepState, bool) {
	for _, st := range state.Steps {
		if st.ID == stepID {
			return st, true
		}
	}
	return StepState{}, false
}

// TestTheCmdStepWaitsForTheStepBeforeIt — a cmd step is a step of the
// plan: the failure of the step before it keeps it from running, and
// the agent decides what comes next.
func TestTheCmdStepWaitsForTheStepBeforeIt(t *testing.T) {
	wf := mustParse(t, cmdWorkflowYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success: false,
		Status:  http.StatusInternalServerError,
	}, false)

	decision, err := Decide(state, wf)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionJudgment {
		t.Fatalf("Kind = %v, want a judgment (the reason was %q)", decision.Kind, decision.Reason)
	}
}
