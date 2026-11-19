// Tests for the run's own activity.
//
// The run speaks the standard outbound vocabulary: the same events a
// coding agent's turn speaks. Every event carries the anchor of the
// session it belongs to, so the chat renders the session onto that
// session's card.

package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")
	return svc, op
}

// replies returns the entries the run published.
func replies(op *fakeOperator) []string {
	out := make([]string, 0, 4)
	for _, m := range op.EmittedOf(messages.OutReply) {
		out = append(out, m.Text)
	}
	return out
}

// TestRun_PublishesTheThinkingOfTheChatModel — the thinking is an event
// of its own: it is not merged into the words the model spoke.
func TestRun_PublishesTheThinkingOfTheChatModel(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("<think>the commit comes first</think>", "c1", toolResume,
			map[string]any{})},
	}}
	_, op := eventsRun(t, provider)

	thinking := op.EmittedOf(messages.OutThinking)
	if len(thinking) != 1 {
		t.Fatalf("the run published %d thinking events, want one: %v", len(thinking), op.Emitted())
	}
	if thinking[0].Text != "the commit comes first" {
		t.Fatalf("the thinking is %q, want the scratchpad of the model", thinking[0].Text)
	}
	for _, text := range replies(op) {
		if strings.Contains(text, "the commit comes first") {
			t.Fatalf("the reply carries the thinking: %q", text)
		}
	}
}

// TestRun_PublishesTheReplyWithoutTheInlineTags — a model that writes
// its scratchpad into its own text does not make the reply show the
// tags.
func TestRun_PublishesTheReplyWithoutTheInlineTags(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("<think>the commit comes first</think>", "c1", toolResume,
			map[string]any{})},
	}}
	_, op := eventsRun(t, provider)

	found := false
	for _, text := range replies(op) {
		if strings.Contains(text, "<think>") {
			t.Fatalf("the reply carries the tags: %q", text)
		}
		if text == "" {
			t.Fatalf("the run published an empty reply: %v", replies(op))
		}
		found = true
	}
	if !found {
		t.Fatalf("the run published no reply: %v", replies(op))
	}
}

// TestRun_PublishesItsToolCalls — the call goes out before it runs and
// the result comes back when it settles.
func TestRun_PublishesItsToolCalls(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	_, op := eventsRun(t, provider)

	starts := op.EmittedOf(messages.OutToolStart)
	if len(starts) == 0 {
		t.Fatalf("the run published no tool call: %v", op.Emitted())
	}
	if starts[0].Tool == nil {
		t.Fatal("the tool call carries no payload")
	}
	if starts[0].Tool.Name != toolResume {
		t.Fatalf("the tool = %q, want %q", starts[0].Tool.Name, toolResume)
	}
	if starts[0].Tool.Args != "" {
		t.Fatalf("the argument line = %q, want none", starts[0].Tool.Args)
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
	if ends[0].Tool.Name != toolResume {
		t.Fatalf("the tool = %q, want %q", ends[0].Tool.Name, toolResume)
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
		{Message: toolCallMsg("c1", toolWait, map[string]any{"duration": "soon"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the duration is not a duration"})},
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
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The first command does not settle: the run reaches a judgment
	// point and the model runs the step again. The second command
	// never settles: the call has no verdict.
	op.steps = []fakeStep{pendingStep("still going"), {open: true}}
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
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

// TestRun_HangsItsToolCallsUnderTheDecisionCard — the call and its
// result hang under the card of the decision that made them.
func TestRun_HangsItsToolCallsUnderTheDecisionCard(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	_, op := eventsRun(t, provider)

	starts := op.EmittedOf(messages.OutToolStart)
	ends := op.EmittedOf(messages.OutToolEnd)
	if len(starts) == 0 || len(ends) == 0 {
		t.Fatalf("the run published %d calls and %d results", len(starts), len(ends))
	}
	if starts[0].ReplyTo != ends[0].ReplyTo {
		t.Fatalf("the call hangs under %q and its result under %q", starts[0].ReplyTo, ends[0].ReplyTo)
	}
	cards := map[string]bool{}
	for _, c := range op.Cards() {
		cards[c.ID] = true
	}
	if !cards[starts[0].ReplyTo] {
		t.Fatalf("the call hangs under %q, want the card of its session", starts[0].ReplyTo)
	}
}

// TestRun_KeepsTheCallAndItsResultUnderOneCard — the run may move on
// while a call runs. The call and its result still hang under the card
// the call started under: the chat pairs them by their anchor.
func TestRun_KeepsTheCallAndItsResultUnderOneCard(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{
		Provider: &scriptedProvider{err: errors.New("the chat model must not be called")},
	})
	// The command never settles: the run stays in the chat.
	op.steps = []fakeStep{{open: true}}
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	r := chatRun(t, svc, "obs-1")
	svc.openDecisionSession(context.Background(), r)
	sess := r.decisionSessionRef()
	if sess == nil {
		t.Fatal("the run has no decision session")
	}

	call := pendingCall{ID: "c1", Name: toolResume, Args: json.RawMessage(`{}`)}
	svc.publishToolStart(context.Background(), r, sess, call)
	// The run opens another session while the call runs.
	svc.openDecisionSession(context.Background(), r)
	svc.publishToolEnd(context.Background(), r, sess, call,
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

// TestRun_PublishesEveryReplyOfTheDecision — every reply of a decision
// is shown: the number of replies is not capped.
func TestRun_PublishesEveryReplyOfTheDecision(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{})

	// The decision keeps every reply it got: the number of them is
	// not capped.
	r := &run{svc: svc, id: "one-1"}
	r.setDecisionSession(&session{kind: sessionDecision, id: "decision-1", anchor: "card-9"})
	for i := 0; i < 5; i++ {
		svc.recordReply(context.Background(), r, fmt.Sprintf("reply %d", i))
	}
	published := 0
	for _, m := range op.EmittedOf(messages.OutReply) {
		if m.ReplyTo == "card-9" {
			published++
		}
	}
	if published != 5 {
		t.Fatalf("the decision published %d replies, want 5", published)
	}
}

// decisionReplies returns the entries of the decision sessions.
func decisionReplies(op *fakeOperator) []string {
	out := make([]string, 0, 8)
	for _, m := range op.EmittedOf(messages.OutReply) {
		if strings.HasPrefix(m.SessionID, "decision-") {
			out = append(out, m.Text)
		}
	}
	return out
}

// waitDecisionReplies waits until the run published that many replies
// on its decision sessions.
func waitDecisionReplies(t *testing.T, op *fakeOperator, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(decisionReplies(op)) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run published %d replies, want %d", len(decisionReplies(op)), n)
}

// TestRun_CountsItsActivityEvenWhenTheChatHidesIt — the counts are
// the run's own: a chat that hides the run's events does not change
// what the run did.
func TestRun_CountsItsActivityEvenWhenTheChatHidesIt(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolResume, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	// The chat hides everything the run publishes.
	op.dropOutbound = true
	// The first command waits: the run reaches the judgment point.
	// The second never settles: the run stays in the chat and the
	// counters can be read.
	op.steps = []fakeStep{pendingStep("still going"), {open: true}}
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitSent(t, op, "/gtw commit")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := chatRun(t, svc, "obs-1")
		r.mu.Lock()
		think, tools := r.thinkCount, r.toolCount
		r.mu.Unlock()
		if think == 1 && tools == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the run counted nothing")
}

// TestTheStateOfAnOlderDaemonStillNamesTheCards — a state file of a
// daemon that predates the anchor names the card of a session: the
// run that comes back goes on with the card it opened.
func TestTheStateOfAnOlderDaemonStillNamesTheCards(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	legacy := &sessionState{Kind: string(sessionRound), CardID: "card-9", ID: "main · round 1", Job: "main", Round: 1}
	sess := sessionFromState(legacy)
	if sess.anchor != "card-9" {
		t.Errorf("the session hangs under %q, want the card of the older daemon", sess.anchor)
	}
	if !sess.ownsMessage {
		t.Error("the session does not own the card it posted itself")
	}
	// A state of the current shape keeps its anchor.
	current := &sessionState{Kind: string(sessionRound), Anchor: "om_start", Owns: false}
	sess = sessionFromState(current)
	if sess.anchor != "om_start" || sess.ownsMessage {
		t.Errorf("the session hangs under %q and owns %v, want om_start and no ownership",
			sess.anchor, sess.ownsMessage)
	}
}

// TestTheActivityOfTheRunSurvivesTheDaemon — the counters, the time of
// the last activity and the open sessions are persisted with the rest
// of the run.
func TestTheActivityOfTheRunSurvivesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textAndToolCall("which branch?", "c1", toolAskHuman,
			map[string]any{"question": "Which branch?"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work", MessageID: "om_start",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitAsked(t, op, "Which branch?")
	waitStateFile(t, dir)

	r := chatRun(t, svc, "obs-1")
	raw := readStateFile(t, filepath.Join(dir, "runs", r.id+".json"))
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
	if state.RunSession == nil || state.RunSession.Anchor != "om_start" {
		t.Errorf("the state file holds no anchor of the run: %+v", state.RunSession)
	}
	if state.RunSession.Owns {
		t.Error("the run owns the message that started it")
	}
	if state.RoundSession == nil || state.RoundSession.Anchor == "" {
		t.Error("the state file holds no card of the round")
	}
	if state.DecisionSession == nil || state.DecisionSession.Anchor == "" {
		t.Error("the state file holds no card of the decision")
	}

	restored := svc.restoreRun(state)
	if restored.thinkCount != 1 || restored.toolCount != 1 {
		t.Errorf("the restored run holds %d model turns and %d tool calls, want 1 and 1",
			restored.thinkCount, restored.toolCount)
	}
	if !restored.lastBeatAt.Equal(state.LastBeatAt) {
		t.Errorf("the restored run stands at %v, want %v", restored.lastBeatAt, state.LastBeatAt)
	}
	if restored.runSessionRef() == nil || restored.runSessionRef().anchor != state.RunSession.Anchor {
		t.Error("the restored run lost the anchor of the run")
	}
	if restored.runSessionRef().ownsMessage {
		t.Error("the restored run owns the message that started it")
	}
	if restored.decisionSessionRef() == nil || restored.decisionSessionRef().anchor != state.DecisionSession.Anchor {
		t.Error("the restored run lost the card of the decision")
	}
}
