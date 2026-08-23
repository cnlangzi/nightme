package chatsession

import "testing"

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
		// default (ThinkModeHide, the off-by-default zero value)
		// — the caller ignores it on ok=false, but the function
		// should still be total.
		if got != ThinkModeHide {
			t.Errorf("ParseThinkMode(%q) returned %v on failure, want ThinkModeHide", in, got)
		}
	}
}

// TestChatSession_New_DefaultThinkModeIsHide locks the safe-
// default invariant on the live ChatSession struct (not just the
// enum). A fresh ChatSession created without a registry entry
// must report ThinkMode() == ThinkModeHide (off by default).
func TestChatSession_New_DefaultThinkModeIsHide(t *testing.T) {
	cs, _ := New("oc_x", "claude")
	if got := cs.ThinkMode(); got != ThinkModeHide {
		t.Errorf("fresh ChatSession.ThinkMode() = %v, want ThinkModeHide", got)
	}
}
