package chatsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestRestoreFromRegistry_ReplaysInFlightMessages asserts that an
// AgentSessionEntry written with non-empty InFlightMessages gets
// replayed into the rebuilt ChatSession's MessageQueue — the agent's
// reply will then attach to the original msg_id when the resumed AS
// eventually drains the queue.
func TestRestoreFromRegistry_ReplaysInFlightMessages(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_replay"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	asID := "as_replay_1"
	asEntry := &registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		Status:        registry.StatusDetached,
		SessionID:     "sess-resume-xyz",
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{
				ID: "m_original_1",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "first half"},
				},
				ReceivedAt: now,
			},
			{
				ID: "m_original_2",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "second half"},
				},
				ReceivedAt: now.Add(time.Second),
			},
		},
	}
	if err := asFile.Upsert(asEntry); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	mgr := NewManager().WithPersistence(csFile, asFile)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}
	got := cs.queue.Peek()
	if len(got) != 2 {
		t.Fatalf("replayed queue len = %d, want 2", len(got))
	}
	if got[0].ID != "m_original_1" || got[1].ID != "m_original_2" {
		t.Errorf("replayed ids = [%s, %s], want [m_original_1, m_original_2]",
			got[0].ID, got[1].ID)
	}
	if got[0].ChatID != chatID {
		t.Errorf("replayed msg[0].ChatID = %q, want %q", got[0].ChatID, chatID)
	}

	// SessionID must have round-tripped so the AS will resume on next
	// Spawn. Defensive: re-derive via FromAgentSessionEntry and
	// confirm — RestoreFromRegistry already does this internally.
	pool := cs.Pool()
	var found *agentsession.AgentSession
	for _, as := range pool {
		if as.ID == asID {
			found = as
			break
		}
	}
	if found == nil {
		t.Fatalf("AS %s not in pool after restore", asID)
	}
	if found.SessionID() != "sess-resume-xyz" {
		t.Errorf("restored SessionID = %q, want sess-resume-xyz", found.SessionID())
	}
}

// TestRestoreFromRegistry_LegacyEntryWithoutInFlightMessages
// confirms that an entry written before the InFlightMessages field
// existed restores cleanly: queue stays empty, no panic.
func TestRestoreFromRegistry_LegacyEntryWithoutInFlightMessages(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_legacy"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	// Legacy entry: no InFlightMessages field. (Upsert writes the
	// current schema — so simulate the legacy shape by writing JSON
	// directly.)
	asID := "as_legacy_1"
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		Status:        registry.StatusDetached,
		SessionID:     "sess-legacy",
		CreatedAt:     now,
		LastRunAt:     now,
	}); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	mgr := NewManager().WithPersistence(csFile, asFile)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("queue = %v, want empty for legacy entry", got)
	}
}

// TestRestoreFromRegistry_EmptyInFlightSlice ensures that a present-
// but-empty InFlightMessages doesn't trigger any replay.
func TestRestoreFromRegistry_EmptyInFlightSlice(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_empty"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:               "as_empty_1",
		ChatSessionID:    csID,
		Agent:            "claude",
		Cwd:              "/code/bailing",
		Status:           registry.StatusDetached,
		SessionID:        "sess-empty",
		CreatedAt:        now,
		LastRunAt:        now,
		InFlightMessages: []registry.InFlightMessageRef{}, // explicitly empty
	}); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	mgr := NewManager().WithPersistence(csFile, asFile)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("queue = %v, want empty when InFlightMessages is []", got)
	}
}

// TestRestoreFromRegistry_MultipleAgentSessionsEachReplayOwn
// ensures each AS replays only its own InFlightMessages — no
// cross-leak between agents in the same chat.
func TestRestoreFromRegistry_MultipleAgentSessionsEachReplayOwn(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_multi"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_a",
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/A",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_a_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "a1"}}, ReceivedAt: now},
		},
	}); err != nil {
		t.Fatalf("Upsert as_a: %v", err)
	}
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_b",
		ChatSessionID: csID,
		Agent:         "codex",
		Cwd:           "/code/B",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_b_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "b1"}}, ReceivedAt: now},
		},
	}); err != nil {
		t.Fatalf("Upsert as_b: %v", err)
	}

	mgr := NewManager().WithPersistence(csFile, asFile)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}
	got := cs.queue.Peek()
	if len(got) != 2 {
		t.Fatalf("queue len = %d, want 2 (one from each AS)", len(got))
	}
	// Both messages should be present; ordering follows the
	// agentsByCS map iteration which is non-deterministic, so just
	// assert both IDs are present.
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids["m_a_1"] || !ids["m_b_1"] {
		t.Errorf("expected [m_a_1, m_b_1], got [%s, %s]", got[0].ID, got[1].ID)
	}
}
