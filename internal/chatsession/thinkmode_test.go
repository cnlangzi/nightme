package chatsession

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

// _assertThinkModeAlias is a compile-time check that
// chatsession.ThinkMode is a true type alias of
// registry.ThinkMode — both directions of assignment must
// compile without an explicit conversion. If a future refactor
// changes `type ThinkMode = registry.ThinkMode` to
// `type ThinkMode registry.ThinkMode` (defined type), this
// stops compiling and immediately surfaces the breakage.
//
// Keeping this as a package-level var (not a test function)
// means it's evaluated at compile time, so a refactor breaks
// the test package build before any test runs.
var _assertThinkModeAlias = []func(){
	func() {
		var a ThinkMode
		var b registry.ThinkMode
		a = b               // registry.ThinkMode -> chatsession.ThinkMode
		b = a               // chatsession.ThinkMode -> registry.ThinkMode
		_ = a
		_ = b
	},
}

// TestThinkMode_ParseDelegation confirms that the package-local
// ParseThinkMode is wired to registry.ParseThinkMode so behaviour
// stays consistent if registry's parser is ever updated.
func TestThinkMode_ParseDelegation(t *testing.T) {
	cases := []struct {
		in       string
		wantMode ThinkMode
		wantOK   bool
	}{
		{"on", ThinkModeShow, true},
		{"off", ThinkModeHide, true},
		{"show", ThinkModeShow, true},
		{"hide", ThinkModeHide, true},
		{"", ThinkModeShow, false},
		{"nope", ThinkModeShow, false},
	}
	for _, c := range cases {
		got, ok := ParseThinkMode(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseThinkMode(%q) ok = %v, want %v", c.in, ok, c.wantOK)
		}
		if got != c.wantMode {
			t.Errorf("ParseThinkMode(%q) = %v, want %v", c.in, got, c.wantMode)
		}
	}
}

// TestChatSession_New_DefaultThinkModeIsShow locks the safe-
// default invariant on the live ChatSession struct (not just the
// enum). A fresh ChatSession created without a registry entry
// must report ThinkMode() == ThinkModeShow.
func TestChatSession_New_DefaultThinkModeIsShow(t *testing.T) {
	cs, _ := New("oc_x", "claude", newTestChannel())
	if got := cs.ThinkMode(); got != ThinkModeShow {
		t.Errorf("fresh ChatSession.ThinkMode() = %v, want ThinkModeShow", got)
	}
}