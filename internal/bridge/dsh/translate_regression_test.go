// translate_regression_test.go — regression test for the
// 2026-08-15 dsh textBuf panic.
//
// The bug: translator.textBuf was []strings.Builder (values, not
// pointers). When the slice grew past its initial capacity, Go's
// append VALUE-COPIED the existing Builders to a new array. The
// copied Builders' internal `addr` field still pointed to the OLD
// array; the next Grow/WriteString on any previously-touched
// contentIndex panicked with:
//
//   strings: illegal use of non-zero Builder copied by value
//
// The fix (translate.go): textBuf is now map[int]*strings.Builder.
// Pointers, no copy on grow, no stale addr.
//
// The test below directly reproduces the production scenario —
// it interleaves chunk events across multiple contentIndex values
// and then re-uses an earlier index. Before the fix this panicked
// on the second re-use. After the fix it accumulates text
// correctly across all chunks.

package dsh

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestTranslator_TextBufGrowAndReuse directly reproduces the
// 2026-08-15 panic. It does NOT touch the network — it constructs
// a translator, then calls handleSessionEvent with synthetic
// assistant/chunk envelopes that grow the (now-map) textBuf
// past its initial capacity and re-uses an earlier contentIndex.
// If the map-vs-slice fix is reverted, this test panics.
func TestTranslator_TextBufGrowAndReuse(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test-ws")
	deliver := func(ev agent.AgentEvent) {
		// We only care that handleSessionEvent does not panic;
		// we don't assert on events here.
		_ = ev
	}

	// Phase 1: fill contentIndex 0..15 with one short chunk each.
	// This forces the textBuf to grow past its starting size. With
	// the old slice-of-values layout this would have copied all
	// prior Builders; the panic would fire on the *next* call
	// against contentIndex 0.
	for idx := range 16 {
		ev := makeChunkEvent(t, idx, "A")
		tr.handleSessionEvent(ev, deliver)
	}

	// Phase 2: go back to contentIndex 0 and append more text.
	// Pre-fix: panic on the Grow() call because the Builder at
	// contentIndex 0 had been value-copied to a new underlying
	// array during phase 1's grow, and its `addr` still pointed
	// to the old array.
	for range 5 {
		ev := makeChunkEvent(t, 0, "B")
		tr.handleSessionEvent(ev, deliver)
	}

	// Phase 3: re-use a middle index (8) that was also migrated
	// during phase 1's grow. Different contentIndex, same
	// "value-copied Builder" failure mode as contentIndex 0.
	for range 3 {
		ev := makeChunkEvent(t, 8, "C")
		tr.handleSessionEvent(ev, deliver)
	}

	// Phase 4: re-use a LATE index (15) which was appended last
	// in phase 1 and therefore was the one that triggered the
	// migration. The Builder at idx=15 in the OLD slice was the
	// one copied last.
	for range 3 {
		ev := makeChunkEvent(t, 15, "D")
		tr.handleSessionEvent(ev, deliver)
	}

	// If we got here without a panic, the fix works. The exact
	// accumulated text is tested by the existing pi/dsh
	// translate_test.go suite; this test exists purely to pin
	// the no-panic invariant on the grow-and-reuse path.
}

// makeChunkEvent builds a muxSessionEvent that the translator
// will dispatch as "assistant/chunk" with the given contentIndex
// and a single-character text payload. We hand-roll the JSON
// rather than constructing the typed struct to keep the test
// focused on the wire → translator path that panicked in prod.
//
// Wire shape (see translate.go handleSessionEvent + protocol.go
// muxSessionEvent):
//   muxSessionEvent.Event = json.RawMessage
//   unmarshalled into {Type string, Data json.RawMessage}
//   where Data is assistantChunkData = {Chunk: {Index, Text}}
func makeChunkEvent(t *testing.T, contentIndex int, text string) muxSessionEvent {
	t.Helper()
	// Outer event envelope: {"type":"assistant/chunk",
	//                         "data":{"chunk":{"index":N,"text":"X"}}}
	outerJSON := `{"type":"assistant/chunk","data":{"chunk":{"index":` +
		itoa(contentIndex) + `,"text":"` + text + `"}}}`
	return muxSessionEvent{
		Event: []byte(outerJSON),
	}
}

// itoa is a tiny strconv.Itoa replacement to avoid importing
// strconv in this test file (keeps the test focused — we only
// need a few small non-negative integers).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b strings.Builder
	for n > 0 {
		b.WriteByte(byte('0' + n%10))
		n /= 10
	}
	// reverse
	s := b.String()
	r := []byte(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
