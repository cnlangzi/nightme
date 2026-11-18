package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// okStep is a command that succeeded.
func okStep(summary string) fakeStep {
	return fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: summary}}
}

// pendingStep is a command that did not finish: the run reaches a
// judgment point and the agent takes over.
func pendingStep(summary string) fakeStep {
	return fakeStep{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: summary}}
}

func TestSystem2_ToolDispatch(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{}),
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	found := false
	for _, cmd := range sent {
		if cmd == "/gtw commit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the chat got %v, want /gtw commit", sent)
	}
	var settled bool
	for _, item := range op.Checklist(op.Cards()[1].ID) {
		if item.Subject == "commit" && item.Status == agent.TaskCompleted {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("the card of the round did not show the settled step: %+v", op.Checklist(op.Cards()[1].ID))
	}
}

func TestStatus_ReportsTheRunWhileItWorks(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The command never settles: the run waits for it.
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	// The status is read while the run works.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := svc.Snapshot("obs-1")
		if st.RunID == "" {
			t.Fatal("the status has no run")
		}
		if st.Step != "/gtw commit" {
			t.Fatalf("the status names the step %q, want /gtw commit", st.Step)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestFastPath_EveryCommandIsWatchedOnItsOwn — each command the plan
// runs is watched on its own message: the watch of a settled command
// does not carry over to the next one.
func TestFastPath_EveryCommandIsWatchedOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if len(op.Watched()) != 2 {
		t.Fatalf("the run watched %v, want one watch per command", op.Watched())
	}
}

func TestSystem2_PolicyViolationComesBackAsAToolError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "maybe", "summary": "committed"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The model saw the violation as a tool error and corrected
	// itself.
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
			if p.IsError && strings.Contains(p.Text, "status must be") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the violation did not come back as a tool error")
	}
}

func TestSystem2_ANoopStepIsNotAToolError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "nothing to commit"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), {outcome: messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to commit"}}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model never took a turn")
	}
	part := toolResultPart(t, r.conversation(), toolResume)
	if part.IsError {
		t.Fatalf("a noop outcome came back as a tool error: %s", part.Text)
	}
	if !strings.Contains(part.Text, "noop") {
		t.Fatalf("the tool result does not carry the status: %s", part.Text)
	}
}

// TestSystem2_TextOnlyReplyKeepsTheRunGoing — a turn of the chat
// model that produced no tool call does not end the run.
//
// The agent's job is to keep the pipeline moving: a turn it did not
// act in costs a chance and the agent is asked again. Its own words
// stay in the conversation, so a model that is asked again sees what
// it just said.
func TestSystem2_TextOnlyReplyKeepsTheRunGoing(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
		{Message: textMsg("Still thinking about it.")},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitCalls(t, provider, 2)

	// The run is still going: it did not end as a failure.
	if r == nil {
		t.Fatal("the run is not registered")
	}
	if phase := r.currentPhase(); phase == PhaseFailed {
		t.Fatalf("the run ended as %q, want it to keep going", phase)
	}
	var listed bool
	for _, item := range op.Checklist(op.Cards()[1].ID) {
		if item.Subject == "commit" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("the card of the round lost the step list: %+v", op.Checklist(op.Cards()[1].ID))
	}

	// The words of the model stayed in the conversation, and the next
	// request carries them.
	req := provider.Request(1)
	var carried bool
	for _, m := range req.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "I think I should commit.") {
				carried = true
			}
		}
	}
	if !carried {
		t.Fatalf("the second turn did not carry the words of the first: %v", req.Messages)
	}
}

// waitCalls waits until the provider served that many calls.
func waitCalls(t *testing.T, provider *scriptedProvider, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if provider.Calls() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the chat model was called %d times, want %d", provider.Calls(), n)
}

// TestRun_HasNoTurnCap — a run takes as many turns as the plan needs.
//
// There is no turn cap any more: only `/me stop` and the daemon's
// shutdown end a run. A plan with more steps than the old cap allowed
// runs to the end.
func TestRun_HasNoTurnCap(t *testing.T) {
	dir := t.TempDir()
	const steps = 100
	var b strings.Builder
	b.WriteString("title: many\njobs:\n  main:\n    steps:\n")
	for i := 1; i <= steps; i++ {
		fmt.Fprintf(&b, "      - id: s%d\n        message: /gtw s%d\n", i, i)
	}
	writeWorkflow(t, dir, "many", b.String())

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider})
	for i := 0; i < steps; i++ {
		op.steps = append(op.steps, fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}})
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	for i := 1; i <= steps; i++ {
		cmd := fmt.Sprintf("/gtw s%d", i)
		found := false
		for _, s := range sent {
			if s == cmd {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the step %q never ran: %d commands were sent", cmd, len(sent))
		}
	}
}

func TestCompact_LeavesTheRunConversationAlone(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	op := newFakeOperator(
		pendingStep("still going"),
		fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "committed"},
			transcript: []observe.Entry{{Kind: messages.OutResult, Text: "the coding agent said hello"}}},
		okStep("pushed"),
	)
	svc := New(Config{
		WorkflowsDir: dir,
		StateDir:     dir + "/runs",
		Provider:     provider,
		Operator:     op,
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitRun(t, svc, "obs-1")

	// The run keeps the transcript of its command: the compaction is
	// for the model, not for the run.
	found := false
	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolResult && strings.Contains(p.Text, "the coding agent said hello") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the compaction rewrote the run's own conversation")
	}
}

func TestCompact_OldToolResultsKeepOnlyTheOutcome(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "go"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCallID: "c1", ToolName: toolResume}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{
			Type: llm.PartToolResult, ToolCallID: "c1", ToolName: toolResume,
			Text: `{"status":"ok","summary":"committed","transcript":"a long transcript"}`,
		}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCallID: "c2", ToolName: toolResume}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{
			Type: llm.PartToolResult, ToolCallID: "c2", ToolName: toolResume,
			Text: `{"status":"ok","summary":"pushed","transcript":"another transcript"}`,
		}}},
	}
	out := compact(msgs)

	first := out[2].Parts[0].Text
	if strings.Contains(first, "transcript") {
		t.Fatalf("the old tool result kept its transcript: %s", first)
	}
	if !strings.Contains(first, "committed") {
		t.Fatalf("the old tool result lost its summary: %s", first)
	}
	last := out[4].Parts[0].Text
	if !strings.Contains(last, "transcript") {
		t.Fatalf("the latest tool result lost its transcript: %s", last)
	}
}

func TestAskHuman_SuspendsAndTheReplyResumes(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two
jobs:
  main:
    steps:
      - id: prepare
        message: /gtw prepare
      - id: commit
        message: /gtw commit
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")

	// The run is waiting: the command did not run yet.
	for _, cmd := range op.Sent() {
		if cmd == "/gtw commit" {
			t.Fatalf("the run went on before the user answered: %v", op.Sent())
		}
	}
	if err := svc.Reply(context.Background(), "obs-1", "the feature branch"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")

	waitSent(t, op, "/gtw commit")
	req := provider.Request(1)
	if !messagesContain(req, "the feature branch") {
		t.Fatalf("the reply did not become the tool result: %v", req.Messages)
	}
}

func TestRestart_ResumesASuspendedRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")

	// The daemon restarts: a new service picks the run up.
	restarted, op2 := testService(t, dir, Config{Provider: provider})
	waitForReply(t, restarted, "obs-1")

	if err := restarted.Reply(context.Background(), "obs-1", "the feature branch"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, restarted, "obs-1")
	waitSent(t, op2, "/gtw commit")
}

func TestShutdown_KeepsTheRunForTheNextDaemon(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")

	// The daemon goes away while the run waits for an answer.
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitStateFile(t, dir)

	// The next daemon picks the run up and it keeps waiting.
	restarted, _ := testService(t, dir, Config{Provider: provider})
	waitForReply(t, restarted, "obs-1")
}

func TestRestart_ResolvesAnInFlightCommandAsInterrupted(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The command never settles: the run waits for it.
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	// Wait until the run persisted the in-flight command: a state file
	// without it would restart the step from the beginning.
	waitInFlight(t, dir)

	// The daemon restarts with a fresh tracker: the execution of the
	// in-flight command is gone.
	restarted, _ := testService(t, dir, Config{Provider: provider})
	restarted.mu.Lock()
	var restored *run
	for _, r := range restarted.runs {
		restored = r
	}
	restarted.mu.Unlock()
	if restored == nil {
		t.Fatal("the run was not resumed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		restored.mu.Lock()
		history := append([]wfe.ActionRecord(nil), restored.policy.History...)
		restored.mu.Unlock()
		if len(history) > 0 {
			if !strings.Contains(history[0].Outcome.Summary, "interrupted") {
				t.Fatalf("the in-flight command was resolved as %q, want an interruption",
					history[0].Outcome.Summary)
			}
			if history[0].Outcome.Status != wfe.OutcomeUnknown {
				t.Fatalf("the interrupted command was recorded as %q, want unknown",
					history[0].Outcome.Status)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the in-flight command was never resolved")
}

// TestRunStep_WaitsForTheAnswerOfTheWorkAgent — the coding agent asks
// a question: the run waits for the answer instead of running the
// command again.
func TestRunStep_WaitsForTheAnswerOfTheWorkAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{
		open: true,
		choices: []messages.Choice{{
			RequestID: "req-1",
			Title:     "Allow the commit?",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "yes"}},
		}},
	}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	if decider.Calls() == 0 {
		t.Fatal("the decision model was not asked")
	}
	if !strings.Contains(decider.Request(0).State, "still running") {
		t.Fatalf("the decision model was not told that the step is waiting: %s", decider.Request(0).State)
	}
	// The command is not run again while it waits for an answer.
	count := 0
	for _, cmd := range op.Sent() {
		if cmd == "/gtw commit" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the command ran %d times, want once", count)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestRunStep_WaitsForTheAgentToFinish — the run has no clock of its
// own: a command that has not settled is still running, and the run
// keeps watching it instead of deciding that it failed.
func TestRunStep_WaitsForTheAgentToFinish(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	// The command has not settled. The run is still watching it: the
	// decision model is not asked, because the agent has not said
	// anything yet.
	time.Sleep(200 * time.Millisecond)
	if decider.Calls() != 0 {
		t.Fatalf("the decision model was asked %d times, want 0 — the run waits for the agent",
			decider.Calls())
	}
	if st := svc.Snapshot("obs-1"); st.Phase != PhaseRunning {
		t.Errorf("the run is %q, want it waiting for the agent", st.Phase)
	}

	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestIntegration_TheWorkflowRunsEndToEnd — the review step does not
// finish, so the agent repairs it and runs the step again; the plan
// carries on and the run ends on its own.
func TestIntegration_TheWorkflowRunsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_review", map[string]any{"args": "-c"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
	}}
	op := newFakeOperator(
		okStep("fixed"),
		okStep("committed"),
		fakeStep{outcome: messages.Outcome{
			Status:  messages.OutcomeIncomplete,
			Summary: "1 of 3 groups unfinished",
			Data:    map[string]string{"groups": "3", "unfinished_groups": "1"},
		}},
		okStep("review finished"),
		okStep("review finished"),
		okStep("pushed"),
		fakeStep{outcome: messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "pull request opened",
			Data:    map[string]string{"pr_number": "7", "pr_url": "https://example.test/pr/7"},
		}},
	)
	svc := New(Config{
		WorkflowsDir: dir,
		StateDir:     dir + "/runs",
		Provider:     provider,
		Operator:     op,
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "review"}},
	}); err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// Every command of the run was spoken in the chat.
	want := []string{
		"/gtw fix 42 -y",
		"/gtw commit",
		"/review",
		"/review -c",
		"/gtw push",
		"/gtw pr",
	}
	spoken := strings.Join(op.Sent(), "\n")
	for _, cmd := range want {
		if !strings.Contains(spoken, cmd) {
			t.Fatalf("the chat never saw %q: %v", cmd, op.Sent())
		}
	}
	// The card of the round shows the steps and how they ended, and
	// the run's card says how the run ended.
	var steps bool
	for _, item := range op.Checklist(op.Cards()[1].ID) {
		if item.Subject == "fix" {
			steps = true
		}
	}
	if !steps {
		t.Fatalf("the card of the round never showed the step: %+v", op.Checklist(op.Cards()[1].ID))
	}
	var done bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.Result == "fix-issue is complete" {
			done = true
		}
	}
	if !done {
		t.Fatalf("the run never said how it ended: %v", op.EmittedOf(messages.OutResult))
	}
}

// waitStateFile waits until a run persisted its state.
// waitInFlight waits until the persisted run carries the command it
// was waiting for.
func waitInFlight(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(filepath.Join(dir, "runs"))
		if err == nil {
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(dir, "runs", e.Name()))
				if err != nil {
					continue
				}
				var state persistedRun
				if err := json.Unmarshal(raw, &state); err != nil {
					continue
				}
				if state.Inflight != nil {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the run did not persist its in-flight command")
}

func waitStateFile(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(filepath.Join(dir, "runs"))
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the run did not persist its state")
}

// messagesContain reports whether a request carries the given text.
func messagesContain(req llm.Request, text string) bool {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, text) {
				return true
			}
		}
	}
	return false
}

// waitForReply waits until a run waits for a reply.
func waitForReply(t *testing.T, svc *Service, chatID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := svc.Snapshot(chatID)
		if st.Phase == PhaseAsking {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the run is not waiting for a reply")
}
