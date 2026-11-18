package me

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// ── the thinking of the owner ─────────────────────────────────────

// fakeOpener opens the sessions that think for the owner.
type fakeOpener struct {
	mu sync.Mutex
	// opened holds the names of the agents that were opened.
	opened []string
	// workspaces holds the directories the sessions opened in.
	workspaces []string
	// sessions holds every session the opener handed out.
	sessions []*fakeBrain
	// failFirst makes the first answer of every session fail.
	failFirst bool
	// reply is what a session answers.
	reply string
}

// Open implements AgentOpener.
func (o *fakeOpener) Open(_ context.Context, agent, workspace string) (AgentSession, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened = append(o.opened, agent)
	o.workspaces = append(o.workspaces, workspace)
	b := &fakeBrain{opener: o, reply: o.reply}
	if o.failFirst && len(o.sessions) == 0 {
		b.failNext = true
	}
	o.sessions = append(o.sessions, b)
	return b, nil
}

// Opens returns the number of sessions that were opened.
func (o *fakeOpener) Opens() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sessions)
}

// Openings returns the names of the agents that were opened.
func (o *fakeOpener) Openings() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.opened...)
}

// Workspaces returns the directories the sessions opened in.
func (o *fakeOpener) Workspaces() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.workspaces...)
}

// Closed returns the number of sessions that were closed.
func (o *fakeOpener) Closed() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, s := range o.sessions {
		if s.closed {
			n++
		}
	}
	return n
}

// fakeBrain is one session of the thinking.
type fakeBrain struct {
	opener   *fakeOpener
	reply    string
	closed   bool
	failNext bool
	asked    []string
}

// Ask implements AgentSession.
func (b *fakeBrain) Ask(_ context.Context, prompt string) (AgentReply, error) {
	b.opener.mu.Lock()
	defer b.opener.mu.Unlock()
	b.asked = append(b.asked, prompt)
	if b.failNext {
		b.failNext = false
		return AgentReply{}, errors.New("the session is stale")
	}
	return AgentReply{Text: b.reply, Model: "test-model"}, nil
}

// Close implements AgentSession.
func (b *fakeBrain) Close() error {
	b.opener.mu.Lock()
	defer b.opener.mu.Unlock()
	b.closed = true
	return nil
}

// Asked returns the questions the session was asked.
func (b *fakeBrain) Asked() []string {
	b.opener.mu.Lock()
	defer b.opener.mu.Unlock()
	return append([]string(nil), b.asked...)
}

// ── the command tools ─────────────────────────────────────────────

// testRunWith builds a run of a workflow with the given commands.
func testRunWith(t *testing.T, svc *Service, name string, commands []CommandInfo, agent string) *run {
	t.Helper()
	r := testRun(t, svc, name)
	r.registry = commands
	r.commands = usableCommands(commands)
	r.policy.Commands = commandNames(r.commands)
	r.agentName = svc.agentName(agent)
	return r
}

// toolNamed returns the tool with the given name.
func toolNamed(tools []llm.Tool, name string) (llm.Tool, bool) {
	for _, tool := range tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return llm.Tool{}, false
}

// TestCommandTools_TheExcludedCommandsAreNoTools — a command that
// ends, switches or resets the chat is not a tool: it would take the
// run with it.
func TestCommandTools_TheExcludedCommandsAreNoTools(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})

	r := testRunWith(t, svc, "one", []CommandInfo{
		{Name: "gtw", Summary: "the git workflow", Subcommands: []CommandInfo{
			{Name: "fix"}, {Name: "config"}, {Name: "plan"}, {Name: "new"},
		}},
		{Name: "me"},
		{Name: "close"},
		{Name: "review"},
	}, "")

	tools := svc.toolCatalogue(r)
	if _, ok := toolNamed(tools, "ntm_gtw"); !ok {
		t.Fatalf("the tools do not carry the gtw command")
	}
	for _, name := range []string{"ntm_me", "ntm_close"} {
		if _, ok := toolNamed(tools, name); ok {
			t.Errorf("the excluded command %q has a tool", name)
		}
	}
	if _, ok := toolNamed(tools, "ntm_review"); !ok {
		t.Errorf("the tools do not carry the review command")
	}
}

// TestCommandTools_TwoCommandsAreNotOneTool — two commands whose
// names clean to the same tool name would give the model two tools of
// the same name.
func TestCommandTools_TwoCommandsAreNotOneTool(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})

	r := testRunWith(t, svc, "one", []CommandInfo{
		{Name: "gtw"},
		{Name: "g tw"},
	}, "")

	names := map[string]bool{}
	for _, tool := range svc.toolCatalogue(r) {
		if names[tool.Name] {
			t.Errorf("the tool %q is registered twice", tool.Name)
		}
		names[tool.Name] = true
	}
	if !names["ntm_gtw"] {
		t.Errorf("the tools are %v, want ntm_gtw", names)
	}
}

// TestCommandTools_TheSubcommandsAreAnEnum — a command with
// subcommands names them, and the excluded ones are left out.
func TestCommandTools_TheSubcommandsAreAnEnum(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})

	r := testRunWith(t, svc, "one", []CommandInfo{
		{Name: "gtw", Summary: "the git workflow", Usage: "/gtw <subcommand>", Subcommands: []CommandInfo{
			{Name: "fix", Summary: "fix an issue"}, {Name: "config"}, {Name: "plan"}, {Name: "new"},
		}},
	}, "")

	tool, ok := toolNamed(svc.toolCatalogue(r), "ntm_gtw")
	if !ok {
		t.Fatal("the gtw command has no tool")
	}
	if !strings.Contains(tool.Description, "the git workflow") {
		t.Errorf("the description does not carry the summary: %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "/gtw <subcommand>") {
		t.Errorf("the description does not carry the usage: %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "fix: fix an issue") {
		t.Errorf("the description does not carry the subcommands: %q", tool.Description)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	sub, ok := schema.Properties["subcommand"]
	if !ok {
		t.Fatalf("the schema has no subcommand: %s", tool.Schema)
	}
	if len(sub.Enum) != 1 || sub.Enum[0] != "fix" {
		t.Errorf("the subcommand enum is %v, want [fix]", sub.Enum)
	}
	if _, ok := schema.Properties["args"]; !ok {
		t.Errorf("the schema has no args: %s", tool.Schema)
	}
}

// TestCommandTools_TheNameIsCleaned — a tool name carries the letters,
// the digits, the dashes and the underscores of the command's name.
func TestCommandTools_TheNameIsCleaned(t *testing.T) {
	if got := toolName("gtw"); got != "ntm_gtw" {
		t.Errorf("toolName = %q, want ntm_gtw", got)
	}
	if got := toolName("we ird!name"); got != "ntm_weirdname" {
		t.Errorf("toolName = %q, want ntm_weirdname", got)
	}
	if got := toolName("!!!"); got != "" {
		t.Errorf("toolName = %q, want an empty name", got)
	}
	long := strings.Repeat("a", 80)
	if got := toolName(long); len(got) != 64 {
		t.Errorf("toolName is %d characters long, want 64", len(got))
	}
	if got := toolName(long); got != "ntm_"+strings.Repeat("a", 60) {
		t.Errorf("toolName = %q, want the whole name cut to 64 characters", got)
	}
}

// TestCommandTool_TheCommandRunsAsTheOwner — the command goes to the
// chat as a message of the run.
func TestCommandTool_TheCommandRunsAsTheOwner(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"subcommand": "commit", "args": "-m"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw", Subcommands: []CommandInfo{{Name: "commit"}}}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var found bool
	for _, m := range op.EmittedOf(messages.OutToolStart) {
		if m.Tool != nil && strings.Contains(m.Tool.Args, "gtw commit -m") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the run published no call of the command: %v", op.EmittedOf(messages.OutToolStart))
	}
}

// TestCommandTool_TheStepKeepsItsVerdict — a command that belongs to
// no step is a repair: it counts against the step, but the step's own
// verdict is the one the plan reads.
func TestCommandTool_TheStepKeepsItsVerdict(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"subcommand": "sync"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("synced")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw", Subcommands: []CommandInfo{{Name: "sync"}}}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The step keeps the verdict it had: the checklist still shows it
	// as the one that did not finish.
	items := op.Checklist(op.Cards()[1].ID)
	if len(items) == 0 {
		t.Fatalf("the card of the round has no checklist")
	}
	if items[0].Status == agent.TaskCompleted {
		t.Errorf("the command took over the step's verdict: %+v", items)
	}
}

// TestCommandTool_AWaitingCommandLeavesTheStepAlone — a command that
// is still running changes nothing: the step is not marked as running.
func TestCommandTool_AWaitingCommandLeavesTheStepAlone(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"subcommand": "sync"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), {open: true}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw", Subcommands: []CommandInfo{{Name: "sync"}}}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw sync")

	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Steps[0].Pending {
		t.Error("the step is marked as running although the command belongs to no step")
	}
	if r.state.Steps[0].Outcome.Status != wfe.OutcomeIncomplete {
		t.Errorf("the step ended as %q, want the verdict it had", r.state.Steps[0].Outcome.Status)
	}
}

// ── the resume and next tools ─────────────────────────────────────

// TestTheTools_TheCatalogueHasNoRunStep — the agent names no step: it
// repairs a gap and says which way to go.
func TestTheTools_TheCatalogueHasNoRunStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	tools := svc.toolCatalogue(r)
	for _, name := range []string{toolResume, toolNext} {
		if _, ok := toolNamed(tools, name); !ok {
			t.Errorf("the tools do not carry %q", name)
		}
	}
	if _, ok := toolNamed(tools, toolRunStep); ok {
		t.Error("the tools still carry the tool that names a step")
	}
}

// TestResumeTool_TheStepTheJudgmentIsAboutRunsAgain — the agent names
// no step: the run knows the one the judgment is about.
func TestResumeTool_TheStepTheJudgmentIsAboutRunsAgain(t *testing.T) {
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
	svc, op := testService(t, dir, Config{})
	r := testRun(t, svc, "two")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgment(`the previous step "commit" ended as "failed"`)
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
		Data:    map[string]string{"error": "the commit failed"},
	})

	res, err := svc.toolResume(context.Background(), r, pendingCall{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Status != string(wfe.OutcomeOK) {
		t.Errorf("the verdict is %q, want the one of the step", res.Status)
	}
	// The step the judgment was about ran again.
	ran := false
	for _, cmd := range op.Sent() {
		switch cmd {
		case "/gtw commit":
			ran = true
		case "/gtw push":
			t.Fatalf("the run went on to the next step: %v", op.Sent())
		}
	}
	if !ran {
		t.Fatalf("the chat got %v, want the step that broke", op.Sent())
	}
}

// TestResumeTool_AStepThatRanMayRunAgain — a step that already ran
// this round may run again: a transient failure is worth a retry.
func TestResumeTool_AStepThatRanMayRunAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
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

	count := 0
	for _, cmd := range op.Sent() {
		if cmd == "/gtw commit" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("the step ran %d times, want twice", count)
	}
}

// TestResumeTool_AShellStepRunsAgain — a step is a step, whatever its
// kind is: the shell step runs again when the agent resumes it.
func TestResumeTool_AShellStepRunsAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "mixed", `title: mixed
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	shell := &fakeShell{}
	svc, _ := testService(t, dir, Config{Shell: shell})
	r := testRun(t, svc, "mixed")

	build, ok := r.stepByID("build")
	if !ok {
		t.Fatal("the workflow has no build step")
	}
	r.setJudgmentStep("build")
	r.recordStep("main", build, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the build failed",
		Data:    map[string]string{"error": "the build failed"},
	})

	if _, err := svc.toolResume(context.Background(), r, pendingCall{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(shell.calls) != 1 {
		t.Fatalf("the shell ran %d times, want once", len(shell.calls))
	}
}

// TestResumeTool_TheBudgetOfTheStepIsChecked — a step that spent its
// budget does not run again: the agent repairs differently or asks.
func TestResumeTool_TheBudgetOfTheStepIsChecked(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{MaxJudgments: 1})
	r := testRun(t, svc, "one")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
		Data:    map[string]string{"error": "the commit failed"},
	})
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
	})

	_, err := svc.toolResume(context.Background(), r, pendingCall{})
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "limit is 1") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestNextTool_TheStepIsRecordedAsSkipped — the agent skips the step:
// the engine continues with the workflow.
func TestNextTool_TheStepIsRecordedAsSkipped(t *testing.T) {
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
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "two")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgment(`the previous step "commit" ended as "failed"`)
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
		Data:    map[string]string{"error": "the commit failed", "reason": "conflict"},
	})

	res, err := svc.toolNext(context.Background(), r, pendingCall{})
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if res.Status != string(wfe.OutcomeNoop) {
		t.Errorf("the verdict is %q, want the one of the skipped step", res.Status)
	}

	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	out := st.Outcome
	if out.Status != wfe.OutcomeNoop {
		t.Errorf("the step ended as %q, want the skipped one", out.Status)
	}
	if out.Summary != agentSkippedSummary {
		t.Errorf("the summary is %q, want %q", out.Summary, agentSkippedSummary)
	}
	if out.Data["reason"] != "conflict" {
		t.Errorf("the facts are %v, want the ones the step produced", out.Data)
	}
	if _, ok := out.Data[wfe.OutcomeKeyError]; ok {
		t.Errorf("the error is still on the step: %v", out.Data)
	}
	if st.Skipped {
		t.Error("the step is marked as never run: it did run")
	}

	// The engine goes on with the workflow.
	decision, err := wfe.Decide(r.state, r.workflow)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Kind != wfe.DecisionRun {
		t.Fatalf("the engine decided %v, want the next step to run", decision.Kind)
	}
	if decision.Step.ID != "push" {
		t.Errorf("the engine runs %q, want push", decision.Step.ID)
	}
}

// TestNextTool_ARunThatIsNotOnAStepIsRefused — there is no step to
// declare done: the tool says so.
func TestNextTool_ARunThatIsNotOnAStepIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	_, err := svc.toolNext(context.Background(), r, pendingCall{})
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "has not run") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestNextTool_AStepThatIsStillRunningIsRefused — the work is in
// flight: the step is not done yet.
func TestNextTool_AStepThatIsStillRunningIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordPending("main", commit)

	_, err := svc.toolNext(context.Background(), r, pendingCall{})
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	st, ok := r.stepState("commit")
	if !ok {
		t.Fatal("the commit step has no state")
	}
	if !st.Pending {
		t.Error("the step is no longer running although the work is in flight")
	}
}

// TestNextTool_AStepThatSucceededIsRefused — a step that did its job
// has nothing to skip.
func TestNextTool_AStepThatSucceededIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{Status: wfe.OutcomeOK, Summary: "committed"})

	_, err := svc.toolNext(context.Background(), r, pendingCall{})
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "nothing to skip") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestResumeTool_AStepThatSucceededIsRefused — a step that did its job
// did not break: there is nothing to repair.
func TestResumeTool_AStepThatSucceededIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{Status: wfe.OutcomeOK, Summary: "committed"})

	_, err := svc.toolResume(context.Background(), r, pendingCall{})
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
	if !strings.Contains(err.Error(), "did not break") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if len(op.Sent()) != 0 {
		t.Errorf("the step ran again: %v", op.Sent())
	}
}

// TestNextTool_TheRunGoesOnWithTheWorkflow — the agent skips the step:
// the engine runs the step after it.
func TestNextTool_TheRunGoesOnWithTheWorkflow(t *testing.T) {
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
		{Message: toolCallMsg("c1", toolNext, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the commit failed",
			Data:    map[string]string{"error": "the commit failed"},
		}},
		okStep("pushed"),
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The engine went on with the workflow: the step after the one the
	// agent skipped ran.
	var pushed bool
	for _, cmd := range op.Sent() {
		if cmd == "/gtw push" {
			pushed = true
		}
	}
	if !pushed {
		t.Errorf("the chat got %v, want the step after the one the agent skipped", op.Sent())
	}
}

// TestNextTool_TheHistoryAgreesWithTheRunState — the step is skipped
// in the run state and in the history: the run can end as done, and
// the report does not contradict itself.
func TestNextTool_TheHistoryAgreesWithTheRunState(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	commit, ok := r.stepByID("commit")
	if !ok {
		t.Fatal("the workflow has no commit step")
	}
	r.setJudgment(`the previous step "commit" ended as "failed"`)
	r.setJudgmentStep("commit")
	r.recordStep("main", commit, wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
		Data:    map[string]string{"error": "the commit failed"},
	})
	r.record(wfe.ActionKindRun, "/gtw commit", "commit", wfe.Outcome{
		Status:  wfe.OutcomeFailed,
		Summary: "the commit failed",
		Data:    map[string]string{"error": "the commit failed"},
	})

	if _, err := svc.toolNext(context.Background(), r, pendingCall{}); err != nil {
		t.Fatalf("next: %v", err)
	}

	// The policy reads the last run of the step: it ended noop, so the
	// run may end as done.
	if err := r.checkFinish("done"); err != nil {
		t.Errorf("the run may not end as done: %v", err)
	}
	// The report shows the skipped step, not the failed run.
	report := svc.renderSituation(context.Background(), r)
	if strings.Contains(report, "last outcome") {
		t.Errorf("the report carries a verdict the step list already has:\n%s", report)
	}
	if !strings.Contains(report, agentSkippedSummary) {
		t.Errorf("the report does not carry the step the agent skipped:\n%s", report)
	}
}

// ── the guards ────────────────────────────────────────────────────

// TestTheGuards_ACommandInAFreeFormTextIsRefused — a command in an
// instruction would run as a turn of the chat, not as a command of the
// run: it is refused before anything is sent.
func TestTheGuards_ACommandInAFreeFormTextIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: build
        message: /gtw build
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolInstruct, map[string]any{"text": "/gtw commit"})},
		{Message: toolCallMsg("c2", toolAgent, map[string]any{"task": "/gtw commit please"})},
		{Message: toolCallMsg("c3", toolInstruct, map[string]any{"text": "/close the chat"})},
		{Message: toolCallMsg("c4", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, AgentOpener: &fakeOpener{}})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw"}, {Name: "close"}},
		Agent:    "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	for _, cmd := range op.Sent() {
		if strings.HasPrefix(cmd, "/gtw commit") || strings.HasPrefix(cmd, "/close") {
			t.Fatalf("the guarded text was sent: %v", op.Sent())
		}
	}
	req := provider.Request(1)
	if !messagesContain(req, "ntm_gtw") {
		t.Errorf("the refusal does not point at the tool: %v", req.Messages)
	}
}

// ── the thinking of the owner ─────────────────────────────────────

// TestTheBrainTool_ItIsNotRegisteredWithoutAnAgent — a run without an
// agent has nobody to think for it.
func TestTheBrainTool_ItIsNotRegisteredWithoutAnAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{AgentOpener: &fakeOpener{}})

	r := testRun(t, svc, "one")
	if _, ok := toolNamed(svc.toolCatalogue(r), toolAgent); ok {
		t.Error("the agent tool is registered although there is no agent")
	}
	r.agentName = "claude"
	if _, ok := toolNamed(svc.toolCatalogue(r), toolAgent); !ok {
		t.Error("the agent tool is not registered although there is an agent")
	}

	// A daemon that cannot open a session has nobody to ask either.
	plain, _ := testService(t, dir, Config{})
	if _, ok := toolNamed(plain.toolCatalogue(r), toolAgent); ok {
		t.Error("the agent tool is registered although there is no way to ask")
	}
}

// TestTheBrainTool_TheAgentNameIsResolved — the daemon names an agent,
// and the chat's agent is the fallback.
func TestTheBrainTool_TheAgentNameIsResolved(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{Agent: "daemon"})

	if got := svc.agentName("chat"); got != "daemon" {
		t.Errorf("the agent is %q, want the one the daemon names", got)
	}
	svc2, _ := testService(t, dir, Config{})
	if got := svc2.agentName("chat"); got != "chat" {
		t.Errorf("the agent is %q, want the one of the chat", got)
	}
	if got := svc2.agentName(""); got != "" {
		t.Errorf("the agent is %q, want none", got)
	}
}

// TestTheBrainTool_TheFastPathDoesNotOpenTheSession — a run that
// needs no thinking opens no session.
func TestTheBrainTool_TheFastPathDoesNotOpenTheSession(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	opener := &fakeOpener{}
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider, AgentOpener: opener})
	op.steps = []fakeStep{okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if opener.Opens() != 0 {
		t.Errorf("the session was opened %d times, want none", opener.Opens())
	}
}

// TestTheBrainTool_TheSessionIsOpenedOnceAndReused — the session
// belongs to the round: the same round in the same directory reuses
// it.
func TestTheBrainTool_TheSessionIsOpenedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	opener := &fakeOpener{reply: "the commit failed because the tree is dirty"}
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAgent, map[string]any{"task": "why did it fail?"})},
		{Message: toolCallMsg("c2", toolAgent, map[string]any{"task": "and now?"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, AgentOpener: opener})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if opener.Opens() != 1 {
		t.Fatalf("the session was opened %d times, want once", opener.Opens())
	}
	if opener.Openings()[0] != "claude" {
		t.Errorf("the session opened %q, want the chat's agent", opener.Openings()[0])
	}
	if opener.Workspaces()[0] != "/tmp/work" {
		t.Errorf("the session opened in %q, want the chat's directory", opener.Workspaces()[0])
	}
	// The opening turn says what the session is for; the turns after
	// it carry the question alone.
	session := opener.sessions[0]
	asked := session.Asked()
	if len(asked) != 2 {
		t.Fatalf("the session was asked %d times, want twice", len(asked))
	}
	if !strings.Contains(asked[0], "You are the thinking of the owner of this work") {
		t.Errorf("the opening turn is %q, want the role and the objective", asked[0])
	}
	if !strings.Contains(asked[0], "why did it fail?") {
		t.Errorf("the opening turn does not carry the question: %q", asked[0])
	}
	if asked[1] != "and now?" {
		t.Errorf("the second turn is %q, want the question alone", asked[1])
	}
}

// TestTheBrainTool_TheOpeningTurnAnswersTheStepThatFailed — the
// session is asked about the step that broke: it does not go looking
// for work.
func TestTheBrainTool_TheOpeningTurnAnswersTheStepThatFailed(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	opener := &fakeOpener{reply: "the cause"}
	svc, _ := testService(t, dir, Config{AgentOpener: opener})
	r := testRunWith(t, svc, "one", nil, "claude")

	if _, err := svc.askBrain(context.Background(), r, "why did the commit fail?"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	asked := opener.sessions[0].Asked()
	if len(asked) == 0 {
		t.Fatal("the session was not asked")
	}
	if strings.Contains(asked[0], "issue tracker") {
		t.Errorf("the opening turn sends the session to the issue tracker: %q", asked[0])
	}
	for _, want := range []string{"the step that failed", "do not look for new work"} {
		if !strings.Contains(asked[0], want) {
			t.Errorf("the opening turn does not carry %q: %q", want, asked[0])
		}
	}
}

// TestTheBrainTool_TheSessionFollowsTheDirectory — a run that moves
// to another directory opens another session.
func TestTheBrainTool_TheSessionFollowsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	opener := &fakeOpener{reply: "the cause"}
	svc, _ := testService(t, dir, Config{AgentOpener: opener})
	r := testRunWith(t, svc, "one", nil, "claude")

	if _, err := svc.askBrain(context.Background(), r, "why?"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// The chat moved to another directory: the session follows it.
	svc.cfg.Cwd = &fixedCwd{dir: "/tmp/other"}
	if _, err := svc.askBrain(context.Background(), r, "why?"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if opener.Opens() != 2 {
		t.Errorf("the session was opened %d times, want a new one for the new directory", opener.Opens())
	}
	if opener.Closed() != 1 {
		t.Errorf("%d sessions were closed, want the old one", opener.Closed())
	}
}

// TestTheBrainTool_AFailingAnswerIsTriedOnceMore — a stale session is
// closed and opened again.
func TestTheBrainTool_AFailingAnswerIsTriedOnceMore(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	opener := &fakeOpener{failFirst: true, reply: "the cause"}
	svc, _ := testService(t, dir, Config{AgentOpener: opener})
	r := testRunWith(t, svc, "one", nil, "claude")

	reply, err := svc.askBrain(context.Background(), r, "why?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Text != "the cause" {
		t.Errorf("the answer is %q, want the second attempt's", reply.Text)
	}
	if opener.Opens() != 2 {
		t.Errorf("the session was opened %d times, want a new one after the failure", opener.Opens())
	}
}

// TestTheBrainTool_TheSessionEndsWithTheRun — the session belongs to
// the run: it goes with it.
func TestTheBrainTool_TheSessionEndsWithTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	opener := &fakeOpener{reply: "the cause"}
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAgent, map[string]any{"task": "why?"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, AgentOpener: opener})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if opener.Closed() != 1 {
		t.Errorf("%d sessions were closed, want the one of the run", opener.Closed())
	}
}

// TestTheBrainTool_ANewRoundOpensANewSession — a session that belongs
// to a round does not carry into the next one.
func TestTheBrainTool_ANewRoundOpensANewSession(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "loop", `title: loop
jobs:
  main:
    iterations: 2
    steps:
      - id: commit
        message: /gtw commit
`)

	opener := &fakeOpener{reply: "the cause"}
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAgent, map[string]any{"task": "why?"})},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
		{Message: toolCallMsg("c3", toolAgent, map[string]any{"task": "why again?"})},
		{Message: toolCallMsg("c4", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, AgentOpener: opener})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed"), pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "loop", ChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if opener.Opens() < 2 {
		t.Errorf("the session was opened %d times, want a new one for the new round", opener.Opens())
	}
}

// fixedCwd reports the same directory for every chat.
type fixedCwd struct {
	dir string
}

// Cwd implements CwdProvider.
func (c *fixedCwd) Cwd(string) string { return c.dir }

// ── the round and the wait ─────────────────────────────────────────

// TestEndRoundTool_TheRoundEndsAndTheFinalStepRuns — the agent ends
// the round of the job the run is on: the steps that did not run are
// skipped, the final step runs in their place and the job goes on.
func TestEndRoundTool_TheRoundEndsAndTheFinalStepRuns(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix
      - id: commit
        message: /gtw commit
      - id: cleanup
        message: /gtw close
        final: true
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolEndRound, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The first step did not end ok: the run reaches a judgment
	// point and the agent takes over. The final step runs after the
	// agent ended the round.
	op.steps = []fakeStep{pendingStep("nothing to fix"), okStep("closed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	var finalRan bool
	for _, cmd := range sent {
		switch cmd {
		case "/gtw close":
			finalRan = true
		case "/gtw commit":
			t.Fatalf("the step the round cut short ran: %v", sent)
		}
	}
	if !finalRan {
		t.Fatalf("the final step did not run: %v", sent)
	}
}

// TestEndRoundTool_ItIsAToolOfTheRun — the agent ends the round with
// a tool, and the tool is one the model knows.
func TestEndRoundTool_ItIsAToolOfTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	tool, ok := toolNamed(svc.toolCatalogue(r), toolEndRound)
	if !ok {
		t.Fatal("the run has no end_round tool")
	}
	if tool.Description == "" {
		t.Error("the end_round tool has no description")
	}
}

// TestWaitTool_TheModelWaitsAndLooksAgain — the wait blocks the
// agent's turn and ends by itself.
func TestWaitTool_TheModelWaitsAndLooksAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolWait, map[string]any{"duration": "10ms"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "gave up"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The step did not end ok: the run reaches a judgment point and
	// the agent takes over.
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() < 2 {
		t.Fatalf("the model was called %d times, want the wait to hand the turn back", provider.Calls())
	}
	req := provider.Request(1)
	if !messagesContain(req, "waited 10ms") {
		t.Errorf("the model did not learn that the wait is over: %v", req.Messages)
	}
}

// TestWaitTool_TheRunIsStoppedDuringTheWait — the wait belongs to the
// run: stopping the run ends it at once.
func TestWaitTool_TheRunIsStoppedDuringTheWait(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	go func() {
		time.Sleep(20 * time.Millisecond)
		r.finish(PhaseStopped, "")
	}()
	_, err := svc.toolWait(context.Background(), r, pendingCall{
		Name: toolWait,
		Args: json.RawMessage(`{"duration":"5m"}`),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the run's cancellation", err)
	}
}

// TestWaitTool_ABadDurationIsRefused — the model gets the error and
// tries again.
func TestWaitTool_ABadDurationIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	_, err := svc.toolWait(context.Background(), r, pendingCall{
		Name: toolWait,
		Args: json.RawMessage(`{"duration":"soon"}`),
	})
	if err == nil || !isToolError(err) {
		t.Fatalf("err = %v, want a tool error the model can correct", err)
	}
}

// TestBrainIteration_ASkippedStepDoesNotMoveTheRun — the run is on
// the job of the last step that ran. A step that was skipped does not
// count: the chain continues past it, so the job it belongs to is not
// the one a judgment belongs to.
func TestBrainIteration_ASkippedStepDoesNotMoveTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix
  other:
    needs: [main]
    steps:
      - id: skipped
        message: /gtw skipped
`)
	svc, _ := testService(t, dir, Config{})
	r := testRun(t, svc, "one")

	fix, ok := r.stepByID("fix")
	if !ok {
		t.Fatal("the workflow has no fix step")
	}
	r.recordStep("main", fix, wfe.Outcome{Status: wfe.OutcomeFailed, Summary: "nothing to fix"})

	skipped, ok := r.stepByID("skipped")
	if !ok {
		t.Fatal("the workflow has no skipped step")
	}
	r.mu.Lock()
	wfe.RecordStep(r.state, "other", skipped, wfe.Outcome{Status: wfe.OutcomeNoop}, true)
	r.mu.Unlock()

	job, _ := r.brainIteration()
	if job != "main" {
		t.Errorf("the run is on job %q, want main", job)
	}
}
