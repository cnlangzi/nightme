package me

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// movableCwd reports the directory a chat works in, and lets the
// test move the chat.
type movableCwd struct {
	mu  sync.Mutex
	dir string
}

// Cwd implements CwdProvider.
func (c *movableCwd) Cwd(string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dir
}

// Move puts the chat in another directory.
func (c *movableCwd) Move(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dir = dir
}

// repoOnBranch creates a repository whose branch is the given one.
func repoOnBranch(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "checkout", "-b", branch)
	gitIn(t, dir, "config", "user.email", "test@test.local")
	gitIn(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitIn(t, dir, "add", "a.go")
	gitIn(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// testRunIn builds a run of a workflow that started in the given
// directory.
func testRunIn(t *testing.T, svc *Service, name, cwd string) *run {
	t.Helper()
	wf, ok := svc.catalog.Lookup(name)
	if !ok {
		t.Fatalf("the workflow %q is not in the catalogue", name)
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	return newRun(svc, Request{Name: name, ChatID: "obs-1", Cwd: cwd}, bound, nil)
}

// TestRepoStatus_ReadsTheChatDirectory — the facts come from the
// directory the chat stands in, not from the one the run started in.
func TestRepoStatus_ReadsTheChatDirectory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	started := repoOnBranch(t, "main")
	worktree := repoOnBranch(t, "feature")

	cwd := &movableCwd{dir: worktree}
	svc, _ := testService(t, dir, Config{Git: realGitRunner{}, Cwd: cwd})
	r := testRunIn(t, svc, "one", started)

	res, err := svc.toolRepoStatus(context.Background(), r, pendingCall{})
	if err != nil {
		t.Fatalf("repo_status: %v", err)
	}
	if res.Data["branch"] != "feature" {
		t.Errorf("the branch is %q, want the branch of the chat's directory", res.Data["branch"])
	}
	if res.Data["cwd"] != worktree {
		t.Errorf("the directory is %q, want %q", res.Data["cwd"], worktree)
	}
	if res.Summary != "feature" {
		t.Errorf("the summary is %q, want the branch", res.Summary)
	}
}

// TestRepoStatus_TheChatMovesAfterTheWake — the directory is read
// when the tool runs, not when the run woke.
func TestRepoStatus_TheChatMovesAfterTheWake(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	started := repoOnBranch(t, "main")
	moved := repoOnBranch(t, "feature")

	cwd := &movableCwd{dir: started}
	svc, _ := testService(t, dir, Config{Git: realGitRunner{}, Cwd: cwd})
	r := testRunIn(t, svc, "one", started)

	res, err := svc.toolRepoStatus(context.Background(), r, pendingCall{})
	if err != nil {
		t.Fatalf("repo_status: %v", err)
	}
	if res.Data["branch"] != "main" {
		t.Fatalf("the branch is %q, want the branch of the start directory", res.Data["branch"])
	}

	// The chat moved into the worktree.
	cwd.Move(moved)

	res, err = svc.toolRepoStatus(context.Background(), r, pendingCall{})
	if err != nil {
		t.Fatalf("repo_status: %v", err)
	}
	if res.Data["branch"] != "feature" {
		t.Errorf("the branch is %q, want the branch of the new directory", res.Data["branch"])
	}
	if res.Data["cwd"] != moved {
		t.Errorf("the directory is %q, want %q", res.Data["cwd"], moved)
	}
}

// TestRepoStatus_NoProviderFallsBackToTheRunDirectory — a daemon that
// cannot tell where the chat stands leaves the run where it was.
func TestRepoStatus_NoProviderFallsBackToTheRunDirectory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	started := repoOnBranch(t, "main")
	svc, _ := testService(t, dir, Config{Git: realGitRunner{}})
	r := testRunIn(t, svc, "one", started)

	res, err := svc.toolRepoStatus(context.Background(), r, pendingCall{})
	if err != nil {
		t.Fatalf("repo_status: %v", err)
	}
	if res.Data["branch"] != "main" {
		t.Errorf("the branch is %q, want the branch of the run's directory", res.Data["branch"])
	}
	if res.Data["cwd"] != started {
		t.Errorf("the directory is %q, want %q", res.Data["cwd"], started)
	}
}

// TestTheWake_ReinitializesTheDeclaredContext — the wake puts the
// run's own record of where it works on the chat's directory.
func TestTheWake_ReinitializesTheDeclaredContext(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	started := repoOnBranch(t, "main")
	worktree := repoOnBranch(t, "feature")

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "done", "summary": "done"})},
	}}
	cwd := &movableCwd{dir: worktree}
	svc, _ := testService(t, dir, Config{Provider: provider, Cwd: cwd})
	r := testRunIn(t, svc, "one", started)

	if _, err := svc.system2(context.Background(), r); err != nil {
		t.Fatalf("system2: %v", err)
	}
	if got := r.workCwd(); got != worktree {
		t.Errorf("the run's directory is %q, want %q", got, worktree)
	}
	if got := r.state.Workspace; got != worktree {
		t.Errorf("the workspace is %q, want %q", got, worktree)
	}
	if got := r.env()["WORKSPACE_CWD"]; got != worktree {
		t.Errorf("WORKSPACE_CWD is %q, want %q", got, worktree)
	}
}

// TestTheStep_ReinitializesTheDeclaredContext — a step runs where
// the chat stands, and the run says so afterwards.
func TestTheStep_ReinitializesTheDeclaredContext(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	started := repoOnBranch(t, "main")
	worktree := repoOnBranch(t, "feature")

	shell := &fakeShell{}
	cwd := &movableCwd{dir: worktree}
	svc, _ := testService(t, dir, Config{Shell: shell, Cwd: cwd})
	r := testRunIn(t, svc, "sh", started)

	step, ok := r.stepByID("build")
	if !ok {
		t.Fatal("the workflow has no build step")
	}
	job := wfe.JobOf(r.workflow, step.ID)
	if _, err := svc.executeStep(context.Background(), r, job, step); err != nil {
		t.Fatalf("executeStep: %v", err)
	}
	if got := r.workCwd(); got != worktree {
		t.Errorf("the run's directory is %q, want %q", got, worktree)
	}
	if got := r.state.Workspace; got != worktree {
		t.Errorf("the workspace is %q, want %q", got, worktree)
	}
	if got := r.env()["WORKSPACE_CWD"]; got != worktree {
		t.Errorf("WORKSPACE_CWD is %q, want %q", got, worktree)
	}
}

// TestSyncWorkDir_AnUnknownDirectoryChangesNothing — a chat the
// daemon cannot find leaves the run where it was.
func TestSyncWorkDir_AnUnknownDirectoryChangesNothing(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	started := repoOnBranch(t, "main")
	svc, _ := testService(t, dir, Config{})
	r := testRunIn(t, svc, "one", started)

	// The daemon has no way to tell where the chat stands.
	svc.syncWorkDir(r)
	if got := r.workCwd(); got != started {
		t.Errorf("the run's directory is %q, want %q", got, started)
	}
	if got := r.state.Workspace; got != started {
		t.Errorf("the workspace is %q, want %q", got, started)
	}
	if got := r.env()["WORKSPACE_CWD"]; got != started {
		t.Errorf("WORKSPACE_CWD is %q, want %q", got, started)
	}

	// The chat session is gone: the provider answers with nothing.
	svc.cfg.Cwd = &movableCwd{dir: ""}
	svc.syncWorkDir(r)
	if got := r.workCwd(); got != started {
		t.Errorf("the run's directory is %q, want %q", got, started)
	}
	if got := r.state.Workspace; got != started {
		t.Errorf("the workspace is %q, want %q", got, started)
	}
	if got := r.env()["WORKSPACE_CWD"]; got != started {
		t.Errorf("WORKSPACE_CWD is %q, want %q", got, started)
	}
}
