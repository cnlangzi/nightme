// Tests for the run's own activity.
//
// The run's tool calls travel the outbound path: the chat renders them
// the way it renders a coding agent's. The card keeps the state and
// the run's own words.

package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
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

// TestRun_PublishesNoThinkingForTheChatModelsMessage — the words of
// the chat model are the run's own: they go on the run's card and not
// into the chat as a thinking card of their own.
func TestRun_PublishesNoThinkingForTheChatModelsMessage(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("the commit comes first", "c1", toolRunStep,
			map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	if thinking := op.EmittedOf(messages.OutThinking); len(thinking) != 0 {
		t.Fatalf("the run published %d thinking cards, want none: %v", len(thinking), op.Emitted())
	}
	if !op.Has("the commit comes first") {
		t.Fatalf("the card does not carry the words of the model: %v", op.Lines())
	}
}

// TestRun_PublishesTheReplyWithoutTheInlineTags — a model that
// writes its scratchpad into its own text does not make the card show
// the tags.
func TestRun_PublishesTheReplyWithoutTheInlineTags(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("<think>the commit comes first</think>", "c1", toolRunStep,
			map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	body := lastBody(op)
	if !strings.Contains(body, "the commit comes first") {
		t.Fatalf("the card does not carry the words of the model: %q", body)
	}
	if strings.Contains(body, "<think>") {
		t.Fatalf("the card carries the tags: %q", body)
	}
}

// lastBody returns the body of the last card the run wrote.
func lastBody(op *fakeOperator) string {
	bodies := op.Lines()
	if len(bodies) == 0 {
		return ""
	}
	return bodies[len(bodies)-1]
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
	if starts[0].Tool.Summary != "" {
		t.Fatalf("the call carries a result before it ran: %q", starts[0].Tool.Summary)
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
	if ends[0].Tool.Summary != "committed" {
		t.Fatalf("the result = %q, want the summary of the step", ends[0].Tool.Summary)
	}
	if ends[0].Tool.Err != nil {
		t.Fatalf("the result carries an error: %v", ends[0].Tool.Err)
	}
}

// TestRun_PublishesTheToolResultOfAFailedCall — a call that could not
// run carries its failure as the error: a failed call must not read
// as one that went through.
func TestRun_PublishesTheToolResultOfAFailedCall(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "nowhere"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the step does not exist"})},
	}}
	_, op := eventsRun(t, provider)

	ends := op.EmittedOf(messages.OutToolEnd)
	if len(ends) == 0 {
		t.Fatalf("the run published no tool result: %v", op.Emitted())
	}
	if ends[0].Tool.Err == nil {
		t.Fatal("the failed call carries no error")
	}
	if ends[0].Tool.Err.Error() == "" {
		t.Fatal("the failed call carries an empty error")
	}
	if ends[0].Tool.Summary != "" {
		t.Fatalf("the failed call also carries a summary: %q", ends[0].Tool.Summary)
	}
}

// TestRun_PublishesNoResultForACallThatIsStillRunning — a call whose
// command has not settled has no verdict yet.
func TestRun_PublishesNoResultForACallThatIsStillRunning(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "commit"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The first command does not settle: the run reaches a judgment
	// point and the model runs the step again. The second command
	// never settles: the call has no verdict.
	op.steps = []fakeStep{pendingStep("still going"), {open: true}}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSentCount(t, op, 2)

	if starts := op.EmittedOf(messages.OutToolStart); len(starts) == 0 {
		t.Fatalf("the run published no tool call: %v", op.Emitted())
	}
	if ends := op.EmittedOf(messages.OutToolEnd); len(ends) != 0 {
		t.Fatalf("the run published %d results for a call that is still running", len(ends))
	}

	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// waitSentCount waits until the chat received that many commands.
func waitSentCount(t *testing.T, op *fakeOperator, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(op.Sent()) >= count {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the chat got %d commands, want %d", len(op.Sent()), count)
}

// TestRun_KeepsTheCallAndItsResultUnderOneCard — the run may move to
// another iteration while a call runs. The call and its result still
// hang under the card the call started under: the chat pairs them by
// their anchor.
func TestRun_KeepsTheCallAndItsResultUnderOneCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{
		Provider: &scriptedProvider{err: errors.New("the chat model must not be called")},
	})
	// The command never settles: the run stays in the chat.
	op.steps = []fakeStep{{open: true}}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the chat has no run")
	}

	call := pendingCall{ID: "c1", Name: toolRunStep, Args: json.RawMessage(`{"step_id":"commit"}`)}
	anchor := r.cardMessageID()
	svc.publishToolStart(context.Background(), r, anchor, call)
	// The run moves to another iteration while the call runs.
	r.setCard("card-2", iteration{job: "main", round: 2})
	svc.publishToolEnd(context.Background(), r, anchor, call,
		stepResult{Status: "ok", Summary: "committed"}, nil)

	starts := op.EmittedOf(messages.OutToolStart)
	ends := op.EmittedOf(messages.OutToolEnd)
	if len(starts) == 0 || len(ends) == 0 {
		t.Fatalf("the run published %d calls and %d results", len(starts), len(ends))
	}
	if starts[0].ReplyTo != ends[0].ReplyTo {
		t.Fatalf("the call hangs under %q and its result under %q", starts[0].ReplyTo, ends[0].ReplyTo)
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

// TestRun_CountsItsActivityEvenWhenTheChatHidesIt — the counts are
// the run's own: a chat that hides the run's events does not change
// what the run did.
func TestRun_CountsItsActivityEvenWhenTheChatHidesIt(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "commit"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The chat hides everything the run publishes.
	op.dropOutbound = true
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !op.Has("💭 1 · 🔧 1") {
		t.Fatalf("the card does not carry the counters of the run: %v", op.Lines())
	}
}

// TestRun_KeepsTheChatModelsMessageAsItsReply — the words of the chat
// model are the run's reply: the run holds them and the card ends
// with them.
func TestRun_KeepsTheChatModelsMessageAsItsReply(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("the commit comes first", "c1", toolAskHuman,
			map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "question: Which branch?")

	r := chatRun(t, svc, "obs-1")
	replies := r.replyList()
	if len(replies) != 1 {
		t.Fatalf("the run holds %d replies, want 1", len(replies))
	}
	if replies[0].Text != "the commit comes first" {
		t.Fatalf("the reply is %q, want the words of the model", replies[0].Text)
	}
	if thinking := op.EmittedOf(messages.OutThinking); len(thinking) != 0 {
		t.Fatalf("the run published %d thinking cards, want none", len(thinking))
	}
	if body := lastBody(op); !strings.HasSuffix(body, "the commit comes first") {
		t.Fatalf("the card does not end with the reply: %q", body)
	}
}

// TestTheActivityOfTheRunSurvivesTheDaemon — the counters, the time
// of the last activity and the replies are persisted with the rest of
// the run.
func TestTheActivityOfTheRunSurvivesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("which branch?", "c1", toolAskHuman,
			map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "question: Which branch?")
	waitStateFile(t, dir)

	r := chatRun(t, svc, "obs-1")
	raw, err := os.ReadFile(filepath.Join(dir, "runs", r.id+".json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state persistedRun
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.ThinkCount != 1 {
		t.Errorf("the state file holds %d model turns, want 1", state.ThinkCount)
	}
	if state.ToolCount != 1 {
		t.Errorf("the state file holds %d tool calls, want 1", state.ToolCount)
	}
	if state.LastBeatAt.IsZero() {
		t.Error("the state file holds no time of the last activity")
	}
	if len(state.Replies) != 1 {
		t.Fatalf("the state file holds %d replies, want 1", len(state.Replies))
	}
	if state.Replies[0].Text != "which branch?" {
		t.Errorf("the persisted reply is %q, want the words of the model", state.Replies[0].Text)
	}

	restored := svc.restoreRun(state)
	if restored.thinkCount != 1 || restored.toolCount != 1 {
		t.Errorf("the restored run holds %d model turns and %d tool calls, want 1 and 1",
			restored.thinkCount, restored.toolCount)
	}
	if !restored.lastBeatAt.Equal(state.LastBeatAt) {
		t.Errorf("the restored run stands at %v, want %v", restored.lastBeatAt, state.LastBeatAt)
	}
	if replies := restored.replyList(); len(replies) != 1 || replies[0].Text != "which branch?" {
		t.Errorf("the restored run holds %v, want the reply of the model", replies)
	}
}

// TestTheCardFollowsTheSnapshotsOfTheCommand — the card follows the
// command the run waits for, and it does not write to the chat on
// every snapshot of it.
func TestTheCardFollowsTheSnapshotsOfTheCommand(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	clock := &testClock{now: time.Date(2026, 11, 16, 14, 35, 22, 0, time.UTC)}
	svc, op := testService(t, dir, Config{Now: clock.Now})
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
	t.Cleanup(r.cancel)

	// The run watches the command it waits for.
	snapshots := make(chan observe.Execution, 8)
	r.setWatchChannel(snapshots)
	go svc.watchCommand(context.Background(), r, "msg-1", "/gtw commit")

	// A snapshot of the command refreshes the card.
	snapshots <- observe.Execution{Phase: observe.PhaseRunning}
	waitCards(t, op, 1)
	first := lastBody(op)
	if !strings.Contains(first, "⏱ 14:35:22") {
		t.Fatalf("the card does not say when the run did something last: %q", first)
	}

	// A second snapshot within the window does not.
	snapshots <- observe.Execution{Phase: observe.PhaseRunning}
	time.Sleep(100 * time.Millisecond)
	if n := len(op.Cards()); n != 1 {
		t.Fatalf("the card was written %d times, want once", n)
	}

	// A snapshot after the window refreshes it again.
	clock.Advance(cardRefreshInterval)
	snapshots <- observe.Execution{Phase: observe.PhaseRunning}
	waitCards(t, op, 2)
	if lastBody(op) == first {
		t.Fatal("the card did not follow the command")
	}
}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the time the clock stands at.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// waitCards waits until the run wrote that many cards.
func waitCards(t *testing.T, op *fakeOperator, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(op.Cards()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run wrote %d cards, want %d", len(op.Cards()), n)
}

// TestRun_HangsItsToolCallsUnderTheCard — the call and its result
// hang under the card of the run: the card is the run's state board,
// and the thread under it is the run's log.
func TestRun_HangsItsToolCallsUnderTheCard(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "commit"})},
	}}
	_, op := eventsRun(t, provider)

	cards := op.Cards()
	if len(cards) == 0 {
		t.Fatal("the run published no card")
	}
	cardID := cards[0].ID
	starts := op.EmittedOf(messages.OutToolStart)
	ends := op.EmittedOf(messages.OutToolEnd)
	if len(starts) == 0 || len(ends) == 0 {
		t.Fatalf("the run published %d calls and %d results", len(starts), len(ends))
	}
	if starts[0].ReplyTo != cardID {
		t.Errorf("the call hangs under %q, want the card %q", starts[0].ReplyTo, cardID)
	}
	if ends[0].ReplyTo != cardID {
		t.Errorf("the result hangs under %q, want the card %q", ends[0].ReplyTo, cardID)
	}
}
