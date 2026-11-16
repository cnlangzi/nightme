// Tests for the run's card.
//
// The card is the run's receipt: it carries the live header, the
// decision the run made last and the run's own words.

package me

import (
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// cardRun builds a run of the one-step workflow, with the card of
// its first iteration.
func cardRun(t *testing.T, svc *Service) *run {
	t.Helper()
	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{Name: "one", ChatID: "obs-1", Cwd: "/tmp/work"}, bound, nil)
	r.setCard("card-1", iteration{job: "main", round: 1})
	return r
}

// cardService builds a service with the one-step workflow in it.
func cardService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)
	svc, _ := testService(t, dir, Config{})
	return svc
}

// firstLine returns the first line of a card body.
func firstLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	return line
}

// TestCard_CarriesTheLiveHeader — the first line of the card says
// what the run did and when it last did it.
func TestCard_CarriesTheLiveHeader(t *testing.T) {
	svc := cardService(t)
	r := cardRun(t, svc)

	at := time.Date(2026, 11, 16, 14, 35, 22, 0, time.UTC)
	r.countThink(at)
	r.countThink(at)
	r.countTool(at)
	r.countTool(at)
	r.countTool(at)

	view := svc.cardView(r, iteration{job: "main", round: 1})
	if got := firstLine(view.Body); got != "💭 2 · 🔧 3 · ⏱ 14:35:22" {
		t.Fatalf("the first line of the card is %q, want the live header", got)
	}
}

// TestCard_HasNoLiveHeaderWithoutActivity — a run that has not done
// anything yet starts with the line of its iteration.
func TestCard_HasNoLiveHeaderWithoutActivity(t *testing.T) {
	svc := cardService(t)
	r := cardRun(t, svc)

	view := svc.cardView(r, iteration{job: "main", round: 1})
	if got := firstLine(view.Body); got != "job: main · round: 1" {
		t.Fatalf("the first line of the card is %q, want the iteration of the run", got)
	}
}

// TestCard_NamesTheDecisionOfTheModel — the decision a model made
// names what it decided.
func TestCard_NamesTheDecisionOfTheModel(t *testing.T) {
	svc := cardService(t)
	r := cardRun(t, svc)

	r.setDecision(decisionChat, "the chat model took the turn")
	r.recordWake(wakeRecord{
		Who:  decisionChat,
		What: "run_step(review)",
		At:   time.Now(),
	})

	view := svc.cardView(r, iteration{job: "main", round: 1})
	if !strings.Contains(view.Body, "decision: the chat model — run_step(review)") {
		t.Fatalf("the card does not name the decision: %q", view.Body)
	}
}

// TestCard_KeepsTheReasonOfTheWorkflowsDecision — the workflow
// decides on its own: there is no model behind it.
func TestCard_KeepsTheReasonOfTheWorkflowsDecision(t *testing.T) {
	svc := cardService(t)
	r := cardRun(t, svc)

	r.setDecision(decisionWorkflow, "the plan says so")

	view := svc.cardView(r, iteration{job: "main", round: 1})
	if !strings.Contains(view.Body, "decision: the workflow — the plan says so") {
		t.Fatalf("the card does not name the decision: %q", view.Body)
	}
}

// TestCard_CarriesTheLastThreeReplies — the run keeps the last three
// of its replies and the card shows them.
func TestCard_CarriesTheLastThreeReplies(t *testing.T) {
	svc := cardService(t)
	r := cardRun(t, svc)

	for _, text := range []string{"the first", "the second", "the third", "the fourth"} {
		r.addReply(replyBlock{Text: text, At: time.Now()})
	}

	view := svc.cardView(r, iteration{job: "main", round: 1})
	if !strings.Contains(view.Body, "reply:\nthe second\n\nthe third\n\nthe fourth") {
		t.Fatalf("the card does not carry the last three replies: %q", view.Body)
	}
	if strings.Contains(view.Body, "the first") {
		t.Fatalf("the card carries a reply that is too old: %q", view.Body)
	}
}

// TestCard_ClipsTheReply — the reply on the card is short: a card is
// read at a glance.
func TestCard_ClipsTheReply(t *testing.T) {
	svc := cardService(t)
	r := cardRun(t, svc)

	lines := make([]string, 0, 20)
	for i := 0; i < 6; i++ {
		lines = append(lines, strings.Repeat("x", 70))
	}
	for i := 0; i < 13; i++ {
		lines = append(lines, strings.Repeat("y", 111))
	}
	lines = append(lines, strings.Repeat("z", 118))
	text := strings.Join(lines, "\n")
	if len(text) != 2000 {
		t.Fatalf("the reply is %d characters, want 2000", len(text))
	}
	r.addReply(replyBlock{Text: clipReply(text), At: time.Now()})

	view := svc.cardView(r, iteration{job: "main", round: 1})
	_, block, _ := strings.Cut(view.Body, "reply:\n")
	if got := len([]rune(block)); got != maxReplyChars {
		t.Errorf("the reply is %d characters, want %d", got, maxReplyChars)
	}
	if got := len(strings.Split(block, "\n")); got != maxReplyLines {
		t.Errorf("the reply has %d lines, want %d", got, maxReplyLines)
	}
	if !strings.HasSuffix(block, "…") {
		t.Errorf("the reply does not end with the ellipsis: %q", block)
	}
}
