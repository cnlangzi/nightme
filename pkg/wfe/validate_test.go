package wfe

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDiagnostic_ErrorRendersLikeACompiler — the diagnostic names the
// place of the problem and what is wrong with it. The segments it
// does not know are left out.
func TestDiagnostic_ErrorRendersLikeACompiler(t *testing.T) {
	for _, tt := range []struct {
		name string
		diag Diagnostic
		want string
	}{
		{
			name: "every segment",
			diag: Diagnostic{Path: "fix.yml", Line: 3, Column: 1, Message: "title is required"},
			want: "fix.yml:3:1: title is required",
		},
		{
			name: "no path",
			diag: Diagnostic{Line: 3, Column: 1, Message: "title is required"},
			want: "3:1: title is required",
		},
		{
			name: "no position",
			diag: Diagnostic{Path: "fix.yml", Message: "title is required"},
			want: "fix.yml: title is required",
		},
		{
			name: "nothing but the message",
			diag: Diagnostic{Message: "title is required"},
			want: "title is required",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.diag.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestValidate_AValidWorkflowHasNoProblem — an empty slice means the
// workflow is valid.
func TestValidate_AValidWorkflowHasNoProblem(t *testing.T) {
	diags := Validate([]byte(fixIssueYAML))
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
}

// TestValidate_ReportsEveryProblem — the validation surface is a
// linter: it reports every problem it found, not only the first.
func TestValidate_ReportsEveryProblem(t *testing.T) {
	diags := Validate([]byte(`
jobs:
  empty:
    steps: []
  main:
    steps:
      - message: hi
        shell: echo
`))
	if len(diags) != 3 {
		t.Fatalf("diagnostics = %v, want three", diags)
	}
	want := []string{
		"title is required",
		`job "empty" has no steps`,
		"exactly one of message / shell / use / wait must be set",
	}
	for i, w := range want {
		if !strings.Contains(diags[i].Message, w) {
			t.Errorf("diagnostics[%d] = %q, want it to mention %q", i, diags[i].Message, w)
		}
	}
}

// TestValidate_TheProblemsAreOrderedByPosition — the problems of one
// file come in the order they stand in the file.
func TestValidate_TheProblemsAreOrderedByPosition(t *testing.T) {
	diags := Validate([]byte(`
jobs:
  a:
    steps: []
  b:
    steps: []
  c:
    steps: []
`))
	if len(diags) != 4 {
		t.Fatalf("diagnostics = %v, want four", diags)
	}
	lines := []int{diags[0].Line, diags[1].Line, diags[2].Line, diags[3].Line}
	want := []int{2, 4, 6, 8}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("the lines of the diagnostics = %v, want %v", lines, want)
		}
	}
}

// TestValidate_TheUnknownKeyIsReported — a key the schema does not
// have is a problem: a typo changes what the workflow does without
// saying so.
func TestValidate_TheUnknownKeyIsReported(t *testing.T) {
	diags := Validate([]byte("titel: x\njobs:\n  main:\n    steps: [{message: hi}]\n"))
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %v, want two", diags)
	}
	var unknown *Diagnostic
	for i := range diags {
		if strings.Contains(diags[i].Message, "titel") {
			unknown = &diags[i]
		}
	}
	if unknown == nil {
		t.Fatalf("diagnostics = %v, want one naming the unknown key", diags)
	}
	if unknown.Line != 1 {
		t.Errorf("the diagnostic is on line %d, want the line of the key", unknown.Line)
	}
	if unknown.Column != 1 {
		t.Errorf("the diagnostic is on column %d, want the column of the key", unknown.Column)
	}
}

// TestValidate_TheUnknownKeyOfAJobIsReported — the key of a job is
// checked too, and the diagnostic says where it stands.
func TestValidate_TheUnknownKeyOfAJobIsReported(t *testing.T) {
	diags := Validate([]byte("title: x\njobs:\n  main:\n    need: [a]\n    steps: [{message: hi}]\n"))
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if !strings.Contains(diags[0].Message, "need") {
		t.Errorf("the diagnostic = %q, want it to name the key", diags[0].Message)
	}
	if diags[0].Line != 4 {
		t.Errorf("the diagnostic is on line %d, want the line of the key", diags[0].Line)
	}
}

// TestValidate_TheKnownKeysAreAccepted — a file that carries the
// keys of the schema reports nothing unknown.
func TestValidate_TheKnownKeysAreAccepted(t *testing.T) {
	diags := Validate([]byte(`
title: x
args:
  - name: issue
    default: "*"
instruct: true
workspaces: [~/work]
on:
  schedule:
    - cron: "0 2 * * *"
  pull_request:
    branches: [main]
    events: [opened]
  branch:
    patterns: ['release/*']
    events: [pushed]
  issue:
    events: [opened]
  mention:
    commands: [review]
jobs:
  main:
    needs: []
    if: "true"
    iterations: 1
    steps:
      - id: one
        if: "true"
        env:
          FOO: bar
        continue-on-error: true
        note: a note
        final: false
        message: hi
      - id: two
        shell: echo
      - id: three
        use: notify
        with:
          channel: feishu
      - id: four
        wait: 5m
`))
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
}

// TestValidate_TheUnknownKeyOfAStepIsReported — the key of a step is
// checked too.
func TestValidate_TheUnknownKeyOfAStepIsReported(t *testing.T) {
	diags := Validate([]byte("title: x\njobs:\n  main:\n    steps: [{message: hi, mesage: ho}]\n"))
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if !strings.Contains(diags[0].Message, "mesage") {
		t.Errorf("the diagnostic = %q, want it to name the key", diags[0].Message)
	}
}

// TestValidate_TheUnknownKeyIsReportedBesideAShapeProblem — the
// unknown-key check reads the tree, so it says what it found even
// when the shape of the document is wrong.
func TestValidate_TheUnknownKeyIsReportedBesideAShapeProblem(t *testing.T) {
	diags := Validate([]byte("args: notalist\nbogus: 1\njobs:\n  main:\n    steps: [{message: hi}]\n"))
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %v, want two", diags)
	}
	if !strings.Contains(diags[0].Message, "`args:` must be a list") {
		t.Errorf("the diagnostic = %q, want the shape problem", diags[0].Message)
	}
	if !strings.Contains(diags[1].Message, "bogus") {
		t.Errorf("the diagnostic = %q, want the unknown key", diags[1].Message)
	}
}

// TestValidate_ADecodeErrorCarriesItsLine — the decoder says where it
// gave up, and the line is the position of the problem.
func TestValidate_ADecodeErrorCarriesItsLine(t *testing.T) {
	diags := Validate([]byte("title: x\njobs:\n  main:\n    steps: [{message: {a: b}}]\n"))
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if diags[0].Line != 4 {
		t.Errorf("the diagnostic is on line %d, want the line the decoder reported", diags[0].Line)
	}
	if strings.HasPrefix(diags[0].Message, "line ") {
		t.Errorf("the diagnostic = %q, want the line out of the message", diags[0].Message)
	}
}

// TestValidate_AnUnknownNeedIsNotACircle — a job that needs a job
// that does not exist is not a circle.
func TestValidate_AnUnknownNeedIsNotACircle(t *testing.T) {
	diags := Validate([]byte("title: x\njobs:\n  a:\n    needs: [b]\n    steps: [{message: hi}]\n"))
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if !strings.Contains(diags[0].Message, `needs unknown job "b"`) {
		t.Errorf("the diagnostic = %q, want the unknown need", diags[0].Message)
	}
}

// TestValidate_JobsInACircleAreACircle — the circle is reported when
// the jobs really need each other.
func TestValidate_JobsInACircleAreACircle(t *testing.T) {
	diags := Validate([]byte(`
title: x
jobs:
  a:
    needs: [b]
    steps: [{message: hi}]
  b:
    needs: [a]
    steps: [{message: hi}]
`))
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "in a circle") {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %v, want the circle", diags)
	}
}

// TestKnownKeys_AreTheTagsOfTheStruct — the known keys are the yaml
// tags of the struct: a field that is added to one is a key the
// validator accepts.
func TestKnownKeys_AreTheTagsOfTheStruct(t *testing.T) {
	for _, tt := range []struct {
		name string
		typ  reflect.Type
	}{
		{"Workflow", reflect.TypeOf(Workflow{})},
		{"Arg", reflect.TypeOf(Arg{})},
		{"Job", reflect.TypeOf(Job{})},
		{"Step", reflect.TypeOf(Step{})},
		{"Trigger", reflect.TypeOf(Trigger{})},
		{"ScheduleEntry", reflect.TypeOf(ScheduleEntry{})},
		{"PRTrigger", reflect.TypeOf(PRTrigger{})},
		{"BranchTrigger", reflect.TypeOf(BranchTrigger{})},
		{"EventFilter", reflect.TypeOf(EventFilter{})},
		{"MentionTrigger", reflect.TypeOf(MentionTrigger{})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			known := knownKeys(tt.typ)
			for i := 0; i < tt.typ.NumField(); i++ {
				f := tt.typ.Field(i)
				tag := f.Tag.Get("yaml")
				if tag == "" || tag == "-" {
					if known[f.Name] {
						t.Errorf("the field %s is no key, but the validator knows it", f.Name)
					}
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if !known[name] {
					t.Errorf("the key %q of the field %s is not known", name, f.Name)
				}
			}
		})
	}
}

// TestKnownKeys_AFieldIsAKey — the known keys come from the struct,
// so a field that is added to one is a key the validator accepts.
func TestKnownKeys_AFieldIsAKey(t *testing.T) {
	type extended struct {
		Title string `yaml:"title"`
		Extra string `yaml:"extra"`
		Skip  string `yaml:"-"`
	}
	known := knownKeys(reflect.TypeOf(extended{}))
	if !known["title"] || !known["extra"] {
		t.Errorf("the known keys = %v, want the tags of the struct", known)
	}
	if known["Skip"] || known["-"] {
		t.Errorf("the known keys = %v, want no key for a field of \"-\"", known)
	}
}

// TestValidateFile_ReturnsTheProblemsOfTheFile — the diagnostics
// carry the file they were found in.
func TestValidateFile_ReturnsTheProblemsOfTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix.yml")
	writeFile(t, path, "jobs:\n  main:\n    steps: [{message: hi}]\n")

	diags := ValidateFile(path)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if diags[0].Path != path {
		t.Errorf("the diagnostic names %q, want %q", diags[0].Path, path)
	}
	if !strings.Contains(diags[0].Message, "title is required") {
		t.Errorf("the diagnostic = %q, want the title to be required", diags[0].Message)
	}
}

// TestValidateFile_AValidFileHasNoProblem — an empty slice means the
// workflow is valid.
func TestValidateFile_AValidFileHasNoProblem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix.yml")
	writeFile(t, path, "title: x\njobs:\n  main:\n    steps: [{message: hi}]\n")

	if diags := ValidateFile(path); len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
}

// TestValidateDir_ReturnsTheProblemsOfEveryFile — the diagnostics are
// ordered by path, then by position within the file.
func TestValidateDir_ReturnsTheProblemsOfEveryFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "workflow.yml"), "jobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "b", "workflow.yml"), "title: x\njobs:\n  main:\n    steps: []\n")

	diags := ValidateDir(dir)
	if len(diags) != 2 {
		t.Fatalf("diagnostics = %v, want two", diags)
	}
	if diags[0].Path != filepath.Join(dir, "a", "workflow.yml") {
		t.Errorf("the first diagnostic names %q, want the first file", diags[0].Path)
	}
	if diags[1].Path != filepath.Join(dir, "b", "workflow.yml") {
		t.Errorf("the second diagnostic names %q, want the second file", diags[1].Path)
	}
}

// TestValidateDir_AValidDirectoryHasNoProblem — an empty slice means
// every workflow is valid.
func TestValidateDir_AValidDirectoryHasNoProblem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix", "workflow.yml"), "title: x\njobs:\n  main:\n    steps: [{message: hi}]\n")

	if diags := ValidateDir(dir); len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
}

// TestValidateDir_TwoFilesClaimTheSameName — the name is ambiguous,
// and the clash is a problem of its own.
func TestValidateDir_TwoFilesClaimTheSameName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fix", "workflow.yml"), "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "fix", "workflow.yaml"), "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")

	diags := ValidateDir(dir)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if !strings.Contains(diags[0].Message, `duplicate workflow name "fix"`) {
		t.Errorf("the diagnostic = %q, want the clash", diags[0].Message)
	}
}

// TestParse_ReportsEveryProblem — the load path carries every problem
// it found, too.
func TestParse_ReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
jobs:
  empty:
    steps: []
  main:
    steps:
      - message: hi
        shell: echo
`))
	if err == nil {
		t.Fatal("the workflow was accepted")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want a *ValidationError", err)
	}
	if len(verr.Diagnostics) != 3 {
		t.Fatalf("the diagnostics = %v, want three", verr.Diagnostics)
	}
}

// TestParse_TheValidationFailureIsReachable — a caller tells a
// validation failure from a read failure.
func TestParse_TheValidationFailureIsReachable(t *testing.T) {
	_, err := Parse([]byte("jobs:\n  main:\n    steps: [{message: hi}]\n"))
	if !errors.Is(err, ErrValidationFailed) {
		t.Errorf("error = %v, want it to be a validation failure", err)
	}
}

// TestValidationError_RendersEveryDiagnostic — the error carries
// every problem, one per line.
func TestValidationError_RendersEveryDiagnostic(t *testing.T) {
	err := &ValidationError{Diagnostics: []Diagnostic{
		{Path: "fix.yml", Line: 3, Column: 1, Message: "title is required"},
		{Path: "fix.yml", Line: 7, Column: 5, Message: `job "main" has no steps`},
	}}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 {
		t.Fatalf("the error = %q, want two lines", err.Error())
	}
	if lines[0] != "fix.yml:3:1: title is required" {
		t.Errorf("the first line = %q", lines[0])
	}
	if lines[1] != `fix.yml:7:5: job "main" has no steps` {
		t.Errorf("the second line = %q", lines[1])
	}
}

// TestLoadFile_RefusesAWorkflowThatDoesNotValidate — the load path
// reports the failure as a validation error.
func TestLoadFile_RefusesAWorkflowThatDoesNotValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix", "workflow.yml")
	writeFile(t, path, "jobs:\n  main:\n    steps: [{message: hi}]\n")

	wf, err := LoadFile(dir, path)
	if err == nil {
		t.Fatal("the workflow was accepted")
	}
	if wf != nil {
		t.Errorf("the workflow = %v, want none", wf)
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want a *ValidationError", err)
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

// TestLoadDir_TheFailureOfAFileKeepsItsFields — a file that does not
// load is reported against the name it would have provided.
func TestLoadDir_TheFailureOfAFileKeepsItsFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken", "workflow.yml")
	writeFile(t, path, "jobs:\n  main:\n    steps: [{message: hi}]\n")

	wfs, failures := LoadDir(dir)
	if len(wfs) != 0 {
		t.Fatalf("workflows = %v, want none", wfs)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	if failures[0].Name != "broken" {
		t.Errorf("the failure names %q, want broken", failures[0].Name)
	}
	if failures[0].Path != path {
		t.Errorf("the failure names %q, want %q", failures[0].Path, path)
	}
	if failures[0].Err == nil {
		t.Fatal("the failure carries no reason")
	}
}

// TestLoadFile_IgnoresTheKeysTheSchemaDoesNotHave — the load path is
// lenient: a workflow file that carries extra fields keeps running.
func TestLoadFile_IgnoresTheKeysTheSchemaDoesNotHave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix", "workflow.yml")
	writeFile(t, path, "title: x\njobs:\n  main:\n    need: [a]\n    steps: [{message: hi}]\n")

	wf, err := LoadFile(dir, path)
	if err != nil {
		t.Fatalf("the unknown key was refused: %v", err)
	}
	if wf.Title != "x" {
		t.Errorf("Title = %q, want x", wf.Title)
	}
}

// TestValidateDir_ReportsADuplicateUnderItsRelativeName — the clash
// is reported against the name the two files claim.
func TestValidateDir_ReportsADuplicateUnderItsRelativeName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "nightme", "coder", "workflow.yml"), "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeFile(t, filepath.Join(dir, "nightme", "coder", "workflow.yaml"), "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")

	diags := ValidateDir(dir)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want one", diags)
	}
	if !strings.Contains(diags[0].Message, `duplicate workflow name "nightme/coder"`) {
		t.Errorf("the diagnostic = %q, want the clash", diags[0].Message)
	}
}
