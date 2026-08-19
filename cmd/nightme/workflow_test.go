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
name: reviewer
workspaces: [~/work/nightme]
on:
  mention:
    commands: [review]
jobs:
  main:
    steps: [{id: s, run: x}]
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
name: reviewer
workspaces: [~/work/nightme]
agent: codex
on:
  mention:
    commands: [review, fix]
jobs:
  main:
    steps:
      - id: review
        prompt: "review this"
        agent: codex
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
		"agent:      codex",
		"mention: commands=[review fix]",
		"prompt:",
		"use:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
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
name: reviewer
workspaces: [~/work/nightme]
on:
  mention:
    commands: [review]
jobs:
  main:
    steps:
      - id: s1
        run: echo
      - id: s2
        prompt: "do thing"
        agent: codex
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
		"echo",        // step 1 run
		"prompt:",     // step 2 prompt
		"dry-run",     // footer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
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
