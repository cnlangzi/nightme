package me

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// brokenStep is a command that broke: its status says so.
func brokenStep(summary string) fakeStep {
	return fakeStep{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: summary,
	}}
}

// cancelledStep is a command the run stopped.
func cancelledStep(summary string) fakeStep {
	return fakeStep{outcome: outcome.Outcome{
		Success:    false,
		Status:     outcome.StatusClientClosedRequest,
		StatusText: summary,
	}}
}

// emptyStep is a command that had nothing to do: it is a normal
// result, not a breakage.
func emptyStep(summary string) fakeStep {
	return fakeStep{outcome: outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: summary,
	}}
}

// unfinishedStep is a command that broke with work left behind: the
// outcome says how much of it there is.
func unfinishedStep(summary string, unfinished int) fakeStep {
	return fakeStep{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: summary,
		Data:       map[string]string{KeyUnfinished: strconv.Itoa(unfinished)},
	}}
}

// throttledStep is a command that broke because the agent was
// throttled: the outcome says 429 and leaves work behind.
func throttledStep(summary string, unfinished int) fakeStep {
	return fakeStep{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusTooManyRequests,
		StatusText: summary,
		Data:       map[string]string{KeyUnfinished: strconv.Itoa(unfinished)},
	}}
}

// fakeSwitcher switches the agent of a chat for the length of one
// attempt.
type fakeSwitcher struct {
	mu sync.Mutex
	// gate holds the switches until the test opens it.
	gate *gate
	// current is the agent the chat works with now.
	current string
	// switched are the agents the chain switched to.
	switched []string
	// switchedAt holds the number of commands the chat had received
	// when the chain switched the agent.
	switchedAt []int
	// restoredAt holds the number of commands the chat had received
	// when the agent was put back.
	restoredAt []int
	// op is the chat the attempts speak in.
	op *fakeOperator
	// err is why the switch fails.
	err error
}

// SwitchAgent implements AgentSwitcher.
func (f *fakeSwitcher) SwitchAgent(_ string, agent string) (func(), error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		gate.wait()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.switched = append(f.switched, agent)
	f.switchedAt = append(f.switchedAt, len(f.op.Sent()))
	previous := f.current
	f.current = agent
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.current = previous
		f.restoredAt = append(f.restoredAt, len(f.op.Sent()))
	}, nil
}

// watch sets the chat the switcher counts the commands of.
func (f *fakeSwitcher) watch(op *fakeOperator) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op = op
}

// agent returns the agent the chat works with now.
func (f *fakeSwitcher) agent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// switchHistory returns the agents the chain switched to.
func (f *fakeSwitcher) switchHistory() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.switched...)
}

// restored returns the number of commands the chat had received when
// the agent was put back.
func (f *fakeSwitcher) restored() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.restoredAt...)
}

// switchedCommands returns the number of commands the chat had
// received when the chain switched the agent.
func (f *fakeSwitcher) switchedCommands() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.switchedAt...)
}

// chainService starts a service with a workflow whose step names a
// chain of agents.
// chainService starts a service that runs a chain of agents.
//
// The gate, when there is one, holds the decisions of the service:
// the decision model is the first thing a chain that breaks before it
// speaks to the chat meets.
func chainService(t *testing.T, body string, sw *fakeSwitcher, gates ...*gate) (*Service, *fakeOperator) {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", body)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	if len(gates) > 0 {
		decider.gate = gates[0]
	}
	svc, op := testService(t, dir, Config{
		Provider:      provider,
		Decider:       decider,
		AgentSwitcher: sw,
	})
	if sw != nil {
		sw.watch(op)
	}
	return svc, op
}

// TestStepChain_TheArgsModePutsTheAgentOnTheCommandLine — the args
// mode names the agent on the command line.
func TestStepChain_TheArgsModePutsTheAgentOnTheCommandLine(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp]
`, nil)
	op.steps = []fakeStep{okStep("reviewed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 1 || sent[0] != "/review -a omp" {
		t.Errorf("the chat got %v, want /review -a omp", sent)
	}
}

// TestStepChain_TheFirstAgentThatDoesNotBreakWins — the chain runs in
// order, and the attempt that does not break is the step's outcome.
func TestStepChain_TheFirstAgentThatDoesNotBreakWins(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`, nil)
	op.steps = []fakeStep{brokenStep("omp broke"), okStep("claude did it")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	want := []string{"/review -a omp", "/review -a claude"}
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != http.StatusOK {
		t.Errorf("the review step ended as %d, want the outcome of the second attempt", out.Status)
	}
}

// TestStepChain_ACancelledAttemptEndsTheChain — the run is stopping:
// another agent would run work nobody asked for.
func TestStepChain_ACancelledAttemptEndsTheChain(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`, nil)
	op.steps = []fakeStep{cancelledStep("the run stopped"), okStep("never")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 1 {
		t.Errorf("the chat got %v, want one command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != outcome.StatusClientClosedRequest {
		t.Errorf("the review step ended as %d, want the cancelled one", out.Status)
	}
}

// TestStepChain_AFailedOutcomeWithoutAnErrorIsANormalResult — there
// was nothing to do: running it on another agent would be work for
// nothing.
func TestStepChain_AFailedOutcomeWithoutAnErrorIsANormalResult(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`, nil)
	op.steps = []fakeStep{emptyStep("nothing to review"), okStep("never")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 1 {
		t.Errorf("the chat got %v, want one command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != http.StatusOK {
		t.Errorf("the review step ended as %d, want the normal one", out.Status)
	}
}

// TestStepChain_EveryAttemptBreaksTheStepFails — the step fails with
// the last attempt's error.
func TestStepChain_EveryAttemptBreaksTheStepFails(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`, nil)
	op.steps = []fakeStep{brokenStep("omp broke"), brokenStep("claude broke")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 2 {
		t.Errorf("the chat got %v, want two commands", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != http.StatusInternalServerError {
		t.Errorf("the review step ended as %d, want failed", out.Status)
	}
	if out.StatusText != "claude broke" {
		t.Errorf("the step carries the words %q, want the last attempt's", out.StatusText)
	}
}

// TestStepChain_TheUseModeSwitchesTheChatAgent — the use mode sends
// the message unchanged, with the chat's agent switched for the
// attempt.
func TestStepChain_TheUseModeSwitchesTheChatAgent(t *testing.T) {
	sw := &fakeSwitcher{current: "claude"}
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: use
            names: [omp]
`, sw)
	op.steps = []fakeStep{okStep("reviewed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 1 || sent[0] != "/review" {
		t.Errorf("the chat got %v, want the message of the step", sent)
	}
	if switched := sw.switchHistory(); len(switched) != 1 || switched[0] != "omp" {
		t.Errorf("the chain switched to %v, want omp", switched)
	}
	if got := sw.agent(); got != "claude" {
		t.Errorf("the chat works with %q, want the agent it had before", got)
	}
	// The agent was switched before the command went out, and put back
	// after it came back: the chat worked with omp while the message
	// was running.
	if at := sw.switchedCommands(); len(at) != 1 || at[0] != 0 {
		t.Errorf("the agent was switched after %v commands, want before the command ran", at)
	}
	restored := sw.restored()
	if len(restored) != 1 || restored[0] != 1 {
		t.Errorf("the agent was put back after %v commands, want after the command ran", restored)
	}
}

// TestStepChain_TheUseModePutsTheChatBackWhenTheStepFails — the agent
// is put back when the attempt returns, whatever it brought back.
func TestStepChain_TheUseModePutsTheChatBackWhenTheStepFails(t *testing.T) {
	sw := &fakeSwitcher{current: "claude"}
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: use
            names: [omp]
`, sw)
	op.steps = []fakeStep{brokenStep("omp broke")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if got := sw.agent(); got != "claude" {
		t.Errorf("the chat works with %q, want the agent it had before", got)
	}
}

// TestStepChain_TheHistoryHoldsOneRunOfTheChain — the chain is one
// run of the step: the attempt the chain swallowed leaves no trace.
func TestStepChain_TheHistoryHoldsOneRunOfTheChain(t *testing.T) {
	sw := &fakeSwitcher{current: "claude"}
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: use
            names: [omp, claude]
`, sw)
	op.steps = []fakeStep{brokenStep("omp broke"), okStep("claude did it")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	runs := stepRuns(r, "review")
	if len(runs) != 1 {
		t.Fatalf("the history holds %d runs of the step, want 1: %+v", len(runs), runs)
	}
	if !strings.Contains(runs[0].Command, "[agent: ") {
		t.Errorf("the run %q does not name the agent it ran with", runs[0].Command)
	}
	if runs[0].Command != "/review [agent: claude]" {
		t.Errorf("the history records %q, want the command of the attempt the chain ended on", runs[0].Command)
	}
}

// TestStepChain_AStepThatRunsAgainStartsAtTheFirstAgent — the chain
// belongs to the step, not to one execution of it.
func TestStepChain_AStepThatRunsAgainStartsAtTheFirstAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "reviewed"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("test", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{brokenStep("omp broke"), brokenStep("claude broke"), okStep("omp did it")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	want := []string{"/review -a omp", "/review -a claude", "/review -a omp"}
	sent := op.Sent()
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}
}

// TestStepChain_AFailedChainAsksTheDecisionModel — the chain does not
// change the path of a broken step: the plan asks the decision model.
func TestStepChain_AFailedChainAsksTheDecisionModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model cannot answer")}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{brokenStep("omp broke"), brokenStep("claude broke")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if decider.Calls() == 0 {
		t.Fatal("the decision model was not asked what to do next")
	}
}

// TestStepChain_ANameThatResolvesToNothingBreaksTheAttempt — the
// agent of the attempt is known when the step runs: a name that
// resolves to nothing leaves the attempt no agent to work with.
func TestStepChain_ANameThatResolvesToNothingBreaksTheAttempt(t *testing.T) {
	g := newGate()
	sw := &fakeSwitcher{current: "claude"}
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: use
            names: ["${{ steps.gone.outputs.data.agent }}"]
`, sw, g)
	op.steps = []fakeStep{okStep("never")}

	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 0 {
		t.Errorf("the chat got %v, want no command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if !strings.Contains(out.StatusText, "resolved to nothing") {
		t.Errorf("the step carries the words %q, want them to name the empty agent", out.StatusText)
	}
}

// TestStepChain_AFailedSwitchComesBackToTheModel — the attempt could
// not run: the model reads why.
func TestStepChain_AFailedSwitchComesBackToTheModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: commit
        message: "/gtw commit"
        agents:
          - mode: use
            names: [omp]
`)

	sw := &fakeSwitcher{err: errors.New("the chat session is gone")}
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"subcommand": "commit"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, AgentSwitcher: sw})
	sw.watch(op)
	op.steps = []fakeStep{okStep("never")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw", Subcommands: []CommandInfo{{Name: "commit"}}}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	req := provider.Request(1)
	found := false
	for _, m := range req.Messages {
		if m.Role != llm.RoleTool {
			continue
		}
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "the chat session is gone") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the reason the attempt broke did not come back to the model")
	}
}

// TestStepChain_TheChainCostsOneChance — the chain is one execution of
// the step: it costs one chance of the step's budget, however many
// agents it tried.
func TestStepChain_TheChainCostsOneChance(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, claude]
`, nil)
	op.steps = []fakeStep{brokenStep("omp broke"), okStep("claude did it")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	if used := stepUsage(r, "review"); used != 1 {
		t.Errorf("the step spent %d chances, want 1", used)
	}
}

// TestStepChain_TheArgsModeNeedsACommand — the agent goes on the
// command line, so the message has to be a command. The load-time
// check sees the expression; the run sees what it became.
func TestStepChain_TheArgsModeNeedsACommand(t *testing.T) {
	g := newGate()
	svc, op := chainService(t, `title: chain
args:
  - name: cmd
    default: "review the changes"
jobs:
  main:
    steps:
      - id: review
        message: "${{ args.cmd }}"
        agents:
          - mode: args
            names: [omp]
`, nil, g)
	op.steps = []fakeStep{okStep("never")}

	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 0 {
		t.Errorf("the chat got %v, want no command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if !strings.Contains(out.StatusText, "args mode") {
		t.Errorf("the step carries the words %q, want them to name the args mode", out.StatusText)
	}
}

// TestStepChain_TheDecisionModelRunsTheChain — the chain is the
// step's agent policy: it runs wherever the step's own message runs.
func TestStepChain_TheDecisionModelRunsTheChain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: commit
        message: "/gtw commit"
        agents:
          - mode: args
            names: [omp]
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"subcommand": "commit"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{okStep("committed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw", Subcommands: []CommandInfo{{Name: "commit"}}}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	found := false
	for _, cmd := range op.Sent() {
		if cmd == "/gtw commit -a omp" {
			found = true
		}
	}
	if !found {
		t.Errorf("the chat got %v, want the chain of the step", op.Sent())
	}
}

// TestStepChain_AStepWithoutAChainIsUnchanged — a step that names no
// agent sends its message once, unchanged.
func TestStepChain_AStepWithoutAChainIsUnchanged(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
`, nil)
	op.steps = []fakeStep{okStep("reviewed")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 1 || sent[0] != "/review" {
		t.Errorf("the chat got %v, want /review", sent)
	}
}

// stepUsage returns the chances a step spent in its round.
func stepUsage(r *run, stepID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.StepUsage(r.stepIndex(stepID))
}

// stepRuns returns the runs the history holds for a step.
func stepRuns(r *run, stepID string) []wfe.ActionRecord {
	var runs []wfe.ActionRecord
	for _, rec := range runHistory(r) {
		if rec.Kind == wfe.ActionKindRun && rec.StepID == stepID {
			runs = append(runs, rec)
		}
	}
	return runs
}

// ── the handover ───────────────────────────────────────────────────

// TestStepChain_TheHandoverCarriesTheUnfinishedWork — an attempt that
// broke with work left behind hands that work over: the next agent
// finishes it instead of running the whole thing again.
func TestStepChain_TheHandoverCarriesTheUnfinishedWork(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`, nil)
	op.steps = []fakeStep{unfinishedStep("2 of 3 groups unfinished", 2), okStep("codex did it")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	want := []string{"/review -a omp", "/review --continue -a codex"}
	sent := op.Sent()
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}

	// The history keeps the command that ran: the chain is one run of
	// the step.
	runs := stepRuns(r, "review")
	if len(runs) != 1 {
		t.Fatalf("the history holds %d runs of the step, want 1: %+v", len(runs), runs)
	}
	if runs[0].Command != "/review --continue -a codex" {
		t.Errorf("the history records %q, want the handover command", runs[0].Command)
	}

	// The step's outcome is the outcome of the attempt that did not
	// break.
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != http.StatusOK {
		t.Errorf("the review step ended as %d, want the outcome of the second attempt", out.Status)
	}
}

// TestStepChain_TheHandoverFollowsWhatTheAttemptLeftBehind — the
// command of an attempt depends on the outcome of the one before it:
// an attempt that left work behind hands it over, an attempt that left
// nothing behind runs the whole thing again.
func TestStepChain_TheHandoverFollowsWhatTheAttemptLeftBehind(t *testing.T) {
	cases := []struct {
		name string
		step fakeStep
		want string
	}{
		{"the attempt reported no count", brokenStep("omp broke"), "/review -a codex"},
		{"the attempt left nothing behind", unfinishedStep("every group finished", 0), "/review -a codex"},
		{"the attempt left work behind", unfinishedStep("2 of 3 groups unfinished", 2), "/review --continue -a codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`, nil)
			op.steps = []fakeStep{tc.step, okStep("codex did it")}

			g := newGate()
			op.gate = g
			startRun(t, svc, g, Request{
				Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
			})
			waitRun(t, svc, "obs-1")

			sent := op.Sent()
			if len(sent) != 2 {
				t.Fatalf("the chat got %v, want two commands", sent)
			}
			if sent[1] != tc.want {
				t.Errorf("the chat got %v, want %q", sent, tc.want)
			}
		})
	}
}

// TestStepChain_AFailedHandoverFallsBackToAFullRun — a handover that
// the command refuses (its flags do not go together) breaks the
// attempt, and the agent after it runs the whole thing again: the
// chain lives on, the work is not carried.
func TestStepChain_AFailedHandoverFallsBackToAFullRun(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex, claude]
`, nil)
	op.steps = []fakeStep{
		unfinishedStep("2 of 3 groups unfinished", 2),
		brokenStep("the flags do not go together"),
		okStep("claude did it"),
	}

	g := newGate()
	op.gate = g
	startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	want := []string{
		"/review -a omp",
		"/review --continue -a codex",
		"/review -a claude",
	}
	sent := op.Sent()
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}
}

// TestStepChain_TheUseModeHasNoHandover — the use mode sends the
// message as it is: there is no place to put the handover in it.
func TestStepChain_TheUseModeHasNoHandover(t *testing.T) {
	sw := &fakeSwitcher{current: "claude"}
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: use
            names: [omp, codex]
`, sw)
	op.steps = []fakeStep{unfinishedStep("2 of 3 groups unfinished", 2), okStep("codex did it")}

	g := newGate()
	op.gate = g
	startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	want := []string{"/review", "/review"}
	sent := op.Sent()
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}
}

// TestStepChain_TheDecisionModelHandsTheWorkOverToo — the chain is
// the step's agent policy: the decision model runs the step's message
// through the same chain, and the handover works there too.
func TestStepChain_TheDecisionModelHandsTheWorkOverToo(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_review", map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "reviewed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{unfinishedStep("2 of 3 groups unfinished", 2), okStep("codex did it")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "review"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	want := []string{"/review -a omp", "/review --continue -a codex"}
	sent := op.Sent()
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}
}

// ── the chain swallows a broken attempt ────────────────────────────

// TestStepChain_ABrokenAttemptDoesNotPauseTheRun — the chain absorbs
// the attempt that broke: the run goes on with the next agent instead
// of waiting for the operator.
func TestStepChain_ABrokenAttemptDoesNotPauseTheRun(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`, nil)
	op.steps = []fakeStep{
		throttledStep("429 rpm exhausted (type=quota_exceeded_error param=8)", 1),
		okStep("codex did it"),
	}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	want := []string{"/review -a omp", "/review --continue -a codex"}
	sent := op.Sent()
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Errorf("the chat got %v, want %v", sent, want)
			break
		}
	}

	if st := svc.Snapshot("obs-1"); st.Paused {
		t.Errorf("the run paused: %s", st.PauseReason)
	}
	for _, text := range op.Sent() {
		if strings.HasPrefix(text, pauseMark) {
			t.Errorf("the chat got the pause announcement %q", text)
		}
	}

	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != http.StatusOK {
		t.Errorf("the review step ended as %d, want the outcome of the second attempt", out.Status)
	}
}

// TestStepChain_EveryAttemptThrottledPausesTheRun — the chain is
// exhausted and its result is a throttle: the run waits for the
// operator.
func TestStepChain_EveryAttemptThrottledPausesTheRun(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`, nil)
	op.steps = []fakeStep{
		throttledStep("429 rpm exhausted (type=quota_exceeded_error param=8)", 1),
		throttledStep("429 rpm exhausted (type=quota_exceeded_error param=8)", 1),
	}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Snapshot("obs-1")
	if !strings.Contains(st.PauseReason, "429") {
		t.Errorf("the pause reason is %q, want it to name the rate limit", st.PauseReason)
	}
	var notice string
	for _, text := range op.Sent() {
		if strings.HasPrefix(text, pauseMark) {
			notice = text
			break
		}
	}
	if notice == "" {
		t.Fatalf("the chat got no pause announcement: %v", op.Sent())
	}
	if !strings.Contains(notice, "/me resume") || !strings.Contains(notice, "/me next") {
		t.Errorf("the pause announcement %q does not name both ways on", notice)
	}
}

// TestStepChain_TheSwallowedAttemptLeavesNoTrace — the attempt the
// chain absorbed is not in the run's record: the operator reads the
// result of the chain, not the failures it swallowed.
func TestStepChain_TheSwallowedAttemptLeavesNoTrace(t *testing.T) {
	svc, op := chainService(t, `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`, nil)
	op.steps = []fakeStep{brokenStep("omp broke"), okStep("codex did it")}

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	runs := stepRuns(r, "review")
	if len(runs) != 1 {
		t.Fatalf("the history holds %d runs of the step, want 1: %+v", len(runs), runs)
	}
	if runs[0].Command != "/review -a codex" {
		t.Errorf("the history records %q, want the command of the attempt the chain ended on", runs[0].Command)
	}
	if lines := errorLines(runHistory(r), reportFilter{}); len(lines) != 0 {
		t.Errorf("the record lists the errors %v, want none", lines)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != http.StatusOK {
		t.Errorf("the review step ended as %d, want the outcome of the second attempt", out.Status)
	}
}

// TestStepChain_ARepairWhileTheChainIsInFlightCountsAgainstTheStep —
// the chain's attempt waits for an answer: the run holds no record of
// the step yet, and a repair counts against the step the run is on.
func TestStepChain_ARepairWhileTheChainIsInFlightCountsAgainstTheStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_push", map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		brokenStep("omp broke"),
		{open: true, choices: []messages.Choice{{Title: "which one?"}}},
	}
	// The chat cannot take the repair: the repair is recorded all the
	// same, against the step the run is on.
	op.failCommand = "/push"

	g := newGate()
	op.gate = g
	r := startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "push"}},
	})
	waitRun(t, svc, "obs-1")

	var repairs []wfe.ActionRecord
	for _, rec := range runHistory(r) {
		if rec.Kind == wfe.ActionKindCommand {
			repairs = append(repairs, rec)
		}
	}
	if len(repairs) != 1 {
		t.Fatalf("the history holds %d repairs, want 1: %+v", len(repairs), repairs)
	}
	if repairs[0].StepID != "review" {
		t.Errorf("the repair is recorded against %q, want the step the run is on", repairs[0].StepID)
	}
	if used := stepUsage(r, "review"); used != 1 {
		t.Errorf("the step spent %d chances, want 1", used)
	}
}

// TestStepChain_TheSwallowedAttemptIsLogged — the attempt the chain
// absorbed leaves a trace in the daemon's log: the step it belongs to
// and the command that broke.
func TestStepChain_TheSwallowedAttemptIsLogged(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", `title: chain
jobs:
  main:
    steps:
      - id: swallowed
        message: "/review"
        agents:
          - mode: args
            names: [omp, codex]
`)

	var buf bytes.Buffer
	svc, op := testService(t, dir, Config{
		Provider: &scriptedProvider{err: errors.New("the chat model must not be called")},
		Decider:  &scriptedDecider{err: errors.New("the decision model must not be called")},
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	})
	op.steps = []fakeStep{brokenStep("omp broke"), okStep("codex did it")}

	g := newGate()
	op.gate = g
	startRun(t, svc, g, Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	waitRun(t, svc, "obs-1")

	out := buf.String()
	if !strings.Contains(out, "swallowed") {
		t.Errorf("the log does not name the step:\n%s", out)
	}
	if !strings.Contains(out, "/review -a omp") {
		t.Errorf("the log does not name the command that broke:\n%s", out)
	}
}
