package wfe

import (
	"strings"
	"testing"
)

// boundFixIssue binds the example workflow to an issue number.
func boundFixIssue(t *testing.T) *Workflow {
	t.Helper()
	wf := mustParse(t, fixIssueYAML)
	bound, err := Bind(wf, map[string]string{"issue": "42"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound
}

func TestPolicy_CheckRun(t *testing.T) {
	bound := boundFixIssue(t)

	spent := make([]ActionRecord, DefaultMaxActions)
	for i := range spent {
		spent[i] = ActionRecord{Kind: ActionKindRun, Command: "/gtw push", StepID: "push"}
	}
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
			name:    "allowed command",
			command: "/review -c",
			wantIdx: 2,
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
			name:    "step limit reached",
			command: "/review",
			history: reviews,
			wantErr: "limit is 5",
		},
		{
			name:    "action budget spent",
			command: "/gtw push",
			history: spent,
			wantErr: "actions in total",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := NewPolicy(bound, tt.history)
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
		})
	}
}

func TestPolicy_CheckRun_ErrorListsAllowedCommands(t *testing.T) {
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

func TestPolicy_CheckInstruct(t *testing.T) {
	allowed := mustParse(t, "name: x\njobs:\n  main:\n    steps: [{message: echo hi}]\n")
	forbidden := mustParse(t, "name: x\ninstruct: false\njobs:\n  main:\n    steps: [{message: echo hi}]\n")

	spent := make([]ActionRecord, DefaultMaxActions)
	for i := range spent {
		spent[i] = ActionRecord{Kind: ActionKindRun, Command: "echo hi", StepID: "main#1"}
	}

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
			name:    "action budget spent",
			wf:      allowed,
			history: spent,
			wantErr: "actions in total",
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
	rem := pol.Remaining()
	if rem.Actions != DefaultMaxActions-2 {
		t.Errorf("Actions = %d, want %d", rem.Actions, DefaultMaxActions-2)
	}
	if len(rem.Steps) != 5 {
		t.Fatalf("Steps = %d, want 5", len(rem.Steps))
	}
	if rem.Steps[2] != 4 {
		t.Errorf("Steps[2] = %d, want 4", rem.Steps[2])
	}
	if rem.Steps[0] != 3 {
		t.Errorf("Steps[0] = %d, want 3 (the step's own limit)", rem.Steps[0])
	}
}

func TestPolicy_CheckRun_AllowedListOmitsExhaustedOwner(t *testing.T) {
	wf := mustParse(t, `
name: x
jobs:
  main:
    steps:
      - id: one
        message: echo
        max: 1
      - id: two
        message: other
        allow: [echo]
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindRun, Command: "echo", StepID: "one"},
	})

	if _, err := pol.CheckRun("echo"); err == nil || !strings.Contains(err.Error(), "limit is 1") {
		t.Errorf("expected step-limit error, got %v", err)
	}
	_, err := pol.CheckRun("nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "echo") {
		t.Errorf("allowed list mentions the exhausted command: %v", err)
	}
}

func TestPolicy_InstructionIsNotAStepAction(t *testing.T) {
	wf := mustParse(t, `
name: x
jobs:
  main:
    steps:
      - id: one
        message: echo hi
        max: 1
`)
	pol := NewPolicy(wf, []ActionRecord{
		{Kind: ActionKindInstruct, Command: "keep going", Outcome: Outcome{Status: OutcomeOK}},
	})

	if err := pol.CheckFinish("done"); err == nil || !strings.Contains(err.Error(), "has not run yet") {
		t.Errorf("expected has-not-run-yet error, got %v", err)
	}
	if rem := pol.Remaining(); rem.Steps[0] != 1 {
		t.Errorf("Steps[0] = %d, want 1 (the instruction took no step budget)", rem.Steps[0])
	}
}

func TestPolicy_Render(t *testing.T) {
	out := NewPolicy(boundFixIssue(t), nil).Render()
	for _, want := range []string{
		"workflow: fix-issue",
		"limits: 30 actions in total",
		"1. /gtw fix 42 -y",
		"3. /review",
		"also allowed: /review -c, /gtw commit",
		"limit: 5 actions",
		"5. /gtw push",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered workflow does not contain %q:\n%s", want, out)
		}
	}
}

func TestPolicy_Render_ShowsTheOtherStepKinds(t *testing.T) {
	wf := mustParse(t, `
name: mixed
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
	if err := pol.CheckStep(0); err != nil {
		t.Fatalf("CheckStep: %v", err)
	}
	reviews := make([]ActionRecord, 5)
	for i := range reviews {
		reviews[i] = ActionRecord{Kind: ActionKindRun, Command: "/review", StepID: "review"}
	}
	pol = NewPolicy(bound, reviews)
	if err := pol.CheckStep(2); err == nil || !strings.Contains(err.Error(), "limit is 5") {
		t.Errorf("expected step-limit error, got %v", err)
	}
}
