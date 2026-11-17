package me

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestEndToEnd_TheRunWorksInTheChatItStartedIn — the whole run
// happens in one chat: every step is a message of that chat, the
// chat holds the run's overview, and the overview ends with the
// summary.
func TestEndToEnd_TheRunWorksInTheChatItStartedIn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name:   "fix-issue",
		Args:   []string{"42"},
		ChatID: "oc_user",
		Cwd:    "/tmp/work",
		Owner:  "ou_user",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "oc_user")

	// Every step of the workflow was said in the chat, with the bot
	// mark in front of it.
	spoken := op.Spoken()
	want := []string{
		"/gtw fix 42 -y",
		"/gtw commit",
		"/review",
		"/gtw push",
		"/gtw pr",
	}
	var said []string
	for _, m := range spoken {
		if !strings.HasPrefix(m.Display, BotMark) {
			t.Fatalf("the chat shows %q, want the bot mark in front", m.Display)
		}
		if m.Display != BotMark+m.Text {
			t.Fatalf("the chat shows %q, want %q", m.Display, BotMark+m.Text)
		}
		said = append(said, m.Text)
	}
	if len(said) != len(want) {
		t.Fatalf("the run said %v, want %v", said, want)
	}
	for i, cmd := range want {
		if said[i] != cmd {
			t.Fatalf("command %d was %q, want %q", i, said[i], cmd)
		}
	}

	// The run's card shows the jobs of the workflow, and the card of
	// the round shows what the step came to.
	runCard := op.Cards()[0].ID
	jobs := op.Checklist(runCard)
	if len(jobs) != 1 || jobs[0].ID != "main" {
		t.Fatalf("the checklist of the run is %+v, want the job of the workflow", jobs)
	}
	roundCard := op.Cards()[1].ID
	var shown bool
	for _, entry := range op.Entries(roundCard) {
		if strings.Contains(entry, "/gtw fix 42 -y") {
			shown = true
		}
	}
	if !shown {
		t.Fatalf("the card of the round does not show the step: %v", op.Entries(roundCard))
	}

	// The run's card says how the run ended.
	var verdict bool
	for _, m := range op.EmittedOf(messages.OutResult) {
		if m.Result == "fix-issue is complete" {
			verdict = true
		}
	}
	if !verdict {
		t.Fatalf("the run published no verdict of its outcome: %v", op.EmittedOf(messages.OutResult))
	}

	// The chat holds no run any more.
	if st := svc.Status("oc_user"); st.Phase != PhaseIdle {
		t.Errorf("the chat is %q, want it free", st.Phase)
	}
}

// TestEndToEnd_OneRunPerChat — a chat has one run at a time.
func TestEndToEnd_OneRunPerChat(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "oc_user", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "oc_user", Cwd: "/tmp/work",
	})
	if err == nil {
		t.Fatal("the chat started a second run")
	}
	if !strings.Contains(err.Error(), "already has a run") {
		t.Fatalf("error = %v, want it to name the run that is there", err)
	}
}
