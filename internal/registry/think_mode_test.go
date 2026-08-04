package registry

import (
	"encoding/json"
	"testing"
)

// TestThinkMode_ZeroValueIsShow locks the safe-default invariant:
// ThinkMode(0) must stringify to "show" so /think reply text and
// log lines are never "unknown" for a fresh ChatSession.
func TestThinkMode_ZeroValueIsShow(t *testing.T) {
	var m ThinkMode
	if m != ThinkModeShow {
		t.Fatalf("zero-value ThinkMode = %d, want ThinkModeShow (%d)", m, ThinkModeShow)
	}
	if got := m.String(); got != "show" {
		t.Errorf("ThinkMode(0).String() = %q, want %q", got, "show")
	}
}

// TestThinkMode_String covers all enum values + the unknown guard.
func TestThinkMode_String(t *testing.T) {
	cases := []struct {
		m    ThinkMode
		want string
	}{
		{ThinkModeShow, "show"},
		{ThinkModeHide, "hide"},
		{ThinkMode(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("ThinkMode(%d).String() = %q, want %q", c.m, got, c.want)
		}
	}
}

// TestParseThinkMode_Aliases accepts both the slash-command form
// (on/off) and the semantic form (show/hide) for each direction.
// Either pair must map to the same enum value.
//
// Whitespace tolerance is the caller's responsibility — the
// /think handler invokes strings.TrimSpace(args[0]) before
// calling ParseThinkMode, mirroring /watch's handler pattern.
func TestParseThinkMode_Aliases(t *testing.T) {
	cases := []struct {
		in       string
		wantMode ThinkMode
		wantOK   bool
	}{
		{"on", ThinkModeShow, true},
		{"show", ThinkModeShow, true},
		{"off", ThinkModeHide, true},
		{"hide", ThinkModeHide, true},
	}
	for _, c := range cases {
		got, ok := ParseThinkMode(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseThinkMode(%q) ok = %v, want %v", c.in, ok, c.wantOK)
		}
		if ok && got != c.wantMode {
			t.Errorf("ParseThinkMode(%q) mode = %v, want %v", c.in, got, c.wantMode)
		}
	}
}

// TestParseThinkMode_UnknownRejects ensures unknown values fall
// through cleanly so the /think handler can reply with a usage
// hint instead of committing a state mutation.
func TestParseThinkMode_UnknownRejects(t *testing.T) {
	unknowns := []string{"", "maybe", "yes", "no", "true", "false", "ON", "Show"}
	for _, in := range unknowns {
		got, ok := ParseThinkMode(in)
		if ok {
			t.Errorf("ParseThinkMode(%q) ok=true, want false (got mode=%v)", in, got)
		}
		// Even on parse failure, the returned mode is the safe
		// default (ThinkModeShow) — the caller ignores it on
		// ok=false, but the function should still be total.
		if got != ThinkModeShow {
			t.Errorf("ParseThinkMode(%q) returned %v on failure, want ThinkModeShow", in, got)
		}
	}
}

// TestChatSessionEntry_ThinkModeRoundTrip ensures the field
// survives JSON marshal / unmarshal. Critical for restart
// semantics: /think off must persist across `nightme run` restart.
func TestChatSessionEntry_ThinkModeRoundTrip(t *testing.T) {
	entry := ChatSessionEntry{
		ID:        "cs_oc_x",
		ChatID:    "oc_x",
		WatchMode: WatchModeAll,
		ThinkMode: ThinkModeHide,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ChatSessionEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ThinkMode != ThinkModeHide {
		t.Errorf("round-trip ThinkMode = %v, want ThinkModeHide", got.ThinkMode)
	}
	if got.WatchMode != WatchModeAll {
		t.Errorf("round-trip WatchMode = %v, want WatchModeAll", got.WatchMode)
	}
}

// TestChatSessionEntry_MissingThinkModeDefaultsToShow mirrors the
// forward-compat invariant: older chat_sessions.json files written
// before F-think lack the thinkMode field. Go's zero-value
// semantics must give them ThinkModeShow (the "preserve existing
// behavior" default).
func TestChatSessionEntry_MissingThinkModeDefaultsToShow(t *testing.T) {
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
	if got.ThinkMode != ThinkModeShow {
		t.Errorf("missing-thinkMode default = %v, want ThinkModeShow", got.ThinkMode)
	}
	if got.WatchMode != WatchModeAll {
		t.Errorf("WatchMode round-trip = %v, want WatchModeAll", got.WatchMode)
	}
}

// TestChatSessionEntry_ThinkModeOmittedFromZeroValue locks the
// on-disk file size invariant: ThinkModeShow (the default) must
// NOT be written to disk. The `omitempty` JSON tag must skip it
// so old-format files (no thinkMode key) and new-format files
// (also no thinkMode key for default-mode chats) are byte-
// identical. This keeps the "missing field == ThinkModeShow"
// invariant robust across upgrades.
func TestChatSessionEntry_ThinkModeOmittedFromZeroValue(t *testing.T) {
	entry := ChatSessionEntry{
		ID:        "cs_oc_x",
		ChatID:    "oc_x",
		WatchMode: WatchModeMention, // zero — also omitted by omitempty
		ThinkMode: ThinkModeShow,    // zero — must be omitted
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsKey(data, "thinkMode") {
		t.Errorf("marshalled JSON should omit zero-value thinkMode: %s", data)
	}
	if containsKey(data, "watchMode") {
		t.Errorf("marshalled JSON should omit zero-value watchMode: %s", data)
	}
}