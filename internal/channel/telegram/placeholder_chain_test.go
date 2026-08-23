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
	a := l.getOrCreate("chat-1", 0, 100)
	a.cursor = -1
	b := l.getOrCreate("chat-2", 0, 200)
	b.cursor = -1

	// First c evict "chat-1" (it's the LRU — head-of-order).
	c := l.getOrCreate("chat-3", 0, 300)
	c.cursor = -1

	if _, ok := l.chains[chainKey{chatID: "chat-1", topicID: 0, userMessageID: 100}]; ok {
		t.Fatalf("expected chat-1 evicted")
	}
	if _, ok := l.chains[chainKey{chatID: "chat-2", topicID: 0, userMessageID: 200}]; !ok {
		t.Fatalf("chat-2 should still be present (was touched last)")
	}
	if _, ok := l.chains[chainKey{chatID: "chat-3", topicID: 0, userMessageID: 300}]; !ok {
		t.Fatalf("chat-3 should be the newest")
	}
}

func TestChainLRU_PurgeRemovesKey(t *testing.T) {
	l := newChainLRU(10)
	a := l.getOrCreate("chat-x", 42, 999)
	a.cursor = -1

	l.purge("chat-x", 42, 999)
	if _, ok := l.chains[chainKey{chatID: "chat-x", topicID: 42, userMessageID: 999}]; ok {
		t.Fatal("expected chat-x purged")
	}
}

func TestChainLRU_ResetClearsAll(t *testing.T) {
	l := newChainLRU(10)
	l.getOrCreate("a", 0, 1)
	l.getOrCreate("b", 0, 2)
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
	body := renderActiveChunkBody(cur)
	if !strings.HasPrefix(body, "💭 3 · 🔧 1") {
		t.Fatalf("body should start with header; got %q", body)
	}
	if strings.Contains(body, "────────") {
		t.Fatalf("no entries → no separator; got %q", body)
	}
}

// TestChain_RotateChunk_HeaderIsFreshNotInherited locks the §11.12.7.2
// invariant that ROTATE produces a chunk whose header reflects its
// creation time, NOT the previous chunk's header. Without this, the
// tail chunk's `⏱ HH:MM:SS` would be the timestamp of the last
// OutHeartbeat (potentially minutes/hours old), confusing users who
// scroll to the bottom of the chat.
func TestChain_RotateChunk_HeaderIsFreshNotInherited(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sentBodies []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sentBodies = append(sentBodies, text)
		return int64(800 + len(sentBodies)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// Cold-create with a segment that fits on one chunk. After this
	// call, chunk[0] has the cold-create headerLine (heartbeatText(nil)
	// from the cold-create path).
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, "first reply", nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(sentBodies) != 1 {
		t.Fatalf("expected 1 sendMessage after cold-create; got %d", len(sentBodies))
	}
	originalHeader := chain.chunks[0].headerText()
	if originalHeader == "" {
		t.Fatal("cold-create chunk should have a header")
	}

	// Force the active chunk to look "stale" by stamping it with a
	// long-outdated heartbeat timestamp — this simulates a turn where
	// the last OutHeartbeat fired minutes ago, so cur.headerText() is
	// the stale timestamp. Then trigger ROTATE.
	stale := "<b>💭 99 · 🔧 99</b> · ⏱ 00:00:00"
	chain.chunks[chain.cursor].setHeader(stale)

	// Append a segment big enough to overflow the chunk and force
	// case 3 (ROTATE) — bufTextSize already includes the first entry
	// from cold-create, so a 3500-char segment definitely overflows.
	huge := strings.Repeat("x", chainChunkThresholdChars)
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, huge, nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.chunks) != 2 {
		t.Fatalf("expected 2 chunks after overflow; got %d", len(chain.chunks))
	}

	// The newly rotated chunk (chain[1) MUST NOT inherit the stale
	// header — it must reflect a fresh heartbeatText(nil) call. We
	// don't pin the exact string (it includes a live timestamp),
	// just verify it's neither empty nor the stale value.
	newHeader := chain.chunks[1].headerText()
	if newHeader == "" {
		t.Fatal("rotated chunk must have a header (heartbeatText(nil) call)")
	}
	if newHeader == stale {
		t.Fatalf("rotated chunk inherited stale cur.headerText()=%q; expected fresh heartbeatText(nil)", newHeader)
	}
	if newHeader == originalHeader {
		// Could theoretically happen if the cold-create header and
		// the rotation happen in the same second — not a bug. Log
		// for visibility but don't fail.
		t.Logf("note: rotated header equals cold-create header (%q); timestamps may match", newHeader)
	}
}

// ---------------------------------------------------------------------------
// §11.12.6 — data-driven footer policy + Render-always invariant.
// Locks:
//   - lastFooter is data-driven: any text-emitting kind carrying
//     status data (AgentName/Model/SessionID/Usage) refreshes
//     chain.lastFooter; the Kind itself is irrelevant.
//   - Render always happens regardless of whether lastFooter was
//     refreshed, because Telegram's editMessageText is a full body
//     replace. Append-only events still drive flushChainNow.
// ---------------------------------------------------------------------------

// TestChain_RenderAlwaysHappen_EvenWhenLastFooterUnchanged locks the
// §11.12.6 "Render 总是发生" invariant. When a non-status-bearing
// event lands on a chain that already has a footer, lastFooter must
// stay put — but the dirty flag must still flip and a flush must
// still render the chain. Without this, lastFooter going stale on a
// subsequent refresh would silently drop the rendered footer.
func TestChain_RenderAlwaysHappen_EvenWhenLastFooterUnchanged(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		return int64(900 + len(sent)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// First event carries status data — refreshes lastFooter.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		"reply with status",
		[]string{"🤖: claude · opus-4.5"},
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	firstFooter := chain.lastFooter
	if firstFooter == nil {
		t.Fatal("expected lastFooter to be set after first event")
	}

	// Second event carries NO status data — lastFooter must stay,
	// but dirty must flip so the next flush re-renders.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		"second thought without status",
		nil,
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}

	if !chain.dirty {
		t.Fatal("chain.dirty must be true even when lastFooter didn't change")
	}
	if chain.lastFooter == nil {
		t.Fatal("lastFooter must NOT be cleared by a non-status-bearing event")
	}
	if len(chain.lastFooter) != len(firstFooter) {
		t.Fatalf("lastFooter was mutated by a non-status event")
	}
}

// TestChain_DataDrivenFooter_OutThinkingWithAgentName_RefreshesFooter
// locks the §11.12.6 design intent: footer refresh is data-driven,
// not Kind-locked. If runtime ever stamps AgentName on a
// non-traditionally-footer-bearing kind (e.g. OutThinking), the
// chain MUST still refresh lastFooter. This test simulates that by
// passing a non-nil statusBarLines from the caller — which is
// exactly what appendSegmentForKind does today for every
// text-emitting kind via statusbar.StatusBarLines(&msg).
func TestChain_DataDrivenFooter_OutThinkingWithAgentName_RefreshesFooter(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		return int64(910 + len(sent)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// First event: no status data. lastFooter stays nil.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		"plain thinking",
		nil,
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if chain.lastFooter != nil {
		t.Fatal("lastFooter should be nil when caller passes nil statusBarLines")
	}

	// Second event: status data present, even though the simulated
	// kind is OutThinking (which §11.12.6 docs as "通常不 stamp"
	// — i.e. convention, not policy). chain.lastFooter MUST refresh.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		"thinking with new agent context",
		[]string{"🤖: gpt-5 · turbo"},
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if chain.lastFooter == nil {
		t.Fatal("lastFooter must refresh when statusBarLines is non-nil, regardless of Kind")
	}
	if got := chain.lastFooter[0]; got != "🤖: gpt-5 · turbo" {
		t.Fatalf("lastFooter[0] = %q; want refreshed value", got)
	}
}

// TestChain_NewChunk_InheritsLastFooter locks §11.12.6 third bullet
// ("新 chunk 创建时如果 chain.lastFooter != nil → 沿用上一 chunk 的
// footer") + §11.12.16 acceptance matrix item
// TestChain_NewChunk_InheritsLastFooter. Without this, the footer
// would silently disappear on every chunk rotation, leaving the
// chat looking footer-less on chunks 2..N.
func TestChain_NewChunk_InheritsLastFooter(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		return int64(920 + len(sent)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// Cold-create with status data → lastFooter populated, chunk 0
	// carries the rendered footer.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		"first reply",
		[]string{"🤖: claude"},
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.chunks) != 1 || chain.chunks[0].footer == "" {
		t.Fatal("first chunk should carry a rendered footer")
	}
	firstChunkFooter := chain.chunks[0].footer

	// Trigger ROTATE with a huge second segment. Second chunk must
	// inherit the footer from lastFooter, even though the new event
	// itself carries nil statusBarLines (so lastFooter doesn't get
	// re-stamped).
	huge := strings.Repeat("y", chainChunkThresholdChars)
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10,
		huge,
		nil,
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.chunks) != 2 {
		t.Fatalf("expected 2 chunks after overflow; got %d", len(chain.chunks))
	}
	if chain.chunks[1].footer == "" {
		t.Fatal("rotated chunk must inherit lastFooter; got empty footer")
	}
	if chain.chunks[1].footer != firstChunkFooter {
		t.Fatalf("rotated chunk footer differs from first chunk; "+
			"want inherited = %q, got %q", firstChunkFooter, chain.chunks[1].footer)
	}
}

// TestChain_MultipleOverflow_ThreeChunks_FirstTwoFrozen locks §11.12.16
// acceptance matrix item TestChain_MultipleOverflow_ThreeChunks_FirstTwoFrozen:
// after two overflows the first two chunks become frozen and don't
// accept subsequent appends. The active chunk is chunk[2].
func TestChain_MultipleOverflow_ThreeChunks_FirstTwoFrozen(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		return int64(930 + len(sent)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	huge := strings.Repeat("z", chainChunkThresholdChars)

	// Three appends, each one big enough to overflow the previous
	// chunk. Expect 3 chunks: [0] frozen, [1] frozen, [2] active.
	for i := 0; i < 3; i++ {
		if err := appendSegment(context.Background(), chain,
			"c1", 0, 10,
			huge,
			nil,
			sendFn, editFn,
		); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if len(chain.chunks) != 3 {
		t.Fatalf("expected 3 chunks after three overflows; got %d", len(chain.chunks))
	}
	if chain.cursor != 2 {
		t.Fatalf("cursor should point to last chunk (2); got %d", chain.cursor)
	}
	for i := 0; i < 2; i++ {
		if !chain.chunks[i].isChunkFull() {
			t.Fatalf("chunk[%d] should be frozen after subsequent overflow", i)
		}
	}
	if chain.chunks[2].isChunkFull() {
		t.Fatal("chunk[2] (active) must NOT be marked full")
	}
}
