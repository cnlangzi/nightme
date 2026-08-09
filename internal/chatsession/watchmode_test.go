package chatsession

import (
	"testing"
)

// TestParseWatchMode covers the slash-command argument parser.
// Returns the WatchMode + a bool that is false for unknown
// inputs (caller should reply with a usage hint).
func TestParseWatchMode(t *testing.T) {
	cases := []struct {
		in       string
		wantMode string
		wantOK   bool
	}{
		{"on", "all", true},
		{"all", "all", true},
		{"off", "mention", true},
		{"mention", "mention", true},
		{"", "mention", false},
		{"yes", "mention", false},
		{"enable", "mention", false},
		{"disable", "mention", false},
		{"ON", "mention", false}, // case-sensitive (lowercase only)
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			mode, ok := ParseWatchMode(tc.in)
			if ok != tc.wantOK {
				t.Errorf("ParseWatchMode(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && mode.String() != tc.wantMode {
				t.Errorf("ParseWatchMode(%q) mode = %q, want %q", tc.in, mode, tc.wantMode)
			}
		})
	}
}

// TestWatchMode_String covers the fmt.Stringer implementation.
func TestWatchMode_String(t *testing.T) {
	cases := []struct {
		mode WatchMode
		want string
	}{
		{WatchModeMention, "mention"},
		{WatchModeAll, "all"},
		{WatchMode(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.mode.String(); got != tc.want {
				t.Errorf("WatchMode(%d).String() = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

// TestChatSession_WatchModeDefault covers that a freshly-created
// ChatSession defaults to WatchModeMention (the safe mode that
// drops non-mention group messages).
func TestChatSession_WatchModeDefault(t *testing.T) {
	cs := New("oc_test", "claude", newTestChannel())
	if got := cs.WatchMode(); got != WatchModeMention {
		t.Errorf("New().WatchMode() = %q, want %q (default safe mode)", got, WatchModeMention)
	}
}

// TestChatSession_SetWatchMode covers state mutation + the no-op
// behaviour for invalid modes (ParseWatchMode rejects bad input
// before SetWatchMode is called).
func TestChatSession_SetWatchMode(t *testing.T) {
	cs := New("oc_test", "claude", newTestChannel())

	if err := cs.SetWatchMode(WatchModeAll); err != nil {
		t.Fatalf("SetWatchMode(All) returned error: %v", err)
	}
	if got := cs.WatchMode(); got != WatchModeAll {
		t.Errorf("after SetWatchMode(All), WatchMode() = %q, want %q", got, WatchModeAll)
	}

	if err := cs.SetWatchMode(WatchModeMention); err != nil {
		t.Fatalf("SetWatchMode(Mention) returned error: %v", err)
	}
	if got := cs.WatchMode(); got != WatchModeMention {
		t.Errorf("after SetWatchMode(Mention), WatchMode() = %q, want %q", got, WatchModeMention)
	}
}