package me

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/version"
)

// requiredWorkflow is a workflow that needs a nightme this build may
// not be.
const requiredWorkflow = `title: one
requires:
  nightme: ">=0.10.0"
jobs:
  main:
    steps:
      - message: /gtw commit
`

// buildVersion sets the version of the build for the test.
func buildVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = orig })
}

// TestStartWorkflow_RefusesAWorkflowThisBuildCannotRun — the run does
// not start, and the refusal says both versions.
func TestStartWorkflow_RefusesAWorkflowThisBuildCannotRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", requiredWorkflow)
	buildVersion(t, "0.9.1")

	svc, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err == nil {
		t.Fatal("the run started on a build that is too old")
	}
	want := "this workflow needs nightme >= 0.10.0; this nightme is 0.9.1"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestStartWorkflow_ARunStartsWhenTheRequirementIsMet — the
// requirement is a condition of the run, not a wall.
func TestStartWorkflow_ARunStartsWhenTheRequirementIsMet(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", requiredWorkflow)
	buildVersion(t, "0.10.0")

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{open: true}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// TestStartWorkflow_ADevelopmentBuildIsNotHeldToTheRequirement — a
// build that carries no release version is not checked: it is the
// newest code there is.
func TestStartWorkflow_ADevelopmentBuildIsNotHeldToTheRequirement(t *testing.T) {
	for _, v := range []string{"dev", "0.9.1-dev", "0.0.0", ""} {
		t.Run(v, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkflow(t, dir, "one", requiredWorkflow)
			buildVersion(t, v)

			provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
			decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
			svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
			op.steps = []fakeStep{{open: true}}

			if _, _, err := svc.StartWorkflow(context.Background(), Request{
				Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
			}); err != nil {
				t.Fatalf("start: %v", err)
			}
		})
	}
}
