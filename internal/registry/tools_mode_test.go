package registry

import (
	"encoding/json"
	"testing"
)

// TestToolsMode_ZeroValueIsHide locks the safe-default invariant:
// ToolsMode(0) must stringify to "hide" so /tools reply text and
// log lines are never "unknown" for a fresh ChatSession. The
// zero value being Hide is what makes "missing toolsMode field on
// disk" decode safely to the conservative default.
func TestToolsMode_ZeroValueIsHide(t *testing.T) {
	var m ToolsMode
	if m != ToolsModeHide {
		t.Fatalf("zero-value ToolsMode = %d, want ToolsModeHide (%d)", m, ToolsModeHide)
	}
	if got := m.String(); got != "hide" {
		t.Errorf("ToolsMode(0).String() = %q, want %q", got, "hide")
	}
}

// TestToolsMode_String covers all enum values + the unknown guard.
func TestToolsMode_String(t *testing.T) {
	cases := []struct {
		m    ToolsMode
		want string
	}{
		{ToolsModeHide, "hide"},
		{ToolsModeShow, "show"},
		{ToolsMode(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("ToolsMode(%d).String() = %q, want %q", c.m, got, c.want)
		}
	}
}

// TestParseToolsMode_Aliases accepts both the slash-command form
// (on/off) and the semantic form (show/hide) for each direction.
// Either pair must map to the same enum value.
//
// Whitespace tolerance is the caller's responsibility — the
// /tools handler invokes strings.TrimSpace(args[0]) before calling
// ParseToolsMode, mirroring /think and /watch handlers.
func TestParseToolsMode_Aliases(t *testing.T) {
	cases := []struct {
		in       string
		wantMode ToolsMode
		wantOK   bool
	}{
		{"on", ToolsModeShow, true},
		{"show", ToolsModeShow, true},
		{"off", ToolsModeHide, true},
		{"hide", ToolsModeHide, true},
	}
	for _, c := range cases {
		got, ok := ParseToolsMode(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseToolsMode(%q) ok = %v, want %v", c.in, ok, c.wantOK)
		}
		if ok && got != c.wantMode {
			t.Errorf("ParseToolsMode(%q) mode = %v, want %v", c.in, got, c.wantMode)
		}
	}
}

// TestParseToolsMode_UnknownRejects ensures unknown values fall
// through cleanly so the /tools handler can reply with a usage
// hint instead of committing a state mutation.
func TestParseToolsMode_UnknownRejects(t *testing.T) {
	unknowns := []string{"", "maybe", "yes", "no", "true", "false", "ON", "Show"}
	for _, in := range unknowns {
		got, ok := ParseToolsMode(in)
		if ok {
			t.Errorf("ParseToolsMode(%q) ok=true, want false (got mode=%v)", in, got)
		}
		// Even on parse failure, the returned mode is the safe
		// default (ToolsModeHide) — the caller ignores it on
		// ok=false, but the function should still be total.
		if got != ToolsModeHide {
			t.Errorf("ParseToolsMode(%q) returned %v on failure, want ToolsModeHide", in, got)
		}
	}
}

// TestChatSessionEntry_ToolsModeRoundTrip ensures the field
// survives JSON marshal / unmarshal. Critical for restart
// semantics: /tools on must persist across `nightme run` restart.
func TestChatSessionEntry_ToolsModeRoundTrip(t *testing.T) {
	entry := ChatSessionEntry{
		ID:        "cs_oc_x",
		ChatID:    "oc_x",
		WatchMode: WatchModeMention,
		ThinkMode: ThinkModeShow,
		ToolsMode: ToolsModeShow,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ChatSessionEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ToolsMode != ToolsModeShow {
		t.Errorf("round-trip ToolsMode = %v, want ToolsModeShow", got.ToolsMode)
	}
	if got.ThinkMode != ThinkModeShow {
		t.Errorf("round-trip ThinkMode = %v, want ThinkModeShow", got.ThinkMode)
	}
	if got.WatchMode != WatchModeMention {
		t.Errorf("round-trip WatchMode = %v, want WatchModeMention", got.WatchMode)
	}
}

// TestChatSessionEntry_MissingToolsModeDefaultsToHide mirrors the
// forward-compat invariant: older chat_sessions.json files written
// before F-38 lack the toolsMode field. Go's zero-value semantics
// must give them ToolsModeHide (the conservative "quiet by
// default" default — same rationale as WatchModeMention default).
func TestChatSessionEntry_MissingToolsModeDefaultsToHide(t *testing.T) {
	// Hand-rolled JSON without toolsMode.
	raw := []byte(`{
		"id": "cs_oc_x",
		"chatId": "oc_x",
		"activeCwd": "/tmp",
		"activeAgent": "claude",
		"primaryAgent": "claude",
		"watchMode": 1,
		"thinkMode": 1
	}`)
	var got ChatSessionEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ToolsMode != ToolsModeHide {
		t.Errorf("missing-toolsMode default = %v, want ToolsModeHide", got.ToolsMode)
	}
	// ThinkMode=1 was explicit in the input; verify it survived
	// the missing-field default for ToolsMode.
	if got.ThinkMode != ThinkModeHide {
		t.Errorf("ThinkMode round-trip = %v, want ThinkModeHide", got.ThinkMode)
	}
}

// TestChatSessionEntry_ToolsModeOmittedFromZeroValue locks the
// on-disk file size invariant: ToolsModeHide (the default) must
// NOT be written to disk. The `omitempty` JSON tag must skip it
// so old-format files (no toolsMode key) and new-format files
// (also no toolsMode key for default-mode chats) are byte-
// identical. This keeps the "missing field == ToolsModeHide"
// invariant robust across upgrades.
func TestChatSessionEntry_ToolsModeOmittedFromZeroValue(t *testing.T) {
	entry := ChatSessionEntry{
		ID:        "cs_oc_x",
		ChatID:    "oc_x",
		WatchMode: WatchModeMention, // zero — also omitted by omitempty
		ThinkMode: ThinkModeShow,    // zero — also omitted by omitempty
		ToolsMode: ToolsModeHide,    // zero — must be omitted
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsKey(data, "toolsMode") {
		t.Errorf("marshalled JSON should omit zero-value toolsMode: %s", data)
	}
	if containsKey(data, "thinkMode") {
		t.Errorf("marshalled JSON should omit zero-value thinkMode: %s", data)
	}
	if containsKey(data, "watchMode") {
		t.Errorf("marshalled JSON should omit zero-value watchMode: %s", data)
	}
}
