package me

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
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
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"}),
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, lb, sender := testService(t, dir, Config{Provider: provider})
	lb.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := lb.Sent()
	found := false
	for _, cmd := range sent {
		if cmd == "/gtw commit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the work chat got %v, want /gtw commit", sent)
	}
	if !sender.Has("✅ /gtw commit") {
		t.Fatalf("the observer did not see the settled step: %v", sender.Lines())
	}
	if !sender.Has("system-2: 1 decisions") {
		t.Fatalf("the summary does not count the turns: %v", sender.Lines())
	}
}

func TestStatus_ReportsTheRunWhileItWorks(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"})},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	// The command never settles: the run waits for it.
	lb.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, lb, "/gtw commit")

	// The status is read while the run works.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := svc.Status("obs-1")
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
	writeWorkflow(t, dir, "two", `name: two
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if len(lb.Watched()) != 2 {
		t.Fatalf("the run watched %v, want one watch per command", lb.Watched())
	}
}

func TestSystem2_PolicyViolationComesBackAsAToolError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw sync"})},
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"command": "/gtw commit"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	lb.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	for _, cmd := range lb.Sent() {
		if cmd == "/gtw sync" {
			t.Fatalf("the policy rejected /gtw sync but it ran anyway: %v", lb.Sent())
		}
	}
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
			if p.IsError && strings.Contains(p.Text, "not allowed") {
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
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "nothing to commit"})},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	lb.steps = []fakeStep{pendingStep("still going"), {outcome: messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to commit"}}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
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
	part := toolResultPart(t, r.conversation(), toolRunStep)
	if part.IsError {
		t.Fatalf("a noop outcome came back as a tool error: %s", part.Text)
	}
	if !strings.Contains(part.Text, "noop") {
		t.Fatalf("the tool result does not carry the status: %s", part.Text)
	}
}

func TestSystem2_TextOnlyReplyIsNudgedAndThenEndsTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
		{Message: textMsg("Still thinking about it.")},
	}}
	svc, lb, sender := testService(t, dir, Config{Provider: provider})
	lb.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitRun(t, svc, "obs-1")

	// The run ended as a failure, not as a finished one.
	if r == nil {
		t.Fatal("the run is not registered")
	}
	if phase := r.currentPhase(); phase != PhaseFailed {
		t.Fatalf("the run ended as %q, want %q", phase, PhaseFailed)
	}

	if provider.Calls() != 2 {
		t.Fatalf("the chat model was called %d times, want 2 (the nudge and the second answer)", provider.Calls())
	}
	req := provider.Request(1)
	if !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "Call a tool") {
		t.Fatalf("the second turn did not carry the nudge: %v", req.Messages)
	}
	if !sender.Has("did not call a tool") {
		t.Fatalf("the observer was not told why the run ended: %v", sender.Lines())
	}
}

func TestSystem2_TokenBudgetStopsTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"}),
			Usage: llm.Usage{InputTokens: 500, OutputTokens: 500}},
	}}
	svc, lb, sender := testService(t, dir, Config{Provider: provider, TokenBudget: 100})
	lb.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !sender.Has("token budget") {
		t.Fatalf("the observer was not told about the budget: %v", sender.Lines())
	}
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
	b.WriteString("name: many\njobs:\n  main:\n    steps:\n")
	for i := 1; i <= steps; i++ {
		fmt.Fprintf(&b, "      - id: s%d\n        message: /gtw s%d\n", i, i)
	}
	writeWorkflow(t, dir, "many", b.String())

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	for i := 0; i < steps; i++ {
		lb.steps = append(lb.steps, fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}})
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "many", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := lb.Sent()
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
	writeWorkflow(t, dir, "two", `name: two
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"})},
	}}
	lb := newFakeLoopback(
		pendingStep("still going"),
		fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "committed"},
			transcript: []loopback.Entry{{Kind: messages.OutResult, Text: "the work agent said hello"}}},
		okStep("pushed"),
	)
	sender := &fakeSender{}
	svc := New(Config{
		WorkflowsDir: dir,
		StateDir:     dir + "/runs",
		Provider:     provider,
		Loopback:     lb,
		Sender:       sender,
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
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
			if p.Type == llm.PartToolResult && strings.Contains(p.Text, "the work agent said hello") {
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
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCallID: "c1", ToolName: toolRunStep}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{
			Type: llm.PartToolResult, ToolCallID: "c1", ToolName: toolRunStep,
			Text: `{"status":"ok","summary":"committed","transcript":"a long transcript"}`,
		}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCallID: "c2", ToolName: toolRunStep}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{
			Type: llm.PartToolResult, ToolCallID: "c2", ToolName: toolRunStep,
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
	writeWorkflow(t, dir, "two", `name: two
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
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"command": "/gtw commit"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, lb, sender := testService(t, dir, Config{Provider: provider})
	lb.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which branch?")

	// The run is waiting: the command did not run yet.
	for _, cmd := range lb.Sent() {
		if cmd == "/gtw commit" {
			t.Fatalf("the run went on before the user answered: %v", lb.Sent())
		}
	}
	if err := svc.Reply(context.Background(), "obs-1", "the feature branch"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")

	waitSent(t, lb, "/gtw commit")
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
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"command": "/gtw commit"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, lb, sender := testService(t, dir, Config{Provider: provider})
	lb.steps = []fakeStep{pendingStep("still going")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which branch?")

	// The daemon restarts: a new service picks the run up.
	restarted, lb2, _ := testService(t, dir, Config{Provider: provider})
	waitForReply(t, restarted, "obs-1")

	if err := restarted.Reply(context.Background(), "obs-1", "the feature branch"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, restarted, "obs-1")
	waitSent(t, lb2, "/gtw commit")
}

func TestShutdown_KeepsTheRunForTheNextDaemon(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"command": "/gtw commit"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "committed"})},
	}}
	svc, _, sender := testService(t, dir, Config{Provider: provider})
	lb := svc.cfg.Loopback.(*fakeLoopback)
	lb.steps = []fakeStep{pendingStep("still going")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which branch?")

	// The daemon goes away while the run waits for an answer.
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitStateFile(t, dir)

	// The next daemon picks the run up and it keeps waiting.
	restarted, _, _ := testService(t, dir, Config{Provider: provider})
	waitForReply(t, restarted, "obs-1")
}

func TestRestart_ResolvesAnInFlightCommandAsInterrupted(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw commit"})},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider})
	svc.cfg.StallTimeout = 50 * time.Millisecond
	// The command never settles: the run waits for it.
	lb.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, lb, "/gtw commit")
	// Wait until the run persisted the in-flight command.
	waitStateFile(t, dir)

	// The daemon restarts with a fresh loopback: the execution of the
	// in-flight command is gone.
	restarted, _, _ := testService(t, dir, Config{Provider: provider})
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

// TestRunStep_WaitsForTheAnswerOfTheWorkAgent — the work agent asks
// a question: the run waits for the answer instead of running the
// command again.
func TestRunStep_WaitsForTheAnswerOfTheWorkAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	lb.steps = []fakeStep{{
		open: true,
		choices: []messages.Choice{{
			RequestID: "req-1",
			Title:     "Allow the commit?",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "yes"}},
		}},
	}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	if decider.Calls() == 0 {
		t.Fatal("the decision model was not asked")
	}
	if !strings.Contains(decider.Request(0).State, "still running") {
		t.Fatalf("the decision model was not told that the step is waiting: %s", decider.Request(0).State)
	}
	// The command is not run again while it waits for an answer.
	count := 0
	for _, cmd := range lb.Sent() {
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

// TestRunStep_ReturnsEarlyOnAStall — the work agent goes quiet: the
// run stops waiting for it and hands the decision to the agent.
func TestRunStep_ReturnsEarlyOnAStall(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	svc.cfg.StallTimeout = 50 * time.Millisecond
	lb.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	if decider.Calls() == 0 {
		t.Fatal("the decision model was not asked")
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestIntegration_TheWorkflowRunsEndToEnd — the review step does not
// finish, so the agent repairs it; the plan carries on and the run
// ends on its own.
func TestIntegration_TheWorkflowRunsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/review -c"})},
	}}
	lb := newFakeLoopback(
		okStep("fixed"),
		okStep("committed"),
		fakeStep{outcome: messages.Outcome{
			Status:  messages.OutcomeIncomplete,
			Summary: "1 of 3 groups unfinished",
			Data:    map[string]string{"groups": "3", "unfinished_groups": "1"},
		}},
		okStep("review finished"),
		okStep("pushed"),
		fakeStep{outcome: messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "pull request opened",
			Data:    map[string]string{"pr_number": "7", "pr_url": "https://example.test/pr/7"},
		}},
	)
	sender := &fakeSender{}
	svc := New(Config{
		WorkflowsDir: dir,
		StateDir:     dir + "/runs",
		Provider:     provider,
		Loopback:     lb,
		Sender:       sender,
		Answerer:     fakeAnswerer{lb: lb},
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	waitRun(t, svc, "obs-1")

	want := []string{
		"▶ /gtw fix 42 -y",
		"✅ /gtw fix 42 -y",
		"▶ /review",
		"⚠️ /review incomplete · 1/3 groups unfinished",
		"▶ /review -c",
		"✅ /review -c",
		"▶ /gtw push",
		"▶ /gtw pr",
		"fix-issue is complete",
	}
	for _, line := range want {
		if !sender.Has(line) {
			t.Fatalf("the observer never saw %q: %v", line, sender.Lines())
		}
	}
}

// waitStateFile waits until a run persisted its state.
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
		st := svc.Status(chatID)
		if st.Phase == PhasePaused {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the run is not waiting for a reply")
}
