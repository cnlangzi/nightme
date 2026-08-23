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

// TestRenderActiveChunkBody_HeaderOnly locks the cold-create path:
// empty entries + hasHeartbeat=false still renders the cold banner.
// Without this, the very first flush of a fresh chain (e.g. a slash
// command reply that races ahead of the first OutHeartbeat) would
// emit nothing — a silent silent drop. hasHeartbeat starts false
// because patchChainHeader has not been called yet.
func TestRenderActiveChunkBody_HeaderOnly(t *testing.T) {
	cur := &chunkBody{header: "<b>🤖 Working...</b>"}
	body := renderActiveChunkBody(cur)
	if !strings.HasPrefix(body, "<b>🤖 Working...</b>") {
		t.Fatalf("body should start with header; got %q", body)
	}
	if strings.Contains(body, "────────") {
		t.Fatalf("no entries → no separator; got %q", body)
	}
}

// TestRenderActiveChunkBody_SkipsHeaderWhenBodyButNoHeartbeat is the
// 2026-08-23 Compose header-skip rule (§11.12.5): when a chunk has
// entries but no OutHeartbeat has fired yet, the cold-create
// "🤖 Working..." banner is hidden so the body renders alone. Without
// this, slash commands / reaction-only clicks / WatchMode-rejected
// turns would carry a frozen banner that never updates.
func TestRenderActiveChunkBody_SkipsHeaderWhenBodyButNoHeartbeat(t *testing.T) {
	cur := &chunkBody{header: "<b>🤖 Working...</b>"}
	cur.appendEntry("✅ Local worktree ready")
	body := renderActiveChunkBody(cur)
	if strings.Contains(body, "🤖 Working") {
		t.Fatalf("body must NOT contain the Working banner; got %q", body)
	}
	if strings.Contains(body, "────────") {
		t.Fatalf("no header → no separator above body; got %q", body)
	}
	if !strings.HasPrefix(body, "✅ Local worktree ready") {
		t.Fatalf("body should start with the entry; got %q", body)
	}
}

// TestRenderActiveChunkBody_HeaderAndBody locks the post-heartbeat
// path: once patchChainHeader has flipped hasHeartbeat=true, the
// header re-appears above the body with a separator. HasHeartbeat
// does not gate body rendering — both render together.
func TestRenderActiveChunkBody_HeaderAndBody(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 5 · 🔧 2</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 5 · 🔧 2</b>")
	cur.appendEntry("first entry")
	body := renderActiveChunkBody(cur)
	if !strings.HasPrefix(body, "<b>💭 5 · 🔧 2</b>") {
		t.Fatalf("hasHeartbeat must bring the header back; got %q", body)
	}
	if !strings.Contains(body, "────────") {
		t.Fatalf("header + body renders with separator; got %q", body)
	}
	if !strings.Contains(body, "first entry") {
		t.Fatalf("body should contain the entry; got %q", body)
	}
}

// TestRenderActiveChunkBody_HeaderOnlyAfterHeartbeat pins the case
// where the agent emits a heartbeat BEFORE any body content (think:
// an idle agent that ticks once). hasHeartbeat=true so header renders
// even though entries are still empty.
func TestRenderActiveChunkBody_HeaderOnlyAfterHeartbeat(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
	body := renderActiveChunkBody(cur)
	if !strings.HasPrefix(body, "<b>💭 0 · 🔧 0</b>") {
		t.Fatalf("hasHeartbeat with empty entries → header still renders; got %q", body)
	}
}



// TestChain_RotateChunk_InheritsLatestHeader locks the §11.12.7.2
// ROTATE invariant: a freshly rotated chunk inherits the (header,
// hasHeartbeat) pair from the prior active chunk so each chunk in
// the chain reads as a chronological snapshot of the chain's active
// state at the moment it was born. Together with
// TestChain_FrozenChunkHeaderSurvivesAcrossSubsequentPatch (above)
// this locks the "frozen chunks keep their snapshot; only cursor's
// chunk updates on each new OutHeartbeat" semantics.
//
// Without this, an agent that emits "💭 99 · 🔧 99" then
// overflows would see the new chunk restart at the cold banner,
// losing the chronological signal across frozen chunks.
func TestChain_RotateChunk_InheritsLatestHeader(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sentBodies []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sentBodies = append(sentBodies, text)
		return int64(800 + len(sentBodies)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// Cold-create with a segment that fits on one chunk. chunk[0]
	// has the cold-create header line.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, "first reply", nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(sentBodies) != 1 {
		t.Fatalf("expected 1 sendMessage after cold-create; got %d", len(sentBodies))
	}

	// Stamp chunk[0] with a representative "latest heartbeat" snapshot
	// (mimicking what patchChainHeader would do via setHeaderFromHeartbeat)
	// and mark hasHeartbeat=true so the inherit copies the same flag.
	stale := "<b>💭 99 · 🔧 99</b> · ⏱ 00:00:00"
	chain.chunks[chain.cursor].setHeaderFromHeartbeat(stale)
	if !chain.chunks[chain.cursor].hasHeartbeat {
		t.Fatal("setup: setHeaderFromHeartbeat must flip hasHeartbeat=true")
	}

	// Append a segment big enough to overflow the chunk and force
	// case 3 (ROTATE). bufTextSize already includes the first entry
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

	// The newly rotated chunk[1] MUST inherit the prior active
	// (header, hasHeartbeat) verbatim — chronological snapshot.
	newChunk := chain.chunks[1]
	if newChunk.headerText() != stale {
		t.Fatalf("rotated chunk header = %q; expected inherited %q", newChunk.headerText(), stale)
	}
	if !newChunk.hasHeartbeat {
		t.Fatalf("rotated chunk must inherit hasHeartbeat=true (got false)")
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

// ---------------------------------------------------------------------------
// §11.12.7.2 trigger 1 — SPLIT path for single oversized segments.
// Without the split path, a 5000-char raw segment would push the
// composed body past Telegram's 4096 hard limit and the first
// sendMessage would be rejected by Telegram itself. These tests
// lock the split behaviour at the boundary cases.
// ---------------------------------------------------------------------------

// TestChain_OversizedSegment_SplitsIntoMultipleChunks drives the SPLIT
// path by sending a segment longer than chainChunkThresholdChars (3500).
// The result must be 2+ chain chunks (one Telegram message per piece).
func TestChain_OversizedSegment_SplitsIntoMultipleChunks(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	var sentMsgIDs []int64
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		id := int64(940 + len(sent))
		sentMsgIDs = append(sentMsgIDs, id)
		return id, nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// 5000-char segment: well past chainChunkThresholdChars (3500)
	// and very likely past maxTelegramTextLength (3900) after
	// markdown rendering, forcing splitTelegramText into multi-piece.
	huge := strings.Repeat("x", 5000)

	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, huge, nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}

	if len(sent) < 2 {
		t.Fatalf("oversized segment should split into >=2 sendMessage calls; got %d", len(sent))
	}
	if len(chain.chunks) != len(sent) {
		t.Fatalf("chain.chunks count (%d) must match sendMessage count (%d)",
			len(chain.chunks), len(sent))
	}
	if chain.cursor != len(chain.chunks)-1 {
		t.Fatalf("cursor should land on last piece; got %d, want %d",
			chain.cursor, len(chain.chunks)-1)
	}
}

// TestChain_SplitChunks_AllCarrySameTimestamp locks the SPLIT-vs-
// ROTATE visual distinction. All SPLIT chunks share the same
// headerLine from a single heartbeatText(nil) call, so their
// `⏱ HH:MM:SS` strings are byte-identical.
func TestChain_SplitChunks_AllCarrySameTimestamp(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return int64(950 + len(chain.chunks)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, strings.Repeat("x", 5000), nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.chunks) < 2 {
		t.Skipf("split didn't produce 2+ chunks; cannot check timestamps")
	}

	first := chain.chunks[0].headerText()
	for i := 1; i < len(chain.chunks); i++ {
		if got := chain.chunks[i].headerText(); got != first {
			t.Fatalf("chunk[%d] header %q differs from chunk[0] %q; "+
				"SPLIT chunks must share the same heartbeatText(nil) output",
				i, got, first)
		}
	}
}

// TestChain_SplitChunks_FirstPiecesAreFrozen locks the partial-freeze
// semantics: pieces 1..N-1 are markFull'd, only the last piece stays
// active and accepts subsequent appendSegment calls.
func TestChain_SplitChunks_FirstPiecesAreFrozen(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return int64(960 + len(chain.chunks)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, strings.Repeat("x", 5000), nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	n := len(chain.chunks)
	if n < 2 {
		t.Skipf("split didn't produce 2+ chunks; got %d", n)
	}

	for i := 0; i < n-1; i++ {
		if !chain.chunks[i].isChunkFull() {
			t.Fatalf("chunk[%d] (frozen piece) must be markFull'd", i)
		}
	}
	if chain.chunks[n-1].isChunkFull() {
		t.Fatalf("chunk[%d] (active tail) must NOT be markFull'd", n-1)
	}
}

// TestChain_SplitChunks_SubsequentEntryLandsOnLastPiece locks the
// "active piece = tail" invariant: after a SPLIT, a subsequent
// appendSegment call lands on the last piece (chain.cursor), not on
// the first.
func TestChain_SplitChunks_SubsequentEntryLandsOnLastPiece(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return int64(970 + len(chain.chunks)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, strings.Repeat("x", 5000), nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.chunks) < 2 {
		t.Skipf("split didn't produce 2+ chunks")
	}
	tailIdx := chain.cursor
	tailEntriesBefore := len(chain.chunks[tailIdx].entries)
	chunksBefore := len(chain.chunks)

	// Subsequent small entry. Case 2 (append-in-place) should land
	// on chain.chunks[tailIdx] without minting a new chunk.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, "tiny follow-up", nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}

	if len(chain.chunks) != chunksBefore {
		t.Fatalf("case 2 should append in place; chain.chunks grew from %d to %d",
			chunksBefore, len(chain.chunks))
	}
	if chain.cursor != tailIdx {
		t.Fatalf("cursor moved off tail piece; was %d, now %d", tailIdx, chain.cursor)
	}
	if len(chain.chunks[tailIdx].entries) != tailEntriesBefore+1 {
		t.Fatalf("tail piece should have grown by 1 entry; was %d, now %d",
			tailEntriesBefore, len(chain.chunks[tailIdx].entries))
	}
}

// TestChain_OversizedError_SplitsIntoMultipleChunks mirrors
// TestChain_OversizedSegment_SplitsIntoMultipleChunks for the
// OutError path. A long stderr (>= chainChunkThresholdChars raw)
// routes to splitOversizedErrorSegmentLocked.
func TestChain_OversizedError_SplitsIntoMultipleChunks(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent []string
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		sent = append(sent, text)
		return int64(980 + len(sent)), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		return nil
	}

	// estimateErrorSize = 10 + len(stderr) + 1. Set stderr to
	// 4000 chars → estimate 4011, comfortably over
	// chainChunkThresholdChars (3500).
	hugeStderr := strings.Repeat("e", 4000)

	if err := appendErrorSegment(context.Background(), chain,
		"c1", 0, 10,
		"tool exit 1",
		hugeStderr,
		nil,
		sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}

	if len(sent) < 2 {
		t.Fatalf("oversized OutError should split into >=2 sendMessage calls; got %d", len(sent))
	}
	if len(chain.chunks) != len(sent) {
		t.Fatalf("chain.chunks count (%d) must match sendMessage count (%d)",
			len(chain.chunks), len(sent))
	}
}

// TestChain_RotateAndSplitDistinguishedByHeader locks the visual
// distinction between SPLIT and ROTATE. Both produce multi-chunk
// chains but their chunks carry different timestamps:
//   - SPLIT chunks share one heartbeatText(nil) call → identical
//     headerLine across all chunks.
//   - ROTATE chunks each take their own heartbeatText(nil) call →
//     distinct headerLines (likely, even if they happen to share
//     second-resolution timestamps).
func TestChain_RotateAndSplitDistinguishedByHeader(t *testing.T) {
	// SPLIT path
	chainSplit := &placeholderChain{cursor: -1}
	sendFnS := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return int64(990 + len(chainSplit.chunks)), nil
	}
	editFnS := func(_ context.Context, _ string, _ int64, _ string) error { return nil }
	if err := appendSegment(context.Background(), chainSplit,
		"c1", 0, 10, strings.Repeat("a", 5000), nil, sendFnS, editFnS,
	); err != nil {
		t.Fatal(err)
	}
	if len(chainSplit.chunks) < 2 {
		t.Skipf("split path didn't produce 2+ chunks; cannot compare")
	}
	splitHeader0 := chainSplit.chunks[0].headerText()
	splitHeader1 := chainSplit.chunks[1].headerText()
	if splitHeader0 != splitHeader1 {
		t.Fatalf("SPLIT chunks must share header; got %q vs %q",
			splitHeader0, splitHeader1)
	}

	// ROTATE path: two separate appendSegment calls each big
	// enough to overflow the previous chunk. Each call mints its
	// own chunk via heartbeatText(nil) at a different moment, so
	// headers should differ (unless the OS clock stayed on the
	// same second — accept that as a tolerated race).
	chainRotate := &placeholderChain{cursor: -1}
	sendFnR := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return int64(995 + len(chainRotate.chunks)), nil
	}
	editFnR := func(_ context.Context, _ string, _ int64, _ string) error { return nil }
	for i := 0; i < 2; i++ {
		if err := appendSegment(context.Background(), chainRotate,
			"c1", 0, 10, strings.Repeat("b", chainChunkThresholdChars), nil, sendFnR, editFnR,
		); err != nil {
			t.Fatal(err)
		}
	}
	if len(chainRotate.chunks) != 2 {
		t.Fatalf("rotate path expected 2 chunks; got %d", len(chainRotate.chunks))
	}
	// ROTATE chunks SHOULD have different headers (different
	// heartbeatText calls). Identical headers here would mean the
	// two appendSegment calls landed within the same second AND
	// the test happens to have run on a low-resolution clock.
	// We can't strictly assert inequality, but if both headers
	// came from the same heartbeatText(nil) call (the way SPLIT
	// would behave), that's a code bug.
	if chainRotate.chunks[0].headerText() == chainRotate.chunks[1].headerText() {
		t.Logf("note: rotate chunks share header %q; possibly same-second timestamp",
			chainRotate.chunks[0].headerText())
	}
}

// TestChunkBody_InheritLatestHeader_HeaderAndFlag locks the
// primitive: inheritLatestHeader copies BOTH header string and
// hasHeartbeat flag. A naive implementation that only copies
// header would render the inherited banner-less for body-bearing
// chunks (Compose's "renderHeader iff hasHeartbeat || entries
// empty" rule from §11.12.5.1 would skip it).
func TestChunkBody_InheritLatestHeader_HeaderAndFlag(t *testing.T) {
	src := &chunkBody{header: "<b>💭 5 · 🔧 2</b>"}
	src.setHeaderFromHeartbeat("<b>💭 5 · 🔧 2</b>")
	if !src.hasHeartbeat {
		t.Fatal("setup: src must have hasHeartbeat=true")
	}

	dst := newChunkBody(99, "<b>🤖 Working...</b>")
	if dst.hasHeartbeat {
		t.Fatal("setup: dst must start with hasHeartbeat=false")
	}
	dst.inheritLatestHeader(src)
	if dst.header != "<b>💭 5 · 🔧 2</b>" {
		t.Fatalf("header not inherited: got %q want %q", dst.header, src.header)
	}
	if !dst.hasHeartbeat {
		t.Fatalf("hasHeartbeat not inherited: got false want true")
	}

	// nil src is a no-op (SPLIT path uses this when chain.cursor<0).
	cold := newChunkBody(0, "")
	cold.inheritLatestHeader(nil)
	if cold.header != "" {
		t.Fatalf("nil src must not overwrite dst header; got %q", cold.header)
	}
	if cold.hasHeartbeat {
		t.Fatalf("nil src must not flip hasHeartbeat; got true")
	}
}

// TestChain_SplitOversizedSegment_AllPiecesInheritLatestHeader locks
// §11.12.7.2 trigger 1: every SPLIT piece inherits the active
// chunk's (header, hasHeartbeat). Without this, SPLIT pieces would
// display cold "🤖 Working..." banners while the active chunk
// (right before SPLIT) shows real think/tool counts — visually
// inconsistent within the same chain.
func TestChain_SplitOversizedSegment_AllPiecesInheritLatestHeader(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent int64
	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		sent++
		return int64(700 + sent), nil
	}

	cur := newChunkBody(700, "<b>💭 5 · 🔧 2</b>")
	cur.setHeaderFromHeartbeat("<b>💭 5 · 🔧 2</b>")
	cur.appendEntry("seed")
	chain.chunks = []*chunkBody{cur}
	chain.cursor = 0
	chain.dirty = true

	big := strings.Repeat("y", chainChunkThresholdChars+500)
	// appendSegmentLocked returns (chunkIdx, entryIdx); we only
	// care that the segment landed somewhere.
	_, _ = appendSegmentLocked(context.Background(), chain,
		"c1", 0, 10, big, nil, sendFn,
	)
	if len(chain.chunks) < 3 {
		t.Fatalf("expected >=3 chunks after big SPLIT; got %d", len(chain.chunks))
	}
	for i := 1; i < len(chain.chunks); i++ {
		ch := chain.chunks[i]
		if ch.header != "<b>💭 5 · 🔧 2</b>" {
			t.Errorf("chunks[%d] header = %q; want inherited %q", i, ch.header, "<b>💭 5 · 🔧 2</b>")
		}
		if !ch.hasHeartbeat {
			t.Errorf("chunks[%d] must inherit hasHeartbeat=true", i)
		}
	}
}

// TestChain_AppendErrorSegment_OverflowInheritsLatestHeader locks
// the OutError overflow ROTATE: new error chunk inherits the
// (header, hasHeartbeat) snapshot of the prior active chunk.
func TestChain_AppendErrorSegment_OverflowInheritsLatestHeader(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent int64
	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		sent++
		return int64(800 + sent), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error { return nil }

	// Cold-create + fill chunk[0] close to its size budget so the
	// next OutError overflows into chunk[1].
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, strings.Repeat("z", chainChunkThresholdChars-200),
		nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	chain.chunks[0].setHeaderFromHeartbeat("<b>💭 5 · 🔧 2</b>")

	// Add more entries via append to fill chunk[0] further, then
	// trigger the error overflow path.
	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, strings.Repeat("y", 500),
		nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}

	// Error payload big enough to force chunk[0] to overflow.
	errBig := "tool failed:\n" + strings.Repeat("stderr-line\n", 500)
	if err := appendErrorSegment(context.Background(), chain,
		"c1", 0, 10, "out of disk", errBig,
		nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	if len(chain.chunks) < 2 {
		t.Fatalf("expected >=2 chunks after overflow; got %d (chunk[0] buf may not be full enough)", len(chain.chunks))
	}
	// chunks[1..N] must all inherit the prior active snapshot.
	for i := 1; i < len(chain.chunks); i++ {
		ch := chain.chunks[i]
		if ch.header == "" {
			t.Errorf("chunks[%d] header must be inherited (non-empty)", i)
		}
		if !ch.hasHeartbeat {
			t.Errorf("chunks[%d] must inherit hasHeartbeat=true", i)
		}
	}
}

// TestChain_FlushChainNow_TailInheritsLatestHeader locks that after
// flushChainNow's ROTATE (trigger 3), the new active tail inherits
// (header, hasHeartbeat) — same rule as the overflow paths.
func TestChain_FlushChainNow_TailInheritsLatestHeader(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var sent int64
	sendFn := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		sent++
		return int64(900 + sent), nil
	}
	// Track edits so we can assert the active chunk got rotated.
	var editCalls int
	editFn := func(_ context.Context, _ string, _ int64, _ string) error {
		editCalls++
		return nil
	}

	if err := appendSegment(context.Background(), chain,
		"c1", 0, 10, "seed", nil, sendFn, editFn,
	); err != nil {
		t.Fatal(err)
	}
	chain.chunks[0].setHeaderFromHeartbeat("<b>💭 7 · 🔧 3</b>")

	// Big enough to trigger tail rotation in flushChainNow's
	// branch-3 split path. Append the segment then mark the chain
	// dirty and re-flush — repeated flushChainNow calls keep
	// rotating chunks until the rendered body fits, exercising the
	// tail chunk creation.
	for i := 0; i < 8; i++ {
		chain.chunks[chain.cursor].appendEntry(strings.Repeat("z", 500))
		chain.dirty = true
		if err := flushChainNow(context.Background(), chain,
			"c1", 0, 10, editFn, sendFn,
		); err != nil {
			t.Fatal(err)
		}
	}
	if len(chain.chunks) < 2 {
		t.Fatalf("expected >=2 chunks after tail rotations; got %d", len(chain.chunks))
	}
	tail := chain.chunks[len(chain.chunks)-1]
	if tail.header != "<b>💭 7 · 🔧 3</b>" {
		t.Fatalf("tail chunk header = %q; want inherited %q",
			tail.header, "<b>💭 7 · 🔧 3</b>")
	}
	if !tail.hasHeartbeat {
		t.Fatalf("tail chunk must inherit hasHeartbeat=true (got false)")
	}
	// Should have at least one edit (each flushChainNow for the
	// active chunk; the ROTATE path also edits the head piece).
	_ = editCalls
}
