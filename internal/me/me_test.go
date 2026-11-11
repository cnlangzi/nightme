package me

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// writeWorkflow writes a workflow file into dir and returns its path.
func writeWorkflow(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

// gitRepo creates a git repository with an origin remote and returns
// its path. The workflow loader reads the origin of every workspace,
// so a workspace has to be a repo for the service to start.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := gitInit(t)
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"https://github.com/foo/bar.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	return dir
}

// gitInit creates a git repository without any remote and returns
// its path.
func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// TestService_Start_MissingWorkflowsDirIsIdle — a daemon whose
// workflows dir was never created still starts: the service has
// nothing to trigger, and that is not an error.
func TestService_Start_MissingWorkflowsDirIsIdle(t *testing.T) {
	svc := New(Config{WorkflowsDir: filepath.Join(t.TempDir(), "nope")}, &fakeClient{})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !svc.started() {
		t.Error("Started = false after Start")
	}
	if len(svc.workflows) != 0 {
		t.Errorf("workflows = %d, want 0", len(svc.workflows))
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if svc.started() {
		t.Error("Started = true after Stop")
	}
}

// TestService_Start_EmptyWorkflowsDirIsIdle — the same for a
// workflows dir that exists but holds no workflow.
func TestService_Start_EmptyWorkflowsDirIsIdle(t *testing.T) {
	svc := New(Config{WorkflowsDir: t.TempDir()}, &fakeClient{})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(svc.workflows) != 0 {
		t.Errorf("workflows = %d, want 0", len(svc.workflows))
	}
	if svc.stateStore != nil {
		t.Error("state store created for a service with nothing to run")
	}
}

// TestService_Start_LoadsWorkflowsAndArmsTriggers — the service
// picks the workflows up, arms one cron job per schedule entry and
// prepares the state store the runs persist into.
func TestService_Start_LoadsWorkflowsAndArmsTriggers(t *testing.T) {
	ws := gitRepo(t)
	dir := t.TempDir()
	writeWorkflow(t, dir, "nightly.yaml", fmt.Sprintf(`
name: nightly
workspaces: ['%s']
on:
  schedule:
    - cron: "0 3 * * *"
    - cron: "30 4 * * *"
jobs:
  main:
    steps:
      - id: only
        prompt: "hello"
`, ws))

	svc := New(Config{WorkflowsDir: dir}, &fakeClient{})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = svc.Stop(context.Background()) }()

	if len(svc.workflows) != 1 {
		t.Fatalf("workflows = %d, want 1", len(svc.workflows))
	}
	if got := svc.triggers.Entries(); got != 2 {
		t.Errorf("cron entries = %d, want 2", got)
	}
	if svc.stateStore == nil {
		t.Fatal("state store not initialised")
	}
	if svc.actions == nil {
		t.Fatal("action registry not initialised")
	}
}

// TestService_Start_InvalidWorkflowIsAnError — a workflow that does
// not parse keeps the service from starting, so the operator learns
// about it instead of silently losing the workflow.
func TestService_Start_InvalidWorkflowIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "broken.yaml", "name: broken\n")

	svc := New(Config{WorkflowsDir: dir}, &fakeClient{})
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("Start: expected an error for an invalid workflow")
	}
}

// TestService_Start_WorkspaceWithoutOriginIsAnError — the workspace
// of every workflow has to be a git repo: the workspace→repo map is
// what a git event trigger matches on.
func TestService_Start_WorkspaceWithoutOriginIsAnError(t *testing.T) {
	ws := gitInit(t)
	dir := t.TempDir()
	writeWorkflow(t, dir, "nightly.yaml", fmt.Sprintf(`
name: nightly
workspaces: ['%s']
on:
  schedule:
    - cron: "0 3 * * *"
jobs:
  main:
    steps:
      - id: only
        prompt: "hello"
`, filepath.ToSlash(ws)))

	svc := New(Config{WorkflowsDir: dir}, &fakeClient{})
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("Start: expected an error for a workspace without a git origin")
	}
}

// TestWorkflowParse is a guard on the fixture the run tests use: the
// DAG they drive has to parse.
func TestWorkflowParse(t *testing.T) {
	if _, err := wfe.Parse([]byte(dagWorkflow)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}
