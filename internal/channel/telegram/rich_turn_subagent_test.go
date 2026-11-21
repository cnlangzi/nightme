// Package telegram — the turn card's background-subagent line.
//
// The agent CLI reports the background subagents it is running; the
// turn card shows how many of them are still going and drops the line
// when the last one finished.
package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// subagentLine returns the background-subagent line of a rendered
// block array, if the card carries one.
func subagentLine(t *testing.T, body string) string {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(body), &blocks); err != nil {
		t.Fatalf("render output must be valid JSON: %v\nbody=%s", err, body)
	}
	for _, b := range blocks {
		if b["type"] != "paragraph" {
			continue
		}
		text, _ := b["text"].(string)
		if strings.Contains(text, "🔀") {
			return text
		}
	}
	return ""
}

// TestRenderRichTurnBlocks_SubagentRunningLine: while at least one
// background subagent runs, the card carries a line with the count.
func TestRenderRichTurnBlocks_SubagentRunningLine(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:     "123",
		messageID:  100,
		headerLine: "💭 1",
		hasContent: true,
		subagents: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", TaskType: "local_agent", Status: agent.BgTaskStatusRunning},
			{TaskID: "a2", Description: "two", TaskType: "local_agent", Status: agent.BgTaskStatusRunning},
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := subagentLine(t, body); got != "🔀 2 个后台 agent 在跑" {
		t.Errorf("subagent line = %q, want the running count", got)
	}
}

// TestRenderRichTurnBlocks_SubagentLineGoneWhenNoneRunning: the empty
// table is the signal to drop the line.
func TestRenderRichTurnBlocks_SubagentLineGoneWhenNoneRunning(t *testing.T) {
	a, _ := newTestAdapter(t)
	turn := &richTurn{
		chatID:     "123",
		messageID:  100,
		headerLine: "💭 1",
		hasContent: true,
		subagents: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", Status: agent.BgTaskStatusCompleted},
		},
	}
	body, err := a.renderRichTurnBlocksLocked(turn)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := subagentLine(t, body); got != "" {
		t.Errorf("subagent line = %q, want none (no task is running)", got)
	}
}

// TestOnPromptEndedRichTurn_DropsTheRunningLine: the line belongs to
// the turn. A turn that ended while a subagent was still running (an
// error turn, /stop, a dead CLI) must not leave the line on the
// finished message — no later frame can reach that turn, the anchor
// is gone with the prompt.
func TestOnPromptEndedRichTurn_DropsTheRunningLine(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	if _, err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_100",
		Kind:    messages.OutReply,
		Text:    "working on it",
		ReplyTo: "77",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}
	if _, err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_100",
		Kind:    messages.OutSubagents,
		ReplyTo: "77",
		Subagents: &agent.SubagentEvent{Tasks: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", Status: agent.BgTaskStatusRunning},
		}},
	}); err != nil {
		t.Fatalf("OutSubagents Send: %v", err)
	}

	api.Calls = api.Calls[:0]
	turn, ok := a.richTurns.lookup("100", 0, 77)
	if !ok || turn == nil {
		t.Fatal("no rich turn for the message")
	}
	if err := a.flushRichTurn(context.Background(), turn); err != nil {
		t.Fatalf("flushRichTurn: %v", err)
	}
	blocks, ok := lastEditBlocks(api.snapshotCalls())
	if !ok {
		t.Fatal("expected an editMessageText from the flush; got none")
	}
	found := false
	for _, b := range blocks {
		text, _ := b["text"].(string)
		if strings.Contains(text, "🔀") {
			found = true
		}
	}
	if !found {
		t.Errorf("the running line is missing from the card: %v", blocks)
	}

	// The turn ends while the subagent is still running: the line
	// belongs to the turn, so the finished message does not keep it.
	api.Calls = api.Calls[:0]
	a.OnPromptEndedRichTurn("100", 0, 77)

	blocks, ok = lastEditBlocks(api.snapshotCalls())
	if !ok {
		t.Fatal("expected an editMessageText from the synchronous flush; got none")
	}
	for _, b := range blocks {
		text, _ := b["text"].(string)
		if strings.Contains(text, "🔀") {
			t.Errorf("the finished message still carries the running line: %q", text)
		}
	}
}

// TestFlushRichTurn_IdenticalBody_SkipsTheEdit: the rich message is
// the full turn transcript, so a state-only event (the
// background-subagent line, whose count did not move) would otherwise
// re-PATCH an identical body on every frame — and Telegram
// rate-limits the edits per chat.
func TestFlushRichTurn_IdenticalBody_SkipsTheEdit(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, UserMessageID: "77"})

	if _, err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_100",
		Kind:    messages.OutReply,
		Text:    "working on it",
		ReplyTo: "77",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}

	api.Calls = api.Calls[:0]
	a.OnPromptEndedRichTurn("100", 0, 77)
	first := len(api.snapshotCalls())
	if first == 0 {
		t.Fatal("expected an editMessageText from the first flush; got none")
	}

	// The same state again: the rendered body did not change, so no
	// edit goes out.
	api.Calls = api.Calls[:0]
	a.OnPromptEndedRichTurn("100", 0, 77)
	if got := len(api.snapshotCalls()); got != 0 {
		t.Errorf("editMessageText calls = %d, want 0 (the body did not change)", got)
	}
}
