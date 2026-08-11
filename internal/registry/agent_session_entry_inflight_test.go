package registry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestAgentSessionEntry_InFlightMessagesRoundTrip pins the contract
// that InFlightMessages survives a JSON round-trip verbatim (one or
// many refs, identical blocks).
func TestAgentSessionEntry_InFlightMessagesRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	entry := AgentSessionEntry{
		ID:            "as_1",
		ChatSessionID: "cs_1",
		Agent:         "codex",
		Cwd:           "/x",
		Status:        StatusRunning,
		SessionID:     "sess-1",
		InFlightMessages: []InFlightMessageRef{
			{
				ID: "m_1",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "first"},
				},
				ReceivedAt: now,
			},
			{
				ID: "m_2",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "second"},
				},
				ReceivedAt: now.Add(time.Second),
			},
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AgentSessionEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.InFlightMessages) != 2 {
		t.Fatalf("InFlightMessages len = %d, want 2", len(got.InFlightMessages))
	}
	if got.InFlightMessages[0].ID != "m_1" || got.InFlightMessages[1].ID != "m_2" {
		t.Errorf("ids = [%s, %s], want [m_1, m_2]",
			got.InFlightMessages[0].ID, got.InFlightMessages[1].ID)
	}
	if got.InFlightMessages[0].Blocks[0].Text != "first" {
		t.Errorf("blocks[0] text = %q, want %q",
			got.InFlightMessages[0].Blocks[0].Text, "first")
	}
}

// TestAgentSessionEntry_LegacyFileWithoutInFlightMessages confirms
// that an entry written before the InFlightMessages field existed
// loads cleanly with InFlightMessages == nil (no migration needed).
func TestAgentSessionEntry_LegacyFileWithoutInFlightMessages(t *testing.T) {
	legacyJSON := []byte(`{
		"id":"as_legacy",
		"chatSessionId":"cs_legacy",
		"agent":"codex",
		"cwd":"/legacy",
		"pid":0,
		"status":"exited",
		"createdAt":"2026-01-01T00:00:00Z",
		"lastRunAt":"2026-01-01T00:00:00Z",
		"sessionId":"sess-legacy"
	}`)
	var got AgentSessionEntry
	if err := json.Unmarshal(legacyJSON, &got); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if got.InFlightMessages != nil {
		t.Errorf("legacy InFlightMessages = %v, want nil", got.InFlightMessages)
	}
	if got.SessionID != "sess-legacy" {
		t.Errorf("legacy SessionID = %q, want %q", got.SessionID, "sess-legacy")
	}
}

// TestAgentSessionEntry_EmptyInFlightOmitsFromJSON asserts the
// `omitempty` tag keeps InFlightMessages off the wire when nil/empty,
// preserving on-disk compatibility.
func TestAgentSessionEntry_EmptyInFlightOmitsFromJSON(t *testing.T) {
	entry := AgentSessionEntry{
		ID:            "as_empty",
		ChatSessionID: "cs_empty",
		Agent:         "codex",
		Cwd:           "/empty",
		Status:        StatusDetached,
		CreatedAt:     time.Unix(0, 0).UTC(),
		LastRunAt:     time.Unix(0, 0).UTC(),
		// InFlightMessages intentionally nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "inFlightMessages") {
		t.Errorf("marshaled JSON contains inFlightMessages key when nil: %s", data)
	}
}
