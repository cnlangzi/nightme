package registry

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRegistryChatSessionEntry_SelectedFieldsPersist — the post-rename
// schema persists SelectedCwd / SelectedAgent / SelectedAgentSessionID
// as `selectedCwd` / `selectedAgent` / `selectedAgentSessionId` JSON
// keys (not the legacy `active*` names). Regression guard for the
// multi-as Phase 1 active→selected rename.
func TestRegistryChatSessionEntry_SelectedFieldsPersist(t *testing.T) {
	id := "as_x1"
	e := ChatSessionEntry{
		ID:                   "cs_x",
		ChatID:               "oc_x",
		SelectedCwd:          "/code/x",
		SelectedAgent:        "codex",
		SelectedAgentSessionID: &id,
		PrimaryAgent:         "codex",
		CreatedAt:            time.Now(),
		LastInteractionAt:    time.Now(),
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Legacy keys must be absent.
	for _, legacy := range []string{"activeCwd", "activeAgent", "activeAgentSessionId"} {
		if containsKey(data, legacy) {
			t.Errorf("marshalled JSON should not contain legacy %q key: %s", legacy, data)
		}
	}
	// Canonical keys must be present.
	for _, canonical := range []string{"selectedCwd", "selectedAgent", "selectedAgentSessionId"} {
		if !containsKey(data, canonical) {
			t.Errorf("marshalled JSON should contain %q key: %s", canonical, data)
		}
	}
}

// TestRegistryChatSessionEntry_SelectedFieldsRoundTrip — marshal +
// unmarshal preserves the selected* fields byte-for-byte (modulo
// JSON encoding quirks).
func TestRegistryChatSessionEntry_SelectedFieldsRoundTrip(t *testing.T) {
	id := "as_y2"
	original := ChatSessionEntry{
		ID:                   "cs_y",
		ChatID:               "oc_y",
		SelectedCwd:          "/code/y",
		SelectedAgent:        "claude",
		SelectedAgentSessionID: &id,
		PrimaryAgent:         "claude",
		CreatedAt:            time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		LastInteractionAt:    time.Date(2026, 8, 9, 12, 5, 0, 0, time.UTC),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ChatSessionEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.SelectedCwd != original.SelectedCwd {
		t.Errorf("SelectedCwd: got %q, want %q", got.SelectedCwd, original.SelectedCwd)
	}
	if got.SelectedAgent != original.SelectedAgent {
		t.Errorf("SelectedAgent: got %q, want %q", got.SelectedAgent, original.SelectedAgent)
	}
	if got.SelectedAgentSessionID == nil || *got.SelectedAgentSessionID != *original.SelectedAgentSessionID {
		t.Errorf("SelectedAgentSessionID: got %v, want %q", got.SelectedAgentSessionID, *original.SelectedAgentSessionID)
	}
}