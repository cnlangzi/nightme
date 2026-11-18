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

// TestTheQuestionIsOfferedAsAChoiceCard — the options the run is
// waiting for become the chat's own choice card, so a click is the
// answer.
func TestTheQuestionIsOfferedAsAChoiceCard(t *testing.T) {
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
	waitAsked(t, op, "Which step should run next?")

	choices := op.EmittedOf(messages.OutChoice)
	if len(choices) == 0 {
		t.Fatalf("the run published no choice: %v", op.Emitted())
	}
	choice := choices[0].Choice
	if choice == nil {
		t.Fatal("the choice carries no payload")
	}
	if choice.RequestID == "" {
		t.Error("the choice has no request id: the answer could not be correlated")
	}
	found := false
	for _, option := range choice.Options {
		if option.Label == "/gtw commit" {
			found = true
		}
		if option.Label == "escalate" {
			t.Errorf("the card offers the operator %q, want the options of the run", option.Label)
		}
	}
	if !found {
		t.Fatalf("the choice has no option of the run: %+v", choice.Options)
	}
}

// TestAClickOnTheChoiceIsTheAnswerOfTheRun — a click on an option of
// the choice card reaches the run.
func TestAClickOnTheChoiceIsTheAnswerOfTheRun(t *testing.T) {
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
	waitAsked(t, op, "Which step should run next?")

	choice := waitChoice(t, op)
	if !op.Pick(choice.RequestID, "0") {
		t.Fatal("the click did not reach the run")
	}
	waitRun(t, svc, "obs-1")
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
	waitAsked(t, op, "Which branch?")

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
	waitAsked(t, op, "Which step should run next?")

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

	// The card of the run and the card of the round: every session
	// owns one card.
	if posted := op.Posted(); posted != 2 {
		t.Fatalf("the run published %d cards, want one for the run and one for the round", posted)
	}
}

// TestTheRoundCardHangsUnderTheCardOfTheRun — the round is the run's
// work, one level below it.
func TestTheRoundCardHangsUnderTheCardOfTheRun(t *testing.T) {
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

	cards := op.Cards()
	if len(cards) != 2 {
		t.Fatalf("the run published %d cards, want the card of the run and the card of the round", len(cards))
	}
	if cards[1].ReplyTo != cards[0].ID {
		t.Errorf("the round card hangs under %q, want the run card %q", cards[1].ReplyTo, cards[0].ID)
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

	if posted := op.Posted(); posted != 3 {
		t.Fatalf("the run published %d cards, want the card of the run and one per round", posted)
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

	// The card of the round that ended keeps its content: the round
	// session is closed with its verdict.
	rounds := op.RoundSessions()
	if len(rounds) != 2 {
		t.Fatalf("the run opened %d round sessions, want one per round", len(rounds))
	}
	if rounds[0] != "main · round 1" || rounds[1] != "main · round 2" {
		t.Errorf("the round sessions are %v, want one per round", rounds)
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

	if posted := op.Posted(); posted != 3 {
		t.Fatalf("the run published %d cards, want the card of the run and one per job", posted)
	}
	rounds := op.RoundSessions()
	if len(rounds) != 2 {
		t.Fatalf("the run opened %d round sessions, want one per job", len(rounds))
	}
	if rounds[0] != "build · round 1" || rounds[1] != "publish · round 1" {
		t.Errorf("the round sessions are %v, want one per job", rounds)
	}
}

// TestTheRunCardCarriesThePhaseInItsFooter — the run's card says how
// the run ended: the verdict rides in the footer of the session.
func TestTheRunCardCarriesThePhaseInItsFooter(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "ship", twoJobWorkflow)

	svc, op := testService(t, dir, noModels())
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "ship", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	results := op.EmittedOf(messages.OutResult)
	if len(results) == 0 {
		t.Fatalf("the run published no result: %v", op.Emitted())
	}
	found := false
	for _, m := range results {
		if m.Result == "ship is complete" {
			found = true
		}
	}
	if !found {
		t.Errorf("the run published no verdict of its outcome: %v", results)
	}
}

// TestAQuestionOpensNoCardOfItsOwn — a run that asks a question keeps
// the cards it has: the question is a choice, not a session.
func TestAQuestionOpensNoCardOfItsOwn(t *testing.T) {
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
	waitAsked(t, op, "Which step should run next?")

	before := op.Posted()

	if err := svc.Reply(context.Background(), "obs-1", "0"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")
	if posted := op.Posted(); posted != before {
		t.Fatalf("the run published %d cards, want the ones it had", posted-before)
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

	// The run keeps the cards it has: a pause is not a session.
	if posted := op.Posted(); posted > 2 {
		t.Fatalf("the run published %d cards, want the card of the run and the card of the round", posted)
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

	rounds := op.RoundSessions()
	if len(rounds) < 2 || rounds[len(rounds)-1] != "publish · round 1" {
		t.Errorf("the card does not follow the step of the command: %v", rounds)
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
	// in the build job: two rounds, two cards. The round the run came
	// back to keeps the card it had.
	rounds := op.RoundSessions()
	if len(rounds) != 2 {
		t.Fatalf("the run opened %d round sessions, want one per round", len(rounds))
	}
	if rounds[0] != "build · round 1" || rounds[1] != "publish · round 1" {
		t.Errorf("the round sessions are %v, want one per round", rounds)
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

	if !logged(logger, "the session card could not be posted") {
		t.Errorf("the failure was not logged: %v", logger.Lines())
	}
	if posted := op.Posted(); posted < 2 {
		t.Fatalf("the run published %d cards, want the card of the run and the one the chat took later", posted)
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
	waitAsked(t, op, "Which step should run next?")
	choices := op.EmittedOf(messages.OutChoice)
	if len(choices) != 1 {
		t.Fatalf("the run published %d choices, want one", len(choices))
	}

	// The operator picks one: the run goes on in another round.
	if err := svc.Reply(context.Background(), "obs-1", "1"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The question is asked once: the run does not ask it again.
	if again := op.EmittedOf(messages.OutChoice); len(again) != 1 {
		t.Fatalf("the run published %d choices, want one", len(again))
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
	if !logged(logger, "the session card could not be posted") {
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

	// The state carries the cards of the open sessions: a run that
	// comes back goes on with the cards it opened.
	state := waitForSession(t, filepath.Join(dir, "runs", runID+".json"))
	runSession, ok := state["run_session"].(map[string]any)
	if !ok || runSession["card_id"] == "" {
		t.Fatalf("the state holds no card of the run: %v", state["run_session"])
	}
	roundSession, ok := state["round_session"].(map[string]any)
	if !ok || roundSession["card_id"] == "" {
		t.Fatalf("the state holds no card of the round: %v", state["round_session"])
	}
	if roundSession["job"] != "main" || roundSession["round"] != float64(1) {
		t.Errorf("the state names the round %v, want main round 1", roundSession)
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

	// The chat has forgotten the cards the run opened: the run tells
	// it again, so the chat patches them instead of sending a second
	// card for the same session.
	adopted := op.Adopted()
	if len(adopted) == 0 {
		t.Fatalf("the resumed run told the chat about no card: %v", adopted)
	}
	want := map[string]bool{}
	for _, card := range op.Cards() {
		want[card.ID] = true
	}
	for _, cardID := range adopted {
		if !want[cardID] {
			t.Errorf("the run told the chat about %q, want a card it owns", cardID)
		}
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

	// The run keeps updating the card /me show published.
	shown := lastPostedCard(op)
	if err := op.answer("obs-1", "", ""); err != nil {
		t.Fatalf("settle the step: %v", err)
	}
	waitForSessionUpdate(t, op, shown.ID)
}

// TestShowPublishesTheOverviewOfTheRun — /me show publishes the same
// body as the run card: the overview of the run is the same text
// wherever it is shown.
func TestShowPublishesTheOverviewOfTheRun(t *testing.T) {
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

	if err := svc.Show(context.Background(), "obs-1"); err != nil {
		t.Fatalf("show: %v", err)
	}
	cards := op.Cards()
	if len(cards) < 2 {
		t.Fatalf("the run published %d cards, want the run card and the shown card", len(cards))
	}
	run := op.Entries(cards[0].ID)
	shown := op.Entries(cards[len(cards)-1].ID)
	if len(run) != 1 || len(shown) != 1 {
		t.Fatalf("the cards carry %d and %d entries, want the overview alone", len(run), len(shown))
	}
	if run[0] != shown[0] {
		t.Errorf("the shown card carries %q, want the overview of the run %q", shown[0], run[0])
	}
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

// waitForSession waits until the run's state file carries the card of
// the round the run is in.
func waitForSession(t *testing.T, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err == nil {
				if sess, ok := state["round_session"].(map[string]any); ok && sess["card_id"] != "" {
					return state
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run state never carried the card of the round: %s", path)
	return nil
}

// waitForSessionUpdate waits until the run publishes an event under
// the card with the given id.
func waitForSessionUpdate(t *testing.T, op *fakeOperator, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range op.Emitted() {
			if m.ReplyTo == id {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run never published an event under the card %q", id)
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

// TestTheDecisionCarriesTheReasonFirst — the reason the decision was
// taken is the first entry of the decision's card.
func TestTheDecisionCarriesTheReasonFirst(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
		Data:    map[string]string{"error": "the command failed"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which step should run next?")

	var reason string
	for _, m := range op.EmittedOf(messages.OutReply) {
		if strings.HasPrefix(m.Text, "reason: ") {
			reason = m.Text
		}
	}
	if reason == "" {
		t.Fatalf("the run published no reason for its decision: %v", op.EmittedOf(messages.OutReply))
	}
	if !strings.Contains(reason, "the command failed") {
		t.Errorf("the reason does not carry the error of the step: %q", reason)
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
	waitAsked(t, op, "Which branch?")

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
