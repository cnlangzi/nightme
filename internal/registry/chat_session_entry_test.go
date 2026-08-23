package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatSessionEntry_ThinkModeRoundTrip ensures the field
// survives JSON marshal / unmarshal. Critical for restart
// semantics: an opted-in /think on must persist across
// `nightme run` restart.
//
// Field types are bare int on the registry side; the enum types
// (chatsession.ThinkMode / chatsession.WatchMode) encode the same
// numeric values, so this round-trip stays compatible with both
// the legacy registry-typed files and the new int-backed files.
func TestChatSessionEntry_ThinkModeRoundTrip(t *testing.T) {
	entry := ChatSessionEntry{
		ID:        "cs_oc_x",
		ChatID:    "oc_x",
		WatchMode: 1, // == chatsession.WatchModeAll
		ThinkMode: 1, // == chatsession.ThinkModeShow (opt-in /think on)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ChatSessionEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ThinkMode != 1 {
		t.Errorf("round-trip ThinkMode = %d, want 1 (ThinkModeShow)", got.ThinkMode)
	}
	if got.WatchMode != 1 {
		t.Errorf("round-trip WatchMode = %d, want 1 (WatchModeAll)", got.WatchMode)
	}
}

// TestChatSessionEntry_MissingThinkModeDefaultsToHide mirrors the
// forward-compat invariant: older chat_sessions.json files written
// before F-think lack the thinkMode field. Go's zero-value
// semantics must give them 0 == chatsession.ThinkModeHide (the
// current default; off by default).
func TestChatSessionEntry_MissingThinkModeDefaultsToHide(t *testing.T) {
	// Hand-rolled JSON without thinkMode.
	raw := []byte(`{
		"id": "cs_oc_x",
		"chatId": "oc_x",
		"activeCwd": "/tmp",
		"activeAgent": "claude",
		"primaryAgent": "claude",
		"watchMode": 1
	}`)
	var got ChatSessionEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ThinkMode != 0 {
		t.Errorf("missing-thinkMode default = %d, want 0 (ThinkModeHide)", got.ThinkMode)
	}
	if got.WatchMode != 1 {
		t.Errorf("WatchMode round-trip = %d, want 1 (WatchModeAll)", got.WatchMode)
	}
}

// TestChatSessionEntry_ThinkModeOmittedFromZeroValue locks the
// on-disk file size invariant: the zero value (chatsession.
// ThinkModeHide default) must NOT be written to disk. The
// `omitempty` JSON tag must skip it so old-format files (no
// thinkMode key) and new-format files (also no thinkMode key for
// default-mode chats) are byte-identical. This keeps the
// "missing field == zero == ThinkModeHide" invariant robust
// across upgrades.
func TestChatSessionEntry_ThinkModeOmittedFromZeroValue(t *testing.T) {
	entry := ChatSessionEntry{
		ID:        "cs_oc_x",
		ChatID:    "oc_x",
		WatchMode: 0, // == chatsession.WatchModeMention (zero, omitted)
		ThinkMode: 0, // == chatsession.ThinkModeHide (zero, omitted)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "thinkMode") {
		t.Errorf("marshalled JSON should omit zero-value thinkMode: %s", data)
	}
	if strings.Contains(string(data), "watchMode") {
		t.Errorf("marshalled JSON should omit zero-value watchMode: %s", data)
	}
}
