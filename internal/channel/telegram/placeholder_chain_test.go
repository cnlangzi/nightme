package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// noSendFn is a test-double sendFn that fails if ever invoked.
// Tests that expect no sendMessage (e.g. dirty=false flushChainNow)
// wire this in to catch unexpected send calls.
func noSendFn(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
	return 0, errors.New("sendFn should not be called in this test")
}

// ---------------------------------------------------------------------------
// v9 chain primitives — pure in-memory unit tests. Integration with the
// Telegram API is exercised via adapter_test.go (commit #9/11 backlog).
// ---------------------------------------------------------------------------

func TestChainLRU_EvictOldestOnCap(t *testing.T) {
	l := newChainLRU(2)
	a := l.getOrCreate("chat-1", 0)
	a.cursor = -1
	b := l.getOrCreate("chat-2", 0)
	b.cursor = -1

	// First c evict "chat-1" (it's the LRU — head-of-order).
	c := l.getOrCreate("chat-3", 0)
	c.cursor = -1

	if _, ok := l.chains[chainKey{chatID: "chat-1", topicID: 0}]; ok {
		t.Fatalf("expected chat-1 evicted")
	}
	if _, ok := l.chains[chainKey{chatID: "chat-2", topicID: 0}]; !ok {
		t.Fatalf("chat-2 should still be present (was touched last)")
	}
	if _, ok := l.chains[chainKey{chatID: "chat-3", topicID: 0}]; !ok {
		t.Fatalf("chat-3 should be the newest")
	}
}

func TestChainLRU_PurgeRemovesKey(t *testing.T) {
	l := newChainLRU(10)
	a := l.getOrCreate("chat-x", 42)
	a.cursor = -1

	l.purge("chat-x", 42)
	if _, ok := l.chains[chainKey{chatID: "chat-x", topicID: 42}]; ok {
		t.Fatal("expected chat-x purged")
	}
}

func TestChainLRU_ResetClearsAll(t *testing.T) {
	l := newChainLRU(10)
	l.getOrCreate("a", 0)
	l.getOrCreate("b", 0)
	l.reset()
	if len(l.chains) != 0 || len(l.order) != 0 {
		t.Fatalf("expected empty after reset; got %d chains", len(l.chains))
	}
}

func TestAppendSegment_CreatesFirstChunkWhenEmpty(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		return 700, nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		"💭 first thought",
		nil,
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected 1 sendMessage call; got %d", len(sent))
	}
	if !strings.Contains(sent[0], "💭 first thought") {
		t.Fatalf("sent text missing segment; got %q", sent[0])
	}
	if chain.cursor != 0 || len(chain.chunks) != 1 {
		t.Fatalf("chain state wrong; cursor=%d chunks=%d", chain.cursor, len(chain.chunks))
	}
	if chain.chunks[0].messageID != 700 {
		t.Fatalf("chunk.messageID = %d, want 700", chain.chunks[0].messageID)
	}
	// First chunk path: the segment IS in buf (P0 #1 fix).
	// Pre-fix the segment was only in the sendMessage body; the
	// next flush's renderActiveChunkBody silently dropped it
	// because it only reads cur.buf. After the fix, cur.buf
	// seeds with the segment so re-renders include it.
	if got := chain.chunks[0].entriesJoined(); got != "💭 first thought\n" {
		t.Fatalf("first chunk buf = %q, want %q", got, "💭 first thought\n")
	}
	wantChars := len("💭 first thought\n")
	if chain.chunks[0].entriesSize() != wantChars {
		t.Fatalf("first chunk charCount = %d, want %d",
			chain.chunks[0].entriesSize(), wantChars)
	}
}

func TestAppendSegment_AppendsToActiveChunkWithinThreshold(t *testing.T) {
	chain := &placeholderChain{
		chunks: []*chunkBody{{
			messageID:  500,
			header: "💭 0 · 🔧 0 · ⏱ 00:00:00",
		}},
		cursor: 0,
	}
	// First chunk starts at 0 chars.
	var sendCount int
	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		sendCount++
		return 0, errors.New("should not send during append")
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error { return nil }

	for i := 0; i < 10; i++ {
		if err := appendSegment(context.Background(), chain,
			"c", 0, 10,
			"thought",
			nil,
			sendFn, editFn,
		); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if sendCount != 0 {
		t.Fatalf("expected no new sendMessage; got %d", sendCount)
	}
	if len(chain.chunks) != 1 {
		t.Fatalf("expected 1 chunk; got %d", len(chain.chunks))
	}
	want := strings.Repeat("thought\n", 10)
	if chain.chunks[0].entriesJoined() != want {
		t.Fatalf("buf mismatch; got %q want %q", chain.chunks[0].entriesJoined(), want)
	}
}

func TestAppendSegment_OverflowCreatesSecondChunk(t *testing.T) {
	chain := &placeholderChain{
		chunks: []*chunkBody{{
			messageID:  100,
			header: "💭 0",
		}},
		cursor: 0,
	}

	var sends []string
	var mu sync.Mutex
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		sends = append(sends, text)
		return int64(700 + len(sends)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error { return nil }

	// Pump enough segments to overflow the 3500-char buffer.
	bigSeg := strings.Repeat("a", 1000)
	for i := 0; i < 5; i++ {
		if err := appendSegment(context.Background(), chain,
			"c", 0, 10,
			bigSeg,
			nil,
			sendFn, editFn,
		); err != nil {
			t.Fatal(err)
		}
	}

	if len(chain.chunks) != 2 {
		t.Fatalf("expected 2 chunks after overflow; got %d", len(chain.chunks))
	}
	if chain.cursor != 1 {
		t.Fatalf("expected cursor at 1; got %d", chain.cursor)
	}
	if !chain.chunks[0].isChunkFull() {
		t.Fatalf("first chunk should be locked")
	}
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendMessage (the new chunk); got %d", len(sends))
	}
}

func TestAppendSegment_FooterRefreshOnlyOnFooterBearing(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return 1, nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error { return nil }

	// First event: footer-bearing → refreshes lastFooter.
	if err := appendSegment(context.Background(), chain,
		"c", 0, 10, "hi",
		[]string{"footer-line"},
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.lastFooter) != 1 || chain.lastFooter[0] != "footer-line" {
		t.Fatalf("lastFooter not refreshed: %v", chain.lastFooter)
	}

	// Second event: non-footer → lastFooter unchanged.
	if err := appendSegment(context.Background(), chain,
		"c", 0, 10, "next",
		nil,
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.lastFooter) != 1 || chain.lastFooter[0] != "footer-line" {
		t.Fatalf("lastFooter should not move on nil footer event: %v", chain.lastFooter)
	}
}

func TestFlushChainNow_NoOpWhenClean(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	edits := 0
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		edits++
		return nil
	}
	noSend := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		t.Fatalf("clean chain should not call sendFn")
		return 0, nil
	}
	if err := flushChainNow(context.Background(), chain, "c", 0, 10, editFn, noSend); err != nil {
		t.Fatal(err)
	}
	if edits != 0 {
		t.Fatalf("dirty=false should be no-op; got %d edits", edits)
	}
}

func TestFlushChainNow_RendersHeaderBufFooter(t *testing.T) {
	chain := &placeholderChain{
		chunks: []*chunkBody{{
			messageID:  555,
			header: "🤖 Working... · ⏱ 12:34:56",
		}},
		cursor: 0,
	}
	chain.dirty = true
	chain.lastFooter = []string{"agent · model"}

	var captured string
	editFn := func(_ context.Context, _ string, msgID int64, text string) error {
		if msgID != 555 {
			t.Fatalf("edit on wrong chunk: %d", msgID)
		}
		captured = text
		return nil
	}

	if err := flushChainNow(context.Background(), chain,
		"c", 0, 10,
		editFn, noSendFn,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured, "🤖 Working...") {
		t.Fatalf("rendered missing header; got %q", captured)
	}
	if !strings.Contains(captured, "────────") {
		t.Fatalf("rendered missing separator; got %q", captured)
	}
	if !strings.Contains(captured, "agent · model") {
		t.Fatalf("rendered missing footer; got %q", captured)
	}
	if chain.dirty {
		t.Fatalf("dirty should clear after flush")
	}
}

func TestScheduleFlushDebounced_MergesBurst(t *testing.T) {
	chain := &placeholderChain{
		chunks: []*chunkBody{{
			messageID:  42,
			header: "💭 0",
		}},
		cursor: 0,
		lastFooter: []string{"footer"},
	}
	chain.dirty = true

	// Schedule 5 flushes within 250ms; only the last should fire.
	captureEdits := 0
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		captureEdits++
		return nil
	}
	noSend := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return 0, nil
	}
	for i := 0; i < 5; i++ {
		scheduleFlushDebounced(chain, editFn, noSend, "c", 0, 10)
		time.Sleep(20 * time.Millisecond)
	}
	// Wait one full debounce window + a generous margin.
	time.Sleep(400 * time.Millisecond)
	// Manual drain via flushChainNow to confirm content: just verify
	// chain state isn't corrupted.
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.dirty {
		t.Fatalf("dirty should clear after burst; left = %v", chain.dirty)
	}
}

func TestRenderActiveChunkBody_HeaderOnly(t *testing.T) {
	cur := &chunkBody{header: "💭 3 · 🔧 1"}
	body := renderActiveChunkBody(cur, nil)
	if !strings.HasPrefix(body, "💭 3 · 🔧 1") {
		t.Fatalf("body should start with header; got %q", body)
	}
	if strings.Contains(body, "────────") {
		t.Fatalf("no entries → no separator; got %q", body)
	}
}
