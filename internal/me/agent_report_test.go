package me

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// TestRenderWorkflow_HidesTheMachinery — the model reads the steps and
// why they are there, not the machinery of the workflow.
func TestRenderWorkflow_HidesTheMachinery(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: Do the thing.
jobs:
  main:
    if: ${{ success() }}
    steps:
      - id: commit
        message: /gtw commit
        if: ${{ success() }}
        env:
          TOKEN: secret
        with:
          key: value
        continue-on-error: true
        note: the commit comes first
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	out := svc.renderWorkflow(r)
	for _, want := range []string{
		"objective: Do the thing.",
		"job main — runs once",
		"commit (message): /gtw commit",
		"note: the commit comes first",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the workflow does not carry %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"if:", "env:", "with:", "continue-on-error", "${{", "workspaces", "on:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the workflow carries %q:\n%s", unwanted, out)
		}
	}
}

// TestRenderWorkflow_TheRoundsOfAJob — a job says how many rounds it
// runs.
func TestRenderWorkflow_TheRoundsOfAJob(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  once:
    steps:
      - id: a
        message: /gtw a
  twice:
    iterations: 3
    steps:
      - id: b
        message: /gtw b
  forever:
    iterations: 0
    steps:
      - id: c
        message: /gtw c
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	out := svc.renderWorkflow(r)
	for _, want := range []string{
		"job once — runs once",
		"job twice — runs 3 rounds",
		"job forever — repeats until the run is stopped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the workflow does not carry %q:\n%s", want, out)
		}
	}
}

// TestRenderSituation_SaysWhereTheRunStands — the report names the
// reason, the round, the steps, what each of them produced and the
// transcript of the last actions.
func TestRenderSituation_SaysWhereTheRunStands(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
args:
  - name: id
    default: "42"
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)
	svc, _ := testService(t, dir, Config{})
	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "one", ChatID: "obs-1", Cwd: "/tmp/work"}, bound,
		map[string]string{"id": "42"})
	r.setJudgment(`the previous step "commit" ended as "incomplete"`)
	out := wfe.Outcome{
		Status:  wfe.OutcomeIncomplete,
		Summary: "the commit did not finish",
		Data:    map[string]string{"reason": "agent_error"},
	}
	step, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.recordStep("main", step, out)
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", out)
	r.setTranscript("/gtw commit", "the coding agent said hello")

	report := svc.renderSituation(context.Background(), r)
	for _, want := range []string{
		"The run needs you: step commit ended as incomplete.",
		"args: id=42",
		"round: main, round 1",
		"directory: /tmp/work",
		"commit: incomplete",
		"push: not run yet",
		"— ready",
		"the commit did not finish",
		"reason: agent_error",
		"hint: the agent did not run, or it produced no usable output.",
		"transcripts (tail):",
		"--- /gtw commit ---",
		"the coding agent said hello",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the situation does not carry %q:\n%s", want, report)
		}
	}
}

// TestRenderSituation_CarriesTheEvidenceOfTheLastTwoActions — the
// evidence that explains a failure is often in the action before the
// one that failed.
func TestRenderSituation_CarriesTheEvidenceOfTheLastTwoActions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix
      - id: commit
        message: /gtw commit
`)
	svc, _ := testService(t, dir, Config{})
	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "one", ChatID: "obs-2", Cwd: "/tmp/work"}, bound,
		map[string]string{})
	r.setJudgment(`the previous step "commit" ended as "failed"`)

	fix, ok := r.stepByID("fix")
	if !ok {
		t.Fatal("the workflow has no fix step")
	}
	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.recordStep("main", fix, wfe.Outcome{
		Status:  wfe.OutcomeOK,
		Summary: "the worktree for branch feat/x is ready",
	})
	r.setTranscript("/gtw fix", "the agent said it could not find the file")
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the worktree has nothing to commit",
		Data:    map[string]string{"reason": "nothing_to_commit"},
	})
	r.setTranscript("/gtw commit", "nothing to commit")

	report := svc.renderSituation(context.Background(), r)
	for _, want := range []string{
		"fix: ok",
		"the worktree for branch feat/x is ready",
		"commit: failed",
		"the worktree has nothing to commit",
		"reason: nothing_to_commit",
		"hint: the working tree carries no changes: there was nothing to commit.",
		"--- /gtw fix ---",
		"the agent said it could not find the file",
		"--- /gtw commit ---",
		"nothing to commit",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the situation does not carry %q:\n%s", want, report)
		}
	}
}

// TestThePrompt_TheEngineIsTheMaster — the agent repairs a gap and
// gives the control back: it is the engine's helper, not the owner's
// avatar.
func TestThePrompt_TheEngineIsTheMaster(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, want := range []string{
		"You are the owner's hands inside a workflow",
		"The engine is the master",
		"it comes to you only when a step raised an exception",
		"get the flow going again and then give the control back",
		"the engine decides what runs next, not you",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "owner's avatar") {
		t.Errorf("the prompt still makes the agent the owner's avatar:\n%s", prompt)
	}
}

// TestThePrompt_ANormalResultIsNothingToRepair — an outcome without
// an error is a normal result: the workflow goes on by itself.
func TestThePrompt_ANormalResultIsNothingToRepair(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, want := range []string{
		"An outcome that carries no error is a normal result",
		"the workflow moves on by itself",
		"there is nothing there for you to repair",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

// TestThePrompt_TheNoteOfAStepWins — the note is the owner's
// instruction for the step.
func TestThePrompt_TheNoteOfAStepWins(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, want := range []string{
		"A step's note is the owner's instruction for that step",
		"The note wins over the ready set and over your own judgment",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

// TestThePrompt_TheWayOfWorking — the prompt says what to do, in the
// order to do it.
func TestThePrompt_TheWayOfWorking(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	steps := []string{
		"1. Read the report",
		"2. Find the cause of the error",
		"3. Repair the gap with the smallest action",
		"4. Then tell the engine which way to go",
		"5. Give the control back",
	}
	at := -1
	for _, step := range steps {
		i := strings.Index(prompt, step)
		if i < 0 {
			t.Fatalf("the prompt does not carry %q:\n%s", step, prompt)
		}
		if i < at {
			t.Errorf("the prompt carries %q out of order", step)
		}
		at = i
	}
	for _, want := range []string{"resume — the gap is repaired", "next — the step is not worth doing"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

// TestThePrompt_TheThingsThatAreNotTheAgents — the prompt names what
// the agent may not do.
func TestThePrompt_TheThingsThatAreNotTheAgents(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, want := range []string{
		"What is not yours to do",
		"You do not decide what the work is",
		"You do not make the workflow's input arrive",
		"do not look for work",
		"do not file or label issues",
		"do not hunt for defects to fix",
		"do not open pull requests the workflow did not ask for",
		"You do not widen the workflow",
		"you do not ask the owner to widen it",
		"You do not repair a step that did not break",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

// TestThePrompt_HidesTheEngine — the model reads the work, not the
// machinery that runs it.
//
// The ready set is the one name of the engine the prompt carries: the
// step's note wins over it, and the model has to know that.
func TestThePrompt_HidesTheEngine(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, unwanted := range []string{
		"judgment point", "fast path", "System 1", "System 2",
		"decider", "policy", "token", "429",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("the prompt carries %q", unwanted)
		}
	}
}

// TestThePrompt_WordsTheTools — the prompt says what the tools are
// for.
func TestThePrompt_WordsTheTools(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, want := range []string{
		"resume()",
		"next()",
		"ntm_* run nightme commands, as the owner",
		"instruct(text)",
		"repo_status",
		"answer_choice",
		"ask_human",
		"wait(duration)",
		"end_round",
		"finish(done)",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "run_step") {
		t.Errorf("the prompt still offers the tool that names a step:\n%s", prompt)
	}
}

// TestThePrompt_TeachesTheNewJudgmentPoints — the prompt names the
// two situations that are still the agent's when a step produced
// nothing.
func TestThePrompt_TeachesTheNewJudgmentPoints(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	for _, want := range []string{
		"When a step produced nothing",
		"An outcome without an error is a normal result",
		"there is nothing to repair",
		"verdict nobody reported",
		"still running in the background",
		"call wait, then look again",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

// TestTheReasonHints_ExplainTheEmptyQueue — the hint of the reason
// code of an empty queue says what to do about it.
func TestTheReasonHints_ExplainTheEmptyQueue(t *testing.T) {
	hint := reasonHint("no_candidate")
	if hint == "" {
		t.Fatal("the empty queue has no hint")
	}
	for _, want := range []string{"queue", "wait"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint does not carry %q: %q", want, hint)
		}
	}
}

// TestTheReasonHints_ExplainTheMergeStates — every reason code
// `/gtw pr view` reports has a hint. A program that branches on the
// code reads the hint next to it; a code without one leaves the
// program guessing.
func TestTheReasonHints_ExplainTheMergeStates(t *testing.T) {
	for _, code := range []string{
		"merge_conflict",
		"pr_behind",
		"pr_blocked",
		"state_unknown",
		"pr_closed",
		"no_pr",
		"pr_not_found",
	} {
		if reasonHint(code) == "" {
			t.Errorf("the reason %q has no hint", code)
		}
	}
}

// TestThePrompt_AWorkflowWithoutInstructions — a workflow that does
// not allow instructions says so.
func TestThePrompt_AWorkflowWithoutInstructions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
instruct: false
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	if strings.Contains(prompt, "instruct(text) asks the agent") {
		t.Errorf("the prompt still offers the instruction tool:\n%s", prompt)
	}
	if !strings.Contains(prompt, "This workflow does not allow instructions") {
		t.Errorf("the prompt does not say that instructions are off:\n%s", prompt)
	}
}

// TestThePrompt_WithoutAnAgentThereIsNoThinking — a run without an
// agent does not offer one.
func TestThePrompt_WithoutAnAgentThereIsNoThinking(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	prompt := svc.systemPrompt(r)
	if strings.Contains(prompt, "agent(task)") {
		t.Errorf("the prompt offers a thinking that is not there:\n%s", prompt)
	}
	if strings.Contains(prompt, "ask agent") {
		t.Errorf("the prompt asks for a thinking that is not there:\n%s", prompt)
	}
}

// TestTheConversation_HoldsNoHints — the decision model leaves no trace
// of its probabilities in the conversation the chat model reads.
func TestTheConversation_HoldsNoHints(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	// The model waits until the test is done with it.
	provider.block = make(chan struct{})
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw commit", 0.2, 0.8),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	// The chat waits until the test has the run: the run deletes
	// itself when it ends.
	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	close(provider.block)
	waitRun(t, svc, "obs-1")

	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "system-1 hints") {
				t.Errorf("the conversation carries the hints: %q", p.Text)
			}
			if strings.Contains(p.Text, "p=") {
				t.Errorf("the conversation carries a probability: %q", p.Text)
			}
		}
	}
}

// TestTheConversation_TheSyntheticCallHasTheNewShape — the decision
// is recorded as the call the model would have made.
func TestTheConversation_TheSyntheticCallHasTheNewShape(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	// The model waits until the test is done with it.
	provider.block = make(chan struct{})
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw commit", 0.95, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	// The chat waits until the test has the run: the run deletes
	// itself when it ends.
	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	close(provider.block)
	waitRun(t, svc, "obs-1")

	found := false
	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if p.Type != llm.PartToolCall {
				continue
			}
			if p.ToolName != toolRunStep {
				continue
			}
			var args map[string]any
			if err := json.Unmarshal(p.Args, &args); err != nil {
				t.Fatalf("the synthetic call has no arguments: %v", err)
			}
			if args["step_id"] != "commit" {
				t.Errorf("the synthetic call names %v, want the step", args["step_id"])
			}
			if _, ok := args["command"]; ok {
				t.Errorf("the synthetic call still takes a command: %v", args)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("the decision was not recorded in the conversation")
	}
}

// TestTheDecisionModelCannotAnswerWithACommandItWasNotGiven — a
// command the decision model was not offered is not a decision: the
// run escalates instead of running it.
func TestTheDecisionModelCannotAnswerWithACommandItWasNotGiven(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw push", 0.99, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model was not asked: the decision was refused")
	}
	for _, cmd := range op.Sent() {
		if cmd == "/gtw push" {
			t.Fatalf("the decision model ran a command it was not offered: %v", op.Sent())
		}
	}
}

// TestTheDecisionModelStillSeesTheSameFacts — the decision model is
// untouched: its options and its state are the ones it always read.
func TestTheDecisionModelStillSeesTheSameFacts(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	options := svc.decisionOptions(r)
	if len(options) != 3 {
		t.Fatalf("the options are %v, want the command plus done and escalate", options)
	}
	if options[0].Value != "/gtw commit" {
		t.Errorf("the first option is %q, want the command of the step", options[0].Value)
	}
	if options[1].Value != optionDone || options[2].Value != optionEscalate {
		t.Errorf("the options are %v, want done and escalate at the end", options)
	}

	state := svc.decisionState(r)
	for _, want := range []string{"workflow: one", "title: Do the one thing.", "steps:", "1. /gtw commit"} {
		if !strings.Contains(state, want) {
			t.Errorf("the decision state does not carry %q:\n%s", want, state)
		}
	}
}

// TestAResumedRunDropsAnOldConversation — a conversation written by an
// older daemon cannot be read by the model the way it was written: it
// is dropped and the run starts from the situation report.
func TestAResumedRunDropsAnOldConversation(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")
	r.messages = []llm.Message{
		{Role: llm.RoleAssistant, Parts: []llm.Part{{
			Type:       llm.PartToolCall,
			ToolCallID: "c1",
			ToolName:   toolRunStep,
			Args:       json.RawMessage(`{"command":"/gtw commit"}`),
		}}},
	}
	raw, err := json.Marshal(persistedRun{
		RunID:    "one-1",
		ChatID:   "obs-1",
		Cwd:      "/tmp/work",
		Workflow: r.workflow,
		State:    r.state,
		Messages: r.messages,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := filepath.Join(dir, "runs", "one-1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	restored := svc.restoreRun(persistedRun{
		RunID:    "one-1",
		ChatID:   "obs-1",
		Cwd:      "/tmp/work",
		Workflow: r.workflow,
		State:    r.state,
		Messages: r.messages,
		PendingCalls: []pendingCall{
			{ID: "c1", Name: toolRunStep, Args: json.RawMessage(`{"command":"/gtw commit"}`)},
		},
	})
	if len(restored.conversation()) != 0 {
		t.Errorf("the old conversation was kept: %v", restored.conversation())
	}
	if len(restored.pendingCallList()) != 0 {
		t.Errorf("the old tool call was kept: %v", restored.pendingCallList())
	}
}

// TestARunKeepsItsCommandsAndItsAgent — a run that is resumed keeps
// the commands it may issue and the agent that thinks for the owner.
func TestARunKeepsItsCommandsAndItsAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{Agent: "daemon"})
	r := testRunWith(t, svc, "one", []CommandInfo{{Name: "gtw"}, {Name: "me"}}, "chat")
	if r.agentName != "daemon" {
		t.Errorf("the agent is %q, want the one the daemon names", r.agentName)
	}
	if len(r.commands) != 1 || r.commands[0].Name != "gtw" {
		t.Errorf("the commands are %v, want the usable ones", r.commands)
	}

	plain, _ := testService(t, dir, Config{})
	restored := plain.restoreRun(persistedRun{
		RunID:    "one-1",
		ChatID:   "obs-1",
		Cwd:      "/tmp/work",
		Workflow: r.workflow,
		State:    r.state,
		Commands: []CommandInfo{{Name: "gtw"}, {Name: "me"}},
		Agent:    "chat",
	})
	if len(restored.commands) != 1 || restored.commands[0].Name != "gtw" {
		t.Errorf("the restored commands are %v, want the usable ones", restored.commands)
	}
	if len(restored.policy.Commands) != 1 || restored.policy.Commands[0] != "gtw" {
		t.Errorf("the restored policy knows %v, want the usable commands", restored.policy.Commands)
	}
	if restored.agentName != "chat" {
		t.Errorf("the restored agent is %q, want the chat's agent", restored.agentName)
	}
}

// TestARunKeepsItsEvidence — a run that comes back after a daemon
// restart still shows the evidence of the actions that led to the
// judgment point.
func TestARunKeepsItsEvidence(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")
	r.setTranscript("/gtw fix", "the agent said it could not find the file")
	r.setTranscript("/gtw commit", "nothing to commit")

	restored := svc.restoreRun(persistedRun{
		RunID:    "one-1",
		ChatID:   "obs-1",
		Cwd:      "/tmp/work",
		Workflow: r.workflow,
		State:    r.state,
		Transcripts: []actionTranscript{
			{Command: "/gtw fix", Text: "the agent said it could not find the file"},
			{Command: "/gtw commit", Text: "nothing to commit"},
		},
	})
	report := svc.renderSituation(context.Background(), restored)
	for _, want := range []string{
		"--- /gtw fix ---",
		"the agent said it could not find the file",
		"--- /gtw commit ---",
		"nothing to commit",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the restored situation does not carry %q:\n%s", want, report)
		}
	}
}

// TestRenderSituation_ShowsTheErrorOfAnOutcome — the report says what
// broke: the error tells the agent that the step needs a repair.
func TestRenderSituation_ShowsTheErrorOfAnOutcome(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")
	r.setJudgment(`the previous step "commit" ended as "failed"`)

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
		Data: map[string]string{
			"error":  "the working tree has unresolved conflicts",
			"reason": "conflict",
		},
	})

	report := svc.renderSituation(context.Background(), r)
	for _, want := range []string{
		"error: the working tree has unresolved conflicts",
		"the working tree has unresolved conflicts",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the situation does not carry %q:\n%s", want, report)
		}
	}
}

// TestRenderSituation_KeepsTheVerdictOfARepair — a command the run
// issued for a step that has not run yet leaves its verdict nowhere
// else, so the report carries it.
func TestRenderSituation_KeepsTheVerdictOfARepair(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")
	r.setJudgment(`the previous step "commit" ended as "failed"`)

	// A repair the run issued for the step: the step has not run, so
	// the run state does not carry the verdict.
	r.record(wfe.ActionKindCommand, "/gtw push", "commit", wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the branch has nothing to push",
		Data:    map[string]string{"reason": "nothing_to_push"},
	})

	report := svc.renderSituation(context.Background(), r)
	for _, want := range []string{
		"the branch has nothing to push",
		"reason: nothing_to_push",
		"hint: the branch carries no commit the remote does not have: there was nothing to push.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the situation does not carry %q:\n%s", want, report)
		}
	}
}
