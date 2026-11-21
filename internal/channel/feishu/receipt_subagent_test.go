// Package feishu — the receipt card's background-subagent line.
//
// The agent CLI reports the background subagents it is running; the
// receipt shows how many of them are still going and drops the line
// when the last one finished.
package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestSetSubagents_RunningCountOnTheCard: while at least one
// background subagent runs, the card carries a line with the count.
func TestSetSubagents_RunningCountOnTheCard(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", bot)
	r.cardMsgID = "om_card"

	err := r.SetSubagents(context.Background(), &agent.SubagentEvent{
		Tasks: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", TaskType: "local_agent", Status: agent.BgTaskStatusRunning},
			{TaskID: "a2", Description: "two", TaskType: "local_agent", Status: agent.BgTaskStatusRunning},
		},
	})
	if err != nil {
		t.Fatalf("SetSubagents: %v", err)
	}

	bodies := bot.patchBodies()
	if len(bodies) == 0 {
		t.Fatal("no PATCH issued — the card was not updated")
	}
	body := bodies[len(bodies)-1]
	if !strings.Contains(body, "🔀 2 个后台 agent 在跑") {
		t.Errorf("card body missing the running count: %s", body)
	}
}

// TestSetSubagents_LineGoneWhenTheLastOneFinished: the empty table is
// the signal to drop the line.
func TestSetSubagents_LineGoneWhenTheLastOneFinished(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", bot)
	r.cardMsgID = "om_card"

	if err := r.SetSubagents(context.Background(), &agent.SubagentEvent{
		Tasks: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", Status: agent.BgTaskStatusRunning},
		},
	}); err != nil {
		t.Fatalf("SetSubagents: %v", err)
	}
	if err := r.SetSubagents(context.Background(), &agent.SubagentEvent{
		Tasks: []agent.SubagentTask{},
	}); err != nil {
		t.Fatalf("SetSubagents (empty): %v", err)
	}

	bodies := bot.patchBodies()
	if len(bodies) == 0 {
		t.Fatal("no PATCH issued — the card was not updated")
	}
	body := bodies[len(bodies)-1]
	if strings.Contains(body, "🔀") {
		t.Errorf("card body still carries the running line: %s", body)
	}
}

// TestSetSubagents_TerminalTasksAreNotCounted: a task that finished
// is in the table but is not running.
func TestSetSubagents_TerminalTasksAreNotCounted(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", bot)
	r.cardMsgID = "om_card"

	err := r.SetSubagents(context.Background(), &agent.SubagentEvent{
		Tasks: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", Status: agent.BgTaskStatusRunning},
			{TaskID: "a2", Description: "two", Status: agent.BgTaskStatusCompleted},
		},
	})
	if err != nil {
		t.Fatalf("SetSubagents: %v", err)
	}

	bodies := bot.patchBodies()
	if len(bodies) == 0 {
		t.Fatal("no PATCH issued — the card was not updated")
	}
	body := bodies[len(bodies)-1]
	if !strings.Contains(body, "🔀 1 个后台 agent 在跑") {
		t.Errorf("card body does not report the single running subagent: %s", body)
	}
}

// TestSetSubagents_NilPayloadClearsTheLine: a nil payload clears the
// receipt's view of the table.
func TestSetSubagents_NilPayloadClearsTheLine(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", bot)
	r.cardMsgID = "om_card"

	if err := r.SetSubagents(context.Background(), &agent.SubagentEvent{
		Tasks: []agent.SubagentTask{
			{TaskID: "a1", Description: "one", Status: agent.BgTaskStatusRunning},
		},
	}); err != nil {
		t.Fatalf("SetSubagents: %v", err)
	}
	if err := r.SetSubagents(context.Background(), nil); err != nil {
		t.Fatalf("SetSubagents (nil): %v", err)
	}

	r.mu.Lock()
	n := len(r.subagents)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("len(subagents) = %d, want 0", n)
	}
}
