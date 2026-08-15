// translate_regression_test.go — regression test for the
// 2026-08-15 dsh textBuf panic, refactored in F-DSH-CHAT-001 (I1/I4).
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
//
// F-DSH-CHAT-001 I4 refactor: the test now constructs a full
// dispatcher (translator + wireState + dispatcher + deliver)
// and feeds chunk events through dispatcher.dispatch — the same
// path production uses. This catches regressions in the dispatcher
// path (e.g. if textBuf moved to wireState in the future, the
// dispatcher.locking boundary would need re-validation).

package dsh

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// makeDispatcherForTest builds a translator + wireState + dispatcher
// triple suitable for feeding session/event envelopes through the
// production dispatcher path. The deliver closure is a no-op
// (we only care that handleSessionEvent — now via dispatcher —
// does not panic; we don't assert on events here).
func makeDispatcherForTest(t *testing.T) (*translator, *eventDispatcher) {
	t.Helper()
	tr := newTranslator("test-agent", "/tmp/test-ws")
	st := newWireState()
	d := newDispatcher(tr, st, nil, func(ev agent.AgentEvent) {
		// We only care that handleSessionEvent does not panic;
		// we don't assert on events here.
		_ = ev
	})
	return tr, d
}

// TestTranslator_TextBufGrowAndReuse directly reproduces the
// 2026-08-15 panic. It does NOT touch the network — it feeds
// assistant/chunk envelopes through the dispatcher that grow the
// (now-map) textBuf past its initial capacity and re-uses an
// earlier contentIndex. If the map-vs-slice fix is reverted, this
// test panics.
func TestTranslator_TextBufGrowAndReuse(t *testing.T) {
	tr, dispatcher := makeDispatcherForTest(t)

	// Phase 1: fill contentIndex 0..15 with one short chunk each.
	// This forces the textBuf to grow past its starting size. With
	// the old slice-of-values layout this would have copied all
	// prior Builders; the panic would fire on the *next* call
	// against contentIndex 0.
	for idx := range 16 {
		ev := makeChunkEvent(t, idx, "A")
		dispatcher.dispatch(ev, nil)
	}

	// Phase 2: go back to contentIndex 0 and append more text.
	// Pre-fix: panic on the Grow() call because the Builder at
	// contentIndex 0 had been value-copied to a new underlying
	// array during phase 1's grow, and its `addr` still pointed
	// to the old array.
	for range 5 {
		ev := makeChunkEvent(t, 0, "B")
		dispatcher.dispatch(ev, nil)
	}

	// Phase 3: re-use a middle index (8) that was also migrated
	// during phase 1's grow. Different contentIndex, same
	// "value-copied Builder" failure mode as contentIndex 0.
	for range 3 {
		ev := makeChunkEvent(t, 8, "C")
		dispatcher.dispatch(ev, nil)
	}

	// Phase 4: re-use a LATE index (15) which was appended last
	// in phase 1 and therefore was the one that triggered the
	// migration. The Builder at idx=15 in the OLD slice was the
	// one copied last.
	for range 3 {
		ev := makeChunkEvent(t, 15, "D")
		dispatcher.dispatch(ev, nil)
	}

	// Verify textBuf is intact (no panic, no corrupted Builder)
	// and accumulator produced the expected text.
	//   - phase 1: index 0..15 each get one "A" (so idx 0 = "A", idx 15 = "A")
	//   - phase 2: index 0 gets 5 more "B"s → "ABBBBB"
	//   - phase 3: index 8 gets 3 more "C"s → "ACCC"
	//   - phase 4: index 15 gets 3 more "D"s → "ADDD"
	if got := tr.textBuf[0].String(); got != "ABBBBB" {
		t.Errorf("contentIndex 0 accumulator = %q, want %q", got, "ABBBBB")
	}
	if got := tr.textBuf[8].String(); got != "ACCC" {
		t.Errorf("contentIndex 8 accumulator = %q, want %q", got, "ACCC")
	}
	if got := tr.textBuf[15].String(); got != "ADDD" {
		t.Errorf("contentIndex 15 accumulator = %q, want %q", got, "ADDD")
	}

	// If we got here without a panic, the fix works. The exact
	// accumulated text is tested by the existing pi/dsh
	// translate_test.go suite; this test exists purely to pin
	// the no-panic invariant on the grow-and-reuse path.
}

// makeChunkEvent builds a sessionEventEnvelope that the dispatcher
// will dispatch as "assistant/chunk" with the given contentIndex
// and a single-character text payload. We hand-roll the JSON
// rather than constructing the typed struct to keep the test
// focused on the wire → dispatcher path that panicked in prod.
//
// Wire shape (see translate.go handleSessionEvent + protocol.go
// muxSessionEvent):
//   muxSessionEvent.Event = json.RawMessage
//   unmarshalled into {Type string, Data json.RawMessage}
//   where Data is assistantChunkData = {Chunk: {Index, Text}}
func makeChunkEvent(t *testing.T, contentIndex int, text string) sessionEventEnvelope {
	t.Helper()
	// Outer event envelope: {"type":"assistant/chunk",
	//                         "data":{"chunk":{"index":N,"text":"X"}}}
	outerJSON := `{"type":"assistant/chunk","data":{"chunk":{"index":` +
		itoa(contentIndex) + `,"text":"` + text + `"}}}`
	var env sessionEventEnvelope
	if err := json.Unmarshal([]byte(outerJSON), &env); err != nil {
		t.Fatalf("decode chunk envelope: %v", err)
	}
	return env
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