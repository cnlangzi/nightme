package wfe

import (
	"strings"
	"testing"
)

// boundFixIssue binds the example workflow to an issue number.
func boundFixIssue(t *testing.T) *Workflow {
	t.Helper()
	wf := mustLoadFile(t, "fix-issue", fixIssueYAML)
	bound, err := Bind(wf, map[string]string{"issue": "42"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound
}

// manyPushes builds a history of the same command.
func manyPushes(n int) []ActionRecord {
	out := make([]ActionRecord, n)
	for i := range out {
		out[i] = ActionRecord{Kind: ActionKindRun, Command: "/gtw push", StepID: "push"}
	}
	return out
}

// manyEchoes builds a history of the same command.
func manyEchoes(n int) []ActionRecord {
	out := make([]ActionRecord, n)
	for i := range out {
		out[i] = ActionRecord{Kind: ActionKindRun, Command: "echo hi", StepID: "main#1"}
	}
	return out
}

func TestPolicy_CheckRun(t *testing.T) {
	bound := boundFixIssue(t)

	reviews := make([]ActionRecord, 5)
	for i := range reviews {
		reviews[i] = ActionRecord{Kind: ActionKindRun, Command: "/review", StepID: "review"}
	}

	tests := []struct {
		name    string
		command string
		history []ActionRecord
		wantIdx int
		wantErr string
	}{
		{
			name:    "first step",
			command: "/gtw fix 42 -y",
			wantIdx: 0,
		},
		{
			name:    "second step",
			command: "/gtw commit",
			wantIdx: 1,
		},
		{
			name:    "a command that is no step's message",
			command: "/review -c",
			wantErr: "is not allowed",
		},
		{
			name:    "whitespace is normalised",
			command: "  /gtw   push  ",
			wantIdx: 4,
		},
		{
			name:    "unknown command",
			command: "/gtw sync",
			wantErr: "is not allowed",
		},
		{
			name:    "command of another workflow",
			command: "/gtw pr --draft",
			wantErr: "is not allowed",
		},
		{
			name:    "the budget of the step is spent",
			command: "/review",
			history: reviews,
			wantErr: "limit is 5",
		},
		{
			name:    "another step spends its own budget",
			command: "/gtw push",
			history: manyPushes(3),
			wantIdx: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := NewPolicy(bound, tt.history)
			pol.Budget = 5
			idx, err := pol.CheckRun(tt.command)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if idx != -1 {
					t.Errorf("step index = %d, want -1 on error", idx)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckRun: %v", err)
			}
			if idx != tt.wantIdx {
				t.Errorf("step index = %d, want %d", idx, tt.wantIdx)
			}
			if !pol.IsStepMessage(idx, tt.command) {
				t.Errorf("the command is not the step's own message, want it to be")
			}
		})
	}
}

func TestPolicy_CheckRun_ErrorListsTheStepCommands(t *testing.T) {
	pol := NewPolicy(boundFixIssue(t), nil)

	_, err := pol.CheckRun("/gtw sync")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"/gtw fix 42 -y", "/gtw commit", "/review", "/gtw push"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list %q", err, want)
		}
	}
}

// TestPolicy_CheckRun_RegisteredCommandIsAcceptedButNotOwned — a
// command of the registry runs, but it belongs to no step: it counts
// against the step the run is on, and it never becomes that step's
// verdict.
func TestPolicy_CheckRun_RegisteredCommandIsAcceptedButNotOwned(t *testing.T) {
	bound := boundFixIssue(t)
	pol := NewPolicy(bound, nil)
	pol.Commands = []string{"gtw", "review"}

	idx, err := pol.CheckRun("/gtw sync")
	if err != nil {
		t.Fatalf("CheckRun: %v", err)
	}
	if pol.IsStepMessage(idx, "/gtw sync") {
		t.Error("the command is owned by a step, want it unowned")
	}
	if idx != -1 {
		t.Errorf("step index = %d, want -1 (no step ran yet)", idx)
	}

	// With a step behind the run, the command counts against it.
	pol = NewPolicy(bound, []ActionRecord{
		{Kind: ActionKindRun, Command: "/gtw commit", StepID: "commit", Outcome: Outcome{Status: OutcomeOK}},
	})
	pol.Commands = []string{"gtw", "review"}
	idx, err = pol.CheckRun("/gtw sync")
	if err != nil {
		t.Fatalf("CheckRun: %v", err)
	}
	if pol.IsStepMessage(idx, "/gtw sync") {
		t.Error("the command is owned by a step, want it unowned")
	}
	if idx != 1 {
		t.Errorf("step index = %d, want 1 (the step the run is on)", idx)
	}
}

// TestPolicy_CheckRun_StepsOwnCommandWins — a step's own command is
// the step's, even when the registry knows the same name.
func TestPolicy_CheckRun_StepsOwnCommandWins(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: /gtw commit
`)
	pol := NewPolicy(wf, nil)
	pol.Commands = []string{"gtw"}

	idx, err := pol.CheckRun("/gtw commit")
	if err != nil {
		t.Fatalf("CheckRun: %v", err)
	}
	if !pol.IsStepMessage(idx, "/gtw commit") {
		t.Error("the command is unowned, want the step to own it")
	}
	if idx != 0 {
		t.Errorf("step index = %d, want 0", idx)
	}
}

// TestPolicy_CommandCountsAgainstTheStepButNotTheVerdict — a
// command of the registry takes a chance of the step, but the
// step's own verdict is the one the plan reads.
func TestPolicy_CommandCountsAgainstTheStepButNotTheVerdict(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: /gtw commit
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, Command: "/gtw commit", StepID: "one", Outcome: Outcome{Status: OutcomeFailed}},
	})
	pol.Commands = []string{"gtw"}
	pol.Budget = 2

	if rem := pol.Remaining(); rem.Steps[0] != 1 {
		t.Errorf("Steps[0] = %d, want 1 (the command took a chance)", rem.Steps[0])
	}

	// The step failed: a command that succeeded does not turn the run
	// into a finished one.
	pol.Record(ActionRecord{
		Kind:    ActionKindCommand,
		Command: "/gtw sync",
		StepID:  "one",
		Outcome: Outcome{Status: OutcomeOK},
	})
	if err := pol.CheckFinish("done"); err == nil || !strings.Contains(err.Error(), "must be ok or noop") {
		t.Errorf("expected must-be-ok error, got %v", err)
	}

	// The budget is spent: the step refuses another action.
	if _, err := pol.CheckRun("/gtw sync"); err == nil || !strings.Contains(err.Error(), "limit is 2") {
		t.Errorf("expected step-limit error, got %v", err)
	}
}

// TestPolicy_StepCommandsAndRenderAreUnchanged — the registered
// commands do not join the list of the workflow's own commands, and
// they do not show up in the rendering.
func TestPolicy_StepCommandsAndRenderAreUnchanged(t *testing.T) {
	bound := boundFixIssue(t)
	before := NewPolicy(bound, nil)

	pol := NewPolicy(bound, nil)
	pol.Commands = []string{"gtw", "review"}

	after := pol.StepCommands()
	if len(after) != len(before.StepCommands()) {
		t.Fatalf("StepCommands = %v, want %v", after, before.StepCommands())
	}
	for i := range after {
		if after[i] != before.StepCommands()[i] {
			t.Errorf("StepCommands = %v, want %v", after, before.StepCommands())
		}
	}
	if out := pol.Render(); out != before.Render() {
		t.Errorf("Render changed:\n%s\nwant:\n%s", out, before.Render())
	}
}

func TestPolicy_CheckInstruct(t *testing.T) {
	allowed := mustParse(t, "title: x\njobs:\n  main:\n    steps: [{message: echo hi}]\n")
	forbidden := mustParse(t, "title: x\ninstruct: false\njobs:\n  main:\n    steps: [{message: echo hi}]\n")

	tests := []struct {
		name    string
		wf      *Workflow
		history []ActionRecord
		wantErr string
	}{
		{
			name: "instruct on",
			wf:   allowed,
		},
		{
			name:    "instruct off",
			wf:      forbidden,
			wantErr: "does not allow free-form instructions",
		},
		{
			name:    "an instruction is not bounded by a total budget",
			wf:      allowed,
			history: manyEchoes(20),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := NewPolicy(tt.wf, tt.history)
			err := pol.CheckInstruct()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckInstruct: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestPolicy_CheckFinish(t *testing.T) {
	bound := boundFixIssue(t)

	tests := []struct {
		name    string
		status  string
		history []ActionRecord
		wantErr string
	}{
		{
			name:    "done before a step ran",
			status:  "done",
			wantErr: "has not run yet",
		},
		{
			name:   "done after the last step succeeded",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw push", StepID: "push", Outcome: Outcome{Status: OutcomeOK}},
			},
		},
		{
			name:   "done after the last step had nothing to do",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw push", StepID: "push", Outcome: Outcome{Status: OutcomeNoop}},
			},
		},
		{
			name:   "done after the last step failed",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw push", StepID: "push", Outcome: Outcome{Status: OutcomeFailed}},
			},
			wantErr: "must be ok or noop",
		},
		{
			name:   "the latest outcome of the last step wins",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw push", StepID: "push", Outcome: Outcome{Status: OutcomeOK}},
				{Kind: ActionKindRun, Command: "/gtw push", StepID: "push", Outcome: Outcome{Status: OutcomeFailed}},
			},
			wantErr: "must be ok or noop",
		},
		{
			name:   "failed is always allowed",
			status: "failed",
		},
		{
			name:    "unknown status",
			status:  "stopped",
			wantErr: "must be",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := NewPolicy(bound, tt.history)
			err := pol.CheckFinish(tt.status)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckFinish: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestPolicy_Remaining(t *testing.T) {
	pol := NewPolicy(boundFixIssue(t), []ActionRecord{
		{Kind: ActionKindRun, Command: "/review", StepID: "review"},
		{Kind: ActionKindInstruct, Command: "keep going"},
	})
	pol.Budget = 5
	rem := pol.Remaining()
	if len(rem.Steps) != 5 {
		t.Fatalf("Steps = %d, want 5", len(rem.Steps))
	}
	if rem.Steps[2] != 4 {
		t.Errorf("Steps[2] = %d, want 4", rem.Steps[2])
	}
	if rem.Steps[0] != 5 {
		t.Errorf("Steps[0] = %d, want 5 (the step spent nothing)", rem.Steps[0])
	}
}

// TestPolicy_RemainingWithoutABudget — a policy without a budget has
// no limit at all.
func TestPolicy_RemainingWithoutABudget(t *testing.T) {
	pol := NewPolicy(boundFixIssue(t), nil)
	for i, left := range pol.Remaining().Steps {
		if left != -1 {
			t.Errorf("Steps[%d] = %d, want -1 (no budget)", i, left)
		}
	}
}

func TestPolicy_CheckRun_StepCommandsOmitTheExhaustedStep(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: echo
      - id: two
        message: other
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, Command: "echo", StepID: "one"},
	})
	pol.Budget = 1

	if _, err := pol.CheckRun("echo"); err == nil || !strings.Contains(err.Error(), "limit is 1") {
		t.Errorf("expected step-limit error, got %v", err)
	}
	_, err := pol.CheckRun("nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "echo") {
		t.Errorf("the step list mentions the exhausted command: %v", err)
	}
}

func TestPolicy_InstructionIsNotAStepAction(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: echo hi
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindInstruct, Command: "keep going", Outcome: Outcome{Status: OutcomeOK}},
	})
	pol.Budget = 1

	if err := pol.CheckFinish("done"); err == nil || !strings.Contains(err.Error(), "has not run yet") {
		t.Errorf("expected has-not-run-yet error, got %v", err)
	}
	if rem := pol.Remaining(); rem.Steps[0] != 1 {
		t.Errorf("Steps[0] = %d, want 1 (the instruction took no chance)", rem.Steps[0])
	}
}

func TestPolicy_Render(t *testing.T) {
	pol := NewPolicy(boundFixIssue(t), nil)
	pol.Budget = 5
	out := pol.Render()
	for _, want := range []string{
		"workflow: fix-issue",
		"title: 把队列里的一个 issue 做完并推上去",
		"budget: 5 chances per step in this round",
		"1. /gtw fix 42 -y",
		"3. /review",
		"5. /gtw push",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered workflow does not contain %q:\n%s", want, out)
		}
	}
}

func TestPolicy_Render_ShowsTheOtherStepKinds(t *testing.T) {
	wf := mustParse(t, `
title: mixed
jobs:
  main:
    steps:
      - id: shell
        shell: make test
      - id: action
        use: notify
`)
	out := NewPolicy(wf, nil).Render()
	for _, want := range []string{"(shell) make test", "(use) notify"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered workflow does not contain %q:\n%s", want, out)
		}
	}
}

func TestPolicy_CheckStep(t *testing.T) {
	bound := boundFixIssue(t)
	pol := NewPolicy(bound, nil)
	pol.Budget = 5
	if err := pol.CheckStep(0); err != nil {
		t.Fatalf("CheckStep: %v", err)
	}
	reviews := make([]ActionRecord, 5)
	for i := range reviews {
		reviews[i] = ActionRecord{Kind: ActionKindRun, Command: "/review", StepID: "review"}
	}
	pol = NewPolicy(bound, reviews)
	pol.Budget = 5
	if err := pol.CheckStep(2); err == nil || !strings.Contains(err.Error(), "limit is 5") {
		t.Errorf("expected step-limit error, got %v", err)
	}
}

// TestPolicy_AStallCountsAgainstTheStep — a turn of the chat model
// that produced no tool call costs a chance of the step the judgment
// was about.
func TestPolicy_AStallCountsAgainstTheStep(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: echo
`)
	pol := NewPolicy(wf, nil)
	pol.Budget = 3
	pol.Record(ActionRecord{Kind: ActionKindStall, StepID: "one"})
	pol.Record(ActionRecord{Kind: ActionKindStall, StepID: "one"})

	if rem := pol.Remaining(); rem.Steps[0] != 1 {
		t.Errorf("Steps[0] = %d, want 1 (the stalls took two chances)", rem.Steps[0])
	}
	if pol.ConsecutiveStalls() != 2 {
		t.Errorf("ConsecutiveStalls() = %d, want 2", pol.ConsecutiveStalls())
	}
	pol.Record(ActionRecord{Kind: ActionKindRun, StepID: "one"})
	if pol.ConsecutiveStalls() != 0 {
		t.Errorf("ConsecutiveStalls() = %d, want 0 (the agent acted)", pol.ConsecutiveStalls())
	}
}

// TestPolicy_AResetStartsTheBudgetOver — the operator took the chat
// back: the step gets a fresh budget.
func TestPolicy_AResetStartsTheBudgetOver(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: echo
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, StepID: "one"},
		{Kind: ActionKindStall, StepID: "one"},
	})
	pol.Budget = 2
	if rem := pol.Remaining(); rem.Steps[0] != 0 {
		t.Fatalf("Steps[0] = %d, want 0 (the budget is spent)", rem.Steps[0])
	}

	pol.Record(ActionRecord{Kind: ActionKindReset, StepID: "one"})
	if rem := pol.Remaining(); rem.Steps[0] != 2 {
		t.Errorf("Steps[0] = %d, want 2 (the budget starts over)", rem.Steps[0])
	}
	if pol.ConsecutiveStalls() != 0 {
		t.Errorf("ConsecutiveStalls() = %d, want 0", pol.ConsecutiveStalls())
	}
}
