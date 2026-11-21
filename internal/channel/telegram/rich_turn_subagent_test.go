// Package telegram — the turn card's background-subagent line.
//
// The agent CLI reports the background subagents it is running; the
// turn card shows how many of them are still going and drops the line
// when the last one finished.
package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
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
