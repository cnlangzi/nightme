// Tests for the run's own activity in the chat.
//
// The run's thinking and the run's tool calls travel the outbound
// path: the chat renders them the way it renders a coding agent's.
// The card keeps the state.

package me

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
)

// textAndToolCall builds an assistant message that says something and
// asks for one tool.
func textAndToolCall(text, id, name string, args map[string]any) llm.Message {
	msg := toolCallMsg(id, name, args)
	msg.Parts = append([]llm.Part{{Type: llm.PartText, Text: text}}, msg.Parts...)
	return msg
}

// eventsRun starts a run whose first step does not settle, so the run
// reaches a judgment point and the chat model takes a turn.
func eventsRun(t *testing.T, provider *scriptedProvider) (*Service, *fakeOperator) {
	t.Helper()
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")
	return svc, op
}

// TestRun_PublishesTheChatModelsThinking — the reasoning the chat
// model writes leaves the card and reaches the chat.
func TestRun_PublishesTheChatModelsThinking(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("the commit comes first", "c1", toolRunStep,
			map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	thinking := op.EmittedOf(messages.OutThinking)
	if len(thinking) == 0 {
		t.Fatalf("the run published no thinking: %v", op.Emitted())
	}
	if thinking[0].Text != "the commit comes first" {
		t.Fatalf("the thinking = %q, want the words of the model", thinking[0].Text)
	}
}

// TestRun_PublishesTheThinkingWithoutTheInlineTags — a model that
// writes its scratchpad into its own text does not make the chat show
// the tags.
func TestRun_PublishesTheThinkingWithoutTheInlineTags(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("<think>the commit comes first</think>", "c1", toolRunStep,
			map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	thinking := op.EmittedOf(messages.OutThinking)
	if len(thinking) == 0 {
		t.Fatalf("the run published no thinking: %v", op.Emitted())
	}
	if thinking[0].Text != "the commit comes first" {
		t.Fatalf("the thinking = %q, want the reasoning without the tags", thinking[0].Text)
	}
	if strings.Contains(thinking[0].Text, "<think>") {
		t.Fatal("the published thinking carries the tags")
	}
}

// TestRun_PublishesItsToolCalls — the call goes out before it runs and
// the result comes back when it settles.
func TestRun_PublishesItsToolCalls(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	starts := op.EmittedOf(messages.OutToolStart)
	if len(starts) == 0 {
		t.Fatalf("the run published no tool call: %v", op.Emitted())
	}
	if starts[0].Tool == nil {
		t.Fatal("the tool call carries no payload")
	}
	if starts[0].Tool.Name != toolRunStep {
		t.Fatalf("the tool = %q, want %q", starts[0].Tool.Name, toolRunStep)
	}
	if starts[0].Tool.Args != "commit" {
		t.Fatalf("the argument line = %q, want the step id", starts[0].Tool.Args)
	}
	if starts[0].Tool.Output != "" {
		t.Fatalf("the call carries a result before it ran: %q", starts[0].Tool.Output)
	}

	ends := op.EmittedOf(messages.OutToolEnd)
	if len(ends) == 0 {
		t.Fatalf("the run published no tool result: %v", op.Emitted())
	}
	if ends[0].Tool == nil {
		t.Fatal("the tool result carries no payload")
	}
	if ends[0].Tool.Name != toolRunStep {
		t.Fatalf("the tool = %q, want %q", ends[0].Tool.Name, toolRunStep)
	}
	if ends[0].Tool.Output != "committed" {
		t.Fatalf("the result = %q, want the summary of the step", ends[0].Tool.Output)
	}
}

// TestRun_PublishesTheToolResultOfAFailedCall — a call that could not
// run carries its failure in the result.
func TestRun_PublishesTheToolResultOfAFailedCall(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "nowhere"})},
	}}
	_, op := eventsRun(t, provider)

	ends := op.EmittedOf(messages.OutToolEnd)
	if len(ends) == 0 {
		t.Fatalf("the run published no tool result: %v", op.Emitted())
	}
	if ends[0].Tool.Output == "" {
		t.Fatal("the failed call carries no summary")
	}
}

// TestRun_AnchorsItsEventsToTheCard — the events hang under the run's
// card: the card is the run's state board, and the thread under it is
// the run's log.
func TestRun_AnchorsItsEventsToTheCard(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("thinking", "c1", toolRunStep, map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run published no card")
	}
	cardID := cards[0].ID
	emitted := op.Emitted()
	if len(emitted) == 0 {
		t.Fatal("the run published no event")
	}
	for _, m := range emitted {
		if m.ReplyTo != cardID {
			t.Fatalf("the event hangs under %q, want the card %q", m.ReplyTo, cardID)
		}
	}
}

// TestRun_PublishesNoReply — the run's voice is its card.
func TestRun_PublishesNoReply(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("thinking", "c1", toolRunStep, map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	if replies := op.EmittedOf(messages.OutReply); len(replies) != 0 {
		t.Fatalf("the run published %d replies, want none", len(replies))
	}
}

// TestCard_CarriesNoNote — the card holds the state, not the model's
// words.
func TestCard_CarriesNoNote(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("the commit comes first", "c1", toolRunStep,
			map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	for _, body := range op.Lines() {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "note:") {
				t.Fatalf("the card carries a note: %q", line)
			}
		}
	}
}

// TestCard_TruncatesTheCommandInFlight — the full command is in the
// chat's tool line; the card keeps a line that fits.
func TestCard_TruncatesTheCommandInFlight(t *testing.T) {
	dir := t.TempDir()
	command := "/gtw commit --message " + strings.Repeat("x", 500)
	writeWorkflow(t, dir, "one", fmt.Sprintf(`title: one
jobs:
  main:
    steps:
      - id: commit
        message: %q
`, command))

	svc, op := testService(t, dir, Config{
		Provider: &scriptedProvider{err: errors.New("the chat model must not be called")},
	})
	// The command never settles: the run waits for it, and the card
	// keeps showing what is in flight.
	op.steps = []fakeStep{{open: true}}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, command)

	line := waitCurrentLine(t, op)
	if got := len([]rune(line)); got > maxCardCommand {
		t.Fatalf("the line of the command in flight is %d characters, want at most %d", got, maxCardCommand)
	}
	if got := len([]rune(line)); got != maxCardCommand {
		t.Fatalf("the line of the command in flight is %d characters, want %d", got, maxCardCommand)
	}

	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// waitCurrentLine waits until a card of the run shows the command in
// flight and returns that line.
func waitCurrentLine(t *testing.T, op *fakeOperator) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, body := range op.Lines() {
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "current: ") {
					return line
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no card showed the command in flight")
	return ""
}
