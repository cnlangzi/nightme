package me

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// TestARateLimitedAgentPausesTheRun — the agent answers 429 when it
// is throttled. The run waits for the operator instead of running
// the command again.
func TestARateLimitedAgentPausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "429 Too Many Requests",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Status("obs-1")
	if !strings.Contains(st.PauseReason, "429") {
		t.Errorf("the pause reason is %q, want it to name the rate limit", st.PauseReason)
	}
	// The command is not run again.
	if sent := op.Sent(); len(sent) != 1 {
		t.Errorf("the chat got %v, want the command once", sent)
	}
}

// TestARateLimitedAgentInTheTranscriptPausesTheRun — the 429 may
// reach the run in the transcript rather than in the summary.
func TestARateLimitedAgentInTheTranscriptPausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
	}, transcript: []observe.Entry{
		{Kind: messages.OutResult, Text: "API error: 429 rate_limit_error"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
}

// TestARateLimitWordWithoutTheCodeIsEnough — the agent may name the
// throttle without a number: "rate_limit_error" is the throttle.
func TestARateLimitWordWithoutTheCodeIsEnough(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
	}, transcript: []observe.Entry{
		{Kind: messages.OutResult, Text: `{"type":"error","error":{"type":"rate_limit_error"}}`},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
}

// TestARateLimitInTheMiddleOfALongTurnIsFound — the transcript the
// model reads is cut down to its head and tail; the scan reads every
// line, so a hint in the middle of a long turn is not lost.
func TestARateLimitInTheMiddleOfALongTurnIsFound(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	entries := make([]observe.Entry, 0, 81)
	for i := 0; i < 40; i++ {
		entries = append(entries, observe.Entry{
			Kind: messages.OutReply,
			Text: strings.Repeat("x", 100),
		})
	}
	entries = append(entries, observe.Entry{
		Kind: messages.OutReply,
		Text: "the api said 429, I am throttled",
	})
	for i := 0; i < 40; i++ {
		entries = append(entries, observe.Entry{
			Kind: messages.OutReply,
			Text: strings.Repeat("y", 100),
		})
	}

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
	}, transcript: entries}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
}

// TestACommandThatWorkedIsNotThrottled — a command that succeeded is
// not rate limited, whatever its transcript says.
func TestACommandThatWorkedIsNotThrottled(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "ok",
	}, transcript: []observe.Entry{
		{Kind: messages.OutResult, Text: "handled 429 responses before"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")
	if st := svc.Status("obs-1"); st.Paused {
		t.Error("the run paused although the command worked")
	}
}

// TestThePromptDoesNotMentionARateLimit — a throttled agent is the
// run's business: the run pauses by itself, so the model is not asked
// to think about it.
func TestThePromptDoesNotMentionARateLimit(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, noModels())
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	if strings.Contains(prompt, "429") {
		t.Errorf("the prompt mentions the rate limit:\n%s", prompt)
	}
}

// TestTheDecisionStateExplainsARateLimit — the decision model reads
// that the agent is throttled, so it does not pick another step.
func TestTheDecisionStateExplainsARateLimit(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, noModels())
	r := testRun(t, svc, "one")
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "429 Too Many Requests",
	})

	state := svc.decisionState(r)
	if !strings.Contains(state, "rate limited") {
		t.Errorf("the decision state does not mention the rate limit:\n%s", state)
	}
}

// testRun builds a run of a workflow in the service's directory.
func testRun(t *testing.T, svc *Service, name string) *run {
	t.Helper()
	wf, ok := svc.catalog.Lookup(name)
	if !ok {
		t.Fatalf("the workflow %q is not in the catalogue", name)
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	return newRun(svc, Request{Name: name, ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
}
