package registry

import (
	"encoding/json"
	"testing"
	"time"
)

// TestChatSessionEntry_Unmarshal_LegacyDefaultAgent covers the
// v1.2-final on-disk migration: an old chat_sessions.json with the
// legacy `defaultAgent` field (v1.2-early naming) is transparently
// read into PrimaryAgent on Unmarshal. New writes only emit
// `primaryAgent`. The migration is one-shot — after the next write,
// the legacy field is gone from the file.
func TestChatSessionEntry_Unmarshal_LegacyDefaultAgent(t *testing.T) {
	legacy := `{
		"id": "cs_legacy",
		"chatId": "oc_legacy",
		"chatType": "p2p",
		"activeCwd": "/code/bailing",
		"activeAgent": "codex",
		"defaultAgent": "claude",
		"agentSessionIds": [],
		"createdAt": "2026-08-01T00:00:00Z",
		"lastInteractionAt": "2026-08-01T00:00:00Z"
	}`

	var e ChatSessionEntry
	if err := json.Unmarshal([]byte(legacy), &e); err != nil {
		t.Fatalf("Unmarshal legacy entry: %v", err)
	}

	if e.PrimaryAgent != "claude" {
		t.Fatalf("PrimaryAgent: got %q, want claude (migrated from legacy defaultAgent)", e.PrimaryAgent)
	}
	if e.SelectedAgent != "codex" {
		t.Fatalf("SelectedAgent: got %q, want codex (preserved from legacy)", e.SelectedAgent)
	}
}

// TestChatSessionEntry_Unmarshal_CanonicalPrimaryAgent covers the
// happy path: new-format entry with `primaryAgent` is read as-is;
// no migration needed.
func TestChatSessionEntry_Unmarshal_CanonicalPrimaryAgent(t *testing.T) {
	canonical := `{
		"id": "cs_new",
		"chatId": "oc_new",
		"chatType": "p2p",
		"activeCwd": "/code/bailing",
		"activeAgent": "claude",
		"primaryAgent": "claude",
		"agentSessionIds": [],
		"createdAt": "2026-08-03T00:00:00Z",
		"lastInteractionAt": "2026-08-03T00:00:00Z"
	}`

	var e ChatSessionEntry
	if err := json.Unmarshal([]byte(canonical), &e); err != nil {
		t.Fatalf("Unmarshal canonical entry: %v", err)
	}

	if e.PrimaryAgent != "claude" {
		t.Fatalf("PrimaryAgent: got %q, want claude", e.PrimaryAgent)
	}
}

// TestChatSessionEntry_RoundTrip_NoLegacyField asserts that after
// Marshal → Unmarshal, the legacy `defaultAgent` field is absent
// from the on-disk form (one-shot migration).
func TestChatSessionEntry_RoundTrip_NoLegacyField(t *testing.T) {
	e := ChatSessionEntry{
		ID:           "cs_x",
		ChatID:       "oc_x",
		SelectedCwd:    "/code/x",
		SelectedAgent:  "claude",
		PrimaryAgent: "claude",
		CreatedAt:    mustTime("2026-08-03T00:00:00Z"),
		LastInteractionAt: mustTime("2026-08-03T00:00:00Z"),
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Confirm the marshalled form does NOT contain the legacy key.
	if containsKey(data, "defaultAgent") {
		t.Fatalf("marshalled JSON should not contain legacy \"defaultAgent\" key: %s", data)
	}
	// And DOES contain the canonical key.
	if !containsKey(data, "primaryAgent") {
		t.Fatalf("marshalled JSON should contain \"primaryAgent\" key: %s", data)
	}
}

func mustTime(s string) time.Time {
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tt
}

func containsKey(data []byte, key string) bool {
	// Lightweight substring check for "key" appearing as a JSON key
	// (e.g. `"key":`). Sufficient for our migration test; not a
	// general JSON validator.
	want := `"` + key + `":`
	for i := 0; i+len(want) <= len(data); i++ {
		if string(data[i:i+len(want)]) == want {
			return true
		}
	}
	return false
}