package wfe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixIssueYAML is the workflow from the feature request.
const fixIssueYAML = `
title: 把队列里的一个 issue 做完并推上去
args:
  - name: issue
    default: "*"
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
        attempts: 3
      - id: commit
        message: "/gtw commit"
      - id: review
        message: "/review"
        note: 没跑完就 /review -c；有改动就先 /gtw commit
        allow: ["/review -c", "/gtw commit"]
        attempts: 5
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

// mustLoadFile writes a workflow file and loads it.
//
// The file's base name is the workflow's identity, so the file has to
// carry the name the test expects.
func mustLoadFile(t *testing.T, name, body string) *Workflow {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFile(t, path, body)
	wf, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return wf
}

func TestParse_Valid(t *testing.T) {
	wf := mustParse(t, fixIssueYAML)

	if wf.Title == "" {
		t.Error("Title is empty")
	}
	if len(wf.Args) != 1 || wf.Args[0].Name != "issue" {
		t.Fatalf("Args = %v, want one argument named issue", wf.Args)
	}
	if wf.Args[0].Default == nil || *wf.Args[0].Default != "*" {
		t.Errorf("Args[0].Default = %v, want *", wf.Args[0].Default)
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
	if fix.Attempts != 3 {
		t.Errorf("Steps[0].Attempts = %d, want 3", fix.Attempts)
	}
}

// TestParse_EveryTopLevelField — the schema carries these fields and
// nothing else.
func TestParse_EveryTopLevelField(t *testing.T) {
	wf := mustParse(t, `
title: do the thing
args:
  - name: issue
    default: "*"
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
	if len(wf.Args) != 1 || wf.Args[0].Name != "issue" {
		t.Errorf("Args = %v", wf.Args)
	}
	if wf.Title != "do the thing" {
		t.Errorf("Title = %q", wf.Title)
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
title: small
jobs:
  main:
    steps: [{message: echo hi}]
`)
	if !wf.Instruct {
		t.Error("Instruct = false, want true by default")
	}
	if len(wf.Workspaces) != 0 {
		t.Errorf("Workspaces = %v, want none", wf.Workspaces)
	}
}

func TestParse_ExplicitInstructFalse(t *testing.T) {
	wf := mustParse(t, `
title: small
instruct: false
jobs:
  main:
    steps: [{message: echo hi}]
`)
	if wf.Instruct {
		t.Error("Instruct = true, want false")
	}
}

func TestParse_SyntheticStepID(t *testing.T) {
	wf := mustParse(t, `
title: x
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

// TestParse_TheStepKinds — a step carries exactly one of the four
// kinds.
func TestParse_TheStepKinds(t *testing.T) {
	wf := mustParse(t, `
title: kinds
jobs:
  main:
    steps:
      - id: talk
        message: "/gtw commit"
      - id: build
        shell: make build
      - id: tell
        use: notify
        with:
          message: hi
      - id: rest
        wait: 5m
`)
	steps := wf.Jobs["main"].Steps
	kinds := []StepKind{
		steps[0].Kind(),
		steps[1].Kind(),
		steps[2].Kind(),
		steps[3].Kind(),
	}
	want := []StepKind{StepKindMessage, StepKindShell, StepKindUse, StepKindWait}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("Steps[%d].Kind() = %v, want %v", i, kinds[i], want[i])
		}
	}
	if steps[3].Wait != "5m" {
		t.Errorf("Wait = %q, want 5m", steps[3].Wait)
	}
}

// TestParse_IterationsAndAttempts — a job may repeat and a step may
// bound what the agent does with it.
func TestParse_IterationsAndAttempts(t *testing.T) {
	wf := mustParse(t, `
title: rounds
jobs:
  main:
    iterations: 3
    steps:
      - id: fix
        message: "/gtw fix"
        attempts: 5
`)
	job := wf.Jobs["main"]
	if job.Iterations == nil || *job.Iterations != 3 {
		t.Fatalf("Iterations = %v, want 3", job.Iterations)
	}
	if job.Rounds() != 3 {
		t.Errorf("Rounds() = %d, want 3", job.Rounds())
	}
	if job.Steps[0].Attempts != 5 {
		t.Errorf("Attempts = %d, want 5", job.Steps[0].Attempts)
	}
}

// TestParse_ZeroIterationsMeansUntilTheRunEnds — a job that does not
// say how many rounds it runs runs one round.
func TestParse_ZeroIterationsMeansUntilTheRunEnds(t *testing.T) {
	one := mustParse(t, "title: x\njobs:\n  main:\n    steps: [{message: hi}]\n")
	if one.Jobs["main"].Rounds() != 1 {
		t.Errorf("Rounds() = %d, want 1", one.Jobs["main"].Rounds())
	}
	forever := mustParse(t, "title: x\njobs:\n  main:\n    iterations: 0\n    steps: [{message: hi}]\n")
	if forever.Jobs["main"].Rounds() != 0 {
		t.Errorf("Rounds() = %d, want 0 (until the run ends)", forever.Jobs["main"].Rounds())
	}
}

// TestParse_AnActionNameMayComeFromAnArgument — `use:` takes an
// argument like every other field of a step.
func TestParse_AnActionNameMayComeFromAnArgument(t *testing.T) {
	wf := mustParse(t, `
title: act
args:
  - name: which
    default: notify
jobs:
  main:
    steps:
      - id: tell
        use: ${{ args.which }}
`)
	bound, err := Bind(wf, nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := bound.Jobs["main"].Steps[0].Use; got != "notify" {
		t.Errorf("Use = %q, want notify", got)
	}
}

// TestParse_AnUndeclaredArgumentIsRefusedEverywhere — the check covers
// every field of a step, `use:` included.
func TestParse_AnUndeclaredArgumentIsRefusedEverywhere(t *testing.T) {
	_, err := Parse([]byte(`
title: act
jobs:
  main:
    steps:
      - id: tell
        use: ${{ args.nope }}
`))
	if err == nil {
		t.Fatal("an undeclared argument was accepted")
	}
	if !strings.Contains(err.Error(), "unknown argument") {
		t.Errorf("error = %v, want it to name the argument", err)
	}
}

// TestParse_TheCountsMustBeWholeNumbers — a fraction would slip past
// a check on the decoded value.
func TestParse_TheCountsMustBeWholeNumbers(t *testing.T) {
	for _, src := range []string{
		"title: x\njobs:\n  main:\n    iterations: 1.5\n    steps: [{message: hi}]\n",
		"title: x\njobs:\n  main:\n    iterations: -0.5\n    steps: [{message: hi}]\n",
		"title: x\njobs:\n  main:\n    steps: [{message: hi, attempts: 0.5}]\n",
		"title: x\njobs:\n  main:\n    steps: [{message: hi, attempts: -0.5}]\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("a fraction was accepted:\n%s", src)
		}
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "missing title",
			src:     "jobs:\n  main:\n    steps: [{message: hi}]\n",
			wantErr: "title is required",
		},
		{
			name:    "missing jobs",
			src:     "title: x\n",
			wantErr: "jobs is required",
		},
		{
			name: "retired name key",
			src: `
name: x
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: `unknown key "name"`,
		},
		{
			name: "retired goal key",
			src: `
goal: x
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: `unknown key "goal"`,
		},
		{
			name: "retired top-level steps",
			src: `
title: x
steps:
  - /gtw commit
`,
			wantErr: "jobs:",
		},
		{
			name: "retired run key",
			src: `
title: x
jobs:
  main:
    steps: [{run: echo}]
`,
			wantErr: "shell:",
		},
		{
			name: "retired prompt key",
			src: `
title: x
jobs:
  main:
    steps: [{prompt: hello}]
`,
			wantErr: "message:",
		},
		{
			name: "retired agent key",
			src: `
title: x
jobs:
  main:
    steps: [{message: hi, agent: codex}]
`,
			wantErr: "agent:",
		},
		{
			name: "unknown top-level key",
			src: `
title: x
worker: 2
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: "worker",
		},
		{
			name: "retired action budget",
			src: `
title: x
max_actions: 30
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: "max_actions",
		},
		{
			name: "a wait that is not a duration",
			src: `
title: x
jobs:
  main:
    steps: [{wait: soon}]
`,
			wantErr: "soon",
		},
		{
			name: "unknown step key",
			src: `
title: x
jobs:
  main:
    steps: [{message: hi, retries: 3}]
`,
			wantErr: "retries",
		},
		{
			name: "two kinds",
			src: `
title: x
jobs:
  main:
    steps: [{message: hi, shell: echo}]
`,
			wantErr: "exactly one of message / shell / use / wait must be set",
		},
		{
			name: "no kind",
			src: `
title: x
jobs:
  main:
    steps: [{note: nothing to do}]
`,
			wantErr: "exactly one of message / shell / use / wait must be set",
		},
		{
			name: "unknown need",
			src: `
title: x
jobs:
  main:
    needs: [nonexistent]
    steps: [{message: hi}]
`,
			wantErr: "nonexistent",
		},
		{
			name: "two steps with the same id",
			src: `
title: x
jobs:
  a:
    steps: [{id: s, message: one}]
  b:
    steps: [{id: s, message: two}]
`,
			wantErr: `the step id "s" is used`,
		},
		{
			name: "a declared id that collides with a synthetic one",
			src: `
title: x
jobs:
  a:
    steps:
      - message: one
      - id: "a#1"
        message: two
`,
			wantErr: `the step id "a#1" is used`,
		},
		{
			name: "jobs in a circle",
			src: `
title: x
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
title: x
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

// TestLoadFile_TheNameIsTheFileName — the identity of a workflow is
// the base name of the file it was loaded from.
func TestLoadFile_TheNameIsTheFileName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix.yml")
	writeFile(t, path, `
title: do the thing
jobs:
  main:
    steps: [{message: hi}]
`)
	wf, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if wf.Name != "fix" {
		t.Errorf("Name = %q, want fix", wf.Name)
	}
	if wf.Title != "do the thing" {
		t.Errorf("Title = %q", wf.Title)
	}
}

// TestLoadFile_RefusesTheRetiredKeys — a file that still names itself
// or states its purpose the old way is refused, and the error says
// what to write instead.
func TestLoadFile_RefusesTheRetiredKeys(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "a name",
			body: "name: fix\ntitle: x\njobs:\n  main:\n    steps: [{message: hi}]\n",
		},
		{
			name: "a goal",
			body: "goal: x\njobs:\n  main:\n    steps: [{message: hi}]\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fix.yml")
			writeFile(t, path, tt.body)
			_, err := LoadFile(path)
			if err == nil {
				t.Fatal("the retired key was accepted")
			}
			msg := err.Error()
			if !strings.Contains(msg, "fix.yml") {
				t.Errorf("error = %v, want it to name the file", err)
			}
			if !strings.Contains(msg, "title") {
				t.Errorf("error = %v, want it to name the replacement", err)
			}
		})
	}
}

// TestLoadFile_RefusesAFileWithoutATitle — the title says what the
// workflow is for, and the agent is given it.
func TestLoadFile_RefusesAFileWithoutATitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix.yml")
	writeFile(t, path, "jobs:\n  main:\n    steps: [{message: hi}]\n")
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Errorf("error = %v, want the title to be required", err)
	}
}

// TestLoadDir_ReadsBothExtensions — a workflow is a *.yaml or a *.yml
// file, and its name is the base name of that file.
func TestLoadDir_ReadsBothExtensions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix.yaml"), "title: fix it\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "other.yml"), "title: other it\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(wfs) != 2 {
		t.Fatalf("workflows = %d, want 2", len(wfs))
	}
	names := []string{wfs[0].Name, wfs[1].Name}
	if names[0] != "fix" || names[1] != "other" {
		t.Errorf("names = %v, want fix and other", names)
	}
}

// TestLoadDir_AFileThatDoesNotLoadAffectsOnlyItself — a broken file
// does not take the workflows beside it down with it.
func TestLoadDir_AFileThatDoesNotLoadAffectsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix.yml"), "title: fix it\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "broken.yml"), "goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "other.yml"), "title: other it\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(wfs) != 2 {
		t.Fatalf("workflows = %d, want 2", len(wfs))
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	if failures[0].Name != "broken" {
		t.Errorf("the failure names %q, want broken", failures[0].Name)
	}
	if !strings.Contains(failures[0].Error(), "broken.yml") {
		t.Errorf("failure = %v, want it to name the file", failures[0])
	}
}

// TestLoadDir_TwoFilesWithTheSameNameMakeItUnusable — the name is
// ambiguous, and neither file is offered under it.
func TestLoadDir_TwoFilesWithTheSameNameMakeItUnusable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix.yaml"), "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "fix.yml"), "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "other.yml"), "title: other it\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(wfs) != 1 || wfs[0].Name != "other" {
		t.Fatalf("workflows = %v, want only other", wfs)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	msg := failures[0].Error()
	if !strings.Contains(msg, `duplicate workflow name "fix"`) {
		t.Errorf("failure = %v, want the duplicate", failures[0])
	}
	if !strings.Contains(msg, "fix.yaml") || !strings.Contains(msg, "fix.yml") {
		t.Errorf("failure = %v, want it to name both files", failures[0])
	}
}

// TestBind_ExpandsTheTitle — the title may hold an argument, the way
// the goal did.
func TestBind_ExpandsTheTitle(t *testing.T) {
	wf := mustParse(t, `
title: fix issue ${{ args.issue }}
args:
  - name: issue
    default: "42"
jobs:
  main:
    steps: [{message: hi}]
`)
	bound, err := Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if bound.Title != "fix issue 42" {
		t.Errorf("Title = %q, want the argument expanded", bound.Title)
	}
}

// writeFile writes a file, failing the test when it cannot.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
title: x
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
title: x
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
title: x
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

func TestBind_ExpandsAnArgumentInsideAListItem(t *testing.T) {
	wf := mustParse(t, `
title: x
args:
  - name: issue
    default: "42"
jobs:
  main:
    steps:
      - id: notify
        use: notify
        with:
          items:
            - name: "${{ args.issue }}"
`)
	bound, err := Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	with := bound.Jobs["main"].Steps[0].With
	items, ok := with["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("the with arguments = %v", with)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("the list item = %v", items[0])
	}
	if item["name"] != "42" {
		t.Errorf("the list item = %v, want the argument expanded", item)
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
