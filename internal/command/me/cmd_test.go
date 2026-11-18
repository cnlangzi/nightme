package me

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// fakeController is a scripted personal agent.
type fakeController struct {
	mu sync.Mutex
	// started holds the requests the command sent.
	started []command.MeRequest
	// startErr is the error a start reports.
	startErr error
	// replyErr is the error a reply reports.
	replyErr error
	// stopErr is the error a stop reports.
	stopErr error
	// pauseErr is the error a pause reports.
	pauseErr error
	// continueErr is the error a continue reports.
	continueErr error
	// showErr is the error a show reports.
	showErr error
	// shown counts the shows.
	shown int
	// replies holds the replies the command sent.
	replies []string
	// stopped counts the stops.
	stopped int
	// paused counts the pauses.
	paused int
	// continued counts the continues.
	continued int
	// notes holds the notes the continues carried.
	notes []string
	// status is the state the controller reports.
	status command.MeStatus
}

func (c *fakeController) StartWorkflow(_ context.Context, req command.MeRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, req)
	if c.startErr != nil {
		return "", c.startErr
	}
	return "run-1", nil
}

func (c *fakeController) Reply(_ context.Context, _ string, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, text)
	return c.replyErr
}

func (c *fakeController) Pause(_ context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	return c.pauseErr
}

func (c *fakeController) Continue(_ context.Context, _ string, note string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.continued++
	c.notes = append(c.notes, note)
	return c.continueErr
}

func (c *fakeController) Show(_ context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shown++
	return c.showErr
}

func (c *fakeController) Stop(_ context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped++
	return c.stopErr
}

func (c *fakeController) Status(_ string) command.MeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// request returns the n-th start request.
func (c *fakeController) request(n int) command.MeRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started[n]
}

// newSession builds a chat session with a workspace.
func newSession(t *testing.T, chatID, cwd string) *chatsession.ChatSession {
	t.Helper()
	cs, _ := newSessionWithOutcome(t, chatID, cwd)
	return cs
}

// recordingCh captures the messages the chat session sends.
type recordingCh struct {
	mu    sync.Mutex
	sends []messages.OutboundMessage
}

func (r *recordingCh) Send(_ context.Context, m messages.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, m)
	return nil
}

// settled returns the outcome the command reported, or nil when it
// reported none.
func (r *recordingCh) settled() *messages.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.sends {
		if m.Kind == messages.OutMessageSettled {
			return m.Outcome
		}
	}
	return nil
}

// newSessionWithOutcome builds a chat session with a workspace and a
// channel that records what the session sends.
func newSessionWithOutcome(t *testing.T, chatID, cwd string) (*chatsession.ChatSession, *recordingCh) {
	t.Helper()
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate(chatID, "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	ch := &recordingCh{}
	cs.WithEmitter(ch)
	if cwd != "" {
		if err := cs.SetSelectedCwd(cwd); err != nil {
			t.Fatalf("SetSelectedCwd: %v", err)
		}
	}
	return cs, ch
}

// runMe runs /me with the given argv and returns the reply.
func runMe(t *testing.T, ctrl command.MeController, cs *chatsession.ChatSession, argv ...string) string {
	t.Helper()
	f := NewFactory(ctrl)
	work := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)
	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    cs.ID,
		MessageID: "msg-1",
		Text:      strings.Join(argv, " "),
		Args:      argv,
	})
	work.Release()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return out.Reply
}

func TestStart_MapsTheArguments(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-1", "/tmp/work")
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	runMe(t, ctrl, cs, "/me", "start", "fix-issue", "42")
	if len(ctrl.started) != 1 {
		t.Fatalf("the controller got %d requests, want 1", len(ctrl.started))
	}
	req := ctrl.request(0)
	if req.Name != "fix-issue" {
		t.Errorf("name = %q, want fix-issue", req.Name)
	}
	if len(req.Args) != 1 || req.Args[0] != "42" {
		t.Errorf("args = %v, want [42]", req.Args)
	}
	if req.ChatID != cs.ID {
		t.Errorf("observer chat = %q, want %q", req.ChatID, cs.ID)
	}
	// The chat session cleans the path it stores, so the comparison
	// is against the cleaned form.
	if want := filepath.Clean("/tmp/work"); req.Cwd != want {
		t.Errorf("cwd = %q, want %q", req.Cwd, want)
	}
}

// TestStart_CarriesTheMessageID — the run's card hangs under the
// message that started the run.
func TestStart_CarriesTheMessageID(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-1", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "start", "fix-issue")
	if len(ctrl.started) != 1 {
		t.Fatalf("the controller got %d requests, want 1", len(ctrl.started))
	}
	if req := ctrl.request(0); req.MessageID != "msg-1" {
		t.Errorf("message id = %q, want msg-1", req.MessageID)
	}
}

// TestStart_CarriesTheCommandsAndTheAgent — the run gets the commands
// it may issue and the agent of the chat that started it.
func TestStart_CarriesTheCommandsAndTheAgent(t *testing.T) {
	command.SetDeps(command.Deps{})

	ctrl := &fakeController{}
	cs := newSession(t, "me-commands", "/tmp/work")
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	runMe(t, ctrl, cs, "/me", "start", "fix-issue", "42")

	req := ctrl.request(0)
	if req.Agent != "claude" {
		t.Errorf("the agent is %q, want the agent of the chat", req.Agent)
	}
	found := false
	for _, c := range req.Commands {
		if c.Name == "me" {
			found = true
			if c.Summary == "" {
				t.Errorf("the command carries no summary: %v", c)
			}
		}
	}
	if !found {
		t.Errorf("the commands are %v, want the registered ones", req.Commands)
	}
}

// TestCommandInfos_TheHiddenSubcommandsStayHidden — a subcommand the
// chat does not show is a subcommand the run does not get.
func TestCommandInfos_TheHiddenSubcommandsStayHidden(t *testing.T) {
	infos := commandInfos([]command.Spec{
		{
			Name:    "gtw",
			Summary: "the git workflow",
			Usage:   "/gtw <subcommand>",
			Subcommands: []command.SubcommandSpec{
				{Name: "fix", Summary: "fix an issue"},
				{Name: "secret", Hidden: true},
			},
		},
	})
	if len(infos) != 1 {
		t.Fatalf("the commands are %v, want one", infos)
	}
	if infos[0].Name != "gtw" || infos[0].Summary != "the git workflow" || infos[0].Usage != "/gtw <subcommand>" {
		t.Errorf("the command is %v, want the registered one", infos[0])
	}
	if len(infos[0].Subcommands) != 1 || infos[0].Subcommands[0].Name != "fix" {
		t.Errorf("the subcommands are %v, want the visible one", infos[0].Subcommands)
	}
}

// TestStart_CarriesTheRoundCount — the flag sets the round count of
// the run and is not one of the workflow's own arguments.
func TestStart_CarriesTheRoundCount(t *testing.T) {
	cases := []struct {
		argv []string
		want int
	}{
		{[]string{"/me", "start", "fix-issue", "-i", "1"}, 1},
		{[]string{"/me", "start", "fix-issue", "--iterations", "2"}, 2},
		{[]string{"/me", "start", "-i", "3", "fix-issue"}, 3},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			ctrl := &fakeController{}
			cs := newSession(t, "me-iterations", "/tmp/work")

			runMe(t, ctrl, cs, tc.argv...)

			if len(ctrl.started) != 1 {
				t.Fatalf("the controller got %d requests, want 1", len(ctrl.started))
			}
			req := ctrl.request(0)
			if req.Iterations == nil {
				t.Fatalf("the request carries no round count, want %d", tc.want)
			}
			if *req.Iterations != tc.want {
				t.Errorf("the round count = %d, want %d", *req.Iterations, tc.want)
			}
			if len(req.Args) != 0 {
				t.Errorf("the workflow's arguments are %v, want none", req.Args)
			}
			if req.Name != "fix-issue" {
				t.Errorf("name = %q, want fix-issue", req.Name)
			}
		})
	}
}

// TestStart_KeepsTheWorkflowsOwnArguments — the tokens that are not
// flags reach the workflow unchanged.
func TestStart_KeepsTheWorkflowsOwnArguments(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-args", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "start", "fix-issue", "42", "id=7")

	req := ctrl.request(0)
	if len(req.Args) != 2 || req.Args[0] != "42" || req.Args[1] != "id=7" {
		t.Errorf("the workflow's arguments are %v, want [42 id=7]", req.Args)
	}
	if req.Iterations != nil {
		t.Errorf("the request carries a round count of %d, want none", *req.Iterations)
	}
}

// TestStart_AfterTheTerminatorEveryTokenIsTheWorkflows — a token that
// looks like a flag is the workflow's own when it follows "--".
func TestStart_AfterTheTerminatorEveryTokenIsTheWorkflows(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-terminator", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "start", "fix-issue", "--", "-i")

	req := ctrl.request(0)
	if len(req.Args) != 1 || req.Args[0] != "-i" {
		t.Errorf("the workflow's arguments are %v, want [-i]", req.Args)
	}
	if req.Iterations != nil {
		t.Errorf("the request carries a round count of %d, want none", *req.Iterations)
	}
}

// TestStart_RefusesABadCommandLine — a flag the lexer does not know, a
// flag without a value, a value that is not a whole number and a
// negative count are all refused, and no run starts.
func TestStart_RefusesABadCommandLine(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"an unknown flag", []string{"/me", "start", "fix-issue", "--iteraton", "1"}, "unknown flag"},
		{"an unknown short flag", []string{"/me", "start", "fix-issue", "-x"}, "unknown flag"},
		{"a missing value", []string{"/me", "start", "fix-issue", "-i"}, "missing value"},
		{"a value that is not a number", []string{"/me", "start", "fix-issue", "-i", "abc"}, "whole number"},
		{"a negative count", []string{"/me", "start", "fix-issue", "-i", "-1"}, "whole number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{}
			cs := newSession(t, "me-bad", "/tmp/work")

			reply := runMe(t, ctrl, cs, tc.argv...)

			if !strings.Contains(reply, tc.want) {
				t.Errorf("reply = %q, want it to mention %q", reply, tc.want)
			}
			if len(ctrl.started) != 0 {
				t.Fatal("the run was started with a bad command line")
			}
		})
	}
}

// TestStart_NamesTheRecognisedFlags — the refusal of an unknown flag
// says which flags the command knows.
func TestStart_NamesTheRecognisedFlags(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-unknown-flag", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "start", "fix-issue", "--iteraton", "1")

	if !strings.Contains(reply, "--iteraton") {
		t.Errorf("reply = %q, want it to name the unknown flag", reply)
	}
	if !strings.Contains(reply, "-i/--iterations") {
		t.Errorf("reply = %q, want it to name the recognised flags", reply)
	}
}

// TestStart_RepliesNothingWhenTheRunStarts — the run's own card
// carries the acknowledgement: one command posts one card.
func TestStart_RepliesNothingWhenTheRunStarts(t *testing.T) {
	ctrl := &fakeController{}
	cs, ch := newSessionWithOutcome(t, "me-1", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "start", "fix-issue")

	if reply != "" {
		t.Errorf("reply = %q, want none", reply)
	}
	outcome := ch.settled()
	if outcome == nil {
		t.Fatal("the command reported no outcome")
	}
	if outcome.Status != messages.OutcomeOK {
		t.Errorf("the outcome is %q, want it ok", outcome.Status)
	}
	if outcome.Summary != "started fix-issue" {
		t.Errorf("the outcome says %q, want it to name the run", outcome.Summary)
	}
	for key, want := range map[string]string{"workflow": "fix-issue", "run_id": "run-1"} {
		if outcome.Data[key] != want {
			t.Errorf("the outcome carries %q = %q, want %q", key, outcome.Data[key], want)
		}
	}
}

// TestStart_ReportsTheRoundCount — the outcome carries the count.
func TestStart_ReportsTheRoundCount(t *testing.T) {
	ctrl := &fakeController{}
	cs, ch := newSessionWithOutcome(t, "me-rounds", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "start", "fix-issue", "-i", "1")

	outcome := ch.settled()
	if outcome == nil {
		t.Fatal("the command reported no outcome")
	}
	if outcome.Data["iterations"] != "1" {
		t.Errorf("the outcome carries %q, want 1", outcome.Data["iterations"])
	}
}

// TestStart_ZeroRoundsMeansUntilTheRunEnds — a count of zero is not
// "no rounds": it is "until the run ends".
func TestStart_ZeroRoundsMeansUntilTheRunEnds(t *testing.T) {
	ctrl := &fakeController{}
	cs, ch := newSessionWithOutcome(t, "me-zero-rounds", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "start", "fix-issue", "-i", "0")

	outcome := ch.settled()
	if outcome == nil {
		t.Fatal("the command reported no outcome")
	}
	if outcome.Data["iterations"] != "0" {
		t.Errorf("the outcome carries %q, want 0", outcome.Data["iterations"])
	}
}

// TestStart_WithoutTheFlagTheCountIsAbsent — the outcome of a run
// that names no count stays as it was.
func TestStart_WithoutTheFlagTheCountIsAbsent(t *testing.T) {
	ctrl := &fakeController{}
	cs, ch := newSessionWithOutcome(t, "me-no-rounds", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "start", "fix-issue")

	outcome := ch.settled()
	if outcome == nil {
		t.Fatal("the command reported no outcome")
	}
	if _, ok := outcome.Data["iterations"]; ok {
		t.Errorf("the outcome carries a round count of %q, want none", outcome.Data["iterations"])
	}
}

func TestStart_WithoutAWorkspace(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-2", "")

	reply := runMe(t, ctrl, cs, "/me", "start", "fix-issue", "42")
	if reply != command.NoActiveCwdReply {
		t.Fatalf("reply = %q, want %q", reply, command.NoActiveCwdReply)
	}
	if len(ctrl.started) != 0 {
		t.Fatal("the run was started without a workspace")
	}
}

func TestStart_ReportsWhyTheRunDidNotStart(t *testing.T) {
	ctrl := &fakeController{startErr: errors.New("this chat already has a run in it (run-1)")}
	cs := newSession(t, "me-3", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "start", "fix-issue")
	if !strings.Contains(reply, "already has a run") {
		t.Fatalf("reply = %q, want the reason", reply)
	}
}

func TestStart_WithoutAWorkflowName(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-4", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "start")
	if !strings.Contains(reply, "Usage:") {
		t.Fatalf("reply = %q, want the usage", reply)
	}
	if len(ctrl.started) != 0 {
		t.Fatal("the run was started without a workflow")
	}
}

func TestReply_DeliversTheText(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-5", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "reply", "the", "feature", "branch")
	if !strings.Contains(reply, "delivered") {
		t.Fatalf("reply = %q, want the delivery", reply)
	}
	if len(ctrl.replies) != 1 || ctrl.replies[0] != "the feature branch" {
		t.Fatalf("the controller got %v, want the reply text", ctrl.replies)
	}
}

func TestReply_KeepsTheTextTheUserTyped(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"/me reply hello world", "hello world"},
		{"/me  reply  hello world", "hello world"},
		{"/me reply   the feature branch  ", "the feature branch"},
		{"/me reply", ""},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got := replyText(tc.text)
			if got != tc.want {
				t.Fatalf("replyText(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestPause_PausesTheRun(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-10", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "pause")
	if !strings.Contains(reply, "pauses") {
		t.Fatalf("reply = %q, want the pause", reply)
	}
	if ctrl.paused != 1 {
		t.Fatalf("the controller paused %d runs, want 1", ctrl.paused)
	}
}

func TestContinue_HandsTheControlBack(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-12", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "continue")
	if !strings.Contains(reply, "goes on") {
		t.Fatalf("reply = %q, want the continuation", reply)
	}
	if ctrl.continued != 1 {
		t.Fatalf("the controller continued %d runs, want 1", ctrl.continued)
	}
	if len(ctrl.notes) != 1 || ctrl.notes[0] != "" {
		t.Fatalf("the controller got the notes %v, want none", ctrl.notes)
	}
}

func TestContinue_CarriesTheNote(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-13", "/tmp/work")

	runMe(t, ctrl, cs, "/me", "continue", "use", "the", "other", "branch")
	if len(ctrl.notes) != 1 || ctrl.notes[0] != "use the other branch" {
		t.Fatalf("the controller got the notes %v, want the note", ctrl.notes)
	}
}

func TestShow_PutsTheStateBackInFrontOfTheOperator(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-14", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "show")
	if !strings.Contains(reply, "shown again") {
		t.Fatalf("reply = %q, want the show", reply)
	}
	if ctrl.shown != 1 {
		t.Fatalf("the controller showed %d times, want 1", ctrl.shown)
	}
}

func TestStop_StopsTheRun(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-6", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "stop")
	if !strings.Contains(reply, "stopped") {
		t.Fatalf("reply = %q, want the stop", reply)
	}
	if ctrl.stopped != 1 {
		t.Fatalf("the controller stopped %d runs, want 1", ctrl.stopped)
	}
}

// TestStop_SaysTheSummaryIsOnItsWay — the reply tells the operator
// that the summary of the session is coming.
func TestStop_SaysTheSummaryIsOnItsWay(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-8", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "stop")
	if !strings.Contains(reply, "stopped") {
		t.Fatalf("reply = %q, want the stop", reply)
	}
	if !strings.Contains(reply, "summary") {
		t.Fatalf("reply = %q, want the summary", reply)
	}
}

func TestStatus_ReportsWhereTheRunStands(t *testing.T) {
	ctrl := &fakeController{status: command.MeStatus{
		Phase:            "running",
		RunID:            "run-1",
		Step:             "/gtw commit",
		Execution:        "running",
		Actions:          2,
		System1Decisions: 3,
		System2Decisions: 1,
		System1Tokens:    120,
		System2Tokens:    800,
	}}
	cs := newSession(t, "me-7", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "status")
	for _, want := range []string{"running", "run-1", "/gtw commit", "2 taken", "system-1: 3 decisions"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want it to mention %q", reply, want)
		}
	}
}

func TestStatus_ReportsWhyTheAgentIsOff(t *testing.T) {
	ctrl := &fakeController{status: command.MeStatus{
		Phase: "idle",
		Error: "neither me.llm nor me.decider is configured",
	}}
	cs := newSession(t, "me-8", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "status")
	if !strings.Contains(reply, "neither me.llm nor me.decider is configured") {
		t.Fatalf("reply = %q, want the reason", reply)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	ctrl := &fakeController{}
	cs := newSession(t, "me-9", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "dance")
	if !strings.Contains(reply, "Unknown /me subcommand") {
		t.Fatalf("reply = %q, want the unknown subcommand", reply)
	}
}

func TestNoController(t *testing.T) {
	cs := newSession(t, "me-10", "/tmp/work")

	reply := runMe(t, nil, cs, "/me", "status")
	if !strings.Contains(reply, "not available") {
		t.Fatalf("reply = %q, want the missing agent", reply)
	}
}
