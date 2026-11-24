// Tests for the card of a decision.
//
// The card of a decision belongs to the step the decision is about: the
// line that says the agent takes the workflow over opens it, and every
// judgment of that step lands on it.

package me

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// takeoverMark opens the body of the card of a decision.
const takeoverMark = "**" + BotMark

// decisionCards returns the cards the run opened for a decision.
func decisionCards(op *fakeOperator) []cardRecord {
	out := make([]cardRecord, 0, 2)
	for _, c := range op.Cards() {
		if strings.HasPrefix(c.Body, takeoverMark) {
			out = append(out, c)
		}
	}
	return out
}

// TestTheTakeoverStandsOutAndNamesTheStepOnlyWhenItHasOne — the
// takeover line carries the mark of the agent in bold, so that it is
// seen at once among the entries of the card, and it names the step
// only when the judgment has one.
func TestTheTakeoverStandsOutAndNamesTheStepOnlyWhenItHasOne(t *testing.T) {
	if got := decisionTakeover("commit"); got != "**🧑‍💻: NightMe is taking over the workflow · commit**" {
		t.Errorf("the takeover is %q, want it to name the step", got)
	}
	if got := decisionTakeover(""); got != "**🧑‍💻: NightMe is taking over the workflow**" {
		t.Errorf("the takeover is %q, want it to name no step", got)
	}
}

// TestTheDecisionOfAStepGoesOnWithItsCard — a step that is judged
// again goes on with the card it has: the chat holds one card for the
// step, and the judgment lands on it.
func TestTheDecisionOfAStepGoesOnWithItsCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, Config{})

	r := testRun(t, svc, "one")
	r.setJudgmentStep("commit")

	svc.openDecisionSession(context.Background(), r)
	first := r.decisionSessionRef()
	if first == nil {
		t.Fatal("the run has no card of the decision")
	}
	if first.anchor == "" {
		t.Fatal("the decision opened no card")
	}

	svc.openDecisionSession(context.Background(), r)
	second := r.decisionSessionRef()
	if second != first {
		t.Fatal("the run opened a second card for the same step")
	}
	if second.anchor != first.anchor {
		t.Fatalf("the card of the step is %q, want %q", second.anchor, first.anchor)
	}
	if cards := decisionCards(op); len(cards) != 1 {
		t.Errorf("the run opened %d cards of the decision, want one: %v", len(cards), op.Cards())
	}
}

// TestTheDecisionOfAnotherStepOpensACardOfItsOwn — a judgment about
// another step ends the card of the step before and opens one of its
// own.
func TestTheDecisionOfAnotherStepOpensACardOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, Config{})

	r := testRun(t, svc, "one")
	r.setJudgmentStep("commit")
	svc.openDecisionSession(context.Background(), r)
	first := r.decisionSessionRef().anchor

	r.setJudgmentStep("review")
	svc.openDecisionSession(context.Background(), r)
	second := r.decisionSessionRef()
	if second.anchor == first {
		t.Fatal("the run went on with the card of the step before")
	}
	cards := decisionCards(op)
	if len(cards) != 2 {
		t.Fatalf("the run opened %d cards of the decision, want one per step: %v", len(cards), op.Cards())
	}
	if !strings.Contains(cards[1].Body, "· review") {
		t.Errorf("the second card opens with %q, want it to name the step", cards[1].Body)
	}
}

// TestTheReasonOfTheStepIsPublishedOnce — the reason of a step is
// published once: a judgment that comes to the same reason again adds
// nothing to the card. A reason that changed is a new entry.
func TestTheReasonOfTheStepIsPublishedOnce(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, Config{})

	r := testRun(t, svc, "one")
	r.setJudgmentStep("commit")
	r.setJudgment(`the previous step "commit" ended as "failed"`)

	svc.openDecisionSession(context.Background(), r)
	card := r.decisionSessionRef().anchor

	// The same reason: the card carries it once.
	svc.openDecisionSession(context.Background(), r)

	// A reason that changed: the card carries it again.
	r.setJudgment(`the previous step "commit" ended as "failed" again`)
	svc.openDecisionSession(context.Background(), r)

	var reasons []string
	for _, entry := range op.Entries(card) {
		if strings.HasPrefix(entry, "reason: ") {
			reasons = append(reasons, entry)
		}
	}
	if len(reasons) != 2 {
		t.Errorf("the card carries %d reasons, want one per reason: %v", len(reasons), op.Entries(card))
	}
}

// TestTheDecisionAsksNothing — the card of a decision is opened by the
// standard outbound events: the run asks the chat nothing. The state of
// the run still asks for a card of its own.
func TestTheDecisionAsksNothing(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, op := testService(t, dir, Config{})

	r := testRun(t, svc, "one")
	r.setJudgmentStep("commit")
	svc.openDecisionSession(context.Background(), r)

	if asked := op.Asked(); len(asked) != 0 {
		t.Errorf("the run asked %v, want the decision to ask nothing", asked)
	}

	// The state of the run opens its card the way it always did.
	svc.showState(context.Background(), r)
	if len(op.Asked()) == 0 {
		t.Error("the state of the run asked nothing, want it to open a card of its own")
	}
}

// TestTheJudgmentOfTheStepLandsOnItsCard — the run wakes a model at a
// judgment point: the card of the step is opened by the takeover, and
// every judgment of the step lands on it.
func TestTheJudgmentOfTheStepLandsOnItsCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw commit", 0.95, 0),
		decisionResp("jev-1.14", "/gtw commit", 0.95, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	// Every attempt of the step breaks: the plan cannot go on by itself
	// and the agent is woken again and again.
	op.steps = []fakeStep{
		{outcome: outcome.Outcome{Success: false, Status: http.StatusInternalServerError, StatusText: "still going"}},
		{outcome: outcome.Outcome{Success: false, Status: http.StatusInternalServerError, StatusText: "still going"}},
	}

	runID, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitRun(t, svc, "obs-1")

	cards := decisionCards(op)
	if len(cards) != 1 {
		t.Fatalf("the run opened %d cards of the decision, want one: %v", len(cards), op.Cards())
	}
	card := cards[0].ID
	if want := "**🧑‍💻: NightMe is taking over the workflow · commit**"; cards[0].Body != want {
		t.Errorf("the card opens with %q, want %q", cards[0].Body, want)
	}

	// The reason of the step is on the card once.
	var reasons int
	for _, entry := range op.Entries(card) {
		if strings.HasPrefix(entry, "reason: ") {
			reasons++
		}
	}
	if reasons != 1 {
		t.Errorf("the card carries %d reasons, want one: %v", reasons, op.Entries(card))
	}

	// Every judgment lands on the card as an entry of its own.
	var verdicts int
	for _, entry := range op.Entries(card) {
		if strings.HasPrefix(entry, decisionDecider+" — ") {
			verdicts++
		}
	}
	if want := len(r.wakeRecords()); verdicts != want {
		t.Errorf("the card carries %d verdicts, want one per judgment (%d): %v",
			verdicts, want, op.Entries(card))
	}
	if verdicts < 2 {
		t.Errorf("the step was judged %d times, want it judged again", verdicts)
	}
}
