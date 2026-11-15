package me

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
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
	if got := toolName(long); got != "ntm_"+strings.Repeat("a", 64) {
		t.Errorf("toolName = %q, want the name cut to 64 characters", got)
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

	if !op.Has("/gtw commit -m") {
		t.Fatalf("the chat got %v, want the command of the tool", op.Sent())
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

	// The step keeps the verdict it had: the card still shows it as
	// the one that did not finish.
	if !op.Has("\u26a0\ufe0f /gtw commit") {
		t.Errorf("the step changed its verdict: %v", op.Lines())
	}
	if op.Has("✅ /gtw commit") {
		t.Errorf("the command took over the step's verdict: %v", op.Lines())
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

// ── the step tool ─────────────────────────────────────────────────

// TestRunStepTool_OnlyTheReadyStepRuns — a step further down the line
// is refused: the steps of a job run in order.
func TestRunStepTool_OnlyTheReadyStepRuns(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two
jobs:
  build:
    steps:
      - id: commit
        message: /gtw commit
      - id: tag
        message: /gtw tag
  publish:
    needs: [build]
    steps:
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "push"})},
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"step_id": "nope"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The step further down the line never ran.
	for _, cmd := range op.Sent() {
		if cmd == "/gtw push" {
			t.Fatalf("the run skipped a step: %v", op.Sent())
		}
	}
	req := provider.Request(1)
	if !messagesContain(req, "not ready") {
		t.Errorf("the refusal did not come back to the model: %v", req.Messages)
	}
	req = provider.Request(2)
	if !messagesContain(req, "not part of this workflow") {
		t.Errorf("the unknown step did not come back to the model: %v", req.Messages)
	}
}

// TestRunStepTool_AStepThatRanMayRunAgain — a step that already ran
// this round may run again: a transient failure is worth a retry.
func TestRunStepTool_AStepThatRanMayRunAgain(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "commit"})},
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

// TestRunStepTool_AShellAndAWaitStepRun — a step is a step, whatever
// its kind is.
func TestRunStepTool_AShellAndAWaitStepRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "mixed", `title: mixed
jobs:
  main:
    steps:
      - id: build
        shell: make build
      - id: rest
        wait: 1ms
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "build"})},
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"step_id": "rest"})},
		{Message: toolCallMsg("c3", toolFinish, map[string]any{"status": "done", "summary": "built"})},
	}}
	shell := &recordingShell{}
	svc, _ := testService(t, dir, Config{Provider: provider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "mixed", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !shell.Called() {
		t.Error("the shell step never ran")
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
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"step_id": "commit"})},
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

// recordingShell remembers the shell commands it ran.
type recordingShell struct {
	mu    sync.Mutex
	calls int
}

// RunShell implements ShellRunner.
func (s *recordingShell) RunShell(context.Context, wfe.ShellSpec) (*wfe.ShellResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return &wfe.ShellResult{ExitCode: 0}, nil
}

// Called reports whether the shell ran.
func (s *recordingShell) Called() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls > 0
}
