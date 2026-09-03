package gtw_test

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestActionLookupUnknown ensures ActionLookup returns ok=false for
// arbitrary / hostile input. The channel adapter renders an
// "unknown action" toast when this happens, so an over-permissive
// lookup would silently misroute user clicks to the wrong draft
// handler instead of surfacing the error.
//
// (v1.5: TestRenderActionLookupContract was removed along with
// the gtw-side interactive cards it was locking down.)
func TestActionLookupUnknown(t *testing.T) {
	for _, tag := range []string{
		"",
		"act:/gtw/",
		"act:/unknown",
		"random string",
		"ACT:/GTW/CANCEL",  // case-sensitive
		"act:/gtw/cancel ", // trailing space
	} {
		if _, ok := messages.ActionLookup(tag); ok {
			t.Errorf("ActionLookup(%q) returned ok=true; expected ok=false (whitelist is exact)", tag)
		}
	}
}
