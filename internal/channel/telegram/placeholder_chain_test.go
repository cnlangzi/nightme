package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

// noSendFn is a test-double sendFn that fails if ever invoked.
// Tests that expect no sendMessage (e.g. dirty=false flushChainNow)
// wire this in to catch unexpected send calls.
func noSendFn(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
	return 0, errors.New("sendFn should not be called in this test")
}

// recordingSend returns a sendFn that records the rendered body
// to a thread-safe slice and returns a unique messageID per call.
// Used by v9 P2 task-section tests that need to observe what
// landed on Telegram without exercising the network. Returns a
// closure so each test owns its own recorder; nil-safe on
// subsequent reads via the returned accessor pointer.
func recordingSend() (sendFn sendChunkFn) {
	var (
		mu      sync.Mutex
		bodies  []string
		nextMID int64 = 1000
	)
	return func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, text)
		mid := nextMID
		nextMID++
		return mid, nil
	}
}

// noEditFn returns an editFn that fails if invoked. v9 P2 tests
// that only exercise the cold-create / append-in-place path
// (no scheduleFlushDebounced equivalent) wire this in to catch
// accidental edit calls.
func noEditFn() editChunkFn {
	return func(_ context.Context, _ string, _ int64, _ string) error {
		return errors.New("editFn should not be called in this test")
	}
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

// ---------------------------------------------------------------------------
// v9 P2 task section (§11.12.6.1) — chunkBody / chain primitive tests.
// ---------------------------------------------------------------------------

// TestRenderActiveChunkBody_TaskOnly pins the task-only turn shape
// (entries empty, taskList non-empty): Compose emits the cold-create
// header + a blank line + the `<b>📋 Tasks</b>` headline + the task
// rows + a blank line + the footer. No separator between header and
// the empty entries (we skip the divider), but a blank line flanks
// the task section so it reads as a distinct block.
func TestRenderActiveChunkBody_TaskOnly(t *testing.T) {
	cur := &chunkBody{header: "<b>🤖 Working...</b>"}
	cur.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "Write tests", Status: agent.TaskInProgress, ActiveForm: "writing"},
		{ID: "t2", Subject: "Refactor", Status: agent.TaskPending},
	})
	cur.setFooter("┌─foo─›")
	body := renderActiveChunkBody(cur)
	if !strings.Contains(body, "<b>🤖 Working...</b>") {
		t.Fatalf("cold-create header missing; got %q", body)
	}
	if !strings.Contains(body, "<b>📋 Tasks</b>") {
		t.Fatalf("task headline missing; got %q", body)
	}
	if !strings.Contains(body, "• [ ] Write tests (writing)") {
		t.Fatalf("in_progress row with ActiveForm missing; got %q", body)
	}
	if !strings.Contains(body, "• [ ] Refactor") {
		t.Fatalf("pending row missing; got %q", body)
	}
	if !strings.Contains(body, "┌─foo─›") {
		t.Fatalf("footer missing; got %q", body)
	}
	// Footer must come AFTER the task section in the rendered
	// body — order is the §11.12.6.1 contract.
	idxFooter := strings.Index(body, "┌─foo─›")
	idxTask := strings.Index(body, "<b>📋 Tasks</b>")
	if !(idxTask < idxFooter) {
		t.Fatalf("task section must precede footer; task=%d footer=%d body=%q",
			idxTask, idxFooter, body)
	}
}

// TestRenderActiveChunkBody_TaskAboveFooter locks the strict render
// order: header → entries → task section → footer. Same shape as
// feishu's `📋 Tasks` card section + statusbar footer pairing.
func TestRenderActiveChunkBody_TaskAboveFooter(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 1 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 1 · 🔧 0</b>")
	cur.appendEntry("first entry")
	cur.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "Plan the API", Status: agent.TaskCompleted},
	})
	cur.setFooter("┌─bar─›")
	body := renderActiveChunkBody(cur)
	idxHeader := strings.Index(body, "<b>💭 1 · 🔧 0</b>")
	idxSep := strings.Index(body, "────────")
	idxEntry := strings.Index(body, "first entry")
	idxTask := strings.Index(body, "<b>📋 Tasks</b>")
	idxFooter := strings.Index(body, "┌─bar─›")
	if !(idxHeader < idxSep && idxSep < idxEntry && idxEntry < idxTask && idxTask < idxFooter) {
		t.Fatalf("expected header < sep < entry < task < footer; got h=%d s=%d e=%d t=%d f=%d body=%q",
			idxHeader, idxSep, idxEntry, idxTask, idxFooter, body)
	}
	if !strings.Contains(body, "• [x] Plan the API") {
		t.Fatalf("completed task row missing; got %q", body)
	}
}

// TestRenderActiveChunkBody_NoTaskWhenEmpty locks the no-orphan-
// headline contract: when taskList is nil or len==0, Compose must
// NOT emit the `<b>📋 Tasks</b>` headline. Otherwise a stale empty
// checklist would paint a phantom title every turn.
func TestRenderActiveChunkBody_NoTaskWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []agent.AgentTaskItem
	}{
		{"nil slice", nil},
		{"empty slice", []agent.AgentTaskItem{}},
		{"only deleted", []agent.AgentTaskItem{{ID: "x", Status: agent.TaskDeleted}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
			cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
			cur.appendEntry("entry")
			cur.setTaskList(tc.tasks)
			body := renderActiveChunkBody(cur)
			if strings.Contains(body, "<b>📋 Tasks</b>") {
				t.Fatalf("empty taskList must not paint headline; got %q", body)
			}
		})
	}
}

// TestRenderActiveChunkBody_TaskHeadlinePreservedThroughRender
// pins that the pre-baked HTML headline is NOT run through
// RenderMarkdown's escapeHTML — `<b>` must survive as a literal
// tag, not get escaped to `&lt;b&gt;`. Same regression guard as
// the `🤖 Working...` header test.
func TestRenderActiveChunkBody_TaskHeadlinePreservedThroughRender(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
	cur.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "Only task", Status: agent.TaskPending},
	})
	body := renderActiveChunkBody(cur)
	if !strings.Contains(body, "<b>📋 Tasks</b>") {
		t.Fatalf("literal <b> tags lost on task headline; got %q", body)
	}
	if strings.Contains(body, "&lt;b&gt;📋 Tasks&lt;/b&gt;") {
		t.Fatalf("task headline was double-escaped; got %q", body)
	}
}

// TestChunkBody_SetTaskList_NilClears locks the defensive-copy +
// nil-reset contract. setTaskList(nil) leaves the field as nil; a
// later Compose() must not paint an orphan headline.
func TestChunkBody_SetTaskList_NilClears(t *testing.T) {
	cur := &chunkBody{}
	cur.setTaskList([]agent.AgentTaskItem{{ID: "t1", Status: agent.TaskPending}})
	if cur.taskListText() == nil {
		t.Fatal("taskList should be set after first setTaskList")
	}
	cur.setTaskList(nil)
	if cur.taskListText() != nil {
		t.Fatalf("setTaskList(nil) must clear taskList; got %v", cur.taskListText())
	}
	cur.setTaskList([]agent.AgentTaskItem{}) // len==0 also
	if cur.taskListText() != nil {
		t.Fatalf("setTaskList(empty) must clear taskList; got %v", cur.taskListText())
	}
}

// TestChunkBody_InheritLatestTaskList_CopiesAndFlag is the
// inheritLatestTaskList primitive test: src.taskList is defensively
// copied (not shared), nil src is a no-op, empty src clears the
// destination. Mirrors TestChunkBody_InheritLatestHeader_HeaderAndFlag.
func TestChunkBody_InheritLatestTaskList_CopiesAndFlag(t *testing.T) {
	src := &chunkBody{}
	src.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "alpha"},
		{ID: "t2", Subject: "beta"},
	})

	dst := &chunkBody{}
	dst.inheritLatestTaskList(src)
	if got := dst.taskListText(); len(got) != 2 || got[0].ID != "t1" || got[1].ID != "t2" {
		t.Fatalf("inherit failed; got %v", got)
	}

	// Defensive copy: mutating the inherited slice must NOT bleed
	// back into src (the chunkBody frozen-state invariant).
	dst.taskListText()[0].Subject = "MUTATED"
	if src.taskList[0].Subject == "MUTATED" {
		t.Fatalf("inherited taskList aliased src; got %q", src.taskList[0].Subject)
	}

	// nil src is a no-op (caller can short-circuit on chain state
	// without a guard).
	dst.inheritLatestTaskList(nil)
	if dst.taskListText()[0].Subject != "MUTATED" {
		t.Fatalf("nil src must be a no-op; got %v", dst.taskListText())
	}

	// Empty src clears the destination (frozen chunks with no
	// task snapshot shouldn't suddenly inherit a non-empty plan
	// when chain.lastTaskList is non-nil but the source chunk
	// itself never had a taskList — but in practice lastTaskList
	// and per-chunk taskList move together so this is defensive).
	srcEmpty := &chunkBody{}
	dst2 := &chunkBody{}
	dst2.setTaskList([]agent.AgentTaskItem{{ID: "t1"}})
	dst2.inheritLatestTaskList(srcEmpty)
	if dst2.taskListText() != nil {
		t.Fatalf("empty src must clear dst; got %v", dst2.taskListText())
	}
}

// TestChain_TaskListUpdate_RefreshesActiveChunkOnly pins the
// inheritance semantics: an OutTaskUpdate after ROTATE only
// refreshes the active chunk; the frozen ROTATE-tail keeps its
// birth snapshot. Same contract as
// TestChain_RotateChunk_InheritsLatestHeader for the header field.
func TestChain_TaskListUpdate_RefreshesActiveChunkOnly(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	chain.cursor = -1

	// Build the first chunk with a 3-task snapshot.
	tasks1 := []agent.AgentTaskItem{
		{ID: "t1", Subject: "Plan", Status: agent.TaskCompleted},
		{ID: "t2", Subject: "Code", Status: agent.TaskInProgress, ActiveForm: "coding"},
		{ID: "t3", Subject: "Test", Status: agent.TaskPending},
	}
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		tasks1, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("first setTaskList: %v", err)
	}
	if len(chain.chunks) != 1 {
		t.Fatalf("expected 1 chunk; got %d", len(chain.chunks))
	}
	frozen := chain.chunks[0]
	if len(frozen.taskListText()) != 3 {
		t.Fatalf("frozen chunk must carry 3 tasks; got %d", len(frozen.taskListText()))
	}

	// Trigger ROTATE: appendSegment on the same chain pushes the
	// active chunk over a 3500-char threshold by stuffing a long
	// entry.
	longSeg := strings.Repeat("x", chainChunkThresholdChars+10)
	if err := appendSegment(context.Background(), chain, "100", 0, 1,
		longSeg, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("appendSegment ROTATE: %v", err)
	}
	if len(chain.chunks) != 2 {
		t.Fatalf("ROTATE must produce 2 chunks; got %d", len(chain.chunks))
	}

	// Now OutTaskUpdate — the new chunk must inherit the prior
	// taskList (active chunk at that moment is chunk[0] which has
	// it), then be replaced wholesale by the new snapshot.
	tasks2 := []agent.AgentTaskItem{
		{ID: "t1", Subject: "Plan", Status: agent.TaskCompleted},
		{ID: "t2", Subject: "Code", Status: agent.TaskCompleted}, // now done
		// t3 removed
		{ID: "t4", Subject: "Ship", Status: agent.TaskPending},
	}
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		tasks2, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("second setTaskList: %v", err)
	}
	// Active chunk now reflects tasks2.
	active := chain.chunks[chain.cursor]
	if len(active.taskListText()) != 3 || active.taskListText()[1].ID != "t2" ||
		active.taskListText()[1].Status != agent.TaskCompleted {
		t.Fatalf("active chunk must carry updated taskList; got %v", active.taskListText())
	}
	// Frozen chunk[0] is unchanged (still 3 tasks).
	if len(chain.chunks[0].taskListText()) != 3 ||
		chain.chunks[0].taskListText()[1].Status != agent.TaskInProgress {
		t.Fatalf("frozen chunk must keep birth taskList; got %v",
			chain.chunks[0].taskListText())
	}
}

// TestChain_Rotate_InheritsLatestTaskList pins the ROTATE path:
// when an appendSegment pushes the active chunk over the
// threshold, the new chunk inherits the prior chunk's taskList
// via inheritLatestTaskList so the plan doesn't disappear at the
// ROTATE boundary.
func TestChain_Rotate_InheritsLatestTaskList(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	chain.cursor = -1

	tasks := []agent.AgentTaskItem{
		{ID: "t1", Subject: "only task", Status: agent.TaskPending},
	}
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		tasks, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("cold-create setTaskList: %v", err)
	}
	// Force ROTATE.
	longSeg := strings.Repeat("x", chainChunkThresholdChars+10)
	if err := appendSegment(context.Background(), chain, "100", 0, 1,
		longSeg, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("appendSegment: %v", err)
	}
	if len(chain.chunks) != 2 {
		t.Fatalf("ROTATE expected; got %d chunks", len(chain.chunks))
	}
	cur := chain.chunks[1]
	got := cur.taskListText()
	if len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("ROTATE tail must inherit taskList; got %v", got)
	}
}

// TestChain_Split_InheritsLatestTaskList pins the SPLIT path
// (trigger 1: single oversized segment). All pieces must inherit
// taskList so the user sees the plan on every slice, not just the
// head piece.
func TestChain_Split_InheritsLatestTaskList(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	chain.cursor = -1

	tasks := []agent.AgentTaskItem{
		{ID: "t1", Subject: "alpha", Status: agent.TaskInProgress},
	}
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		tasks, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("cold-create: %v", err)
	}

	// SPLIT trigger 1: a single segment > 3500 chars (after
	// RenderMarkdown + splitTelegramText it ends up as multiple
	// pieces).
	seg := strings.Repeat("abcdef ", 1000) // ~7000 chars
	if err := appendSegment(context.Background(), chain, "100", 0, 1,
		seg, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("SPLIT appendSegment: %v", err)
	}
	if len(chain.chunks) < 2 {
		t.Fatalf("SPLIT expected ≥2 chunks; got %d", len(chain.chunks))
	}
	for i, ch := range chain.chunks {
		got := ch.taskListText()
		if len(got) != 1 || got[0].ID != "t1" {
			t.Fatalf("chunk[%d] must inherit taskList; got %v", i, got)
		}
	}
}

// TestChain_NewChunk_InheritsLastTaskList_OnColdCreate pins the
// cold-create-with-prior-lastTaskList path: if chain.lastTaskList
// is set (e.g. a prior turn's chain was LRU-evicted but the next
// setTaskList races ahead of any entries / heartbeat) the first
// cold-created chunk must carry the lastTaskList snapshot.
//
// We simulate this by directly mutating chain.lastTaskList before
// calling setTaskList (no real LRU race window can be triggered
// from a single goroutine, but the test verifies the field
// propagation logic).
func TestChain_NewChunk_InheritsLastTaskList_OnColdCreate(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	chain.cursor = -1
	chain.lastTaskList = []agent.AgentTaskItem{
		{ID: "t0", Subject: "carried from prior turn", Status: agent.TaskPending},
	}

	// setTaskList overwrites lastTaskList, but Compose uses the
	// chunk's own taskList field — cold-create seeds it from
	// chain.lastTaskList when items is non-empty (which it is, by
	// virtue of the parameter check).
	tasks := []agent.AgentTaskItem{
		{ID: "t1", Subject: "new turn", Status: agent.TaskInProgress},
	}
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		tasks, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("setTaskList: %v", err)
	}
	if got := chain.chunks[0].taskListText(); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("cold-create chunk must carry the fresh taskList; got %v", got)
	}
	// chain.lastTaskList reflects the latest call (data-driven).
	if len(chain.lastTaskList) != 1 || chain.lastTaskList[0].ID != "t1" {
		t.Fatalf("chain.lastTaskList must mirror the latest snapshot; got %v",
			chain.lastTaskList)
	}
}

// TestSetTaskList_EmptyList_SilentDrop pins the silent-drop guard:
// nil / len==0 items → no chain mutation, no send call, no error.
// This is the contract that lets OutTaskUpdate use an empty slice
// as the "clear the checklist" signal without leaving orphan
// Telegram state.
// TestSetTaskList_EmptyItems_ClearsOnActiveChain pins the
// post-review clear semantic: empty / nil items on an ACTIVE
// chain (cursor ≥ 0) is the bridge "clear the checklist" signal
// (per outbound.go OutTaskUpdate comment). setTaskList clears
// chain.lastTaskList + the active chunk's taskList so the next
// Compose() omits the section entirely. chain.dirty=true so the
// debounced flush re-renders.
//
// Mirrors feishu SetTaskList: an empty Items slice means "no
// tasks remain". Replaces the v9 P2 silent-drop behaviour that
// was reverted in review.
func TestSetTaskList_EmptyItems_ClearsOnActiveChain(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	// Seed: cold-create with one task.
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		[]agent.AgentTaskItem{{ID: "t1", Subject: "do thing", Status: agent.TaskPending}},
		nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("seed setTaskList: %v", err)
	}
	if got := chain.chunks[0].taskListText(); len(got) != 1 {
		t.Fatalf("seed taskList missing; got %v", got)
	}
	if chain.lastTaskList == nil {
		t.Fatalf("seed lastTaskList must be set")
	}
	chain.dirty = false // reset for the clear path observation

	// Act: clear via empty items.
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		[]agent.AgentTaskItem{}, nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("clear setTaskList: %v", err)
	}
	if got := chain.chunks[chain.cursor].taskListText(); got != nil {
		t.Fatalf("active chunk taskList must be cleared; got %v", got)
	}
	if chain.lastTaskList != nil {
		t.Fatalf("chain.lastTaskList must be cleared; got %v", chain.lastTaskList)
	}
	if !chain.dirty {
		t.Fatalf("chain.dirty must be true after clear; got false")
	}
	// Compose must NOT render the section after clear.
	body := chain.chunks[chain.cursor].Compose()
	if strings.Contains(body, "<b>📋 Tasks</b>") {
		t.Fatalf("cleared section must not paint headline; got %q", body)
	}
	if strings.Contains(body, "do thing") {
		t.Fatalf("cleared section must not contain prior row; got %q", body)
	}
}

// TestSetTaskList_EmptyItems_SilentDropOnFreshChain pins the
// defensive guard: empty / nil items on a FRESH chain (cursor
// < 0) is silent drop. There's no chunk to clear, no chunk to
// render — creating one for a clear signal would be wasteful.
// This case is unusual in production (OutTaskCreate always has
// ≥1 task; OutTaskUpdate only fires after at least one
// OutTaskCreate seeded the chain) but the test locks the
// behaviour so a future refactor doesn't regress it.
func TestSetTaskList_EmptyItems_SilentDropOnFreshChain(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	sends := 0
	send := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		sends++
		return 0, nil
	}

	for _, items := range [][]agent.AgentTaskItem{nil, {}} {
		if err := setTaskList(context.Background(), chain, "100", 0, 1,
			items, nil, send, noEditFn()); err != nil {
			t.Fatalf("empty-on-fresh setTaskList must not error; got %v", err)
		}
	}
	if sends != 0 {
		t.Fatalf("empty-on-fresh setTaskList must not call sendFn; got %d", sends)
	}
	if len(chain.chunks) != 0 {
		t.Fatalf("empty-on-fresh setTaskList must not materialise a chunk; got %d",
			len(chain.chunks))
	}
	// lastTaskList IS updated even on silent drop (data-driven:
	// the chain reflects "no tasks now"); callers reading it get
	// the cleared snapshot.
	if chain.lastTaskList != nil {
		t.Fatalf("lastTaskList must be nil after empty setTaskList; got %v",
			chain.lastTaskList)
	}
}

// TestRenderActiveChunkBody_TaskSection_TruncatesAtBudget pins
// the rune budget enforcement: a long checklist is truncated at
// taskSectionBudgetRunes, the last visible row gets a `…` suffix,
// the markdown list shape is preserved. Without this, a
// 50-task snapshot would blow past Telegram's 4096 hard limit
// and the first sendMessage would be rejected.
func TestRenderActiveChunkBody_TaskSection_TruncatesAtBudget(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
	// 60 long tasks (each subject ~80 chars + checkbox + newline
	// ≈ 90 runes). 60 × 90 = 5400 runes — well over the 3000
	// budget. Expect ~30 rows visible plus `…`.
	const totalTasks = 60
	tasks := make([]agent.AgentTaskItem, totalTasks)
	for i := range tasks {
		tasks[i] = agent.AgentTaskItem{
			ID:      "t" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Subject: strings.Repeat("x", 80), // 80-char subject
			Status:  agent.TaskPending,
		}
	}
	cur.setTaskList(tasks)
	body := renderActiveChunkBody(cur)
	if !strings.Contains(body, "<b>📋 Tasks</b>") {
		t.Fatalf("headline missing; got %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "…") {
		t.Fatalf("body must end with `…` truncation suffix; got tail %q",
			body[len(body)-50:])
	}
	// Body rune count must stay under Telegram's 4096 hard limit.
	// Headline (~14 runes) + visible rows + `…` suffix should be
	// safely below 3500 to leave room for header (~50) + footer
	// (~100).
	if utf8.RuneCountInString(body) > 3500 {
		t.Fatalf("rendered body %d runes exceeds 3500 budget; would bust Telegram 4096 with header+footer",
			utf8.RuneCountInString(body))
	}
	// Visible row count: each row is ~90 runes; budget 3000 - 14
	// (headline) ≈ 2986 runes available for rows; 2986/90 ≈ 33
	// rows. Allow some slack — just check we have FEWER rows
	// than totalTasks (truncation actually happened).
	visibleRows := strings.Count(body, "• [ ]")
	if visibleRows >= totalTasks {
		t.Fatalf("expected truncation to drop rows; got %d visible / %d total",
			visibleRows, totalTasks)
	}
	if visibleRows < 20 {
		t.Fatalf("too few rows visible; expected ~33, got %d (budget may be too tight)", visibleRows)
	}
}

// TestRenderActiveChunkBody_TaskCancelled_Filtered locks the
// cursor-only TaskCancelled status filter: rows with this status
// are dropped before the rune budget is consumed, so a 100-task
// list with 50 cancelled rows still renders the 50 visible ones
// without burning budget on invisible items. Mirrors feishu's
// buildTaskChecklistChunks which only buckets
// InProgress / Pending / Completed.
func TestRenderActiveChunkBody_TaskCancelled_Filtered(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
	cur.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "alive", Status: agent.TaskPending},
		{ID: "t2", Subject: "dead-by-cancel", Status: agent.TaskCancelled},
		{ID: "t3", Subject: "alive2", Status: agent.TaskCompleted},
	})
	body := renderActiveChunkBody(cur)
	if strings.Contains(body, "dead-by-cancel") {
		t.Fatalf("TaskCancelled row must be filtered; got %q", body)
	}
	if !strings.Contains(body, "alive") || !strings.Contains(body, "alive2") {
		t.Fatalf("live rows must still render; got %q", body)
	}
}

// TestRenderActiveChunkBody_TaskSubjectEscapesHTML locks the
// RenderMarkdown escape path: a subject containing HTML tags
// (e.g. user-supplied subject like `<script>`) must be rendered
// as literal text — `<` / `>` escaped to `&lt;` / `&gt;` — so
// Telegram's HTML parse_mode doesn't try to interpret it. XSS
// regression guard.
func TestRenderActiveChunkBody_TaskSubjectEscapesHTML(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
	cur.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "<script>alert('xss')</script>", Status: agent.TaskPending},
		{ID: "t2", Subject: "with & ampersand", Status: agent.TaskPending},
	})
	body := renderActiveChunkBody(cur)
	if strings.Contains(body, "<script>") {
		t.Fatalf("subject <script> must be HTML-escaped; got %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("subject should appear as &lt;script&gt;...; got %q", body)
	}
	if !strings.Contains(body, "&amp;") {
		t.Fatalf("ampersand must be HTML-escaped to &amp;; got %q", body)
	}
}

// TestSetTaskList_ColdCreateWithStatusBar pins the production
// path: first-ever setTaskList on a fresh chain lands together
// with statusbar lines (agent typically stamps StatusBar on the
// same OutTaskCreate event). The cold-created chunk must carry
// both task section AND footer panel — no round-trip through
// two separate events.
func TestSetTaskList_ColdCreateWithStatusBar(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	tasks := []agent.AgentTaskItem{
		{ID: "t1", Subject: "Plan", Status: agent.TaskInProgress, ActiveForm: "planning"},
	}
	footer := []string{"Agent: claude · Model: opus-4-5", "Session: sess-1", "Workspace: main"}
	if err := setTaskList(context.Background(), chain, "100", 0, 1,
		tasks, footer, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("cold-create: %v", err)
	}
	// chain.lastFooter must mirror the input.
	if len(chain.lastFooter) != 3 {
		t.Fatalf("chain.lastFooter not refreshed; got %d lines", len(chain.lastFooter))
	}
	body := chain.chunks[0].Compose()
	if !strings.Contains(body, "<b>📋 Tasks</b>") {
		t.Fatalf("task headline missing; got %q", body)
	}
	if !strings.Contains(body, "• [ ] Plan (planning)") {
		t.Fatalf("in_progress row missing; got %q", body)
	}
	if !strings.Contains(body, "claude") {
		t.Fatalf("footer agent line missing; got %q", body)
	}
	if !strings.Contains(body, "sess-1") {
		t.Fatalf("footer session line missing; got %q", body)
	}
}

// TestRenderActiveChunkBody_TaskInProgress_ShowsActiveFormSuffix
// locks the row shape: in_progress tasks render as
// `- [ ] Subject (ActiveForm)`. If ActiveForm is empty the suffix
// is omitted (no trailing parens). pending / completed rows never
// carry the suffix.
func TestRenderActiveChunkBody_TaskInProgress_ShowsActiveFormSuffix(t *testing.T) {
	cur := &chunkBody{header: "<b>💭 0 · 🔧 0</b>"}
	cur.setHeaderFromHeartbeat("<b>💭 0 · 🔧 0</b>")
	cur.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "with form", Status: agent.TaskInProgress, ActiveForm: "doing it"},
		{ID: "t2", Subject: "no form", Status: agent.TaskInProgress},
		{ID: "t3", Subject: "done", Status: agent.TaskCompleted},
		{ID: "t4", Subject: "later", Status: agent.TaskPending},
	})
	body := renderActiveChunkBody(cur)
	wants := []string{
		"• [ ] with form (doing it)",
		"• [ ] no form",        // no trailing parens
		"• [x] done",
		"• [ ] later",
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("body missing %q; got %q", w, body)
		}
	}
	// Defensive: pending / completed rows must not pick up the
	// active-form suffix even if ActiveForm is populated.
	if strings.Contains(body, "• [x] done (") || strings.Contains(body, "• [ ] later (") {
		t.Fatalf("non-in_progress rows must not carry ActiveForm suffix; got %q", body)
	}
}

// ---------------------------------------------------------------------------
// v9 P3 — chunkBody.hasVisibleEntries / materializeChunk / blank-chunk bug fix
// ---------------------------------------------------------------------------

// TestChunkBody_HasVisibleEntries_Empty verifies a freshly-constructed
// chunkBody (no entries, no taskList) returns false — there's nothing
// to render.
func TestChunkBody_HasVisibleEntries_Empty(t *testing.T) {
	b := newChunkBody(0, "<b>🤖 Working...</b>")
	if b.hasVisibleEntries() {
		t.Fatalf("empty chunk must report hasVisibleEntries=false")
	}
}

// TestChunkBody_HasVisibleEntries_WhitespaceOnly is the central
// regression test for the blank-chunk bug (§11.12.19.2): ROTATE /
// SPLIT paths were minting chunks whose entries were pure
// whitespace. hasVisibleEntries must return false so materializeChunk
// drops them.
func TestChunkBody_HasVisibleEntries_WhitespaceOnly(t *testing.T) {
	b := newChunkBody(0, "<b>🤖 Working...</b>")
	b.appendEntry(" ")
	b.appendEntry("\n")
	b.appendEntry("\t\n")
	if b.hasVisibleEntries() {
		t.Fatalf("whitespace-only entries must report hasVisibleEntries=false")
	}
}

// TestChunkBody_HasVisibleEntries_MixedHasReal verifies the predicate
// is OR-over entries: any single non-whitespace entry makes it true.
func TestChunkBody_HasVisibleEntries_MixedHasReal(t *testing.T) {
	b := newChunkBody(0, "")
	b.appendEntry("")
	b.appendEntry("\n")
	b.appendEntry("real content")
	if !b.hasVisibleEntries() {
		t.Fatalf("mixed entries with at least one non-whitespace must report true")
	}
}

// TestChunkBody_HasVisibleEntries_FooterDoesNotCount is the
// §11.12.19.2 root-cause regression test. The whole point of
// hasVisibleEntries (vs strings.TrimSpace(Compose())) is that the
// footer's box-drawing chars must NOT count as content. A chunk
// with whitespace entries + a StatusBar footer must still report
// false — that's the bug P3 fixes.
func TestChunkBody_HasVisibleEntries_FooterDoesNotCount(t *testing.T) {
	b := newChunkBody(0, "<b>💭 1 · 🔧 0</b>")
	b.appendEntry("\n")
	b.setFooter("┌──────────────›\n│ 🤖: claude · ...\n│ 💰: $0.05\n│ 📁: code/x\n└───────────────›")
	if b.hasVisibleEntries() {
		t.Fatalf("footer chrome must not flip hasVisibleEntries to true; the bug is exactly that it would")
	}
}

// TestChunkBody_HasVisibleEntries_TaskListCounts covers the
// setTaskList cold-create path: a chunk with NO entries but a
// non-empty taskList must still report true — the `<b>📋 Tasks</b>`
// + task rows are visible content. Regression for the bug where
// setTaskList cold-create would be silently dropped by
// materializeChunk.
func TestChunkBody_HasVisibleEntries_TaskListCounts(t *testing.T) {
	b := newChunkBody(0, "<b>🤖 Working...</b>")
	b.setTaskList([]agent.AgentTaskItem{
		{ID: "t1", Subject: "only task", Status: agent.TaskPending},
	})
	if !b.hasVisibleEntries() {
		t.Fatalf("chunk with only taskList must report hasVisibleEntries=true")
	}
}

// TestMaterializeChunk_DropsBlank verifies that materializeChunk
// returns (false, nil) for a blank chunk and does NOT invoke
// sendFn — the core blank-chunk bug fix.
func TestMaterializeChunk_DropsBlank(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	b := newChunkBody(0, "<b>🤖 Working...</b>")
	b.appendEntry("\n")
	b.setFooter("┌──›\n│ 🤖: ...\n└───›")

	sendFn := noSendFn // any send call would fail this test
	materialized, err := materializeChunk(context.Background(), chain, b,
		"100", 0, 1, sendFn)
	if err != nil {
		t.Fatalf("materializeChunk on blank chunk must return nil err; got %v", err)
	}
	if materialized {
		t.Fatalf("blank chunk must report materialized=false")
	}
	if len(chain.chunks) != 0 {
		t.Fatalf("blank chunk must NOT be appended to chain.chunks; got %d", len(chain.chunks))
	}
	if b.messageID != 0 {
		t.Fatalf("blank chunk must NOT have messageID set; got %d", b.messageID)
	}
}

// TestMaterializeChunk_SendsVisible covers the happy path: a chunk
// with real entries + footer gets sent, messageID is written back,
// and the chunk is appended to chain.chunks.
func TestMaterializeChunk_SendsVisible(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	b := newChunkBody(0, "<b>💭 1</b>")
	b.appendEntry("real content")

	materialized, err := materializeChunk(context.Background(), chain, b,
		"100", 0, 1, recordingSend())
	if err != nil {
		t.Fatalf("materializeChunk: %v", err)
	}
	if !materialized {
		t.Fatalf("visible chunk must report materialized=true")
	}
	if len(chain.chunks) != 1 {
		t.Fatalf("visible chunk must be appended; got %d chunks", len(chain.chunks))
	}
	if b.messageID == 0 {
		t.Fatalf("visible chunk must have messageID set")
	}
	if !chain.dirty {
		t.Fatalf("chain.dirty must be true after materializeChunk")
	}
}

// TestMaterializeChunk_PartialFooter_StillBlank: even with footer
// chrome (which strings.TrimSpace(Compose()) can't detect as blank),
// the predicate still drops it because footer doesn't count.
func TestMaterializeChunk_PartialFooter_StillBlank(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	b := newChunkBody(0, "<b>💭 1</b>")
	b.appendEntry("\n")
	chain.lastFooter = []string{"fake-line"}

	materialized, err := materializeChunk(context.Background(), chain, b,
		"100", 0, 1, noSendFn)
	if err != nil {
		t.Fatalf("materializeChunk: %v", err)
	}
	if materialized {
		t.Fatalf("whitespace entries + footer must still report blank; that's the bug")
	}
}

// TestMaterializeChunk_SendFnErrorPropagates: sendFn failure must
// propagate and the chain.chunks / chain.dirty must NOT be mutated.
func TestMaterializeChunk_SendFnErrorPropagates(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	b := newChunkBody(0, "")
	b.appendEntry("real")

	failingSend := func(_ context.Context, _ string, _ int, _ int, _ string) (int64, error) {
		return 0, errors.New("simulated Telegram API failure")
	}
	materialized, err := materializeChunk(context.Background(), chain, b,
		"100", 0, 1, failingSend)
	if err == nil {
		t.Fatalf("sendFn error must propagate")
	}
	if materialized {
		t.Fatalf("materialized must be false on sendFn error")
	}
	if len(chain.chunks) != 0 {
		t.Fatalf("failed chunk must NOT be appended; got %d", len(chain.chunks))
	}
	if chain.dirty {
		t.Fatalf("chain.dirty must remain false on sendFn error")
	}
}

// TestAppendSegment_WhitespaceSegment_NoNewChunk — the regression
// test for the user-visible blank-chunk bug (§11.12.19.1). An
// appendSegment with a whitespace-only segment must NOT mint a
// new Telegram message; chain.chunks stays at 0 (no cold-create)
// when starting from an empty chain.
func TestAppendSegment_WhitespaceSegment_NoNewChunk(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	sendFn := noSendFn // any send call would fail this test
	err := appendSegment(context.Background(), chain, "100", 0, 1,
		"\n  \n\t", nil, sendFn, noEditFn())
	if err != nil {
		t.Fatalf("appendSegment with whitespace segment must return nil err; got %v", err)
	}
	if len(chain.chunks) != 0 {
		t.Fatalf("whitespace cold-create must NOT mint a chunk; got %d", len(chain.chunks))
	}
	if chain.cursor != -1 {
		t.Fatalf("chain.cursor must remain -1; got %d", chain.cursor)
	}
}

// TestAppendSegment_WhitespaceSegment_NoRotateIntoNewChunk — the
// ROTATE-side of the same bug. A real segment first, then a
// whitespace ROTATE trigger must NOT mint a second chunk.
func TestAppendSegment_WhitespaceSegment_NoRotateIntoNewChunk(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	if err := appendSegment(context.Background(), chain, "100", 0, 1,
		"first real content", nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("first appendSegment: %v", err)
	}
	if len(chain.chunks) != 1 {
		t.Fatalf("first chunk should materialize; got %d", len(chain.chunks))
	}
	// Force a ROTATE by stuffing entries past the 3500 threshold.
	if err := appendSegment(context.Background(), chain, "100", 0, 1,
		strings.Repeat("x", chainChunkThresholdChars+10),
		nil, recordingSend(), noEditFn()); err != nil {
		t.Fatalf("long appendSegment: %v", err)
	}
	if len(chain.chunks) != 2 {
		t.Fatalf("ROTATE should have created chunk 2; got %d", len(chain.chunks))
	}
	preCount := len(chain.chunks)
	// Now a whitespace segment that happens to trigger ROTATE must
	// NOT mint chunk 3.
	err := appendSegment(context.Background(), chain, "100", 0, 1,
		"\n\n", nil, noSendFn, noEditFn())
	if err != nil {
		t.Fatalf("whitespace ROTATE appendSegment: %v", err)
	}
	if len(chain.chunks) != preCount {
		t.Fatalf("whitespace ROTATE must NOT mint a new chunk; pre=%d post=%d", preCount, len(chain.chunks))
	}
}

// TestAppendSegmentLocked_WhitespaceSegment_NoNewChunk — same
// regression but on the OutToolStart path (which calls
// appendSegmentLocked directly, bypassing the adapter-level guard).
func TestAppendSegmentLocked_WhitespaceSegment_NoNewChunk(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	chunkIdx, entryIdx := appendSegmentLocked(context.Background(), chain,
		"100", 0, 1, "\n", nil, noSendFn)
	if chunkIdx != -1 || entryIdx != -1 {
		t.Fatalf("whitespace appendSegmentLocked must return (-1, -1); got (%d, %d)", chunkIdx, entryIdx)
	}
	if len(chain.chunks) != 0 {
		t.Fatalf("whitespace appendSegmentLocked must NOT mint a chunk; got %d", len(chain.chunks))
	}
}

// TestAppendErrorSegment_WhitespaceError_NoNewChunk — the OutError
// path also must drop blank (text="", stderr="") cold-create calls.
func TestAppendErrorSegment_WhitespaceError_NoNewChunk(t *testing.T) {
	chain := &placeholderChain{cursor: -1}
	err := appendErrorSegment(context.Background(), chain, "100", 0, 1,
		"", "", nil, noSendFn, noEditFn())
	if err != nil {
		t.Fatalf("appendErrorSegment with empty text+stderr: %v", err)
	}
	if len(chain.chunks) != 0 {
		t.Fatalf("empty error must NOT mint a chunk; got %d", len(chain.chunks))
	}
}

// TestSplitOversizedSegment_BlankPiece_NoSendMessage — SPLIT path
// must not send a Telegram message for a piece that's pure
// whitespace (rare, but the predicate still gates it). Locks in
// the v9 P3 "no double-append" regression: each materialized
// piece must produce exactly one chunk in chain.chunks AND exactly
// one sendMessage call (pre-fix code appended twice, once inside
// materializeChunk and once in the SPLIT loop after the loop).
func TestSplitOversizedSegment_BlankPiece_NoSendMessage(t *testing.T) {
	chain := &placeholderChain{cursor: -1}

	var (
		mu       sync.Mutex
		sendHits int
	)
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		// Reject empty / whitespace-only bodies — this would catch
		// a regression where a blank piece slips through
		// hasVisibleEntries.
		if strings.TrimSpace(text) == "" {
			t.Errorf("sendFn must NOT be called with whitespace-only body; got %q", text)
		}
		sendHits++
		return int64(1000 + sendHits), nil
	}

	// Construct a segment whose split would produce at least one
	// piece. splitTelegramText splits on \n boundaries; an oversize
	// segment full of content forces SPLIT trigger 1.
	oversize := strings.Repeat("x", chainChunkThresholdChars+200)
	err := appendSegment(context.Background(), chain, "100", 0, 1,
		oversize, nil, sendFn, noEditFn())
	if err != nil {
		t.Fatalf("appendSegment: %v", err)
	}

	mu.Lock()
	hits := sendHits
	mu.Unlock()

	// The v9 P3 fix: materializeChunk appends to chain.chunks
	// atomically. The SPLIT loop only counts via materializedCount
	// (no slice append). So sendFn count must equal chain.chunks
	// count after SPLIT — no double-append, no phantom chunks.
	if hits == 0 {
		t.Fatalf("SPLIT must invoke sendFn at least once")
	}
	if hits != len(chain.chunks) {
		t.Fatalf("sendFn count (%d) must match chain.chunks count (%d); mismatch indicates double-append or phantom chunk", hits, len(chain.chunks))
	}
	for i, ch := range chain.chunks {
		if ch.messageID == 0 {
			t.Fatalf("chunk[%d] must have a non-zero messageID after SPLIT", i)
		}
	}
	if chain.cursor < 0 || chain.cursor >= len(chain.chunks) {
		t.Fatalf("cursor %d out of range after SPLIT", chain.cursor)
	}
	if chain.chunks[chain.cursor].messageID == 0 {
		t.Fatalf("active cursor chunk must have a non-zero messageID")
	}
}

// TestFlushChainNow_OverflowWithBlankTail_DirtyInvariant — locks in
// the v9 P3 F1 fix. Scenario: splitTelegramText yields ≥3 pieces,
// intermediates materialize, tail drops (blank). Without F1, the
// tail block only cleared chain.dirty when the tail materialized;
// with intermediates setting chain.dirty=true via materializeChunk,
// dirty stayed true while cursor pointed at the frozen cur. Next
// flushChainNow would Compose cur (entries=nil after
// freezeAfterOverflow + footer) and editFn would clobber the user's
// existing pieces[0] message with header-only content.
//
// Repro path: seed cur with enough content that Compose > 3900
// (forces overflow); chain.dirty must be false after flushChainNow
// completes regardless of whether the tail piece was blank.
func TestFlushChainNow_OverflowWithBlankTail_DirtyInvariant(t *testing.T) {
	chain := &placeholderChain{cursor: 0, chunks: []*chunkBody{{
		messageID: 100,
		header:    "<b>💭 1</b>",
	}}}

	// Stuff entries to push Compose well past 3900 so splitTelegramText
	// produces multiple pieces. The tail-piece boundary is
	// determined by splitTelegramText; we don't control which piece
	// is "last" directly — but the F1 invariant holds regardless:
	// chain.dirty must be false after flushChainNow returns.
	bigEntry := strings.Repeat("x", maxTelegramTextLength-100) // forces multi-piece split
	chain.chunks[0].appendEntry(bigEntry)
	chain.dirty = true

	var (
		mu       sync.Mutex
		sendHits int
	)
	sendFn := func(_ context.Context, _ string, _ int, _ int, text string) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		// Reject empty bodies — would catch a regression where
		// the blank-tail predicate fails to drop.
		if strings.TrimSpace(text) == "" {
			t.Errorf("sendFn must NOT be called with whitespace body; got %q", text)
		}
		sendHits++
		return int64(2000 + sendHits), nil
	}
	editFn := func(_ context.Context, _ string, _ int64, _ string) error { return nil }

	if err := flushChainNow(context.Background(), chain,
		"100", 0, 1, editFn, sendFn); err != nil {
		t.Fatalf("flushChainNow: %v", err)
	}

	// The F1 invariant: chain.dirty must be false after the
	// overflow completes. Pre-fix bug left dirty=true with cursor
	// at frozen cur; a follow-up flushChainNow would then Compose
	// cur (entries=nil after freezeAfterOverflow) and editFn
	// clobber pieces[0]'s messageID with empty render.
	if chain.dirty {
		t.Fatalf("chain.dirty must be false after flushChainNow completes; F1 invariant violated")
	}
	if chain.cursor < 0 {
		t.Fatalf("cursor must point at a valid chunk after flushChainNow; got %d", chain.cursor)
	}
	// And cursor must NOT point at the original frozen cur (which
	// has entries=nil + flushedLen set from the overflow). It must
	// point at the last materialized chunk.
	if chain.cursor == 0 && len(chain.chunks) > 1 {
		t.Fatalf("cursor stuck at frozen cur[0]; should advance to last materialized chunk")
	}
	// A follow-up flushChainNow must be a no-op (dirty=false gate),
	// confirming the F1 invariant end-to-end: no clobbering
	// editFn call on the original cur.messageID.
	editFnCalls := 0
	guardedEditFn := func(_ context.Context, _ string, _ int64, _ string) error {
		editFnCalls++
		return nil
	}
	if err := flushChainNow(context.Background(), chain,
		"100", 0, 1, guardedEditFn, sendFn); err != nil {
		t.Fatalf("follow-up flushChainNow: %v", err)
	}
	if editFnCalls != 0 {
		t.Fatalf("follow-up flushChainNow must no-op (dirty=false); editFn called %d times — F1 invariant broken", editFnCalls)
	}
}
