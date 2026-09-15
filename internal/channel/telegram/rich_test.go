package telegram

import "testing"

// TestBlockThresholdForRich pins the relationship between
// richBlockCountLimit and the preflight margin (changing one
// without the other is a regression). The threshold caps the rich
// path at 4/5 of the server's per-message block ceiling so the
// heuristic never wastes cycles assembling a payload the server
// will reject with RICH_MESSAGE_BLOCKS_TOO_MANY.
func TestBlockThresholdForRich(t *testing.T) {
	if got := blockThresholdForRich(); got != 400 {
		t.Fatalf("blockThresholdForRich() = %d, want 400", got)
	}
}
