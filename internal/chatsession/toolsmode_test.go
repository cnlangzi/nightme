package chatsession

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

// _assertToolsModeAlias is a compile-time check that
// chatsession.ToolsMode is a true type alias of
// registry.ToolsMode — both directions of assignment must compile
// without an explicit conversion. If a future refactor changes
// `type ToolsMode = registry.ToolsMode` to `type ToolsMode
// registry.ToolsMode` (defined type), this stops compiling and
// immediately surfaces the breakage.
//
// Keeping this as a package-level var (not a test function) means
// it's evaluated at compile time, so a refactor breaks the test
// package build before any test runs.
var _assertToolsModeAlias = []func(){
	func() {
		var a ToolsMode
		var b registry.ToolsMode
		a = b               // registry.ToolsMode -> chatsession.ToolsMode
		b = a               // chatsession.ToolsMode -> registry.ToolsMode
		_ = a
		_ = b
	},
}

// TestToolsMode_ParseDelegation confirms that the package-local
// ParseToolsMode is wired to registry.ParseToolsMode so behaviour
// stays consistent if registry's parser is ever updated.
func TestToolsMode_ParseDelegation(t *testing.T) {
	cases := []struct {
		in       string
		wantMode ToolsMode
		wantOK   bool
	}{
		{"on", ToolsModeShow, true},
		{"off", ToolsModeHide, true},
		{"show", ToolsModeShow, true},
		{"hide", ToolsModeHide, true},
		{"", ToolsModeHide, false},
		{"nope", ToolsModeHide, false},
	}
	for _, c := range cases {
		got, ok := ParseToolsMode(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseToolsMode(%q) ok = %v, want %v", c.in, ok, c.wantOK)
		}
		if got != c.wantMode {
			t.Errorf("ParseToolsMode(%q) = %v, want %v", c.in, got, c.wantMode)
		}
	}
}

// TestChatSession_New_DefaultToolsModeIsHide locks the
// safe-default invariant on the live ChatSession struct (not just
// the enum). A fresh ChatSession created without a registry entry
// must report ToolsMode() == ToolsModeHide. Distinct from
// ThinkMode (default Show) — see internal/registry/tools_mode.go
// doc for the rationale ("quiet by default; opt in").
func TestChatSession_New_DefaultToolsModeIsHide(t *testing.T) {
	cs := New("oc_x", "claude")
	if got := cs.ToolsMode(); got != ToolsModeHide {
		t.Errorf("fresh ChatSession.ToolsMode() = %v, want ToolsModeHide", got)
	}
}
