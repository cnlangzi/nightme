package messages

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestStampRunResult_PopulatesAllFields(t *testing.T) {
	msg := &OutboundMessage{
		ChatID:  "tg_1",
		ReplyTo: "m1",
		Text:    "review body",
	}
	result := agent.RunResult{
		Model:     "MiniMax-M3",
		SessionID: "sess-1",
		Usage: &agent.UsageInfo{
			InputTokens:  12300,
			OutputTokens: 400,
			CostUSD:      0.012,
		},
	}

	StampRunResult(msg, "codex", result)

	if msg.AgentName != "codex" {
		t.Errorf("AgentName = %q, want codex", msg.AgentName)
	}
	if msg.Model != "MiniMax-M3" {
		t.Errorf("Model = %q, want MiniMax-M3", msg.Model)
	}
	if msg.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", msg.SessionID)
	}
	if msg.Usage == nil {
		t.Fatal("Usage = nil, want from result")
	}
	if msg.Usage.InputTokens != 12300 || msg.Usage.OutputTokens != 400 || msg.Usage.CostUSD != 0.012 {
		t.Errorf("Usage fields not propagated: %+v", msg.Usage)
	}
}

func TestStampRunResult_NilUsageLeavesFieldNil(t *testing.T) {
	msg := &OutboundMessage{}
	StampRunResult(msg, "claude", agent.RunResult{Text: "ok", Model: "m"})

	if msg.Usage != nil {
		t.Errorf("Usage = %+v, want nil when result.Usage is nil", msg.Usage)
	}
	if msg.AgentName != "claude" {
		t.Errorf("AgentName = %q, want claude", msg.AgentName)
	}
	if msg.Model != "m" {
		t.Errorf("Model = %q, want m", msg.Model)
	}
}

func TestStampRunResult_PreStampedFieldsRespected(t *testing.T) {
	// Bridges that pre-stamp Model / SessionID on the
	// OutboundMessage (e.g. dispatcher that already saw the
	// bridge wire frame) should keep their values.
	msg := &OutboundMessage{
		Model:     "pre-stamped",
		SessionID: "pre-sid",
	}
	StampRunResult(msg, "codex", agent.RunResult{
		Model:     "from-result",
		SessionID: "from-result-sid",
	})

	if msg.Model != "pre-stamped" {
		t.Errorf("Model overwritten: got %q, want pre-stamped", msg.Model)
	}
	if msg.SessionID != "pre-sid" {
		t.Errorf("SessionID overwritten: got %q, want pre-sid", msg.SessionID)
	}
	if msg.AgentName != "codex" {
		t.Errorf("AgentName = %q, want codex (runnerName is always set)", msg.AgentName)
	}
}
