package me

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// brokenStep is a command that broke: it carries the error
// description the plan reads.
func brokenStep(summary string) fakeStep {
	return fakeStep{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: summary,
		Data:    map[string]string{messages.OutcomeKeyError: summary},
	}}
}

// cancelledStep is a command the run stopped.
func cancelledStep(summary string) fakeStep {
	return fakeStep{outcome: messages.Outcome{
		Status:  messages.OutcomeCancelled,
		Summary: summary,
	}}
}

// emptyStep is a command that failed without saying why: it is a
// normal result, not a breakage.
func emptyStep(summary string) fakeStep {
	return fakeStep{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: summary,
	}}
}

// fakeSwitcher switches the agent of a chat for the length of one
// attempt.
type fakeSwitcher struct {
	mu sync.Mutex
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
func chainService(t *testing.T, body string, sw *fakeSwitcher) (*Service, *fakeOperator) {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "chain", body)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
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
	if out.Status != wfe.OutcomeOK {
		t.Errorf("the review step ended as %q, want the outcome of the second attempt", out.Status)
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 1 {
		t.Errorf("the chat got %v, want one command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != wfe.OutcomeCancelled {
		t.Errorf("the review step ended as %q, want the cancelled one", out.Status)
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 1 {
		t.Errorf("the chat got %v, want one command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != wfe.OutcomeFailed {
		t.Errorf("the review step ended as %q, want the failed one", out.Status)
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 2 {
		t.Errorf("the chat got %v, want two commands", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != wfe.OutcomeFailed {
		t.Errorf("the review step ended as %q, want failed", out.Status)
	}
	if desc := out.Data[wfe.OutcomeKeyError]; desc != "claude broke" {
		t.Errorf("the step carries the error %q, want the last attempt's", desc)
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

// TestStepChain_TheHistoryHoldsEveryAttempt — every attempt is a run
// of the step, and the use mode names the agent it ran with.
func TestStepChain_TheHistoryHoldsEveryAttempt(t *testing.T) {
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	var runs []wfe.ActionRecord
	for _, rec := range runHistory(r) {
		if rec.Kind == wfe.ActionKindRun && rec.StepID == "review" {
			runs = append(runs, rec)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("the history holds %d runs of the step, want 2: %+v", len(runs), runs)
	}
	for _, rec := range runs {
		if !strings.Contains(rec.Command, "[agent: ") {
			t.Errorf("the run %q does not name the agent it ran with", rec.Command)
		}
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
`, sw)
	op.steps = []fakeStep{okStep("never")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 0 {
		t.Errorf("the chat got %v, want no command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if desc := out.Data[wfe.OutcomeKeyError]; !strings.Contains(desc, "resolved to nothing") {
		t.Errorf("the step carries the error %q, want it to name the empty agent", desc)
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

// TestStepChain_EveryAttemptCostsAChance — an attempt is real work: it
// costs a chance of the step's budget.
func TestStepChain_EveryAttemptCostsAChance(t *testing.T) {
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

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	if used := stepUsage(r, "review"); used != 2 {
		t.Errorf("the step spent %d chances, want 2", used)
	}
}

// TestStepChain_TheArgsModeNeedsACommand — the agent goes on the
// command line, so the message has to be a command. The load-time
// check sees the expression; the run sees what it became.
func TestStepChain_TheArgsModeNeedsACommand(t *testing.T) {
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
`, nil)
	op.steps = []fakeStep{okStep("never")}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "chain", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	if sent := op.Sent(); len(sent) != 0 {
		t.Errorf("the chat got %v, want no command", sent)
	}
	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if !strings.Contains(out.Data[wfe.OutcomeKeyError], "args mode") {
		t.Errorf("the step carries the error %q, want it to name the args mode", out.Data[wfe.OutcomeKeyError])
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
