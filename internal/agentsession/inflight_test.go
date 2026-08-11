package agentsession

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestEntry_IncludesInFlightMessages asserts the snapshot taken by
// Entry() picks up the in-memory InFlightMessages field. This is the
// field the chat layer relies on to drive restart-replay.
func TestEntry_IncludesInFlightMessages(t *testing.T) {
	as := newTestAgentSession()

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	as.asMu.Lock()
	as.inFlightMessages = []registry.InFlightMessageRef{
		{
			ID:         "m_1",
			Blocks:     []agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}},
			ReceivedAt: now,
		},
	}
	as.asMu.Unlock()

	entry := as.Entry()
	if len(entry.InFlightMessages) != 1 {
		t.Fatalf("entry.InFlightMessages len = %d, want 1", len(entry.InFlightMessages))
	}
	if entry.InFlightMessages[0].ID != "m_1" {
		t.Errorf("entry.InFlightMessages[0].ID = %q, want m_1", entry.InFlightMessages[0].ID)
	}
}

// TestFromAgentSessionEntry_HydratesInFlightMessages asserts the
// registry → in-memory path: FromAgentSessionEntry must rebuild
// inFlightMessages from e.InFlightMessages. This is what makes
// restart-replay possible.
func TestFromAgentSessionEntry_HydratesInFlightMessages(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	entry := &registry.AgentSessionEntry{
		ID:            "as_hydrate",
		ChatSessionID: "cs_hydrate",
		Agent:         "codex",
		Cwd:           "/x",
		Status:        registry.StatusDetached,
		SessionID:     "sess-hydrate",
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "a"}}, ReceivedAt: now},
			{ID: "m_2", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "b"}}, ReceivedAt: now},
		},
	}

	as := FromAgentSessionEntry(entry)
	if len(as.inFlightMessages) != 2 {
		t.Fatalf("as.inFlightMessages len = %d, want 2", len(as.inFlightMessages))
	}
	if as.inFlightMessages[0].ID != "m_1" || as.inFlightMessages[1].ID != "m_2" {
		t.Errorf("ids = [%s, %s], want [m_1, m_2]",
			as.inFlightMessages[0].ID, as.inFlightMessages[1].ID)
	}

	// Defense-in-depth: mutating the entry after restore must not
	// affect the in-memory slice (we copied it).
	entry.InFlightMessages[0].ID = "MUTATED"
	if as.inFlightMessages[0].ID != "m_1" {
		t.Errorf("restored slice aliases entry: inFlightMessages[0].ID = %q after entry mutation",
			as.inFlightMessages[0].ID)
	}
}

// TestFromAgentSessionEntry_NilInFlightMessages asserts the legacy
// path (entry without the field) yields a nil in-memory slice.
func TestFromAgentSessionEntry_NilInFlightMessages(t *testing.T) {
	entry := &registry.AgentSessionEntry{
		ID:            "as_nil",
		ChatSessionID: "cs_nil",
		Agent:         "codex",
		Cwd:           "/x",
		Status:        registry.StatusDetached,
		CreatedAt:     time.Now(),
		LastRunAt:     time.Now(),
		// InFlightMessages intentionally nil
	}
	as := FromAgentSessionEntry(entry)
	if as.inFlightMessages != nil {
		t.Errorf("as.inFlightMessages = %v, want nil", as.inFlightMessages)
	}
}

// TestEntry_JsonRoundTrip covers the full Entry → JSON →
// FromAgentSessionEntry loop with a non-empty InFlightMessages.
// This is the disk persistence path.
func TestEntry_JsonRoundTrip(t *testing.T) {
	as := newTestAgentSession()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	as.asMu.Lock()
	as.inFlightMessages = []registry.InFlightMessageRef{
		{
			ID:         "m_rt",
			Blocks:     []agent.ContentBlock{{Type: agent.ContentText, Text: "round-trip"}},
			ReceivedAt: now,
		},
	}
	as.asMu.Unlock()

	entry := as.Entry()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded registry.AgentSessionEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.InFlightMessages) != 1 || decoded.InFlightMessages[0].ID != "m_rt" {
		t.Fatalf("round-trip lost data: %+v", decoded.InFlightMessages)
	}

	restored := FromAgentSessionEntry(&decoded)
	if len(restored.inFlightMessages) != 1 || restored.inFlightMessages[0].ID != "m_rt" {
		t.Fatalf("restored lost data: %+v", restored.inFlightMessages)
	}
}
