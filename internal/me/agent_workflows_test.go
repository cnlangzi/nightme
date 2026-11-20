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

// TestStartWorkflow_ANameWithoutAnOwnerIsTheProjectsOwn — a name that
// carries no owner is the workflow of that name when there is one, and
// the project's own namespace otherwise.
func TestStartWorkflow_ANameWithoutAnOwnerIsTheProjectsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "nightme/coder", `title: code it
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	_, name, err := svc.StartWorkflow(context.Background(), Request{
		Name: "coder", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if name != "nightme/coder" {
		t.Errorf("the workflow is %q, want nightme/coder", name)
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestStartWorkflow_TheWorkflowOfThatNameComesFirst — a workflow of
// that name is more specific than the project's own namespace, and it
// is the one that runs.
func TestStartWorkflow_TheWorkflowOfThatNameComesFirst(t *testing.T) {
	dir := t.TempDir()
	body := `title: do the thing
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`
	writeWorkflow(t, dir, "coder", body)
	writeWorkflow(t, dir, "nightme/coder", body)

	svc, _ := testService(t, dir, noModels())
	_, name, err := svc.StartWorkflow(context.Background(), Request{
		Name: "coder", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if name != "coder" {
		t.Errorf("the workflow is %q, want coder", name)
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestStartWorkflow_TheNameIsTheBundle — a workflow's identity is the
// path of its bundle under the workflows dir: the run and its state
// carry it.
func TestStartWorkflow_TheNameIsTheBundle(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work",
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
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
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

	svc, _ := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work",
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
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
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
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-2", Cwd: "/tmp/work",
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
	if err := svc.Stop(context.Background(), "obs-2", "om_stop"); err != nil {
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

	svc, _ := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	_, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "broken", ChatID: "obs-2", Cwd: "/tmp/work",
	})
	if err == nil {
		t.Fatal("the broken workflow was accepted")
	}
	if !strings.Contains(err.Error(), "broken could not be loaded") {
		t.Errorf("error = %v, want the failure of the workflow", err)
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

	svc, _ := testService(t, dir, noModels())
	_, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "nope", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err == nil {
		t.Fatal("an unknown workflow was accepted")
	}
	if !strings.Contains(err.Error(), `there is no workflow named "nope" (known: fix)`) {
		t.Errorf("error = %v, want the known names", err)
	}
}

// TestStartWorkflow_TheRoundCountOverridesEveryJob — the count the
// command line names reaches every job of the workflow, the ones that
// declare a count and the ones that declare none.
func TestStartWorkflow_TheRoundCountOverridesEveryJob(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    iterations: 0
    steps:
      - id: rest
        wait: 5m
  check:
    needs: [main]
    steps:
      - id: look
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	rounds := 2
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work", Iterations: &rounds,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		waitRun(t, svc, "obs-1")
	}()

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range []string{"main", "check"} {
		if got := r.workflow.Jobs[job].Rounds(); got != rounds {
			t.Errorf("the rounds of %s = %d, want %d", job, got, rounds)
		}
	}
}

// TestStartWorkflow_TheOverrideLeavesTheCatalogueAlone — the count of
// a run is the run's own: the file and the catalogue's copy of it keep
// their counts.
func TestStartWorkflow_TheOverrideLeavesTheCatalogueAlone(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    iterations: 0
    steps:
      - id: rest
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	rounds := 1
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work", Iterations: &rounds,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	wf, ok := svc.catalog.Lookup("fix")
	if !ok {
		t.Fatal("the catalogue does not hold the workflow")
	}
	if got := wf.Jobs["main"].Rounds(); got != 0 {
		t.Errorf("the rounds of the catalogue's workflow = %d, want 0", got)
	}
}

// TestStartWorkflow_WithoutTheCountTheFileCountsStand — a run that
// names no count uses the counts of the file.
func TestStartWorkflow_WithoutTheCountTheFileCountsStand(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix", `title: do the thing
jobs:
  main:
    iterations: 3
    steps:
      - id: rest
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		waitRun(t, svc, "obs-1")
	}()

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if got := r.workflow.Jobs["main"].Rounds(); got != 3 {
		t.Errorf("the rounds of main = %d, want 3", got)
	}
}

// TestStop_StopsTheRunOfTheChat — `/me stop` ends the run of the chat
// it is typed in: the waiting step ends at once and the chat is free
// for a new run.
func TestStop_StopsTheRunOfTheChat(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "wait", `title: wait for it
jobs:
  main:
    steps:
      - id: rest
        wait: 5m
`)

	svc, _ := testService(t, dir, noModels())
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "wait", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	svc.mu.Lock()
	_, running := svc.active["obs-1"]
	svc.mu.Unlock()
	if running {
		t.Error("the chat still holds a run")
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "wait", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("the chat could not start a new run: %v", err)
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
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
	if err := os.WriteFile(filepath.Join(dir, "fix", "workflow.yaml"), []byte("goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	svc, _ := testService(t, dir, noModels())
	_, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err == nil {
		t.Fatal("the ambiguous name was accepted")
	}
	if !strings.Contains(err.Error(), `duplicate workflow name "fix"`) {
		t.Errorf("error = %v, want the clash", err)
	}
}
