package me

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// TestRun_AWaitSurvivesARestart — a daemon that goes away while a run
// waits comes back and finds the run.
func TestRun_AWaitSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "wait", `title: wait
jobs:
  main:
    steps:
      - id: rest
        wait: 1h
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "wait", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The wait runs for an hour: the run is still going when the
	// daemon goes away.
	time.Sleep(100 * time.Millisecond)

	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatalf("read the runs dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the wait wrote no state: a restart would lose the run")
	}

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestRun_AShellStepSurvivesARestart — the same for a shell step.
func TestRun_AShellStepSurvivesARestart(t *testing.T) {
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
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-shell.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell step never ran")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatalf("read the runs dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the shell step wrote no state: a restart would lose the run")
	}
}

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

// TestRunStep_AFailedRepairDoesNotConsumeTheStep — a repair of a step
// the plan has not reached yet does not consume that step: the plan
// still runs it.
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
        allow: ["/two -c"]
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/two -c"})},
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"command": "/two"})},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	lb.failCommand = "/two -c"
	// The first step fails, so the plan reaches a judgment point and
	// the agent is asked what to do. The second command never settles,
	// so the run is still going while the test looks at the state.
	lb.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "it failed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}, open: true},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, lb, "/two")

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
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "the work chat is gone"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("test", "/gtw commit", 1, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.failCommand = "/gtw commit"

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
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
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
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
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
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
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	action := &recordingAction{name: "notify"}
	svc.actions.Register(action)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "act", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !action.Called() {
		t.Fatal("the action the argument names never ran")
	}
}
