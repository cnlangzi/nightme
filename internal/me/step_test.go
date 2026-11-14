package me

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// fakeCwd reports the directory a chat works in.
type fakeCwd struct {
	dir string
}

func (c fakeCwd) Cwd(string) string { return c.dir }

// TestRun_AJobWithIterationsRunsItsStepsAgain — a repeating job runs
// its steps round after round, and the run ends when the rounds are
// used up.
func TestRun_AJobWithIterationsRunsItsStepsAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repeat", `title: repeat
jobs:
  main:
    iterations: 2
    steps:
      - id: fix
        message: "/gtw fix"
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repeat", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	runs := 0
	for _, cmd := range lb.Sent() {
		if cmd == "/gtw fix" {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("the step ran %d times, want 2 (two rounds)", runs)
	}
}

// TestStepKind_WaitWaitsAndReportsOk — a `wait:` step waits out its
// duration and reports ok.
func TestStepKind_WaitWaitsAndReportsOk(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "wait", `title: wait
jobs:
  main:
    steps:
      - id: rest
        wait: 40ms
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	start := time.Now()
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "wait", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("the run took %s, want it to wait 40ms", elapsed)
	}
}

// TestStepKind_WaitEndsWithTheRun — a cancelled run ends the wait at
// once.
func TestStepKind_WaitEndsWithTheRun(t *testing.T) {
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
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the wait ran for %s after the run was stopped", elapsed)
	}
}

// TestStepKind_ShellRunsInTheWorkChatDirectory — a shell step runs
// where the work chat stands, not where the run started.
func TestStepKind_ShellRunsInTheWorkChatDirectory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	shell := &fakeShell{}
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{
		Provider: provider,
		Decider:  decider,
		Shell:    shell,
		Cwd:      fakeCwd{dir: "/tmp/worktree"},
	})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	shell.mu.Lock()
	calls := append([]wfe.ShellSpec(nil), shell.calls...)
	shell.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("the shell ran %d commands, want 1", len(calls))
	}
	if calls[0].Cwd != "/tmp/worktree" {
		t.Errorf("the cwd = %q, want the work chat's directory", calls[0].Cwd)
	}
}

// gatedShell runs a command when the test opens the gate.
type gatedShell struct {
	gate    chan struct{}
	started chan struct{}
	ret     *wfe.ShellResult
}

func newGatedShell() *gatedShell {
	return &gatedShell{gate: make(chan struct{}), started: make(chan struct{})}
}

func (g *gatedShell) RunShell(ctx context.Context, _ wfe.ShellSpec) (*wfe.ShellResult, error) {
	close(g.started)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.ret, nil
}

// TestStepKind_ShellReportsWhatItProduced — the step's outputs carry
// the exit code, the output and the key=value lines.
func TestStepKind_ShellReportsWhatItProduced(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	shell := newGatedShell()
	shell.ret = &wfe.ShellResult{
		ExitCode: 0,
		Stdout:   "issue=42\n",
		Stderr:   "a warning\n",
		Outputs:  map[string]string{"issue": "42"},
	}
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
	close(shell.gate)
	waitRun(t, svc, "obs-1")

	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if len(state.Steps) != 1 {
		t.Fatalf("the state holds %d steps, want 1", len(state.Steps))
	}
	data := state.Steps[0].Outcome.Data
	for key, want := range map[string]string{
		"exit_code": "0",
		"stdout":    "issue=42\n",
		"stderr":    "a warning\n",
		"issue":     "42",
	} {
		if data[key] != want {
			t.Errorf("the output %q = %q, want %q", key, data[key], want)
		}
	}
}

// TestStepKind_AFailedCommandReachesTheAgent — a command that could
// not run at all ends the step as failed, and the plan asks the agent
// what to do next.
func TestStepKind_AFailedCommandReachesTheAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
      - id: after
        message: "/after"
`)

	shell := &fakeShell{err: errors.New("the shell is gone")}
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, lb, sender := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	for _, cmd := range lb.Sent() {
		if cmd == "/after" {
			t.Fatal("the chain continued after the failed step")
		}
	}
	if !sender.Has("failed") {
		t.Errorf("the observer was not told about the failure: %v", sender.Lines())
	}
}

// TestStepKind_ShellOutputIsBounded — a command that prints a
// megabyte does not put a megabyte into the run state.
func TestStepKind_ShellOutputIsBounded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	shell := newGatedShell()
	shell.ret = &wfe.ShellResult{
		ExitCode: 0,
		Stdout:   strings.Repeat("x", 100_000),
	}
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
	close(shell.gate)
	waitRun(t, svc, "obs-1")

	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	stdout := state.Steps[0].Outcome.Data["stdout"]
	if len(stdout) > maxShellOutput+len("…") {
		t.Errorf("the step kept %d bytes of output, want at most %d", len(stdout), maxShellOutput)
	}
	if stdout == "" {
		t.Error("the step kept no output at all")
	}
}

// TestTool_ARuntimeFailureComesBackAsAToolError — a command the work
// chat cannot take comes back to the agent as an error it can correct,
// and the run does not end.
func TestTool_ARuntimeFailureComesBackAsAToolError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: commit
        message: "/gtw commit"
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the work chat is gone"})},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	lb.failCommand = "/gtw commit"

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	req := provider.Request(1)
	if len(req.Messages) == 0 {
		t.Fatal("the second turn carried no history")
	}
	found := false
	for _, m := range req.Messages {
		if m.Role != llm.RoleTool {
			continue
		}
		for _, p := range m.Parts {
			if p.IsError && strings.Contains(p.Text, "the work chat is gone") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the runtime failure did not come back as a tool error")
	}
}

// TestStepKind_TheStepKindIsPartOfThePolicy — the wait and the shell
// steps are not commands the agent may run.
func TestStepKind_TheStepKindIsPartOfThePolicy(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "mixed", `title: mixed
jobs:
  main:
    steps:
      - id: build
        shell: make build
      - id: rest
        wait: 10ms
`)

	shell := newGatedShell()
	shell.ret = &wfe.ShellResult{ExitCode: 0}
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "mixed", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
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
	close(shell.gate)
	waitRun(t, svc, "obs-1")

	allowed := r.allowedCommands()
	for _, cmd := range allowed {
		if strings.Contains(cmd, "make build") {
			t.Errorf("the policy allows the shell command %q", cmd)
		}
	}
}
