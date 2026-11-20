package me

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// TestShellOutputs_TheReservedFactsWin — a command that prints
// `exit_code=0` does not report an exit code it never had.
func TestShellOutputs_TheReservedFactsWin(t *testing.T) {
	out := shellOutputs(&wfe.ShellResult{
		ExitCode: 3,
		Stdout:   "exit_code=0\n",
		Outputs:  map[string]string{"exit_code": "0", "issue": "42"},
	})
	if out["exit_code"] != "3" {
		t.Errorf("exit_code = %q, want 3", out["exit_code"])
	}
	if out["issue"] != "42" {
		t.Errorf("issue = %q, want 42", out["issue"])
	}
}

// TestRunStep_AFailedRepairDoesNotConsumeTheStep — a repair that
// fails does not consume the step the plan has not reached yet: the
// plan still runs it.
func TestRunStep_AFailedRepairDoesNotConsumeTheStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: one
        message: "/one"
      - id: two
        message: "/two"
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_two", map[string]any{"args": "-c"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.failCommand = "/two -c"
	// The first step fails, so the plan reaches a judgment point and
	// the agent is asked what to do. The second command never settles,
	// so the run is still going while the test looks at the state.
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "it failed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}, open: true},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "two"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/two")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is gone")
	}
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if wfe.HasRun(state, "two") {
		t.Error("the failed repair consumed the step: its own message can never run")
	}
}

// TestActOnDecision_ARuntimeFailureEscalates — a command that cannot
// run comes back to the agent, it does not end the run.
func TestActOnDecision_ARuntimeFailureEscalates(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "the chat is gone"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("test", "/gtw commit", 1, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.failCommand = "/gtw commit"

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model was never asked: the failure ended the run")
	}
}

// TestExecuteShell_ACancelledStepHasNoVerdict — a cancelled shell step
// is not recorded as failed.
func TestExecuteShell_ACancelledStepHasNoVerdict(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	shell := newBlockingShell()
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-shell.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell step never ran")
	}
	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is gone")
	}
	if err := svc.Stop(context.Background(), "obs-1", "om_stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	for _, st := range state.Steps {
		if st.Outcome.Status == wfe.OutcomeFailed {
			t.Errorf("the step %q was recorded as failed after the run was stopped", st.ID)
		}
	}
}

// TestUseStep_AnActionNameMayComeFromAnArgument — `use:` takes an
// argument like every other field of a step.
func TestUseStep_AnActionNameMayComeFromAnArgument(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "act", `title: act
args:
  - name: which
    default: notify
jobs:
  main:
    steps:
      - id: tell
        use: ${{ args.which }}
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	action := &recordingAction{name: "notify"}
	svc.actions.Register(action)

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "act", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !action.Called() {
		t.Fatal("the action the argument names never ran")
	}
}
