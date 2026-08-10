package chatsession

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

// TestParseToolsMode_Aliases covers slash-command form (on/off)
// and semantic form (show/hide); either pair must map to the
// same enum value.
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
		// default (ToolsModeHide).
		if got != ToolsModeHide {
			t.Errorf("ParseToolsMode(%q) returned %v on failure, want ToolsModeHide", in, got)
		}
	}
}

// TestChatSession_ToolsModeRoundTrip checks that the JSON layer
// (registry-side int field) stays compatible with the enum's
// numeric value: ToolsModeShow/1 is preserved through marshal +
// unmarshal via a bare-int struct mirror.
func TestChatSession_ToolsModeRoundTrip(t *testing.T) {
	type entry struct {
		ToolsMode int `json:"toolsMode,omitempty"`
	}
	in := entry{ToolsMode: int(ToolsModeShow)}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out entry
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ToolsMode(out.ToolsMode) != ToolsModeShow {
		t.Errorf("round-trip ToolsMode = %v, want ToolsModeShow", ToolsMode(out.ToolsMode))
	}
}
