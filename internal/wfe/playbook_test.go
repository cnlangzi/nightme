package wfe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixIssueYAML is the playbook from the feature request: string
// shorthand steps mixed with one object step.
const fixIssueYAML = `
name: fix-issue
args: [id]
goal: "Fix issue #${{ args.id }} and ship it as a PR."
steps:
  - /gtw fix ${{ args.id }} -y
  - /gtw commit
  - run: /review
    note: If it fails or does not finish, continue with /review -c.
    allow: [/review -c, /gtw commit]
    max: 5
  - /gtw push
  - /gtw pr
`

// dagYAML is a DAG workflow; it must stay out of the playbook list.
const dagYAML = `
name: pr-reviewer
workspaces: [~/work/nightme]
on: { mention: {} }
jobs:
  review:
    steps: [{run: echo}]
`

func mustParsePlaybook(t *testing.T, src string) *Playbook {
	t.Helper()
	pb, err := ParsePlaybook([]byte(src))
	if err != nil {
		t.Fatalf("ParsePlaybook: %v", err)
	}
	return pb
}

func mustBind(t *testing.T, pb *Playbook, args map[string]string) *Playbook {
	t.Helper()
	bound, err := Bind(pb, args)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound
}

func TestParsePlaybook_Steps(t *testing.T) {
	pb := mustParsePlaybook(t, fixIssueYAML)

	if pb.Name != "fix-issue" {
		t.Errorf("Name = %q, want fix-issue", pb.Name)
	}
	if len(pb.Args) != 1 || pb.Args[0] != "id" {
		t.Errorf("Args = %v, want [id]", pb.Args)
	}
	if pb.Goal != "Fix issue #${{ args.id }} and ship it as a PR." {
		t.Errorf("Goal = %q", pb.Goal)
	}
	if pb.MaxActions != DefaultMaxActions {
		t.Errorf("MaxActions = %d, want %d", pb.MaxActions, DefaultMaxActions)
	}
	if !pb.Instruct {
		t.Error("Instruct = false, want true by default")
	}
	if len(pb.Steps) != 5 {
		t.Fatalf("Steps = %d, want 5", len(pb.Steps))
	}
	if pb.Steps[0].Run != "/gtw fix ${{ args.id }} -y" {
		t.Errorf("Steps[0].Run = %q", pb.Steps[0].Run)
	}
	if pb.Steps[1].Run != "/gtw commit" {
		t.Errorf("Steps[1].Run = %q", pb.Steps[1].Run)
	}
	review := pb.Steps[2]
	if review.Run != "/review" {
		t.Errorf("Steps[2].Run = %q, want /review", review.Run)
	}
	if review.Note == "" {
		t.Error("Steps[2].Note empty")
	}
	if len(review.Allow) != 2 || review.Allow[0] != "/review -c" || review.Allow[1] != "/gtw commit" {
		t.Errorf("Steps[2].Allow = %v", review.Allow)
	}
	if review.Max != 5 {
		t.Errorf("Steps[2].Max = %d, want 5", review.Max)
	}
	if pb.Steps[4].Run != "/gtw pr" {
		t.Errorf("Steps[4].Run = %q", pb.Steps[4].Run)
	}
}

func TestParsePlaybook_Defaults(t *testing.T) {
	pb := mustParsePlaybook(t, `
name: small
max_actions: 4
instruct: false
steps: [echo hi]
`)
	if pb.MaxActions != 4 {
		t.Errorf("MaxActions = %d, want 4", pb.MaxActions)
	}
	if pb.Instruct {
		t.Error("Instruct = true, want false")
	}
}

func TestParsePlaybook_Errors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "missing name",
			src:     "steps: [echo hi]\n",
			wantErr: "name is required",
		},
		{
			name:    "no steps",
			src:     "name: x\n",
			wantErr: "steps is required",
		},
		{
			name:    "empty run",
			src:     "name: x\nsteps:\n  - run: \"  \"\n",
			wantErr: "run is required",
		},
		{
			name:    "step max below one",
			src:     "name: x\nsteps:\n  - run: echo\n    max: 0\n",
			wantErr: "max must be >= 1",
		},
		{
			name:    "max_actions below one",
			src:     "name: x\nmax_actions: 0\nsteps: [echo hi]\n",
			wantErr: "max_actions must be >= 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePlaybook([]byte(tt.src))
			if err == nil {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBind(t *testing.T) {
	pb := mustParsePlaybook(t, fixIssueYAML)

	tests := []struct {
		name    string
		args    map[string]string
		wantErr string
	}{
		{
			name: "bound",
			args: map[string]string{"id": "42"},
		},
		{
			name:    "unknown argument",
			args:    map[string]string{"id": "42", "issue": "7"},
			wantErr: "unknown argument",
		},
		{
			name:    "missing argument",
			args:    map[string]string{},
			wantErr: "missing argument",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bound, err := Bind(pb, tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if bound.Goal != "Fix issue #42 and ship it as a PR." {
				t.Errorf("Goal = %q", bound.Goal)
			}
			if bound.Steps[0].Run != "/gtw fix 42 -y" {
				t.Errorf("Steps[0].Run = %q", bound.Steps[0].Run)
			}
			if pb.Steps[0].Run != "/gtw fix ${{ args.id }} -y" {
				t.Errorf("source playbook was modified: %q", pb.Steps[0].Run)
			}
		})
	}
}

func TestBind_UnknownArgumentInTemplate(t *testing.T) {
	pb := mustParsePlaybook(t, `
name: x
steps: ["/gtw fix ${{ args.id }}"]
`)
	_, err := Bind(pb, map[string]string{"id": "1"})
	if err == nil || !strings.Contains(err.Error(), "unknown argument") {
		t.Errorf("expected unknown-argument error, got %v", err)
	}
}

func TestBind_RejectsUnsupportedExpressions(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		args    map[string]string
		wantErr string
	}{
		{
			name:    "a function call",
			src:     "name: x\nsteps: [\"echo ${{ success() }}\"]\n",
			wantErr: "unsupported expression",
		},
		{
			name:    "another namespace",
			src:     "name: x\nsteps: [\"echo ${{ env.HOME }}\"]\n",
			wantErr: "unsupported expression",
		},
		{
			name:    "a nested argument",
			src:     "name: x\nargs: [id]\nsteps: [\"echo ${{ args.id.extra }}\"]\n",
			args:    map[string]string{"id": "42"},
			wantErr: "unsupported expression",
		},
		{
			name:    "an undeclared argument",
			src:     "name: x\nargs: [id]\nsteps: [\"echo ${{ args.nope }}\"]\n",
			args:    map[string]string{"id": "42"},
			wantErr: "unknown argument",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pb := mustParsePlaybook(t, tt.src)
			_, err := Bind(pb, tt.args)
			if err == nil {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBind_EmptyRunAfterBinding(t *testing.T) {
	pb := mustParsePlaybook(t, "name: x\nargs: [id]\nsteps: [\"${{ args.id }}\"]\n")
	_, err := Bind(pb, map[string]string{"id": ""})
	if err == nil || !strings.Contains(err.Error(), "run is required") {
		t.Errorf("expected run-is-required error, got %v", err)
	}
}

func TestPolicy_CheckRun(t *testing.T) {
	pb := mustParsePlaybook(t, fixIssueYAML)
	bound := mustBind(t, pb, map[string]string{"id": "42"})

	spent := make([]ActionRecord, DefaultMaxActions)
	for i := range spent {
		spent[i] = ActionRecord{Kind: ActionKindRun, Command: "/gtw push", StepIndex: 3}
	}
	reviews := make([]ActionRecord, 5)
	for i := range reviews {
		reviews[i] = ActionRecord{Kind: ActionKindRun, Command: "/review", StepIndex: 2}
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
			wantIdx: 3,
		},
		{
			name:    "unknown command",
			command: "/gtw sync",
			wantErr: "is not allowed",
		},
		{
			name:    "command of another playbook",
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
	pb := mustParsePlaybook(t, fixIssueYAML)
	pol := NewPolicy(mustBind(t, pb, map[string]string{"id": "42"}), nil)

	_, err := pol.CheckRun("/gtw sync")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"/gtw fix 42 -y", "/gtw commit", "/review", "/gtw push", "/gtw pr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list %q", err, want)
		}
	}
}

func TestPolicy_CheckInstruct(t *testing.T) {
	allowed := mustParsePlaybook(t, "name: x\nsteps: [echo hi]\n")
	forbidden := mustParsePlaybook(t, "name: x\ninstruct: false\nsteps: [echo hi]\n")

	spent := make([]ActionRecord, DefaultMaxActions)
	for i := range spent {
		spent[i] = ActionRecord{Kind: ActionKindRun, Command: "echo hi", StepIndex: 0}
	}

	tests := []struct {
		name    string
		pb      *Playbook
		history []ActionRecord
		wantErr string
	}{
		{
			name: "instruct on",
			pb:   allowed,
		},
		{
			name:    "instruct off",
			pb:      forbidden,
			wantErr: "does not allow free-form instructions",
		},
		{
			name:    "action budget spent",
			pb:      allowed,
			history: spent,
			wantErr: "actions in total",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := NewPolicy(tt.pb, tt.history)
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
	pb := mustParsePlaybook(t, fixIssueYAML)
	bound := mustBind(t, pb, map[string]string{"id": "42"})

	tests := []struct {
		name    string
		status  string
		history []ActionRecord
		wantErr string
	}{
		{
			name:    "done before the last step ran",
			status:  "done",
			wantErr: "has not run yet",
		},
		{
			name:   "done after the last step succeeded",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw pr", StepIndex: 4, Outcome: Outcome{Status: OutcomeOK}},
			},
		},
		{
			name:   "done after the last step had nothing to do",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw pr", StepIndex: 4, Outcome: Outcome{Status: OutcomeNoop}},
			},
		},
		{
			name:   "done after the last step failed",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw pr", StepIndex: 4, Outcome: Outcome{Status: OutcomeFailed}},
			},
			wantErr: "must be ok or noop",
		},
		{
			name:   "the latest outcome of the last step wins",
			status: "done",
			history: []ActionRecord{
				{Kind: ActionKindRun, Command: "/gtw pr", StepIndex: 4, Outcome: Outcome{Status: OutcomeOK}},
				{Kind: ActionKindRun, Command: "/gtw pr", StepIndex: 4, Outcome: Outcome{Status: OutcomeFailed}},
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
	pb := mustParsePlaybook(t, fixIssueYAML)
	bound := mustBind(t, pb, map[string]string{"id": "42"})

	pol := NewPolicy(bound, []ActionRecord{
		{Kind: ActionKindRun, Command: "/review", StepIndex: 2},
		{Kind: ActionKindInstruct, Command: "keep going", StepIndex: -1},
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
	if rem.Steps[0] != -1 {
		t.Errorf("Steps[0] = %d, want -1 (no limit)", rem.Steps[0])
	}
}

func TestPolicy_CheckRun_AllowedListOmitsExhaustedOwner(t *testing.T) {
	pb := mustParsePlaybook(t, `
name: x
steps:
  - run: echo
    max: 1
  - run: other
    allow: [echo]
`)
	pol := NewPolicy(pb, []ActionRecord{
		{Kind: ActionKindRun, Command: "echo", StepIndex: 0},
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
	pb := mustParsePlaybook(t, `
name: x
steps:
  - run: echo hi
    max: 1
`)
	pol := NewPolicy(pb, []ActionRecord{
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
	pb := mustParsePlaybook(t, fixIssueYAML)
	bound := mustBind(t, pb, map[string]string{"id": "42"})

	out := NewPolicy(bound, nil).Render()
	for _, want := range []string{
		"playbook: fix-issue",
		"goal: Fix issue #42 and ship it as a PR.",
		"limits: 30 actions in total",
		"1. /gtw fix 42 -y",
		"3. /review",
		"note: If it fails",
		"also allowed: /review -c, /gtw commit",
		"limit: 5 actions",
		"5. /gtw pr",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered playbook does not contain %q:\n%s", want, out)
		}
	}
}

func TestLoadDir_SeparatesPlaybooksFromWorkflows(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dag.yaml"), []byte(dagYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "playbook.yaml"), []byte(fixIssueYAML), 0644); err != nil {
		t.Fatal(err)
	}

	wfs, pbs, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(wfs) != 1 || wfs[0].Name != "pr-reviewer" {
		t.Errorf("workflows = %v, want one pr-reviewer", wfs)
	}
	if len(pbs) != 1 || pbs[0].Name != "fix-issue" {
		t.Errorf("playbooks = %v, want one fix-issue", pbs)
	}
}

func TestParse_RejectsPlaybook(t *testing.T) {
	_, err := Parse([]byte(fixIssueYAML))
	if err == nil || !strings.Contains(err.Error(), "playbook") {
		t.Errorf("expected playbook error, got %v", err)
	}
}

func TestLoadDir_DuplicatePlaybookName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fixIssueYAML), 0644); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate playbook name") {
		t.Errorf("expected duplicate-playbook error, got %v", err)
	}
}

func TestLoadDir_RejectsFileWithJobsAndSteps(t *testing.T) {
	dir := t.TempDir()
	src := dagYAML + "\nsteps: [echo hi]\n"
	if err := os.WriteFile(filepath.Join(dir, "both.yaml"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "both steps and jobs") {
		t.Errorf("expected both-steps-and-jobs error, got %v", err)
	}
}
