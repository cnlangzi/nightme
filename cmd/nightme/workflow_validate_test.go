package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// TestWorkflowValidate_ReportsTheProblemsOfTheConfiguredDirectory —
// the command validates the workflows dir when it is given no path.
func TestWorkflowValidate_ReportsTheProblemsOfTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	body := `
title: review the changes
jobs:
  main:
    steps:
      - id: s1
        run: echo
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	var err error
	out := captureStdout(t, func() error {
		err = runRoot("workflow", "validate")
		return nil
	})
	if !strings.Contains(out, "exactly one of message / shell / use / wait must be set") {
		t.Errorf("output = %q, want the problem of the file", out)
	}
	if !strings.Contains(out, filepath.Join(dir, "reviewer.yaml")) {
		t.Errorf("output = %q, want it to name the file", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeValidationError {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeValidationError)
	}
}

// TestWorkflowValidate_AValidWorkflowPrintsNothing — a run where
// every workflow is valid is quiet, the way a linter is.
func TestWorkflowValidate_AValidWorkflowPrintsNothing(t *testing.T) {
	dir := t.TempDir()
	body := "title: review the changes\njobs:\n  main:\n    steps: [{id: s1, message: /review}]\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	var err error
	out := captureStdout(t, func() error {
		err = runRoot("workflow", "validate")
		return nil
	})
	if out != "" {
		t.Errorf("output = %q, want nothing", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeSuccess {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeSuccess)
	}
}

// TestWorkflowValidate_TheGivenPaths — the command validates the
// files and the directories it is given.
func TestWorkflowValidate_TheGivenPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "reviewer.yaml")
	body := "title: review the changes\njobs:\n  main:\n    steps: [{id: s1, message: /review}]\n"
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("jobs:\n  main:\n    steps: [{message: hi}]\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var err error
	out := captureStdout(t, func() error {
		err = runRoot("workflow", "validate", file, dir)
		return nil
	})
	if !strings.Contains(out, broken) {
		t.Errorf("output = %q, want the problem of the file", out)
	}
	if !strings.Contains(out, "title is required") {
		t.Errorf("output = %q, want the title to be required", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeValidationError {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeValidationError)
	}
}

// TestWorkflowValidate_AMissingPath — a path that does not exist is
// reported and makes the command exit 9.
func TestWorkflowValidate_AMissingPath(t *testing.T) {
	dir := t.TempDir()
	body := "title: review the changes\njobs:\n  main:\n    steps: [{id: s1, message: /review}]\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	missing := filepath.Join(dir, "nope.yaml")

	var err error
	errOut := captureStderr(t, func() error {
		err = runRoot("workflow", "validate", missing)
		return nil
	})
	if !strings.Contains(errOut, missing) {
		t.Errorf("stderr = %q, want it to name the path", errOut)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeNotFound {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeNotFound)
	}
}

// TestWorkflowValidate_AMissingWorkflowsDir — a workflows dir that is
// not there is a path that does not exist: the command says so
// instead of passing silently.
func TestWorkflowValidate_AMissingWorkflowsDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nowhere")
	t.Setenv("NIGHTME_WORKFLOWS_DIR", missing)

	var err error
	errOut := captureStderr(t, func() error {
		err = runRoot("workflow", "validate")
		return nil
	})
	if !strings.Contains(errOut, missing) {
		t.Errorf("stderr = %q, want it to name the path", errOut)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeNotFound {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeNotFound)
	}
}

// TestWorkflowValidate_AWorkflowsDirThatCannotBeRead — a directory
// the command cannot read is a path it cannot check: the same exit
// code as a missing one, whether the path is a file or a directory.
func TestWorkflowValidate_AWorkflowsDirThatCannotBeRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no permission bits: the directory stays readable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads every path")
	}
	dir := t.TempDir()
	body := "title: review the changes\njobs:\n  main:\n    steps: [{id: s1, message: /review}]\n"
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(dir, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

	var err error
	captureStderr(t, func() error {
		err = runRoot("workflow", "validate", dir)
		return nil
	})
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeNotFound {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeNotFound)
	}
}

// TestWorkflowList_IndentsEveryLineOfAFailure — a file that did not
// load carries every problem it has, one per line, and every line of
// it is indented.
func TestWorkflowList_IndentsEveryLineOfAFailure(t *testing.T) {
	dir := t.TempDir()
	body := `
jobs:
  a:
    steps: []
  main:
    steps:
      - message: hi
        shell: echo
`
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	_, list, found := strings.Cut(out, "the files that did not load:")
	if !found {
		t.Fatalf("output = %q, want the failures", out)
	}
	for _, line := range strings.Split(list, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("the line %q is not indented", line)
		}
	}
	if !strings.Contains(out, `job "a" has no steps`) {
		t.Errorf("output = %q, want every problem of the file", out)
	}
}

// TestWorkflowValidate_TheJSONArray — the --json flag swaps the lines
// for a JSON array.
func TestWorkflowValidate_TheJSONArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reviewer.yaml")
	body := "jobs:\n  main:\n    steps: [{message: hi}]\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := captureStdout(t, func() error {
		return runRoot("workflow", "validate", path, "--json")
	})
	var diags []struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Column  int    `json:"column"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(out), &diags); err != nil {
		t.Fatalf("the output is not a JSON array: %v\n%s", err, out)
	}
	if len(diags) != 1 {
		t.Fatalf("the diagnostics = %v, want one", diags)
	}
	if diags[0].Path != path {
		t.Errorf("the diagnostic names %q, want %q", diags[0].Path, path)
	}
	if diags[0].Line != 1 {
		t.Errorf("the diagnostic is on line %d, want 1", diags[0].Line)
	}
	if !strings.Contains(diags[0].Message, "title is required") {
		t.Errorf("the diagnostic = %q, want the title to be required", diags[0].Message)
	}
}

// TestWorkflowValidate_TheJSONArrayOfAValidWorkflow — a passing run
// prints an empty array.
func TestWorkflowValidate_TheJSONArrayOfAValidWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reviewer.yaml")
	body := "title: review the changes\njobs:\n  main:\n    steps: [{id: s1, message: /review}]\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := captureStdout(t, func() error {
		return runRoot("workflow", "validate", path, "--json")
	})
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("output = %q, want an empty array", out)
	}
}

// TestWorkflowValidate_TheHelpIsThere — the command is a subcommand of
// `nightme workflow`.
func TestWorkflowValidate_TheHelpIsThere(t *testing.T) {
	out := captureStdout(t, func() error {
		return runRoot("workflow", "validate", "--help")
	})
	for _, want := range []string{"validate", "--json", "path"} {
		if !strings.Contains(out, want) {
			t.Errorf("the help is missing %q\n--- help ---\n%s", want, out)
		}
	}
}

// TestWorkflowValidate_TheOtherCommandsStillWork — the command sits
// beside the ones that were there.
func TestWorkflowValidate_TheOtherCommandsStillWork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	for _, args := range [][]string{
		{"workflow", "list", "--help"},
		{"workflow", "show", "--help"},
		{"workflow", "run", "--help"},
	} {
		out := captureStdout(t, func() error {
			return runRoot(args...)
		})
		if !strings.Contains(out, "Usage:") {
			t.Errorf("the help of %v is empty\n--- help ---\n%s", args, out)
		}
	}
}

// TestWorkflowValidate_AWorkflowName — an argument that is not a path
// on disk is looked up as a workflow name.
func TestWorkflowValidate_AWorkflowName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme", "coder.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("title: code it\njobs:\n  main:\n    steps: [{message: hi}]\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	var err error
	out := captureStdout(t, func() error {
		err = runRoot("workflow", "validate", "nightme/coder")
		return nil
	})
	if out != "" {
		t.Errorf("output = %q, want nothing", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeSuccess {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeSuccess)
	}
}

// TestWorkflowValidate_AWorkflowNameThatDoesNotValidate — the name
// resolves to the file, and the file's problems are reported.
func TestWorkflowValidate_AWorkflowNameThatDoesNotValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme", "coder.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("jobs:\n  main:\n    steps: [{message: hi}]\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	var err error
	out := captureStdout(t, func() error {
		err = runRoot("workflow", "validate", "nightme/coder")
		return nil
	})
	if !strings.Contains(out, path) {
		t.Errorf("output = %q, want it to name the file", out)
	}
	if !strings.Contains(out, "title is required") {
		t.Errorf("output = %q, want the title to be required", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeValidationError {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeValidationError)
	}
}

// TestWorkflowValidate_ANameThatIsNeitherAPathNorAWorkflow — an
// argument that names nothing is a path that does not exist.
func TestWorkflowValidate_ANameThatIsNeitherAPathNorAWorkflow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	var err error
	errOut := captureStderr(t, func() error {
		err = runRoot("workflow", "validate", "nightme/nope")
		return nil
	})
	if !strings.Contains(errOut, "nightme/nope") {
		t.Errorf("stderr = %q, want it to name the argument", errOut)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeNotFound {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeNotFound)
	}
}

// TestWorkflowList_NamesTheWorkflowsByTheirPath — the list carries
// the names the workflows are started by.
func TestWorkflowList_NamesTheWorkflowsByTheirPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme", "coder.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("title: code it\njobs:\n  main:\n    steps: [{message: hi}]\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	if !strings.Contains(out, "nightme/coder") {
		t.Errorf("output = %q, want the name of the workflow", out)
	}
}
