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
	// replies holds the replies the command sent.
	replies []string
	// stopped counts the stops.
	stopped int
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
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate(chatID, "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if cwd != "" {
		if err := cs.SetSelectedCwd(cwd); err != nil {
			t.Fatalf("SetSelectedCwd: %v", err)
		}
	}
	return cs
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

	reply := runMe(t, ctrl, cs, "/me", "start", "fix-issue", "42")
	if !strings.Contains(reply, "started") {
		t.Fatalf("reply = %q, want the run to start", reply)
	}
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
	if req.ObserverChatID != cs.ID {
		t.Errorf("observer chat = %q, want %q", req.ObserverChatID, cs.ID)
	}
	// The chat session cleans the path it stores, so the comparison
	// is against the cleaned form.
	if want := filepath.Clean("/tmp/work"); req.Cwd != want {
		t.Errorf("cwd = %q, want %q", req.Cwd, want)
	}
	if req.Agent != "claude" {
		t.Errorf("agent = %q, want claude", req.Agent)
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

func TestStatus_ReportsWhereTheRunStands(t *testing.T) {
	ctrl := &fakeController{status: command.MeStatus{
		Phase:            "running",
		RunID:            "run-1",
		Step:             "/gtw commit",
		Execution:        "running",
		Actions:          2,
		RemainingActions: 28,
		System1Decisions: 3,
		System2Decisions: 1,
		System1Tokens:    120,
		System2Tokens:    800,
	}}
	cs := newSession(t, "me-7", "/tmp/work")

	reply := runMe(t, ctrl, cs, "/me", "status")
	for _, want := range []string{"running", "run-1", "/gtw commit", "2 taken, 28 left", "system-1: 3 decisions"} {
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

// TestWorkAgent_FallsBackToThePrimaryAgent — a chat that selected no
// agent runs on its primary one.
func TestWorkAgent_FallsBackToThePrimaryAgent(t *testing.T) {
	if got := workAgent(nil); got != "" {
		t.Errorf("workAgent(nil) = %q, want an empty agent", got)
	}
	cs := newSession(t, "me-11", "/tmp/work")
	if got := workAgent(cs); got != cs.PrimaryAgent() {
		t.Errorf("workAgent = %q, want the primary agent %q", got, cs.PrimaryAgent())
	}
}

func TestNoController(t *testing.T) {
	cs := newSession(t, "me-10", "/tmp/work")

	reply := runMe(t, nil, cs, "/me", "status")
	if !strings.Contains(reply, "not available") {
		t.Fatalf("reply = %q, want the missing agent", reply)
	}
}
