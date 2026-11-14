package me

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
)

// waitForStatus waits until the run of a chat stands in the given
// phase.
func waitForStatus(t *testing.T, svc *Service, chatID string, phase Phase) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if svc.Status(chatID).Phase == phase {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run is %q, want %q", svc.Status(chatID).Phase, phase)
}

// TestPause_DoesNotInterruptTheRunningStep — a pause stops the run
// after the step it is on: that step's outcome is recorded.
func TestPause_DoesNotInterruptTheRunningStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The step that was running is not interrupted.
	time.Sleep(50 * time.Millisecond)
	if st := svc.Status("obs-1"); st.Paused {
		t.Fatal("the run paused while its step was running")
	}

	// The step ends, and the run pauses.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Status("obs-1")
	if !st.Paused {
		t.Error("the run does not report that it is paused")
	}
	if st.PauseReason == "" {
		t.Error("the run does not say why it paused")
	}
}

// TestContinue_HandsTheControlBack — the run goes on where it
// stopped.
func TestContinue_HandsTheControlBack(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	if err := svc.Continue(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("continue: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhaseRunning)
}

// TestContinue_CarriesTheNote — the note is the operator's
// instruction: it joins the conversation the chat model reads.
func TestContinue_CarriesTheNote(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	if err := svc.Pause(context.Background(), "obs-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	if err := svc.Continue(context.Background(), "obs-1", "use the other branch"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhaseRunning)

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is gone")
	}
	found := false
	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if p.Type == "text" && strings.Contains(p.Text, "use the other branch") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the note did not join the conversation")
	}
}

// TestTheCardOffersTheQuestionAsButtons — the options the run is
// waiting for become the card's buttons, so a click is the answer.
func TestTheCardOffersTheQuestionAsButtons(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	buttons := op.Buttons()
	found := false
	for _, b := range buttons {
		if b.Action == "reply /gtw commit" {
			found = true
		}
		if b.Action == "reply escalate" {
			t.Errorf("the card offers the operator %q, want the options of the run", b.Action)
		}
	}
	if !found {
		t.Fatalf("the card has no answer button: %+v", buttons)
	}
}

// TestTakeover_APersonsMessagePausesTheRun — somebody who writes in
// the chat while the run is going takes the chat back: the run stops
// after the step it is on.
func TestTakeover_APersonsMessagePausesTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	// Somebody writes in the chat.
	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		Text:      "wait, stop there",
		Origin:    messages.OriginHuman,
	})

	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhasePaused)

	st := svc.Status("obs-1")
	if !strings.Contains(st.PauseReason, "continue") {
		t.Errorf("the pause reason is %q, want it to name the way back", st.PauseReason)
	}
}

// TestTakeover_AReplyToTheQuestionIsNotATakeover — the answer to the
// run's own question is an answer, not somebody taking the chat back.
func TestTakeover_AReplyToTheQuestionIsNotATakeover(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolAskHuman, map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which branch?")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is gone")
	}
	askID := r.askMsgID()
	if askID == "" {
		t.Fatal("the question has no message id")
	}

	// The person answers the question.
	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_human",
		ReplyTo:   askID,
		Text:      "the feature branch",
		Origin:    messages.OriginHuman,
	})
	if st := svc.Status("obs-1"); st.Paused {
		t.Fatal("the answer to the question paused the run")
	}
}

// TestTakeover_AMessageOfTheRunIsNotATakeover — what the run says is
// not somebody taking the chat back.
func TestTakeover_AMessageOfTheRunIsNotATakeover(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	svc.Seen(&messages.InboundMessage{
		ChatID:    "obs-1",
		MessageID: "om_me",
		Text:      "/gtw commit",
		Origin:    messages.OriginMe,
	})

	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForStatus(t, svc, "obs-1", PhaseRunning)
}

// TestStop_EndsTheRun — only the run's own command ends it.
func TestStop_EndsTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
	if st := svc.Status("obs-1"); st.Phase != PhaseIdle {
		t.Errorf("the run is %q, want it gone", st.Phase)
	}
	// The chat is told to stop working.
	if !containsText(op.Sent(), "/stop") {
		t.Errorf("the chat was not told to stop: %v", op.Sent())
	}
}

// containsText reports whether the list holds the text.
func containsText(list []string, text string) bool {
	for _, item := range list {
		if item == text {
			return true
		}
	}
	return false
}
