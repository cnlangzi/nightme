package wfe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockRT is a controllable Runtime for tick tests.
type mockRT struct {
	mu          sync.Mutex
	now         time.Time
	shellCalls  []ShellSpec
	shellRet    *ShellResult
	shellErr    error
	promptCalls []PromptSpec
	promptRet   *Reply
	promptErr   error
	actionCalls []ActionSpec
	actionRet   *ActionResult
	actionErr   error
}

func (m *mockRT) Now() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.now }

func (m *mockRT) RunShell(_ context.Context, s ShellSpec) (*ShellResult, error) {
	m.mu.Lock()
	m.shellCalls = append(m.shellCalls, s)
	r, e := m.shellRet, m.shellErr
	m.mu.Unlock()
	return r, e
}
func (m *mockRT) SendPrompt(_ context.Context, s PromptSpec) (*Reply, error) {
	m.mu.Lock()
	m.promptCalls = append(m.promptCalls, s)
	r, e := m.promptRet, m.promptErr
	m.mu.Unlock()
	return r, e
}
func (m *mockRT) RunAction(_ context.Context, s ActionSpec) (*ActionResult, error) {
	m.mu.Lock()
	m.actionCalls = append(m.actionCalls, s)
	r, e := m.actionRet, m.actionErr
	m.mu.Unlock()
	return r, e
}

func newState(wf *Workflow, env map[string]string) *RunState {
	return &RunState{
		RunID:        "test-run",
		WorkflowName: wf.Name,
		Workspace:    "/tmp",
		Status:       StatusRunning,
		Env:          env,
		ChatID:       "test-chat",
		StartedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
}

func TestTick_RunStep(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: build
        run: echo hi
`))
	rt := &mockRT{
		now:      time.Now(),
		shellRet: &ShellResult{ExitCode: 0, Outputs: map[string]string{"status": "ok"}},
	}
	state := newState(wf, nil)

	newState, err := Tick(context.Background(), state, wf, rt)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if newState.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", newState.Status)
	}
	if got := rt.shellCalls[0].Command; got != "echo hi" {
		t.Errorf("cmd = %q", got)
	}
	if newState.StepOutputs["build"]["status"] != "ok" {
		t.Errorf("outputs = %v", newState.StepOutputs)
	}
}

func TestTick_PromptStep(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
agent: codex
on: { mention: {} }
jobs:
  main:
    steps:
      - id: ask
        prompt: "say hi"
`))
	rt := &mockRT{
		now:       time.Now(),
		promptRet: &Reply{Text: "hello", Outputs: map[string]string{"verdict": "ok"}},
	}
	state := newState(wf, nil)

	newState, _ := Tick(context.Background(), state, wf, rt)
	if newState.Status != StatusSucceeded {
		t.Errorf("status = %q", newState.Status)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("prompt calls = %d", len(rt.promptCalls))
	}
	if rt.promptCalls[0].Agent != "codex" {
		t.Errorf("agent = %q, want codex (from workflow)", rt.promptCalls[0].Agent)
	}
	if rt.promptCalls[0].Prompt != "say hi" {
		t.Errorf("prompt = %q", rt.promptCalls[0].Prompt)
	}
	if newState.StepOutputs["ask"]["text"] != "hello" {
		t.Errorf("text output = %v", newState.StepOutputs["ask"])
	}
	if newState.StepOutputs["ask"]["verdict"] != "ok" {
		t.Errorf("verdict = %v", newState.StepOutputs["ask"])
	}
}

func TestTick_UseStep(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: notify
        use: notify
        with:
          channel: feishu
          target: oc_xxx
          message: hi
`))
	rt := &mockRT{
		now:       time.Now(),
		actionRet: &ActionResult{Outputs: map[string]any{"sent": true}},
	}
	state := newState(wf, nil)

	newState, _ := Tick(context.Background(), state, wf, rt)
	if newState.Status != StatusSucceeded {
		t.Errorf("status = %q", newState.Status)
	}
	if rt.actionCalls[0].Name != "notify" {
		t.Errorf("action name = %q", rt.actionCalls[0].Name)
	}
	if rt.actionCalls[0].With["channel"] != "feishu" {
		t.Errorf("with[channel] = %v", rt.actionCalls[0].With["channel"])
	}
}

func TestTick_StepFailure(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: s
        run: false
`))
	rt := &mockRT{
		now:      time.Now(),
		shellErr: errors.New("exit 1"),
	}
	state := newState(wf, nil)
	_, err := Tick(context.Background(), state, wf, rt)
	if err == nil || !errors.Is(err, ErrStepFailed) {
		t.Errorf("expected ErrStepFailed, got %v", err)
	}
	if state.Status != StatusFailed {
		t.Errorf("status = %q, want failed", state.Status)
	}
}

func TestTick_ContinueOnError(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: s1
        run: false
        continue-on-error: true
      - id: s2
        run: echo done
`))
	rt := &mockRT{
		now:      time.Now(),
		shellErr: errors.New("exit 1"),
	}
	state := newState(wf, nil)

	// First tick: s1 fails with continue-on-error
	s1, _ := Tick(context.Background(), state, wf, rt)
	if s1.Status != StatusRunning {
		t.Errorf("after s1 (continue-on-error), status = %q, want running", s1.Status)
	}
	if s1.StepOutputs["s1"]["error"] == "" {
		t.Errorf("s1 should record error in outputs")
	}

	// Second tick: s2 runs
	rt.shellErr = nil
	rt.shellRet = &ShellResult{ExitCode: 0}
	s2, _ := Tick(context.Background(), s1, wf, rt)
	if s2.Status != StatusSucceeded {
		t.Errorf("after s2, status = %q, want succeeded", s2.Status)
	}
}

func TestTick_IfFalse(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: skip
        run: echo skipped
        if: ${{ failure() }}
      - id: keep
        run: echo done
`))
	rt := &mockRT{now: time.Now()}
	state := newState(wf, nil)

	// First tick: skip is evaluated, if:false → marked ran, no shell call
	s, _ := Tick(context.Background(), state, wf, rt)
	if len(rt.shellCalls) != 0 {
		t.Fatalf("first tick: expected 0 shell calls (skip is if:false), got %d", len(rt.shellCalls))
	}
	if _, ran := s.StepOutputs["skip"]; !ran {
		t.Error("'skip' should be marked ran when if=false")
	}
	if s.Status != StatusRunning {
		t.Errorf("after if:false skip, status = %q, want running", s.Status)
	}

	// Second tick: 'keep' runs
	s, _ = Tick(context.Background(), s, wf, rt)
	if len(rt.shellCalls) != 1 {
		t.Fatalf("second tick: expected 1 shell call (keep), got %d", len(rt.shellCalls))
	}
	if rt.shellCalls[0].Command != "echo done" {
		t.Errorf("ran %q, want 'echo done'", rt.shellCalls[0].Command)
	}
	if s.Status != StatusSucceeded {
		t.Errorf("after keep, status = %q, want succeeded", s.Status)
	}
}

func TestTick_JobNeeds(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  first:
    steps:
      - id: a1
        run: echo first
  second:
    needs: [first]
    steps:
      - id: b1
        run: echo second
`))
	rt := &mockRT{now: time.Now(), shellRet: &ShellResult{ExitCode: 0}}
	state := newState(wf, nil)

	// First tick: runs a1, still running (b1 not yet ready)
	s, _ := Tick(context.Background(), state, wf, rt)
	if s.Status != StatusRunning {
		t.Errorf("after a1, status = %q, want running", s.Status)
	}
	if rt.shellCalls[0].Command != "echo first" {
		t.Errorf("first call = %q", rt.shellCalls[0].Command)
	}

	// Second tick: runs b1
	s, _ = Tick(context.Background(), s, wf, rt)
	if s.Status != StatusSucceeded {
		t.Errorf("after b1, status = %q, want succeeded", s.Status)
	}
	if rt.shellCalls[1].Command != "echo second" {
		t.Errorf("second call = %q", rt.shellCalls[1].Command)
	}
}

func TestTick_EnvInterpolation(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: s
        run: echo ${{ env.GREETING }}
`))
	rt := &mockRT{now: time.Now(), shellRet: &ShellResult{ExitCode: 0}}
	state := newState(wf, map[string]string{"GREETING": "hello"})

	Tick(context.Background(), state, wf, rt)
	if rt.shellCalls[0].Command != "echo hello" {
		t.Errorf("cmd = %q, want 'echo hello'", rt.shellCalls[0].Command)
	}
}

func TestTick_StepEnvOverride(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: s
        run: x
        env:
          GREETING: world
`))
	rt := &mockRT{now: time.Now(), shellRet: &ShellResult{ExitCode: 0}}
	state := newState(wf, map[string]string{"GREETING": "hello"})

	Tick(context.Background(), state, wf, rt)
	got := rt.shellCalls[0].Env["GREETING"]
	if got != "world" {
		t.Errorf("step env override: env[GREETING] = %q, want world", got)
	}
}

func TestTick_AlredyTerminated(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps: [{id: s, run: x}]
`))
	rt := &mockRT{}
	state := newState(wf, nil)
	state.Status = StatusSucceeded

	_, err := Tick(context.Background(), state, wf, rt)
	if err != nil {
		t.Errorf("terminated state should be no-op, got %v", err)
	}
	if len(rt.shellCalls) != 0 {
		t.Errorf("no calls should be made on terminated state")
	}
}

func TestMatch(t *testing.T) {
	wf, err := Parse([]byte(`
name: x
workspaces: [a]
on:
  pull_request:
    branches: [main]
    events: [opened]
  branch:
    patterns: [release/*]
    events: [pushed]
  issue:
    events: [opened, commented]
  mention:
    commands: [review, fix]
  schedule:
    - cron: '0 9 * * *'
jobs:
  main:
    steps: [{id: s, run: x}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name string
		ev   Event
		want bool
	}{
		{"schedule match", Event{Kind: "schedule", Data: map[string]any{"cron": "0 9 * * *"}}, true},
		{"schedule no match", Event{Kind: "schedule", Data: map[string]any{"cron": "0 0 * * *"}}, false},
		{"PR match", Event{Kind: "pull_request", Data: map[string]any{"branch": "main", "action": "opened"}}, true},
		{"PR wrong branch", Event{Kind: "pull_request", Data: map[string]any{"branch": "dev", "action": "opened"}}, false},
		{"branch glob match", Event{Kind: "branch", Data: map[string]any{"name": "release/v1.0", "action": "pushed"}}, true},
		{"branch no match", Event{Kind: "branch", Data: map[string]any{"name": "feature/x", "action": "pushed"}}, false},
		{"issue match", Event{Kind: "issue", Data: map[string]any{"action": "commented"}}, true},
		{"issue wrong action", Event{Kind: "issue", Data: map[string]any{"action": "closed"}}, false},
		{"mention match", Event{Kind: "mention", Data: map[string]any{"command": "review"}}, true},
		{"mention wrong cmd", Event{Kind: "mention", Data: map[string]any{"command": "deploy"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(wf, tt.ev); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchMentionNoCommands(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on:
  mention: {}
jobs:
  main:
    steps: [{id: s, run: x}]
`))
	// No commands whitelist = any mention fires
	if !Match(wf, Event{Kind: "mention", Data: map[string]any{"command": "anything"}}) {
		t.Error("any mention should fire when no commands whitelist")
	}
}

func TestMatchSchedule(t *testing.T) {
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on:
  schedule:
    - cron: '0 9 * * *'
    - cron: '0 18 * * 5'
jobs:
  main:
    steps: [{id: s, run: x}]
`))
	if !Match(wf, Event{Kind: "schedule", Data: map[string]any{"cron": "0 9 * * *"}}) {
		t.Error("first cron should match")
	}
	if !Match(wf, Event{Kind: "schedule", Data: map[string]any{"cron": "0 18 * * 5"}}) {
		t.Error("second cron should match")
	}
	if Match(wf, Event{Kind: "schedule", Data: map[string]any{"cron": "0 0 * * *"}}) {
		t.Error("non-listed cron should not match")
	}
	// PR event should not match when only schedule is configured
	if Match(wf, Event{Kind: "pull_request"}) {
		t.Error("PR event should not match a schedule-only workflow")
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"release/*", "release/v1.0", true},
		{"release/*", "feature/x", false},
		{"foo", "foo", true},
		{"foo", "bar", false},
		{"*foo", "xfoo", true},
		{"*foo", "xfo", false},
		{"foo*", "foobar", true},
		{"foo*", "bar", false},
		{"a*b", "axxxb", true},
		{"a*b", "axb", true},
		{"a*b", "ab", true},
		{"a*b", "ax", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.s, func(t *testing.T) {
			if got := matchGlob(tt.pattern, tt.s); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
			}
		})
	}
}

func TestStringifyAnyMap(t *testing.T) {
	m := map[string]any{
		"sent":    true,
		"channel": "feishu",
		"count":   42,
		"nested":  map[string]any{"a": "x", "b": 1},
	}
	got := stringifyAnyMap(m)
	if got["sent"] != "true" {
		t.Errorf("sent = %q", got["sent"])
	}
	if got["channel"] != "feishu" {
		t.Errorf("channel = %q", got["channel"])
	}
	if got["count"] != "42" {
		t.Errorf("count = %q", got["count"])
	}
	if got["nested.a"] != "x" {
		t.Errorf("nested.a = %q", got["nested.a"])
	}
}

func TestNewRunStateHelper(t *testing.T) {
	// Smoke test for any future helper.
	wf, _ := Parse([]byte(`
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps: [{id: s, run: x}]
`))
	_ = wf
	_ = strings.TrimSpace
}
