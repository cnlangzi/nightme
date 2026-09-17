package discord

import "testing"

func TestSessionChatID_RoundTrip(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"123456789012345678", "dc_123456789012345678"},
		{"1", "dc_1"},
	}
	for _, tc := range cases {
		got := sessionChatID(tc.raw)
		if got != tc.want {
			t.Errorf("sessionChatID(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		back, ok := splitSessionID(got)
		if !ok || back != tc.raw {
			t.Errorf("splitSessionID(%q) = (%q, %v), want (%q, true)", got, back, ok, tc.raw)
		}
	}
}

func TestSplitSessionID_MissingPrefix(t *testing.T) {
	if _, ok := splitSessionID("tg_123"); ok {
		t.Error("splitSessionID accepted telegram prefix")
	}
	if _, ok := splitSessionID(""); ok {
		t.Error("splitSessionID accepted empty string")
	}
}

func TestSplitSessionID_EmptyBody(t *testing.T) {
	if _, ok := splitSessionID("dc_"); ok {
		t.Error("splitSessionID accepted dc_ with empty body")
	}
}

func TestRawChannelIDFromSession(t *testing.T) {
	if got := rawChannelIDFromSession("dc_12345"); got != "12345" {
		t.Errorf("rawChannelIDFromSession(dc_12345) = %q, want 12345", got)
	}
	// Fallback: non-prefixed string is passed through (the
	// adapter lets Discord's own 404 surface the misroute).
	if got := rawChannelIDFromSession("plain"); got != "plain" {
		t.Errorf("rawChannelIDFromSession(plain) = %q, want plain", got)
	}
}
