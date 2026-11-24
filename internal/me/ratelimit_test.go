package me

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// TestARateLimitedAgentPausesTheRun — the agent answers 429 when it
// is throttled. The run waits for the operator instead of running
// the command again.
func TestARateLimitedAgentPausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "429 Too Many Requests",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !strings.Contains(st.PauseReason, "429") {
		t.Errorf("the pause reason is %q, want it to name the rate limit", st.PauseReason)
	}
	// The operator is told that the run waits.
	var notice string
	var commands []string
	for _, text := range op.Sent() {
		if strings.HasPrefix(text, pauseMark) {
			notice = text
			continue
		}
		commands = append(commands, text)
	}
	if notice == "" {
		t.Errorf("the chat got no pause announcement: %v", op.Sent())
	}
	// The command is not run again.
	if len(commands) != 1 {
		t.Errorf("the chat got %v, want the command once", commands)
	}
}

// TestAThrottledStatusPausesTheRunWithoutTheText — the status says
// it all: the outcome carries 429 and not one of its words says so.
func TestAThrottledStatusPausesTheRunWithoutTheText(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusTooManyRequests,
		StatusText: "the upstream refused the request",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !strings.Contains(st.PauseReason, "429") {
		t.Errorf("the pause reason is %q, want it to name the rate limit", st.PauseReason)
	}
}

// TestARateLimitedAgentInTheTranscriptPausesTheRun — the 429 may
// reach the run in the transcript rather than in the summary.
func TestARateLimitedAgentInTheTranscriptPausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the command failed",
	}, transcript: []observe.Entry{
		{Kind: messages.OutResult, Text: "API error: 429 rate_limit_error"},
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
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
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the command failed",
	}, transcript: []observe.Entry{
		{Kind: messages.OutResult, Text: `{"type":"error","error":{"type":"rate_limit_error"}}`},
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
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
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the command failed",
	}, transcript: entries}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)
}

// TestAThrottledCommandTheChatModelIssuedPausesTheRun — no chain
// covers the command the chat model issued: its result is the run's
// own, so a throttle holds the run up.
func TestAThrottledCommandTheChatModelIssuedPausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_review", map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "the commit failed",
		}},
		{outcome: outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "429 Too Many Requests",
		}},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw"}, {Name: "review"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !strings.Contains(st.PauseReason, "429") {
		t.Errorf("the pause reason is %q, want it to name the rate limit", st.PauseReason)
	}
}

// TestAThrottledInstructionPausesTheRun — no chain covers the
// free-form instruction: its result is the run's own, so a throttle
// holds the run up.
func TestAThrottledInstructionPausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: Do the one thing.
instruct: true
jobs:
  main:
    steps:
      - id: commit
        message: "/gtw commit"
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolInstruct, map[string]any{"text": "review the changes"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "the commit failed",
		}},
		{outcome: outcome.Outcome{
			Success:    false,
			Status:     http.StatusInternalServerError,
			StatusText: "429 Too Many Requests",
		}},
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !strings.Contains(st.PauseReason, "429") {
		t.Errorf("the pause reason is %q, want it to name the rate limit", st.PauseReason)
	}
}

// TestACommandThatWorkedIsNotThrottled — a command that succeeded is
// not rate limited, whatever its transcript says.
func TestACommandThatWorkedIsNotThrottled(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: "ok",
	}, transcript: []observe.Entry{
		{Kind: messages.OutResult, Text: "handled 429 responses before"},
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")
	if st := svc.Snapshot("obs-1"); st.Paused {
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
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "429 Too Many Requests",
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

// TestARateLimitedStepHoldsTheRunWithoutAWakeUp — a throttle is the
// run's own business: the run pauses instead of waking the agent, and
// no model is asked while it waits.
func TestARateLimitedStepHoldsTheRunWithoutAWakeUp(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusTooManyRequests,
		StatusText: "the upstream refused the request",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	// The run waits for the operator: it did not open a decision
	// session, and it did not ask a model what to do.
	for _, card := range op.Cards() {
		if strings.HasPrefix(card.Body, decisionLine) {
			t.Errorf("the run opened a decision session while it was paused: %q", card.Body)
		}
	}
	if n := provider.Calls(); n != 0 {
		t.Errorf("the chat model was called %d times, want none", n)
	}
	if n := decider.Calls(); n != 0 {
		t.Errorf("the decision model was called %d times, want none", n)
	}
}
