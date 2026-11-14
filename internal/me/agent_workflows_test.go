package me

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noModels is a pair of models that must not be called: the fast path
// runs the workflow alone.
func noModels() Config {
	return Config{
		Provider: &scriptedProvider{err: errors.New("the chat model must not be called")},
		Decider:  &scriptedDecider{err: errors.New("the decision model must not be called")},
	}
}

// TestStartWorkflow_TheNameIsTheFileName — a workflow's identity is
// the base name of its file: the run and its state carry it.
func TestStartWorkflow_TheNameIsTheFileName(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _, _ := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	name := r.state.WorkflowName
	r.mu.Unlock()
	if name != "fix" {
		t.Errorf("the run state names %q, want fix", name)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestStartWorkflow_PicksUpAnEditedFile — the directory is read when a
// workflow is asked for, so an edited file takes effect without a
// restart.
func TestStartWorkflow_PicksUpAnEditedFile(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: the first title
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _, _ := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	title := r.workflow.Title
	r.mu.Unlock()
	if title != "the first title" {
		t.Errorf("the run has the title %q, want the first one", title)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The file is edited: the next start uses it.
	writeWorkflow(t, dir, "fix", `title: the second title
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ObserverChatID: "obs-2", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r = svc.active["obs-2"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	title = r.workflow.Title
	r.mu.Unlock()
	if title != "the second title" {
		t.Errorf("the run has the title %q, want the edited one", title)
	}
	if err := svc.Stop(context.Background(), "obs-2"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-2")
}

// TestStartWorkflow_ABrokenFileAffectsOnlyItself — the workflows beside
// a broken file work, and the broken name reports its own failure.
func TestStartWorkflow_ABrokenFileAffectsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)
	writeWorkflow(t, dir, "broken", `goal: no title
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _, _ := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "broken", ObserverChatID: "obs-2", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("the broken workflow was accepted")
	}
	if !strings.Contains(err.Error(), "broken.yaml could not be loaded") {
		t.Errorf("error = %v, want the failure of the file", err)
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error = %v, want it to name the replacement", err)
	}
}

// TestStartWorkflow_ReportsTheKnownNames — a name no file carries is
// refused with the names that are there.
func TestStartWorkflow_ReportsTheKnownNames(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _, _ := testService(t, dir, noModels())
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "nope", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("an unknown workflow was accepted")
	}
	if !strings.Contains(err.Error(), `there is no workflow named "nope" (known: fix)`) {
		t.Errorf("error = %v, want the known names", err)
	}
}

// TestStop_StopsTheRunOfTheChat — `/me stop` ends the run of the chat
// it is typed in: the waiting step ends at once, the state file is
// gone and the chat is free for a new run.
func TestStop_StopsTheRunOfTheChat(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "wait", `title: wait for it
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _, sender := testService(t, dir, noModels())
	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "wait", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "waiting")

	if _, err := os.Stat(svc.statePath(runID)); err != nil {
		t.Fatalf("the run state was not written: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	svc.mu.Lock()
	_, running := svc.active["obs-1"]
	svc.mu.Unlock()
	if running {
		t.Error("the chat still holds a run")
	}
	if _, err := os.Stat(svc.statePath(runID)); !os.IsNotExist(err) {
		t.Errorf("the run state is still there: %v", err)
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "wait", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("the chat could not start a new run: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestStartWorkflow_ReportsTheClashOfTwoFiles — the name two files
// claim is unusable, and the clash is the reason the daemon gives.
func TestStartWorkflow_ReportsTheClashOfTwoFiles(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)
	// A second file claims the same name.
	if err := os.WriteFile(filepath.Join(dir, "fix.yml"), []byte("goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	svc, _, _ := testService(t, dir, noModels())
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("the ambiguous name was accepted")
	}
	if !strings.Contains(err.Error(), `duplicate workflow name "fix"`) {
		t.Errorf("error = %v, want the clash", err)
	}
}

// TestResume_ARunPersistedBeforeTheTitleKeepsItsPurpose — a run that
// was persisted before the title replaced the goal keeps its purpose
// when the daemon resumes it.
func TestResume_ARunPersistedBeforeTheTitleKeepsItsPurpose(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	// A run state written before the title replaced the goal.
	state := `{
  "run_id": "one-1",
  "observer_chat_id": "obs-1",
  "work_chat_id": "lb_one-1",
  "cwd": "/tmp/work",
  "agent": "claude",
  "workflow": {
    "name": "one",
    "goal": "Do the one thing.",
    "jobs": {"main": {"steps": [{"id": "rest", "wait": "5m"}]}}
  },
  "state": {
    "run_id": "one-1",
    "workflow": "one",
    "workspace": "/tmp/work",
    "status": "running",
    "steps": [],
    "started_at": "2026-01-01T00:00:00Z"
  },
  "history": [],
  "messages": [],
  "phase": "running"
}`
	runs := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runs, "one-1.json"), []byte(state), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	svc, _, _ := testService(t, dir, noModels())
	svc.mu.Lock()
	var restored *run
	for _, r := range svc.runs {
		restored = r
	}
	svc.mu.Unlock()
	if restored == nil {
		t.Fatal("the run was not resumed")
	}
	restored.mu.Lock()
	title := restored.workflow.Title
	restored.mu.Unlock()
	if title != "Do the one thing." {
		t.Errorf("the resumed run has the title %q, want the migrated goal", title)
	}
	if rendered := restored.renderPolicy(); !strings.Contains(rendered, "title: Do the one thing.") {
		t.Errorf("the resumed policy does not carry the purpose:\n%s", rendered)
	}
}
