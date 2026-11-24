package wfe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
        description: 从 nightme/assigned 队列里挑一个
      - id: commit
        message: "/gtw commit"
      - id: review
        message: "/review"
        description: 没跑完就 /review -c；有改动就先 /gtw commit
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
// A workflow is a bundle: the file sits in a directory under the
// workflows directory, and the directory's path is the workflow's
// name.
func mustLoadFile(t *testing.T, name, body string) *Workflow {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name, "workflow.yml")
	writeFile(t, path, body)
	wf, err := LoadFile(dir, path)
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

// TestParse_Iterations — a job may repeat.
func TestParse_Iterations(t *testing.T) {
	wf := mustParse(t, `
title: rounds
jobs:
  main:
    iterations: 3
    steps:
      - id: fix
        message: "/gtw fix"
`)
	job := wf.Jobs["main"]
	if job.Iterations == nil || *job.Iterations != 3 {
		t.Fatalf("Iterations = %v, want 3", job.Iterations)
	}
	if job.Rounds() != 3 {
		t.Errorf("Rounds() = %d, want 3", job.Rounds())
	}
}

// TestParse_IgnoresTheKeysAStepCarries — a key the schema does not
// have is ignored: the step keeps the kind it names.
func TestParse_IgnoresTheKeysAStepCarries(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  string
	}{
		{"attempts", "attempts: 5"},
		{"allow", `allow: ["/gtw push"]`},
		{"agent", "agent: codex"},
		{"run", "run: echo"},
		{"prompt", "prompt: hello"},
		{"a misspelled key", "messege: hi"},
		{"a value of the wrong type", `attempts: "many"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wf := mustParse(t, "title: x\njobs:\n  main:\n    steps: [{message: hi, "+tt.key+"}]\n")
			step := wf.Jobs["main"].Steps[0]
			if step.Message != "hi" {
				t.Errorf("Message = %q, want the message the step kept", step.Message)
			}
			if step.Kind() != StepKindMessage {
				t.Errorf("Kind = %v, want message", step.Kind())
			}
		})
	}
}

// TestParse_IgnoresAnUnknownKeyAtEveryLevel — a key the schema never
// had is treated exactly like a retired one.
func TestParse_IgnoresAnUnknownKeyAtEveryLevel(t *testing.T) {
	wf := mustParse(t, `
title: x
messege: hello
args:
  - name: issue
    default: "*"
    description: the issue to fix
on:
  schedulee:
    - cron: "0 2 * * *"
jobs:
  main:
    worker: 2
    steps:
      - id: one
        message: hi
        iff: always
`)
	if wf.Title != "x" {
		t.Errorf("Title = %q, want x", wf.Title)
	}
	if len(wf.Args) != 1 || wf.Args[0].Name != "issue" {
		t.Errorf("Args = %v, want one argument named issue", wf.Args)
	}
	if len(wf.On.Schedule) != 0 {
		t.Errorf("On.Schedule = %v, want none", wf.On.Schedule)
	}
	step := wf.Jobs["main"].Steps[0]
	if step.Message != "hi" {
		t.Errorf("Message = %q, want hi", step.Message)
	}
	if step.If != "" {
		t.Errorf("If = %q, want the condition to be empty", step.If)
	}
}

// TestParse_AStepWithOnlyAnUnusedKindFails — a step still has to name
// one kind: the keys the schema does not have name none.
func TestParse_AStepWithOnlyAnUnusedKindFails(t *testing.T) {
	for _, src := range []string{
		"title: x\njobs:\n  main:\n    steps: [{run: echo}]\n",
		"title: x\njobs:\n  main:\n    steps: [{prompt: hello}]\n",
	} {
		_, err := Parse([]byte(src))
		if err == nil {
			t.Errorf("a step with no kind was accepted:\n%s", src)
			continue
		}
		want := "exactly one of message / shell / use / wait must be set (got 0)"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// TestParse_ARootKeyThatNamesNoJobIsIgnored — the steps live in
// `jobs:`: a file that carries the old key has no jobs.
func TestParse_ARootKeyThatNamesNoJobIsIgnored(t *testing.T) {
	_, err := Parse([]byte("title: x\nsteps:\n  - /gtw commit\n"))
	if err == nil || !strings.Contains(err.Error(), "jobs is required") {
		t.Errorf("error = %v, want the jobs to be required", err)
	}
}

// TestParse_ARootKeyThatNamesNoTitleIsIgnored — the workflow's name is
// the name of the file: a file that carries the old key has no title.
func TestParse_ARootKeyThatNamesNoTitleIsIgnored(t *testing.T) {
	for _, src := range []string{
		"name: fix\njobs:\n  main:\n    steps: [{message: hi}]\n",
		"goal: fix\njobs:\n  main:\n    steps: [{message: hi}]\n",
	} {
		_, err := Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "title is required") {
			t.Errorf("error = %v, want the title to be required", err)
		}
	}
}

// TestParse_TheRetiredFinalIsNotRead — a step used to name the step
// that cleans up after the round. The load path ignores the keys the
// schema does not have, so the file loads and the step is one like any
// other.
func TestParse_TheRetiredFinalIsNotRead(t *testing.T) {
	withFinal := mustParse(t, `
title: rounds
jobs:
  main:
    steps:
      - id: fix
        message: "/gtw fix"
      - id: cleanup
        message: "/gtw close"
        final: true
`)
	withoutFinal := mustParse(t, `
title: rounds
jobs:
  main:
    steps:
      - id: fix
        message: "/gtw fix"
      - id: cleanup
        message: "/gtw close"
`)
	if !reflect.DeepEqual(withFinal.Jobs["main"].Steps, withoutFinal.Jobs["main"].Steps) {
		t.Errorf("the steps = %+v, want them to be the steps of a file without the key",
			withFinal.Jobs["main"].Steps)
	}
}

// TestParse_AConditionThatCallsAnUnknownFunctionIsRefused
// — a condition is parsed when the file is loaded: a function the
// language does not have is refused there.
func TestParse_AConditionThatCallsAnUnknownFunctionIsRefused(t *testing.T) {
	_, err := Parse([]byte(`
title: x
jobs:
  main:
    steps:
      - id: one
        message: "/one"
        if: ${{ always() }}
`))
	if err == nil {
		t.Fatal("the workflow was loaded")
	}
	if !strings.Contains(err.Error(), `unknown function "always"`) {
		t.Errorf("error = %v, want it to name the function", err)
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
			name: "a name that names no title",
			src: `
name: x
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: "title is required",
		},
		{
			name: "a goal that names no title",
			src: `
goal: x
jobs:
  main:
    steps: [{message: hi}]
`,
			wantErr: "title is required",
		},
		{
			name: "steps that name no job",
			src: `
title: x
steps:
  - /gtw commit
`,
			wantErr: "jobs is required",
		},
		{
			name: "a run key that names no kind",
			src: `
title: x
jobs:
  main:
    steps: [{run: echo}]
`,
			wantErr: "exactly one of message / shell / use / wait must be set",
		},
		{
			name: "a prompt key that names no kind",
			src: `
title: x
jobs:
  main:
    steps: [{prompt: hello}]
`,
			wantErr: "exactly one of message / shell / use / wait must be set",
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
    steps: [{description: nothing to do}]
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

// TestParse_TheShapeErrorsKeepTheirWording — the shape of a workflow
// is checked, and the error says what is wrong.
func TestParse_TheShapeErrorsKeepTheirWording(t *testing.T) {
	for _, tt := range []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "args that are not a list",
			src:     "title: x\nargs: hi\njobs:\n  main:\n    steps: [{message: hi}]\n",
			wantErr: "`args:` must be a list (got a value)",
		},
		{
			name:    "an argument that is not a mapping",
			src:     "title: x\nargs: [hi]\njobs:\n  main:\n    steps: [{message: hi}]\n",
			wantErr: "`args:`: expected a mapping, got a value",
		},
		{
			name:    "a trigger that is not a mapping",
			src:     "title: x\non: hi\njobs:\n  main:\n    steps: [{message: hi}]\n",
			wantErr: "`on:` must be a mapping (got a value)",
		},
		{
			name:    "jobs that are not a mapping",
			src:     "title: x\njobs: hi\n",
			wantErr: "`jobs:` must be a mapping (got a value)",
		},
		{
			name:    "a job that is not a mapping",
			src:     "title: x\njobs: {main: hi}\n",
			wantErr: `job "main": expected a mapping, got a value`,
		},
		{
			name:    "steps that are not a list",
			src:     "title: x\njobs:\n  main:\n    steps: hi\n",
			wantErr: "job \"main\": `steps:` must be a list (got a value)",
		},
		{
			name:    "a step that is not a mapping",
			src:     "title: x\njobs:\n  main:\n    steps: [hi]\n",
			wantErr: "job \"main\": `steps:`: expected a mapping, got a value",
		},
	} {
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

// TestParse_IgnoresTheRetiredAllowKey — a step that still carries an
// allow list loads: the key is ignored.
func TestParse_IgnoresTheRetiredAllowKey(t *testing.T) {
	src := `title: x
jobs:
  main:
    steps:
      - id: one
        message: /gtw commit
        allow: ["/gtw push"]
`
	wf, err := parse([]byte(src))
	if err != nil {
		t.Fatalf("the unused key was refused: %v", err)
	}
	if got := wf.Jobs["main"].Steps[0].Message; got != "/gtw commit" {
		t.Errorf("Message = %q, want the message the step kept", got)
	}
}

// TestLoadFile_TheNameIsTheBundle — the identity of a workflow is the
// path of the bundle it was loaded from, under the workflows dir.
func TestLoadFile_TheNameIsTheBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix", "workflow.yml")
	writeFile(t, path, `
title: do the thing
jobs:
  main:
    steps: [{message: hi}]
`)
	wf, err := LoadFile(dir, path)
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

// TestLoadFile_RefusesAFileThatIsNotAWorkflowFile — a file that is
// not the workflow file of a bundle is not a workflow.
func TestLoadFile_RefusesAFileThatIsNotAWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix", "extra.yml")
	writeFile(t, path, "title: x\njobs:\n  main:\n    steps: [{message: hi}]\n")

	_, err := LoadFile(dir, path)
	if err == nil || !strings.Contains(err.Error(), "not a workflow file") {
		t.Errorf("error = %v, want the file to be refused", err)
	}
}

// TestLoadFile_RefusesAFileThatIsNotInABundle — a file that lies
// directly in the workflows directory is not in a bundle.
func TestLoadFile_RefusesAFileThatIsNotInABundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.yml")
	writeFile(t, path, "title: x\njobs:\n  main:\n    steps: [{message: hi}]\n")

	_, err := LoadFile(dir, path)
	if err == nil || !strings.Contains(err.Error(), "not in a bundle directory") {
		t.Errorf("error = %v, want the file to be refused", err)
	}
}

// TestLoadFile_IgnoresTheRetiredKeys — a file that still names itself
// or states its purpose the old way loads: the keys are ignored.
func TestLoadFile_IgnoresTheRetiredKeys(t *testing.T) {
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
			body: "goal: x\ntitle: x\njobs:\n  main:\n    steps: [{message: hi}]\n",
		},
		{
			name: "steps at the root",
			body: "title: x\nsteps:\n  - /gtw commit\njobs:\n  main:\n    steps: [{message: hi}]\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fix", "workflow.yml")
			writeFile(t, path, tt.body)
			wf, err := LoadFile(dir, path)
			if err != nil {
				t.Fatalf("the unused key was refused: %v", err)
			}
			if wf.Title != "x" {
				t.Errorf("Title = %q, want x", wf.Title)
			}
			if got := wf.Jobs["main"].Steps[0].Message; got != "hi" {
				t.Errorf("Message = %q, want hi", got)
			}
		})
	}
}

// TestLoadFile_RefusesAFileWithoutATitle — the title says what the
// workflow is for, and the agent is given it.
func TestLoadFile_RefusesAFileWithoutATitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix", "workflow.yml")
	writeFile(t, path, "jobs:\n  main:\n    steps: [{message: hi}]\n")
	_, err := LoadFile(dir, path)
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Errorf("error = %v, want the title to be required", err)
	}
}

// TestLoadDir_ReadsBothFileNames — a bundle's workflow file is a
// workflow.yml or a workflow.yaml, and the bundle's path under the
// workflows dir is the name.
func TestLoadDir_ReadsBothFileNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix", "workflow.yaml"), "title: fix it\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "other", "workflow.yml"), "title: other it\njobs:\n  main:\n    steps: [{message: hi}]\n")

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
	writeFile(t, filepath.Join(dir, "fix", "workflow.yml"), "title: fix it\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "broken", "workflow.yml"), "goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "other", "workflow.yml"), "title: other it\njobs:\n  main:\n    steps: [{message: hi}]\n")

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
	if !strings.Contains(failures[0].Error(), "broken") {
		t.Errorf("failure = %v, want it to name the workflow", failures[0])
	}
}

// TestLoadDir_TwoFilesWithTheSameNameMakeItUnusable — the name is
// ambiguous, and neither file is offered under it.
func TestLoadDir_TwoFilesWithTheSameNameMakeItUnusable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix", "workflow.yaml"), "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "fix", "workflow.yml"), "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "other", "workflow.yml"), "title: other it\njobs:\n  main:\n    steps: [{message: hi}]\n")

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
	if !strings.Contains(msg, "workflow.yaml") || !strings.Contains(msg, "workflow.yml") {
		t.Errorf("failure = %v, want it to name both files", failures[0])
	}
}

// TestLoadDir_TheClashIsTheOnlyFailureOfAName — a name two files
// claim carries one failure: the clash, not the state of one of them.
func TestLoadDir_TheClashIsTheOnlyFailureOfAName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix", "workflow.yaml"), "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "fix", "workflow.yml"), "goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(wfs) != 0 {
		t.Fatalf("workflows = %v, want none", wfs)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	if !strings.Contains(failures[0].Error(), `duplicate workflow name "fix"`) {
		t.Errorf("failure = %v, want the clash", failures[0])
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
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

// TestWorkflow_UnmarshalJSON_MigratesTheRetiredGoal — a run that was
// persisted before the title replaced the goal keeps its purpose when
// the daemon resumes it.
func TestWorkflow_UnmarshalJSON_MigratesTheRetiredGoal(t *testing.T) {
	var wf Workflow
	raw := `{"name":"fix","goal":"do the thing","jobs":{"main":{"steps":[{"id":"s","message":"hi"}]}}}`
	if err := json.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if wf.Title != "do the thing" {
		t.Errorf("Title = %q, want the migrated goal", wf.Title)
	}
	if wf.Name != "fix" {
		t.Errorf("Name = %q, want fix", wf.Name)
	}
	job, ok := wf.Jobs["main"]
	if !ok || len(job.Steps) != 1 || job.Steps[0].Message != "hi" {
		t.Errorf("Jobs = %v, want the step of the persisted run", wf.Jobs)
	}
}

// TestWorkflow_UnmarshalJSON_KeepsTheTitle — a title that is there
// wins over the retired goal.
func TestWorkflow_UnmarshalJSON_KeepsTheTitle(t *testing.T) {
	var wf Workflow
	raw := `{"name":"fix","title":"the title","goal":"the old goal"}`
	if err := json.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if wf.Title != "the title" {
		t.Errorf("Title = %q, want the title", wf.Title)
	}
}

// TestWorkflowPaths_AnEmptyDirectoryHoldsNoWorkflows — an unset
// workflows directory holds nothing, not the workflows of the working
// directory.
func TestWorkflowPaths_AnEmptyDirectoryHoldsNoWorkflows(t *testing.T) {
	paths, err := WorkflowPaths("")
	if err != nil {
		t.Fatalf("WorkflowPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
}

// TestWorkflowPaths_ReadsTheTreeAndSkipsTheReservedDirectories — the
// loader walks the workflows directory at any depth, and the reserved
// directories are never descended into.
func TestWorkflowPaths_ReadsTheTreeAndSkipsTheReservedDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top", "workflow.yml"), "title: top\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "a", "b", "workflow.yml"), "title: deep\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "actions", "x", "workflow.yml"), "title: action\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "runs", "r.json"), "{}\n")

	paths, err := WorkflowPaths(dir)
	if err != nil {
		t.Fatalf("WorkflowPaths: %v", err)
	}
	want := []string{
		filepath.Join(dir, "a", "b", "workflow.yml"),
		filepath.Join(dir, "top", "workflow.yml"),
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// TestWorkflowPaths_OnlyTheWorkflowFileOfABundleIsAWorkflow — a file
// that lies directly in the workflows directory, and a file that sits
// beside a bundle's workflow.yml, are not workflows.
func TestWorkflowPaths_OnlyTheWorkflowFileOfABundleIsAWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix.yml"), "title: flat\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "nightme", "coder", "workflow.yml"), "title: coder\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "nightme", "coder", "extra.yml"), "title: extra\njobs:\n  main:\n    steps: [{message: hi}]\n")

	paths, err := WorkflowPaths(dir)
	if err != nil {
		t.Fatalf("WorkflowPaths: %v", err)
	}
	want := []string{filepath.Join(dir, "nightme", "coder", "workflow.yml")}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// TestWorkflowPaths_AFolderWithoutAWorkflowFileIsNoBundle — a
// directory that holds no workflow file offers nothing.
func TestWorkflowPaths_AFolderWithoutAWorkflowFileIsNoBundle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.md"), "# notes\n")

	paths, err := WorkflowPaths(dir)
	if err != nil {
		t.Fatalf("WorkflowPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
}

// TestWorkflowPaths_TheReservedDirectoriesAreNotWorkflows — a bundle
// under `actions` or `runs` is never offered.
func TestWorkflowPaths_TheReservedDirectoriesAreNotWorkflows(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "actions", "fix", "workflow.yml"), "title: fix\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "runs", "fix", "workflow.yml"), "title: fix\njobs:\n  main:\n    steps: [{message: hi}]\n")

	paths, err := WorkflowPaths(dir)
	if err != nil {
		t.Fatalf("WorkflowPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
}

// TestLoadDir_NamesTheWorkflowsByTheirPath — a workflow's name is the
// path of its bundle under the workflows directory.
func TestLoadDir_NamesTheWorkflowsByTheirPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix", "workflow.yml"), "title: fix\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "nightme", "coder", "workflow.yml"), "title: coder\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	names := make([]string, 0, len(wfs))
	for _, wf := range wfs {
		names = append(names, wf.Name)
	}
	want := []string{"fix", "nightme/coder"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestLoadFile_NamesTheWorkflowByItsPathUnderTheRoot — the loader
// names the workflow after its place under the root.
func TestLoadFile_NamesTheWorkflowByItsPathUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nightme", "coder", "workflow.yml")
	writeFile(t, path, "title: coder\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wf, err := LoadFile(root, path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if wf.Name != "nightme/coder" {
		t.Errorf("Name = %q, want nightme/coder", wf.Name)
	}
}

// TestLoadDir_TwoFilesThatClaimOneNameMakeItUnusable — a duplicate
// name is one failure, and neither file is offered under it.
func TestLoadDir_TwoFilesThatClaimOneNameMakeItUnusable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "nightme", "coder", "workflow.yml"), "title: coder\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "nightme", "coder", "workflow.yaml"), "title: coder\njobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(wfs) != 0 {
		t.Fatalf("workflows = %v, want none", wfs)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	if failures[0].Name != "nightme/coder" {
		t.Errorf("the failure names %q, want nightme/coder", failures[0].Name)
	}
	if !strings.Contains(failures[0].Err.Error(), "duplicate workflow name") {
		t.Errorf("the failure says %q, want it to name the clash", failures[0].Err)
	}
}

// TestWorkflowName_AnEmptyRootMeansTheFilesOwnBundle — a file loaded
// without a root keeps the name of the bundle it sits in.
func TestWorkflowName_AnEmptyRootMeansTheFilesOwnBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix", "workflow.yml")
	writeFile(t, path, "title: fix\njobs:\n  main:\n    steps: [{message: hi}]\n")

	if got := WorkflowName("", path); got != "fix" {
		t.Errorf("WorkflowName = %q, want fix", got)
	}
}

// TestWorkflowName_AFileOutsideTheRootKeepsTheNameOfItsBundle — a
// file that is not under the root has no path-relative name.
func TestWorkflowName_AFileOutsideTheRootKeepsTheNameOfItsBundle(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	path := filepath.Join(other, "fix", "workflow.yml")
	writeFile(t, path, "title: fix\njobs:\n  main:\n    steps: [{message: hi}]\n")

	if got := WorkflowName(root, path); got != "fix" {
		t.Errorf("WorkflowName = %q, want fix", got)
	}
}

// TestWorkflowName_AFileInTheRootHasNoName — the workflows directory
// is not a bundle: a file that lies in it has no name.
func TestWorkflowName_AFileInTheRootHasNoName(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workflow.yml")
	writeFile(t, path, "title: fix\njobs:\n  main:\n    steps: [{message: hi}]\n")

	if got := WorkflowName(root, path); got != "" {
		t.Errorf("WorkflowName = %q, want no name", got)
	}
}

// TestParse_TheRequiresIsRead — a workflow can say which nightme it
// needs.
func TestParse_TheRequiresIsRead(t *testing.T) {
	wf := mustParse(t, `
title: x
requires:
  nightme: ">=0.10.0"
jobs:
  main:
    steps: [{message: hi}]
`)
	if wf.Requires.Nightme != ">=0.10.0" {
		t.Errorf("Requires.Nightme = %q, want \">=0.10.0\"", wf.Requires.Nightme)
	}
}

// TestParse_ABadConstraintDoesNotLoad — a constraint that does not
// parse is a problem of the file: the workflow does not load.
func TestParse_ABadConstraintDoesNotLoad(t *testing.T) {
	_, err := Parse([]byte(`
title: x
requires:
  nightme: "0.10.0"
jobs:
  main:
    steps: [{message: hi}]
`))
	if err == nil {
		t.Fatal("Parse: no error, want one")
	}
	if !strings.Contains(err.Error(), "requires nightme") {
		t.Errorf("the error = %q, want it to name the requirement", err.Error())
	}
}

// TestParse_TheRetiredNoteIsNotRead — the step description used to be
// called a note. The load path ignores the keys the schema does not
// have, so the file loads and the description is empty.
func TestParse_TheRetiredNoteIsNotRead(t *testing.T) {
	wf := mustParse(t, `
title: x
jobs:
  main:
    steps:
      - id: one
        message: hi
        note: the old description
`)
	step := wf.Jobs["main"].Steps[0]
	if step.Description != "" {
		t.Errorf("Description = %q, want it to be empty", step.Description)
	}
}

// TestBind_TheRequiresSurvivesTheBinding — a bound workflow keeps the
// requirement of the source.
func TestBind_TheRequiresSurvivesTheBinding(t *testing.T) {
	wf := mustParse(t, `
title: x
requires:
  nightme: ">=0.10.0"
jobs:
  main:
    steps: [{message: hi}]
`)
	bound, err := Bind(wf, nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if bound.Requires.Nightme != ">=0.10.0" {
		t.Errorf("Requires.Nightme = %q, want \">=0.10.0\"", bound.Requires.Nightme)
	}
}
