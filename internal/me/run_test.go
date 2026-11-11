package me

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// dagWorkflow is the fixture the run tests drive: two jobs, the
// second one waiting for the first, each with a prompt step.
const dagWorkflow = `
name: nightly
workspaces: ['/tmp/nightly-workspace']
agent: claude
on:
  schedule:
    - cron: "0 3 * * *"
jobs:
  first:
    steps:
      - id: first-prompt
        prompt: "first question"
  second:
    needs: [first]
    steps:
      - id: second-prompt
        prompt: "second question"
`

// fakeClient is a CommandRunner that records what it was asked and
// answers with a scripted execution.
type fakeClient struct {
	mu    sync.Mutex
	calls []string
	reply func(text string) (loopback.Execution, error)
}

func (f *fakeClient) Do(_ context.Context, _ string, text string) (loopback.Execution, error) {
	f.mu.Lock()
	f.calls = append(f.calls, text)
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(text)
	}
	return loopback.Execution{Outcome: messages.Outcome{Status: messages.OutcomeOK}}, nil
}

// recorded returns the commands the client was asked to run, in
// order.
func (f *fakeClient) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// executionWithResult builds a settled execution whose transcript
// holds a streamed reply and the final result — the shape the daemon
// produces for an agent turn.
func executionWithResult(reply, result string) loopback.Execution {
	now := time.Now()
	return loopback.Execution{
		Outcome: messages.Outcome{Status: messages.OutcomeOK},
		Transcript: []loopback.Entry{
			{Kind: messages.OutReply, Text: reply, At: now},
			{Kind: messages.OutResult, Text: result, At: now},
		},
	}
}

// newRunService returns a service wired to a fake client and a
// throwaway state store.
func newRunService(t *testing.T, client CommandRunner) *Service {
	t.Helper()
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}
	svc := New(Config{WorkflowsDir: t.TempDir()}, client)
	svc.stateStore = store
	svc.actions = NewActionRegistry()
	return svc
}

// TestService_DAGRun_ProducesStepOutputs — a cron DAG driven
// through the loopback client produces the step outputs the
// workflow declares: every prompt step carries the text the agent
// answered with, and the job that needs the other one runs after
// it.
func TestService_DAGRun_ProducesStepOutputs(t *testing.T) {
	client := &fakeClient{reply: func(text string) (loopback.Execution, error) {
		switch text {
		case "first question":
			return executionWithResult("streaming one", "answer one"), nil
		case "second question":
			return executionWithResult("streaming two", "answer two"), nil
		default:
			return loopback.Execution{Outcome: messages.Outcome{Status: messages.OutcomeOK}}, nil
		}
	}}
	svc := newRunService(t, client)

	wf, err := wfe.Parse([]byte(dagWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws := wf.Workspaces[0]
	ev := wfe.Event{Kind: "schedule", Time: time.Now()}

	r := svc.startRun(wf, ws)
	svc.driveRun(context.Background(), r, ev)

	state, err := svc.stateStore.Load(r.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Status != wfe.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", state.Status)
	}
	want := map[string]string{
		"first-prompt":  "answer one",
		"second-prompt": "answer two",
	}
	for step, text := range want {
		got, ok := state.StepOutputs[step]
		if !ok {
			t.Fatalf("step %q produced no output", step)
		}
		if got["text"] != text {
			t.Errorf("step %q text = %q, want %q", step, got["text"], text)
		}
	}
}

// TestService_DAGRun_SetupOrderIsEnforced — the chat learns its
// workspace before its agent, and both before the first prompt. The
// daemon answers each command before the next one is sent, so /use
// never runs against a chat without a workspace.
func TestService_DAGRun_SetupOrderIsEnforced(t *testing.T) {
	client := &fakeClient{}
	svc := newRunService(t, client)

	wf, err := wfe.Parse([]byte(dagWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws := wf.Workspaces[0]
	ev := wfe.Event{Kind: "schedule", Time: time.Now()}

	r := svc.startRun(wf, ws)
	svc.driveRun(context.Background(), r, ev)

	want := []string{
		"/cwd " + ws,
		"/use claude",
		"first question",
		"second question",
	}
	got := client.recorded()
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestService_DAGRun_StepResultIsTheFinalText — the step result is
// the text the agent finished with, not the first thing the daemon
// said while the turn was still running.
func TestService_DAGRun_StepResultIsTheFinalText(t *testing.T) {
	client := &fakeClient{reply: func(text string) (loopback.Execution, error) {
		if strings.HasPrefix(text, "/") {
			return loopback.Execution{Outcome: messages.Outcome{Status: messages.OutcomeOK}}, nil
		}
		return executionWithResult("the first thing said", "the final answer"), nil
	}}
	svc := newRunService(t, client)

	wf, err := wfe.Parse([]byte(dagWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ev := wfe.Event{Kind: "schedule", Time: time.Now()}

	r := svc.startRun(wf, wf.Workspaces[0])
	svc.driveRun(context.Background(), r, ev)

	state, err := svc.stateStore.Load(r.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.StepOutputs["first-prompt"]["text"]; got != "the final answer" {
		t.Errorf("step text = %q, want %q", got, "the final answer")
	}
}

// TestService_SetupFailure_FailsRun — a setup command that did not
// do its job fails the run. The steps that follow would work
// against a chat that is not ready.
func TestService_SetupFailure_FailsRun(t *testing.T) {
	client := &fakeClient{reply: func(text string) (loopback.Execution, error) {
		return loopback.Execution{
			Outcome: messages.Outcome{
				Status:  messages.OutcomeFailed,
				Summary: "the path does not exist",
			},
		}, nil
	}}
	svc := newRunService(t, client)

	wf, err := wfe.Parse([]byte(dagWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ev := wfe.Event{Kind: "schedule", Time: time.Now()}

	r := svc.startRun(wf, wf.Workspaces[0])
	svc.driveRun(context.Background(), r, ev)

	state, err := svc.stateStore.Load(r.runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Status != wfe.StatusFailed {
		t.Errorf("status = %q, want failed", state.Status)
	}
	if len(state.StepOutputs) != 0 {
		t.Errorf("step outputs = %v, want none", state.StepOutputs)
	}
	for _, cmd := range client.recorded() {
		if !strings.HasPrefix(cmd, "/") {
			t.Errorf("step ran after a failed setup: %q", cmd)
		}
	}
}

// TestStartRun_ChatIDIsLoopbackNamespaced — the run drives a chat of
// the loopback channel; a chat id outside that namespace is refused
// by the channel.
func TestStartRun_ChatIDIsLoopbackNamespaced(t *testing.T) {
	svc := newRunService(t, &fakeClient{})
	wf, err := wfe.Parse([]byte(dagWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := svc.startRun(wf, wf.Workspaces[0])
	if !strings.HasPrefix(r.chatID, loopback.ChatIDPrefix) {
		t.Errorf("chat id = %q, want the %q prefix", r.chatID, loopback.ChatIDPrefix)
	}
}

func TestCanonicalRepoURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://github.com/foo/bar.git", "foo/bar"},
		{"https://github.com/foo/bar", "foo/bar"},
		{"git@github.com:foo/bar.git", "foo/bar"},
		{"git@github.com:foo/bar", "foo/bar"},
		{"", ""},
		{"  https://github.com/foo/bar.git  ", "foo/bar"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := canonicalRepoURL(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseKVOutput(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{"FOO=bar\nBAZ=qux", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"  KEY = val  \n", map[string]string{"KEY": "val"}},
		{"no-equals\n=starts-equals\nKEY=", nil},
		{"", nil},
		{"just text\nA=1", map[string]string{"A": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseKVOutput(tt.in)
			if !mapsEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"~/work/nightme", "home_work_nightme"},
		{"/tmp/foo", "tmp_foo"},
		{"./relative", "relative"},
		{"a:b/c d", "a_b_c_d"},
		// Windows: filepath.Clean converts "/" → "\", so
		// sanitize must ToSlash first to avoid leading "\"
		// surviving TrimLeft("/"). Colon → '_' and slash
		// immediately after → '_' (both characters
		// independently replaced).
		{`C:\Users\alice\work`, "C__Users_alice_work"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
