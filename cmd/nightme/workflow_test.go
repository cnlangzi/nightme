package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowList_NoWorkflows verifies the empty-state message
// when the workflows dir is missing or empty.
func TestWorkflowList_NoWorkflows(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	if !strings.Contains(out, "no workflows found") {
		t.Errorf("output = %q, want 'no workflows found'", out)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("output = %q, want to mention dir %s", out, dir)
	}
}

// TestWorkflowList_OneWorkflow exercises the happy path: write
// one *.yaml, list it, see it in the output.
func TestWorkflowList_OneWorkflow(t *testing.T) {
	dir := t.TempDir()
	yaml := `
title: review the changes
workspaces: [~/work/nightme]
on:
  mention:
    commands: [review]
jobs:
  main:
    steps: [{id: s, message: /review}]
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	if !strings.Contains(out, "reviewer") {
		t.Errorf("output = %q, want 'reviewer'", out)
	}
	if !strings.Contains(out, "mention") {
		t.Errorf("output = %q, want 'mention'", out)
	}
}

// TestWorkflowShow_Found verifies the detail view for a known
// workflow.
func TestWorkflowShow_Found(t *testing.T) {
	dir := t.TempDir()
	yaml := `
title: review the changes
workspaces: [~/work/nightme]
on:
  mention:
    commands: [review, fix]
jobs:
  main:
    steps:
      - id: review
        message: "review this"
      - id: notify
        use: notify
        with: { channel: feishu, target: oc_xxx, message: hi }
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "show", "reviewer")
	})
	for _, want := range []string{
		"name:       reviewer",
		"mention: commands=[review fix]",
		"message:",
		"use:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestWorkflowShow_TheMergedSchema — the detail view carries the
// rounds and the wait step, and names no agent: the run uses the
// agent of the chat that starts it.
func TestWorkflowShow_TheMergedSchema(t *testing.T) {
	dir := t.TempDir()
	yaml := `
title: review the changes
jobs:
  main:
    iterations: 0
    steps:
      - id: rest
        wait: 5m
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "show", "reviewer")
	})
	for _, want := range []string{"wait: 5m", "iterations: until the run ends"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "agent") {
		t.Errorf("the output names an agent\n--- output ---\n%s", out)
	}
}

// TestWorkflowShow_NotFound verifies the error path.
func TestWorkflowShow_NotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	// cobra's RunE returns the error; check via the returned err
	// (cobra doesn't auto-print errors in non-verbose mode).
	err := runRoot("workflow", "show", "nope")
	if err == nil {
		t.Fatal("expected error for missing workflow")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want 'not found'", err)
	}
}

// TestWorkflowRun_DryRun verifies the execution plan is printed.
func TestWorkflowRun_DryRun(t *testing.T) {
	dir := t.TempDir()
	yaml := `
title: review the changes
workspaces: [~/work/nightme]
on:
  mention:
    commands: [review]
jobs:
  main:
    steps:
      - id: s1
        shell: echo
      - id: s2
        message: "do thing"
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "run", "reviewer", "--workspace", "/tmp/test")
	})
	for _, want := range []string{
		"workflow:   reviewer",
		"workspace:  /tmp/test",
		"execution plan",
		"echo",     // step 1 shell
		"message:", // step 2 message
		"dry-run",  // footer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestWorkflowRun_WithoutWorkspaces — a workflow that declares no
// workspace takes the one of the chat that starts it, so the plan
// still prints.
func TestWorkflowRun_WithoutWorkspaces(t *testing.T) {
	dir := t.TempDir()
	yaml := `
title: review the changes
jobs:
  main:
    steps:
      - id: s1
        message: /review
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "run", "reviewer")
	})
	for _, want := range []string{"workflow:   reviewer", "execution plan", "/review"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestWorkflowList_ReportsAFileThatDidNotLoad — a file that does not
// load is reported beside the ones that did: it does not take them
// down with it.
func TestWorkflowList_ReportsAFileThatDidNotLoad(t *testing.T) {
	dir := t.TempDir()
	broken := `
title: review the changes
jobs:
  main:
    steps:
      - id: s1
        run: echo
`
	good := `
title: ship the fix
jobs:
  main:
    steps:
      - id: s1
        message: /gtw commit
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(broken), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ship.yml"), []byte(good), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	if !strings.Contains(out, "ship") {
		t.Errorf("output = %q, want the workflow that loaded", out)
	}
	if !strings.Contains(out, "reviewer.yaml") {
		t.Errorf("output = %q, want the file that did not load", out)
	}
	if !strings.Contains(out, "shell:") {
		t.Errorf("output = %q, want the error to name the replacement", out)
	}
}

// TestWorkflowList_ReadsBothExtensionsAndTheTitle — a workflow is a
// *.yaml or a *.yml file, its name is the base name of that file and
// the list shows its title.
func TestWorkflowList_ReadsBothExtensionsAndTheTitle(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fix.yaml", "other.yml"} {
		body := "title: do the thing\njobs:\n  main:\n    steps: [{message: hi}]\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	for _, want := range []string{"fix", "other", "do the thing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
}

// TestWorkflowShow_FindsAWorkflowByItsFileName — the name of a
// workflow is the base name of its file.
func TestWorkflowShow_FindsAWorkflowByItsFileName(t *testing.T) {
	dir := t.TempDir()
	body := `
title: review the changes
jobs:
  main:
    steps:
      - id: s1
        message: /review
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yml"), []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	out := captureStdout(t, func() error {
		return runRoot("workflow", "show", "reviewer")
	})
	for _, want := range []string{"name:       reviewer", "title:      review the changes"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
}

// captureStdout / captureStderr run fn and return what was
// written to the corresponding file descriptor. Pattern copied
// from existing test files in this package.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := fn()
	w.Close()
	os.Stdout = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = err
	return string(buf[:n])
}

func captureStderr(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := fn()
	w.Close()
	os.Stderr = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = err
	return string(buf[:n])
}

// TestWorkflowList_OnlyTheFailuresOfABrokenDirectory — a directory
// whose files all failed says why, not that it holds no workflow.
func TestWorkflowList_OnlyTheFailuresOfABrokenDirectory(t *testing.T) {
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

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	if strings.Contains(out, "no workflows found") {
		t.Errorf("output = %q, want the failure of the file", out)
	}
	if !strings.Contains(out, "reviewer.yaml") {
		t.Errorf("output = %q, want the file that did not load", out)
	}
}

// TestWorkflowShow_ReportsTheClashOfTwoFiles — the name two files
// claim is unusable, and the clash is the reason.
func TestWorkflowShow_ReportsTheClashOfTwoFiles(t *testing.T) {
	dir := t.TempDir()
	good := "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n"
	broken := "goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n"
	if err := os.WriteFile(filepath.Join(dir, "fix.yaml"), []byte(good), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fix.yml"), []byte(broken), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)

	err := runRoot("workflow", "show", "fix")
	if err == nil {
		t.Fatal("the ambiguous name was accepted")
	}
	if !strings.Contains(err.Error(), `duplicate workflow name "fix"`) {
		t.Errorf("err = %v, want the clash", err)
	}
}
