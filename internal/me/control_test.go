package me

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
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
	writeWorkflow(t, dir, "one", continueWorkflow)

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
	writeWorkflow(t, dir, "one", continueWorkflow)

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
		if b.Label == "/gtw commit" && b.Action == "reply 0" {
			found = true
		}
		if b.Label == "escalate" {
			t.Errorf("the card offers the operator %q, want the options of the run", b.Label)
		}
	}
	if !found {
		t.Fatalf("the card has no answer button: %+v", buttons)
	}
}

// TestTheQuestionIsNotDispatched — the question belongs to the run,
// not to the agent: the agent never sees what the run asked.
func TestTheQuestionIsNotDispatched(t *testing.T) {
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
	waitForLine(t, op, "question: Which branch?")

	asked := op.Asked()
	if len(asked) != 1 {
		t.Fatalf("the run asked %d questions, want 1", len(asked))
	}
	if !strings.Contains(asked[0].Text, "Which branch?") {
		t.Errorf("the question is %q, want the model's question", asked[0].Text)
	}
	for _, m := range op.Spoken() {
		if strings.Contains(m.Text, "Which branch?") {
			t.Fatalf("the question was dispatched to the agent: %+v", m)
		}
	}
}

// TestANumberedAnswerIsTheOption — the card's buttons carry the
// number of the option, because a chat button has room for a few
// bytes only.
func TestANumberedAnswerIsTheOption(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	if err := svc.Reply(context.Background(), "obs-1", "0"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitSent(t, op, "/gtw commit")
}

// TestContinue_CancelsAPauseThatHasNotTakenEffect — the operator
// changed their mind before the step ended: the run does not pause.
func TestContinue_CancelsAPauseThatHasNotTakenEffect(t *testing.T) {
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
	if err := svc.Continue(context.Background(), "obs-1", ""); err != nil {
		t.Fatalf("continue: %v", err)
	}

	// The step ends: the run goes on instead of pausing.
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if st := svc.Status("obs-1"); st.Paused {
		t.Fatal("the run paused although the operator cancelled the pause")
	}
}

// TestOneIterationGetsOneCard — a card covers every step of a job in
// one round: the run does not leave a card behind per step.
func TestOneIterationGetsOneCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two steps.
jobs:
  main:
    steps:
      - id: one
        message: /gtw commit
      - id: two
        message: /gtw push
`)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 1 {
		t.Fatalf("the run published %d cards, want one for the iteration", posted)
	}
}

// TestTheCardOfTheStartIsAdopted — the card the run starts with
// belongs to the first iteration: the run adopts it instead of
// publishing a second one.
func TestTheCardOfTheStartIsAdopted(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	if posted := op.Posted(); posted != 1 {
		t.Fatalf("the run published %d cards, want the one it started with", posted)
	}
	body := lastCardBody(op, "card-1")
	if !strings.Contains(body, "job: main · round: 1") {
		t.Errorf("the card does not name the iteration it records: %q", body)
	}
}

// TestEveryRoundGetsItsOwnCard — a job that runs several rounds gets
// a card per round, and the card of the round that ended stays in the
// chat with the content it had.
func TestEveryRoundGetsItsOwnCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repeat", `title: repeat.
jobs:
  main:
    iterations: 2
    steps:
      - id: fix
        message: /gtw fix
`)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repeat", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want one per round", posted)
	}

	// The card of the second round stands in front of the command of
	// that round.
	log := op.Log()
	first := indexOf(log, "speak:/gtw fix")
	second := indexOfAfter(log, "speak:/gtw fix", first+1)
	if first < 0 || second < 0 {
		t.Fatalf("the run did not send the command of both rounds: %v", log)
	}
	published := false
	for _, entry := range log[first+1 : second] {
		if strings.HasPrefix(entry, "card:") {
			published = true
		}
	}
	if !published {
		t.Fatalf("the card of the second round came after its first command: %v", log)
	}

	// The card of the round that ended keeps its content.
	body := lastCardBody(op, "card-1")
	if !strings.Contains(body, "1. ✅ /gtw fix") {
		t.Errorf("the card of the first round does not show its step: %q", body)
	}
	if !strings.HasPrefix(body, "job: main · round: 1") {
		t.Errorf("the card of the first round does not name its round: %q", body)
	}
}

// TestAnotherJobGetsACardOfItsOwn — a run that moves to another job
// gets a card of its own, and the card of the job that ended keeps the
// content it had.
func TestAnotherJobGetsACardOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want one per job", posted)
	}
	body := lastCardBody(op, "card-1")
	if !strings.Contains(body, "1. ✅ /gtw commit") {
		t.Errorf("the card of the first job does not show its step: %q", body)
	}
	if !strings.HasPrefix(body, "job: build · round: 1") {
		t.Errorf("the card of the first job does not name its iteration: %q", body)
	}
}

// TestTheCardKeepsItsTitle — the title of the card is the workflow
// and the phase it is in.
func TestTheCardKeepsItsTitle(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run published no card")
	}
	for _, c := range cards {
		if !strings.HasPrefix(c.Title, "ship · ") {
			t.Errorf("the card is titled %q, want the workflow and the phase", c.Title)
		}
	}
	last := cards[len(cards)-1]
	if last.Title != "ship · done" {
		t.Errorf("the last card is titled %q, want the final phase", last.Title)
	}
	if !strings.Contains(last.Body, "result: ") {
		t.Errorf("the last card does not carry the summary: %q", last.Body)
	}
}

// TestAQuestionUpdatesTheCardInPlace — a run that asks a question
// keeps the card it has: the question is an update, not a card of its
// own.
func TestAQuestionUpdatesTheCardInPlace(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw commit", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	if posted := op.Posted(); posted != 1 {
		t.Fatalf("the run published %d cards, want the one it started with", posted)
	}

	if err := svc.Reply(context.Background(), "obs-1", "0"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")
	if posted := op.Posted(); posted != 1 {
		t.Fatalf("the run published %d cards, want the one it started with", posted)
	}
}

// TestAPauseUpdatesTheCardInPlace — a run that is paused and
// continued keeps the card it has.
func TestAPauseUpdatesTheCardInPlace(t *testing.T) {
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
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 1 {
		t.Fatalf("the run published %d cards, want the one it started with", posted)
	}
}

// TestTheModelPathFollowsTheStepOfTheCommand — the card follows the
// job of the step the model runs.
func TestTheModelPathFollowsTheStepOfTheCommand(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want one per job", posted)
	}
	body := lastCardBody(op, "card-2")
	if !strings.Contains(body, "job: publish · round: 1") {
		t.Errorf("the card does not follow the step of the command: %q", body)
	}
}

// TestAnIterationKeepsItsCard — a run that comes back to an iteration
// goes on with the card that iteration has: one card per iteration,
// not one per visit.
func TestAnIterationKeepsItsCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", buildTagWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_gtw", map[string]any{"args": "push"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "tagged"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The run was in the build job, then in the publish job, and back
	// in the build job: two iterations, two cards.
	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want one per iteration", posted)
	}
	// The card of the build iteration was written after the card of
	// the publish iteration: the run went back to it.
	cards := op.Cards()
	publish, build := -1, -1
	for i, c := range cards {
		if c.ID == "card-2" && c.Posted {
			publish = i
		}
		if c.ID == "card-1" {
			build = i
		}
	}
	if publish < 0 || build < publish {
		t.Fatalf("the run did not go back to the card of the build iteration: %v", cards)
	}
}

// TestACardTheChatRefusedIsPublishedLater — a card the chat refuses
// does not stop the run: the iteration gets its card as soon as the
// chat takes one.
func TestACardTheChatRefusedIsPublishedLater(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", publishTwiceWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "done", "summary": "pushed"})},
	}}
	logger := &recordingLogger{}
	svc, op := testService(t, dir, Config{Provider: provider, Logger: slog.New(logger)})
	// The chat refuses the card of the publish iteration — the second
	// attempt — and takes the one that comes after it.
	op.refuseAttempt = 2
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "tagged"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !logged(logger, "the run card could not be published") {
		t.Errorf("the failure was not logged: %v", logger.Lines())
	}
	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want the start card and the one the chat took later", posted)
	}
	body := lastCardBody(op, "card-2")
	if !strings.Contains(body, "job: publish · round: 1") {
		t.Errorf("the card does not name the iteration it records: %q", body)
	}
}

// TestTheCardOfAnAnsweredQuestionOffersNoChoice — the card the run
// leaves behind does not offer the answer of a question that is
// answered.
func TestTheCardOfAnAnsweredQuestionOffersNoChoice(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", "/gtw push", 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed", Data: map[string]string{"error": "the commit failed"}}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "pushed"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")
	if !hasReplyButton(op.Buttons()) {
		t.Fatalf("the card does not offer the answer: %+v", op.Buttons())
	}

	// The operator picks one: the run goes on in another iteration.
	if err := svc.Reply(context.Background(), "obs-1", "1"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitForPosted(t, op, 2)

	// The card the run left behind does not offer the answer any more.
	buttons := cardButtons(op, "card-1")
	for _, b := range buttons {
		if strings.HasPrefix(b.Action, cardActionReply) {
			t.Fatalf("the card of the iteration that ended still offers the answer: %+v", buttons)
		}
	}
	if body := lastCardBody(op, "card-1"); strings.Contains(body, "question: ") {
		t.Errorf("the card of the iteration that ended still shows the question: %q", body)
	}
}

// TestACardThatCannotBePublishedDoesNotStopTheRun — the card is for
// people: the work goes on when the chat refuses it.
func TestACardThatCannotBePublishedDoesNotStopTheRun(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	logger := &recordingLogger{}
	cfg := noModels()
	cfg.Logger = slog.New(logger)
	svc, op := testService(t, dir, cfg)
	// The chat takes the card the run starts with and refuses the
	// ones that come after it.
	op.failCardAfter = 1

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
	if len(sent) != 2 || sent[1] != "/gtw push" {
		t.Fatalf("the chat got %v, want both steps of the run", sent)
	}
	if posted := op.Posted(); posted != 1 {
		t.Errorf("the run published %d cards, want the one the chat took", posted)
	}
	if !logged(logger, "the run card could not be published") {
		t.Errorf("the failure was not logged: %v", logger.Lines())
	}
}

// TestResume_KeepsTheCardItOwns — a run that comes back after a
// daemon restart keeps the card it owns: the card's iteration travels
// with the run's state.
func TestResume_KeepsTheCardItOwns(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	op := newFakeOperator()
	op.steps = []fakeStep{{open: true}}
	cfg := Config{
		Provider:     &scriptedProvider{},
		Operator:     op,
		WorkflowsDir: dir,
		StateDir:     filepath.Join(dir, "runs"),
	}
	svc := New(cfg)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	// The state carries the iteration of the card the run owns.
	state := waitForCardIteration(t, filepath.Join(dir, "runs", runID+".json"))
	if state["card_job"] != "main" {
		t.Errorf("the state names the job %v, want main", state["card_job"])
	}
	if state["card_round"] != float64(1) {
		t.Errorf("the state names the round %v, want 1", state["card_round"])
	}
	// The card of the iteration travels with the run: a run that comes
	// back to it goes on with that card.
	cards, ok := state["cards"].([]any)
	if !ok || len(cards) != 1 {
		t.Fatalf("the state holds no card of an iteration: %v", state["cards"])
	}
	entry, ok := cards[0].(map[string]any)
	if !ok || entry["job"] != "main" || entry["round"] != float64(1) {
		t.Errorf("the state names the card of %v, want the card of main", cards[0])
	}

	// The daemon goes away and comes back.
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	posted := op.Posted()

	resumed := New(cfg)
	t.Cleanup(func() { _ = resumed.Shutdown(context.Background()) })
	if err := resumed.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// The resumed run stays in the same iteration: it publishes no
	// card of its own.
	time.Sleep(100 * time.Millisecond)
	if op.Posted() != posted {
		t.Errorf("the resumed run published %d cards, want none", op.Posted()-posted)
	}
}

// TestShow_PutsTheStateBackInFrontOfTheOperator — /me show posts the
// current state as a card of its own, and the run keeps updating that
// one from here on.
func TestShow_PutsTheStateBackInFrontOfTheOperator(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, noModels())
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")
	before := op.Posted()

	if err := svc.Show(context.Background(), "obs-1"); err != nil {
		t.Fatalf("show: %v", err)
	}
	if op.Posted() != before+1 {
		t.Fatalf("the run published %d cards, want one more", op.Posted())
	}
	if !op.Has("/gtw commit") {
		t.Errorf("the card does not show the step: %v", op.Lines())
	}

	// The run keeps updating the card /me show published.
	shown := lastPostedCard(op)
	if !strings.Contains(shown.Body, "job: main · round: 1") {
		t.Errorf("the card does not name the iteration it records: %q", shown.Body)
	}
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForCardUpdate(t, op, shown.ID)
}

// twoJobWorkflow is a workflow of two jobs: the second one waits for
// the first.
const twoJobWorkflow = `title: two jobs.
jobs:
  build:
    steps:
      - id: commit
        message: /gtw commit
  publish:
    needs: [build]
    steps:
      - id: push
        message: /gtw push
`

// continueWorkflow is a workflow that keeps the run going after the
// step it pauses on: the wait gives the run something to do.
const continueWorkflow = `title: Do the one thing, then wait.
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: rest
        wait: 1h
`

// buildTagWorkflow is a workflow whose build job has two steps: the
// run comes back to it after it has worked somewhere else.
const buildTagWorkflow = `title: two jobs.
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
`

// publishTwiceWorkflow is a workflow whose publish job has two steps:
// the run stays in that job for another turn.
const publishTwiceWorkflow = `title: two jobs.
jobs:
  build:
    steps:
      - id: commit
        message: /gtw commit
  publish:
    needs: [build]
    steps:
      - id: push
        message: /gtw push
      - id: tag
        message: /gtw tag
`

// lastCardBody returns the body that was written to the card with the
// given id last.
func lastCardBody(op *fakeOperator, id string) string {
	cards := op.Cards()
	body := ""
	for _, c := range cards {
		if c.ID == id {
			body = c.Body
		}
	}
	return body
}

// cardButtons returns the buttons of the card with the given id, as
// they were written last.
func cardButtons(op *fakeOperator, id string) []channel.CardButton {
	cards := op.Cards()
	buttons := op.CardButtons()
	for i := len(cards) - 1; i >= 0; i-- {
		if cards[i].ID == id {
			return buttons[i]
		}
	}
	return nil
}

// hasReplyButton reports whether the buttons answer a question.
func hasReplyButton(buttons []channel.CardButton) bool {
	for _, b := range buttons {
		if strings.HasPrefix(b.Action, cardActionReply) {
			return true
		}
	}
	return false
}

// waitForPosted waits until the run published that many cards.
func waitForPosted(t *testing.T, op *fakeOperator, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op.Posted() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run published %d cards, want %d", op.Posted(), n)
}

// lastPostedCard returns the card the run published last.
func lastPostedCard(op *fakeOperator) cardRecord {
	cards := op.Cards()
	for i := len(cards) - 1; i >= 0; i-- {
		if cards[i].Posted {
			return cards[i]
		}
	}
	return cardRecord{}
}

// waitForCardUpdate waits until the run updates the card with the
// given id.
func waitForCardUpdate(t *testing.T, op *fakeOperator, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range op.Cards() {
			if c.ID == id && !c.Posted {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run never updated the card %q: %v", id, op.Cards())
}

// waitForCardIteration waits until the run's state file carries the
// iteration of the card the run owns.
func waitForCardIteration(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err == nil {
				if job, ok := state["card_job"].(string); ok && job != "" {
					return state
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run state never carried the card's iteration: %s", path)
	return nil
}

// indexOf returns the position of the first entry that equals the
// text, or -1.
func indexOf(list []string, text string) int {
	for i, item := range list {
		if item == text {
			return i
		}
	}
	return -1
}

// indexOfAfter returns the position of the first entry after the
// given one that equals the text, or -1.
func indexOfAfter(list []string, text string, from int) int {
	for i := from; i < len(list); i++ {
		if list[i] == text {
			return i
		}
	}
	return -1
}

// TestShow_WithoutARunIsRefused — a chat without a run has no state
// to show.
func TestShow_WithoutARunIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, noModels())
	if err := svc.Show(context.Background(), "obs-1"); err == nil {
		t.Fatal("a chat without a run showed a state")
	}
}

// TestTheErrorHasItsOwnLine — the error says why the run stopped, on
// its own line.
func TestTheErrorHasItsOwnLine(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "the step did not work"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var found bool
	for _, line := range op.Lines() {
		for _, l := range strings.Split(line, "\n") {
			if strings.HasPrefix(l, "error: ") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the card has no error line: %v", op.Lines())
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
	waitForLine(t, op, "question: Which branch?")

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
