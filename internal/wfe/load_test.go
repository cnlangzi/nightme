package wfe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixIssueYAML is the workflow from the feature request.
const fixIssueYAML = `
name: fix-issue
args:
  - name: issue
    default: "*"
goal: 把队列里的一个 issue 做完并推上去
max_actions: 30
on:
  schedule:
    - cron: "0 2 * * *"
jobs:
  ship:
    steps:
      - id: fix
        message: "/gtw fix ${{ args.issue }} -y"
        note: 从 nightme/assigned 队列里挑一个
        allow: ["/gtw abort"]
        max: 3
      - id: commit
        message: "/gtw commit"
      - id: review
        message: "/review"
        note: 没跑完就 /review -c；有改动就先 /gtw commit
        allow: ["/review -c", "/gtw commit"]
        max: 5
      - id: commit2
        message: "/gtw commit"
      - id: push
        message: "/gtw push"
`

func mustParse(t *testing.T, src string) *Workflow {
	t.Helper()
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return wf
}

func TestParse_Valid(t *testing.T) {
	wf := mustParse(t, fixIssueYAML)

	if wf.Name != "fix-issue" {
		t.Errorf("Name = %q, want fix-issue", wf.Name)
	}
	if len(wf.Args) != 1 || wf.Args[0].Name != "issue" {
		t.Fatalf("Args = %v, want one argument named issue", wf.Args)
	}
	if wf.Args[0].Default == nil || *wf.Args[0].Default != "*" {
		t.Errorf("Args[0].Default = %v, want *", wf.Args[0].Default)
	}
	if wf.Goal == "" {
		t.Error("Goal is empty")
	}
	if wf.MaxActions != 30 {
		t.Errorf("MaxActions = %d, want 30", wf.MaxActions)
	}
	if !wf.Instruct {
		t.Error("Instruct = false, want true by default")
	}
	if len(wf.On.Schedule) != 1 || wf.On.Schedule[0].Cron != "0 2 * * *" {
		t.Errorf("On.Schedule = %v", wf.On.Schedule)
	}
	job := wf.Jobs["ship"]
	if len(job.Steps) != 5 {
		t.Fatalf("Steps = %d, want 5", len(job.Steps))
	}
	fix := job.Steps[0]
	if fix.ID != "fix" {
		t.Errorf("Steps[0].ID = %q, want fix", fix.ID)
	}
	if fix.Message != "/gtw fix ${{ args.issue }} -y" {
		t.Errorf("Steps[0].Message = %q", fix.Message)
	}
	if fix.Kind() != StepKindMessage {
		t.Errorf("Steps[0].Kind = %v, want message", fix.Kind())
	}
	if len(fix.Allow) != 1 || fix.Allow[0] != "/gtw abort" {
		t.Errorf("Steps[0].Allow = %v", fix.Allow)
	}
	if fix.Max != 3 {
		t.Errorf("Steps[0].Max = %d, want 3", fix.Max)
	}
}

// TestParse_EveryTopLevelField — the schema carries these fields and
// nothing else.
func TestParse_EveryTopLevelField(t *testing.T) {
	wf := mustParse(t, `
name: everything
args:
  - name: issue
    default: "*"
goal: do the thing
max_actions: 12
instruct: true
workspaces: [~/work/nightme]
on:
  schedule:
    - cron: "0 2 * * *"
jobs:
  main:
    steps:
      - id: one
        message: /gtw commit
`)
	if wf.Name != "everything" {
		t.Errorf("Name = %q", wf.Name)
	}
	if len(wf.Args) != 1 || wf.Args[0].Name != "issue" {
		t.Errorf("Args = %v", wf.Args)
	}
	if wf.Goal != "do the thing" {
		t.Errorf("Goal = %q", wf.Goal)
	}
	if wf.MaxActions != 12 {
		t.Errorf("MaxActions = %d, want 12", wf.MaxActions)
	}
	if !wf.Instruct {
		t.Error("Instruct = false, want true")
	}
	if len(wf.Workspaces) != 1 || wf.Workspaces[0] != "~/work/nightme" {
		t.Errorf("Workspaces = %v", wf.Workspaces)
	}
	if len(wf.On.Schedule) != 1 {
		t.Errorf("On.Schedule = %v", wf.On.Schedule)
	}
	if len(wf.Jobs) != 1 {
		t.Errorf("Jobs = %v", wf.Jobs)
	}
}

func TestParse_Defaults(t *testing.T) {
	wf := mustParse(t, `
name: small
jobs:
  main:
    steps: [{message: echo hi}]
`)
	if wf.MaxActions != DefaultMaxActions {
		t.Errorf("MaxActions = %d, want %d", wf.MaxActions, DefaultMaxActions)
	}
	if !wf.Instruct {
		t.Error("Instruct = false, want true by default")
	}
	if len(wf.Workspaces) != 0 {
		t.Errorf("Workspaces = %v, want none", wf.Workspaces)
	}
}

func TestParse_ExplicitInstructFalse(t *testing.T) {
	wf := mustParse(t, `
name: small
max_actions: 4
instruct: false
jobs:
  main:
    steps: [{message: echo hi}]
`)
	if wf.MaxActions != 4 {
		t.Errorf("MaxActions = %d, want 4", wf.MaxActions)
	}
	if wf.Instruct {
		t.Error("Instruct = true, want false")
	}
}

func TestParse_SyntheticStepID(t *testing.T) {
	wf := mustParse(t, `
name: x
jobs:
  ship:
    steps:
      - message: one
      - message: two
`)
	steps := wf.Jobs["ship"].Steps
	if steps[0].ID != "ship#1" {
		t.Errorf("Steps[0].ID = %q, want ship#1", steps[0].ID)
	}
	if steps[1].ID != "ship#2" {
		t.Errorf("Steps[1].ID = %q, want ship#2", steps[1].ID)
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "missing name",
			src:     "jobs:\n  main:\n    steps: [{message: hi}]\n",
			wantErr: "name is required",
		},
		{
			name:    "missing jobs",
			src:     "name: x\n",
			wantErr: "jobs is required",
		},
		{
			name: "retired top-level steps",
			src: `
name: x
steps:
  - /gtw commit
`,
			wantErr: "jobs:",
		},
		{
			name: "retired run key",
			src: `
name: x
jobs:
  main:
    steps: [{run: echo}]
`,
			wantErr: "shell:",
		},
		{
			name: "retired prompt key",
			src: `
name: x
jobs:
  main:
    steps: [{prompt: hello}]
`,
			wantErr: "message:",
		},
		{
			name: "retired agent key",
			src: `
name: x
jobs:
  main:
    steps: [{message: hi, agent: codex}]
`,
			wantErr: "agent:",
		},
		{
			name: "unknown top-level key",
			src: `
name: x
worker: 2
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: "worker",
		},
		{
			name: "unknown step key",
			src: `
name: x
jobs:
  main:
    steps: [{message: hi, retries: 3}]
`,
			wantErr: "retries",
		},
		{
			name: "two kinds",
			src: `
name: x
jobs:
  main:
    steps: [{message: hi, shell: echo}]
`,
			wantErr: "exactly one of message / shell / use must be set",
		},
		{
			name: "no kind",
			src: `
name: x
jobs:
  main:
    steps: [{note: nothing to do}]
`,
			wantErr: "exactly one of message / shell / use must be set",
		},
		{
			name: "unknown need",
			src: `
name: x
jobs:
  main:
    needs: [nonexistent]
    steps: [{message: hi}]
`,
			wantErr: "nonexistent",
		},
		{
			name: "jobs in a circle",
			src: `
name: x
jobs:
  a:
    needs: [b]
    steps: [{message: hi}]
  b:
    needs: [a]
    steps: [{message: hi}]
`,
			wantErr: "in a circle",
		},
		{
			name: "undeclared argument",
			src: `
name: x
jobs:
  main:
    steps: [{message: "${{ args.nope }}"}]
`,
			wantErr: `unknown argument "nope"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadDir_DuplicateName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.yaml", "b.yaml"} {
		os.WriteFile(filepath.Join(dir, name), []byte(`
name: dup
jobs:
  main:
    steps: [{message: hi}]
`), 0644)
	}
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-name error, got %v", err)
	}
}

func TestTopoSort(t *testing.T) {
	wf := &Workflow{
		Jobs: map[string]Job{
			"a": {Steps: []Step{{ID: "a1", Message: "x"}}},
			"b": {Needs: []string{"a"}, Steps: []Step{{ID: "b1", Message: "x"}}},
			"c": {Needs: []string{"b"}, Steps: []Step{{ID: "c1", Message: "x"}}},
		},
	}
	order := topoSort(wf.Jobs)
	if len(order) != 3 {
		t.Fatalf("order = %v, want 3 entries", order)
	}
	posA, posB, posC := -1, -1, -1
	for i, n := range order {
		switch n {
		case "a":
			posA = i
		case "b":
			posB = i
		case "c":
			posC = i
		}
	}
	if !(posA < posB && posB < posC) {
		t.Errorf("order %v does not respect deps a < b < c", order)
	}
}

func TestBind_DefaultArgument(t *testing.T) {
	wf := mustParse(t, fixIssueYAML)
	bound, err := Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	msg := bound.Jobs["ship"].Steps[0].Message
	if msg != "/gtw fix * -y" {
		t.Errorf("message = %q, want the default to be filled in", msg)
	}
}

func TestBind_MissingRequiredArgument(t *testing.T) {
	wf := mustParse(t, `
name: x
args: [{name: issue}]
jobs:
  main:
    steps: [{message: "/gtw fix ${{ args.issue }}"}]
`)
	_, err := Bind(wf, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), `missing argument "issue"`) {
		t.Errorf("expected a missing-argument error, got %v", err)
	}
}

func TestBind_UnknownArgument(t *testing.T) {
	wf := mustParse(t, `
name: x
args: [{name: issue}]
jobs:
  main:
    steps: [{message: hi}]
`)
	_, err := Bind(wf, map[string]string{"nope": "1"})
	if err == nil || !strings.Contains(err.Error(), `unknown argument "nope"`) {
		t.Errorf("expected an unknown-argument error, got %v", err)
	}
}

func TestBind_LeavesOtherExpressionsAlone(t *testing.T) {
	wf := mustParse(t, `
name: x
args: [{name: issue, default: "1"}]
jobs:
  main:
    steps:
      - id: a
        message: "/gtw fix ${{ args.issue }}"
      - id: b
        message: "echo ${{ steps.a.outputs.status }}"
`)
	bound, err := Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	msg := bound.Jobs["main"].Steps[1].Message
	if msg != "echo ${{ steps.a.outputs.status }}" {
		t.Errorf("message = %q, want the step expression left alone", msg)
	}
}

func TestBind_LeavesTheSourceAlone(t *testing.T) {
	wf := mustParse(t, fixIssueYAML)
	if _, err := Bind(wf, map[string]string{"issue": "42"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := wf.Jobs["ship"].Steps[0].Message; got != "/gtw fix ${{ args.issue }} -y" {
		t.Errorf("the source workflow was changed: %q", got)
	}
}
