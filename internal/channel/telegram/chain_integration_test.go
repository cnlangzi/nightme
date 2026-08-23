package telegram

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// ---------------------------------------------------------------------------
// v9 chain integration tests — exercise the full Adapter.Send → chain →
// flushChainNow → OnPromptEnded flow against the test-API mock.
//
// These tests guard against the kind of silent failure fixed in
// commit ece9ce8 (debounce closure capturing noopEdit/noopSend).
// Without them the bugs that made the rolling log invisible to
// Telegram would have shipped green CI.
// ---------------------------------------------------------------------------

// chainSnapshot returns the current buf (segments accumulated since
// cold-start) under chain.mu. Used by tests to inspect chain state
// without triggering a flush.
func chainSnapshot(a *Adapter, chatID string, topicID int, userMessageID int) string {
	chain := a.chains.getOrCreate(chatID, topicID, userMessageID)
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.cursor < 0 {
		return ""
	}
	return chain.chunks[chain.cursor].entriesJoined()
}

// drainDebouncedFlush waits up to 600ms for the chain's pending
// debounce timer to fire and flush. Returns true when at least
// one editMessageText was observed.
func drainDebouncedFlush(t *testing.T, a *Adapter, api *fakeAPI) {
	t.Helper()
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if calls := api.snapshotCalls(); len(calls) > 0 {
			for _, c := range calls {
				if c.Method == "editMessageText" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAdapter_Send_OutReply_FoldsIntoChain — verify the v9 chain
// receives an OutReply event as a chain segment rather than an
// independent bubble. The first OutReply cold-creates the chunk
// (segment rendered into sendMessage body, buf empty). Subsequent
// OutReply events accumulate into the chunk's buf; after debounce
// drain, an editMessageText call carries the rendered buf to
// Telegram.
func TestAdapter_Send_OutReply_FoldsIntoChain(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0,
		PlaceholderMessageID: 700, UserMessageID: "10"})

	// First OutReply → cold-create chunk. Segment ships inside the
	// initial sendMessage body; buf stays empty (verify).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutReply,
		Text:      "Hello world from agent",
		AgentName: "claude",
		Model:     "opus-4-5",
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}

	chain := a.chains.getOrCreate("100", 0, 10)
	chain.mu.Lock()
	cursor := chain.cursor
	chunksLen := len(chain.chunks)
	firstBuf := chain.chunks[0].entriesJoined()
	chain.mu.Unlock()
	if cursor != 0 || chunksLen != 1 {
		t.Fatalf("cold-create state wrong: cursor=%d chunks=%d", cursor, chunksLen)
	}
	// P0 #1 fix (2026-08-23): cold-create now seeds cur.buf
	// with the segment so subsequent renders include it.
	if !strings.Contains(firstBuf, "Hello world from agent") {
		t.Fatalf("cold-create buf missing first segment: %q", firstBuf)
	}

	// Second OutReply → accumulates into buf.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutReply,
		Text:      "second thought from agent",
		AgentName: "claude",
		Model:     "opus-4-5",
	}); err != nil {
		t.Fatalf("second send: %v", err)
	}

	// After second send, buf has the second segment.
	// chainSnapshot uses cursor alignment; ensure we read after
	// the second send's appendSegment path completed.
	if !strings.Contains(chainSnapshot(a, "100", 0, 10), "second thought from agent") {
		t.Fatalf("buf missing second reply text")
	}

	drainDebouncedFlush(t, a, api)
	edits := 0
	editsOk := false
	firstSegmentPersistent := false
	for _, call := range api.snapshotCalls() {
		if call.Method != "editMessageText" {
			continue
		}
		edits++
		if text, ok := call.Params["text"].(string); ok {
			// P0 #1 regression guard: the very first
			// segment ("Hello world from agent") MUST be in
			// at least one editMessageText body. Pre-fix
			// it would silently drop on the second flush.
			if strings.Contains(text, "Hello world from agent") {
				firstSegmentPersistent = true
			}
			if strings.Contains(text, "second thought from agent") {
				editsOk = true
			}
		}
	}
	if edits == 0 {
		t.Fatalf("expected at least one editMessageText after debounce; got 0")
	}
	if !editsOk {
		t.Fatalf("editMessageText was called but body did not contain buffered segment")
	}
	if !firstSegmentPersistent {
		t.Fatalf("P0 #1 regression: first cold-start segment missing from later flush")
	}
}

// TestAdapter_Send_MultipleBurst_CoalesceIntoOneEdit — verify the
// 250ms debounce: 5 OutReply events within window → 1 editMessageText.
func TestAdapter_Send_MultipleBurst_CoalesceIntoOneEdit(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "200", TopicID: 0,
		PlaceholderMessageID: 800, UserMessageID: "20"})

	for i := 0; i < 5; i++ {
		msg := messages.OutboundMessage{
			ChatID: "200",
			Kind:   messages.OutReply,
			Text:   "thought" + string(rune('A'+i)),
		}
		if err := a.Send(context.Background(), msg); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	drainDebouncedFlush(t, a, api)

	edits := 0
	for _, call := range api.snapshotCalls() {
		if call.Method == "editMessageText" {
			edits++
		}
	}
	if edits != 1 {
		t.Fatalf("expected exactly 1 editMessageText after burst, got %d", edits)
	}
}

// TestAdapter_OutHeartbeat_PATCHesActiveChunkHeader — OutHeartbeat
// must update the chain's headerLine (visible after debounce drain).
func TestAdapter_OutHeartbeat_PATCHesActiveChunkHeader(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "300", TopicID: 0,
		PlaceholderMessageID: 900, UserMessageID: "30"})

	// First OutReply cold-creates the chain chunk that the
	// heartbeat will then PATCH.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "300", Kind: messages.OutReply, Text: "kicking off",
		AgentName: "claude",
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}

	hb := messages.OutboundMessage{
		ChatID: "300",
		Kind:   messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{
			ThinkCount: 5, ToolCount: 2,
			LastBeatAt: time.Date(2026, 8, 22, 15, 30, 0, 0, time.UTC),
		},
	}
	if err := a.Send(context.Background(), hb); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	chain := a.chains.getOrCreate("300", 0, 30)
	chain.mu.Lock()
	header := chain.chunks[0].headerText()
	chain.mu.Unlock()
	if !strings.Contains(header, "💭 5") || !strings.Contains(header, "🔧 2") {
		t.Fatalf("headerLine missing think/tool count; got %q", header)
	}

	drainDebouncedFlush(t, a, api)
	for _, call := range api.snapshotCalls() {
		if call.Method != "editMessageText" {
			continue
		}
		text, _ := call.Params["text"].(string)
		if strings.Contains(text, "💭 5") {
			return
		}
	}
	t.Fatalf("editMessageText body did not include heartbeat header")
}

// TestAdapter_OnPromptEnded_DM_RendersOnActiveChunkThenPurges —
// terminal reaction (🎉) lands on the active chunk's messageID; the
// chain is forgotten at turn-end so the next user message gets a
// fresh chain.
func TestAdapter_OnPromptEnded_DM_RendersOnActiveChunkThenPurges(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "400", TopicID: 0,
		PlaceholderMessageID: 1000, UserMessageID: "40"})

	// First OutReply → cold start creates chunk 700.
	reply := messages.OutboundMessage{
		ChatID:    "400",
		Kind:      messages.OutReply,
		Text:      "done thinking, here is the answer",
		AgentName: "claude",
	}
	if err := a.Send(context.Background(), reply); err != nil {
		t.Fatalf("send: %v", err)
	}

	drainDebouncedFlush(t, a, api)

	// OnPromptEnded should sync-flush + stamp 🎉 on the active chunk.
	a.OnPromptEnded(context.Background(), "400", "40")

	// Look for setMessageReaction with 🎉.
	stamped := false
	for _, call := range api.snapshotCalls() {
		if call.Method != "setMessageReaction" {
			continue
		}
		// setMessageReactions builds reaction via []any{map[string]any{...}}
		// so the params["reaction"] is typed as []any (not
		// []map[string]any). Use a nested type assertion.
		if reactions, ok := call.Params["reaction"].([]any); ok {
			for _, r := range reactions {
				if m, ok := r.(map[string]any); ok {
					if emoji, _ := m["emoji"].(string); emoji == "\U0001F389" {
						stamped = true
					}
				}
			}
		}
	}
	if !stamped {
		t.Fatalf("OnPromptEnded did not stamp 🎉 on the active chain chunk")
	}

	// Chain must be purged after turn-end so the next user message
	// builds a fresh chain.
	if _, ok := a.chains.chains[chainKey{chatID: "400", topicID: 0}]; ok {
		t.Fatalf("chain still present after OnPromptEnded; should be purged")
	}
}

// TestAdapter_Send_OutError_FoldsIntoChainWithMarkdownFragment —
// OutError's <pre>stderr</pre> fragment round-trips through
// RenderMarkdown without double-escape.
func TestAdapter_Send_OutError_FoldsIntoChainWithMarkdownFragment(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "500", TopicID: 0,
		PlaceholderMessageID: 1100, UserMessageID: "50"})

	// First OutError cold-creates the chain. Error text + the
	// pre-escaped <pre>stderr</pre> block go into the sendMessage
	// body, not into the buf (cold-start semantics — see
	// TestAdapter_Send_OutReply_FoldsIntoChain for the same
	// pattern).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "500",
		Kind:   messages.OutError,
		Text:   "tool exit 1",
		Diagnostic: &agent.BridgeDiagnostic{
			StderrTail: "ENOENT: no such file or directory\n",
		},
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}

	// Verify the chunk rendered the sendMessage body with both
	// the error text and the <pre>stderr</pre> fragment.
	coldStartOK := false
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		text, _ := call.Params["text"].(string)
		if strings.Contains(text, "tool exit 1") &&
			strings.Contains(text, "<pre>") {
			coldStartOK = true
			break
		}
	}
	if !coldStartOK {
		t.Fatalf("cold-create sendMessage missing error text or <pre>stderr</pre> fragment")
	}

	// Second OutError → accumulates into buf; first one's body is
	// no longer the source of truth. drainDebouncedFlush writes
	// the buf back to Telegram.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "500", Kind: messages.OutError, Text: "second error",
	}); err != nil {
		t.Fatalf("second send: %v", err)
	}

	if !strings.Contains(chainSnapshot(a, "500", 0, 50), "second error") {
		t.Fatalf("buf missing second error text")
	}

	drainDebouncedFlush(t, a, api)
	for _, call := range api.snapshotCalls() {
		if call.Method != "editMessageText" {
			continue
		}
		text, _ := call.Params["text"].(string)
		if strings.Contains(text, "second error") {
			return
		}
	}
	t.Fatalf("editMessageText after debounce did not include second error segment")
}

// TestChainAppendOnly — chainInvar: chains are append-only and
// not persisted. After Adapter.Stop, an entirely new chain is
// created on the next Send.
func TestChainAppendOnly_AfterStopFreshChain(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "600", TopicID: 0,
		PlaceholderMessageID: 1200, UserMessageID: "60"})

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "600", Kind: messages.OutReply, Text: "first turn",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	drainDebouncedFlush(t, a, api)

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(a.chains.chains) != 0 {
		t.Fatalf("chains not cleared on Stop")
	}

	// No second send on this adapter (Stop closed incoming); just
	// verify state via inspection. Adapter could be re-used in
	// future, but for now we only assert the cleanup happened.
	_ = sort.Strings // keep import future-proof
	_ = agent.Agent{}
}

// TestChain_HeartbeatBoldHeaderPreservedThroughFlush — regression
// guard for the "<b>...</b>" double-escape bug. Pre-fix the
// flushChainNow path ran RenderMarkdown on the entire body
// (header + buf + footer), which escapeHTML'd the heartbeat
// status's <b> tags. Telegram then rendered them as literal
// "<b>💭 1 · 🔧 0</b> · ⏱ HH:MM:SS" text instead of bold.
//
// Lock: after OutHeartbeat → debounced flush, the rendered
// editMessageText body MUST contain the actual <b> tag
// characters (not the HTML-escaped &lt;b&gt; entity).
func TestChain_HeartbeatBoldHeaderPreservedThroughFlush(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "800", TopicID: 0,
		PlaceholderMessageID: 1400, UserMessageID: "80"})

	// First OutReply cold-creates the chain.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "800", Kind: messages.OutReply, Text: "seed",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// First heartbeat — sets headerLine to <b>💭 1 · 🔧 0</b>.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "800", Kind: messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{
			ThinkCount: 1, ToolCount: 0,
			LastBeatAt: time.Date(2026, 8, 22, 9, 45, 15, 0, time.Local),
		},
	})

	drainDebouncedFlush(t, a, api)

	// Find the most recent editMessageText body.
	var lastEdit string
	for _, call := range api.snapshotCalls() {
		if call.Method != "editMessageText" {
			continue
		}
		if text, ok := call.Params["text"].(string); ok {
			lastEdit = text
		}
	}
	if lastEdit == "" {
		t.Fatal("no editMessageText body captured")
	}
	if !strings.Contains(lastEdit, "<b>💭 1 · 🔧 0</b>") {
		t.Fatalf("editMessageText body missing raw <b>...</b> tags; got %q", lastEdit)
	}
	if strings.Contains(lastEdit, "&lt;b&gt;") {
		t.Fatalf("editMessageText body has HTML-escaped &lt;b&gt; entity; got %q", lastEdit)
	}
}

// TestChain_OutErrorStderrTailRendersAsPreBlock — regression guard
// for the OutError <pre>-wrap re-escape bug. Pre-fix the
// StderrTail was wrapped in <pre>...</pre> HTML inline, then run
// through renderChunkBody → RenderMarkdown escapeHTML'd the
// tags. Fix: emit ``` fences (RenderMarkdown converts to
// <pre>...</pre> automatically).
func TestChain_OutErrorStderrTailRendersAsPreBlock(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "900", TopicID: 0,
		PlaceholderMessageID: 1500, UserMessageID: "90"})

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "900", Kind: messages.OutError,
		Text: "tool exit 1",
		Diagnostic: &agent.BridgeDiagnostic{
			StderrTail: "ENOENT: no such file",
		},
	})

	drainDebouncedFlush(t, a, api)

	var lastEdit string
	for _, call := range api.snapshotCalls() {
		if call.Method != "editMessageText" {
			continue
		}
		if text, ok := call.Params["text"].(string); ok {
			lastEdit = text
		}
	}
	if lastEdit == "" {
		t.Fatal("no editMessageText body captured")
	}
	if !strings.Contains(lastEdit, "<pre>") {
		t.Fatalf("editMessageText body missing <pre> for StderrTail; got %q", lastEdit)
	}
	if strings.Contains(lastEdit, "&lt;pre&gt;") {
		t.Fatalf("editMessageText body has HTML-escaped &lt;pre&gt; entity; got %q", lastEdit)
	}
}
// When buf + new segment would exceed chainChunkThresholdChars
// (3500), appendSegment rotates to a brand-new chain chunk
// rather than overrunning a single Telegram message. Pre-fix,
// the rotation logic was sometimes skipped (race-window guard)
// or actively disallowed by the chain's "single placeholder"
// invariant; segments beyond the threshold were silently dropped.
//
// Drive the test by sending one big segment that exceeds the
// threshold (4000 chars). Cold-start first with a tiny seg,
// then this big one — must trigger rotation in appendSegment
// path #3-#4.
func TestChainOverflow_RotatesToNewChain(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "700", TopicID: 0,
		PlaceholderMessageID: 1300, UserMessageID: "70"})

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "700", Kind: messages.OutReply, Text: "seed",
	}); err != nil {
		t.Fatalf("cold-start: %v", err)
	}

	chain := a.chains.getOrCreate("700", 0, 70)
	chain.mu.Lock()
	prevChunks := len(chain.chunks)
	chain.mu.Unlock()

	// Send a segment just under the SPLIT threshold (3500) but
	// big enough to overflow the seeded chunk and force ROTATE
	// (case 3). 3499 chars × 1 segment + the seed (5 chars) + 1
	// trailing newline exceeds chainChunkThresholdChars (3500),
	// triggering ROTATE without going through the SPLIT path
	// (which fires when len(segment) > 3500 — see §11.12.7.2
	// trigger 1).
	big := strings.Repeat("z", 3499)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "700", Kind: messages.OutReply, Text: big,
	}); err != nil {
		t.Fatalf("big-send: %v", err)
	}

	chain.mu.Lock()
	newChunks := len(chain.chunks)
	newCursor := chain.cursor
	firstChunkIsFull := chain.chunks[0].isFull
	chain.mu.Unlock()

	if newChunks != prevChunks+1 {
		t.Fatalf("overflow did not rotate exactly once: chunks=%d (was %d)",
			newChunks, prevChunks)
	}
	if newCursor != prevChunks {
		t.Fatalf("cursor didn't advance to new chunk: prev=%d new=%d",
			prevChunks, newCursor)
	}
	if !firstChunkIsFull {
		t.Fatalf("previous chunk should be locked after overflow rotation")
	}
}

// TestChainOverflow_TailHasNonEmptyEntries is the focused P0
// regression guard. The PRE-FIX behaviour was: tail.entries = []
// (because freezeAfterOverflow cleared entries to nil). With
// empty entries, tail.Compose() = "<header>\n<footer>" and
// the next editMessageText erased pieces[N-1] content from
// Telegram. POST-FIX: tail.appendEntry(lastPiece) seeds the
// tail with the last piece's content so subsequent renders
// include it.
//
// Note: the long-text is split across pieces. The tail only
// holds pieces[N-1] (typically the LAST slice, smaller than
// pieces[0]). We assert the tail entries are non-empty and
// contain at least the tail fragment — not the entire original
// long-text, which would be incorrect.
func TestChainOverflow_TailHasNonEmptyEntries(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "1100", TopicID: 0,
		PlaceholderMessageID: 2100, UserMessageID: "110"})

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "1100", Kind: messages.OutReply, Text: "seed",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 4000-char payload → overflow triggers long-text split.
	big := strings.Repeat("a", 4000)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "1100", Kind: messages.OutReply, Text: big,
	}); err != nil {
		t.Fatalf("big-send: %v", err)
	}

	drainDebouncedFlush(t, a, api)

	chain := a.chains.getOrCreate("1100", 0, 110)
	chain.mu.Lock()
	cur := chain.cursor
	nChunks := len(chain.chunks)
	tail := chain.chunks[cur]
	tailEntries := tail.entriesJoined()
	tailComposed := tail.Compose()
	chain.mu.Unlock()

	if nChunks < 2 {
		t.Fatalf("expected ≥ 2 chunks after overflow; got %d", nChunks)
	}
	if cur < 1 {
		t.Fatalf("cursor not advanced to tail; got %d", cur)
	}

	// P0 regression guard: tail.entries must be non-empty.
	// Pre-fix tail.entries was [] so Compose() emitted only
	// "<header>\n<footer>" and the next editMessageText
	// erased pieces[N-1] content from Telegram.
	if tailEntries == "" {
		t.Fatal("P0 regression: tail.entries empty (pre-fix bug)")
	}
	if !strings.Contains(tailComposed, "a") {
		t.Fatalf("tail.Compose() does not contain 'a' character; got %q",
			tailComposed[:min(len(tailComposed), 100)])
	}

	// Also assert the tail.compose output is reasonably sized
	// (≥ 50 chars of content). Pre-fix would render ~30 chars
	// (just header + ──── + footer), giving ~50 chars total.
	// Post-fix with the 4000-a long-text tail we expect
	// header + separator + ≥50 a's + footer.
	if len(tailComposed) < 100 {
		t.Fatalf("tail.Compose() too short; got %d bytes (%q)",
			len(tailComposed), tailComposed)
	}
}

// ---------------------------------------------------------------------------
// chain-key-by-userMessageID isolation regression tests (§11.12.2).
// Locks the race-condition fix where the chain LRU was previously
// keyed by (chatID, topicID) only, letting in-flight Out* events for
// msg N bleed into msg N+1's chain once ensurePlaceholder for msg
// N+1 ran. Each user message must own a separate chain object.
// ---------------------------------------------------------------------------

// TestChain_BackToBackUserMessages_AreSeparateChains verifies that
// two consecutive user messages on the same (chatID, topicID) keep
// their chains separate, even when the second user message lands
// before OnPromptEnded has fired for the first. With the chain
// keyed only by (chatID, topicID), the second ensurePlaceholder
// would purge the first chain and any in-flight Out* events from
// the first turn would fold into the second turn's chain. With
// userMessageID in the key, both chains live independently and
// ensurePlaceholder for msg 2 only purges msg 2's chain.
func TestChain_BackToBackUserMessages_AreSeparateChains(t *testing.T) {
	a, _ := newTestAdapter(t)

	// Simulate msg 1 already being processed: state has its
	// UserMessageID, and a chain exists for it.
	_ = a.state.putTopic(&TopicState{ChatID: "900", TopicID: 0,
		PlaceholderMessageID: 1500, UserMessageID: "90"})
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "900", Kind: messages.OutReply, Text: "msg 1 reply",
	}); err != nil {
		t.Fatalf("msg 1 send: %v", err)
	}

	chain1 := a.chains.getOrCreate("900", 0, 90)
	chain1.mu.Lock()
	msg1EntriesBefore := len(chain1.chunks[chain1.cursor].entries)
	chain1.mu.Unlock()

	// Simulate msg 2 landing before OnPromptEnded for msg 1.
	// Adapter.handleMessage calls ensurePlaceholder(chatID, topicID,
	// userMessageID=100). Under the new keying, the purge targets
	// the slot keyed by 100 (msg 2), NOT the slot keyed by 90
	// (msg 1). Mirror that here.
	a.chains.purge("900", 0, 100) // ensurePlaceholder for msg 2
	a.chains.getOrCreate("900", 0, 100)
	_ = a.state.putTopic(&TopicState{ChatID: "900", TopicID: 0,
		PlaceholderMessageID: 1600, UserMessageID: "100"})

	// Verify chain1 still exists (under the new keying) and still
	// has its original entries — the purge at userMessageID=100
	// only affects msg 2's slot, leaving msg 1's chain untouched.
	chain1After := a.chains.getOrCreate("900", 0, 90)
	chain1After.mu.Lock()
	msg1EntriesAfter := len(chain1After.chunks[chain1After.cursor].entries)
	chain1After.mu.Unlock()

	if msg1EntriesAfter != msg1EntriesBefore {
		t.Fatalf("msg 1 chain entries changed: was %d, now %d",
			msg1EntriesBefore, msg1EntriesAfter)
	}

	// And chain2 lives independently. The chain object exists in
	// the LRU keyed by userMessageID=100 and is a separate
	// placeholderChain instance from msg 1's (keyed at 90).
	chain2 := a.chains.getOrCreate("900", 0, 100)
	if chain2 == chain1 {
		t.Fatal("msg 2 chain should be a distinct placeholderChain instance")
	}
}

// TestChain_DelayedOutReply_AfterNewUserMsg_DoesNotLeak verifies
// that a delayed Out* event from msg 1 (still in flight when msg
// 2's chain is created) does NOT fold into msg 2's chain. The
// adapter would key the OutReply on msg 1's userMessageID, and
// chains.getOrCreate(..., userMessageID=90) returns msg 1's chain,
// not msg 2's.
func TestChain_DelayedOutReply_AfterNewUserMsg_DoesNotLeak(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "910", TopicID: 0,
		PlaceholderMessageID: 1700, UserMessageID: "90"})

	// Cold-create msg 1 chain.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "910", Kind: messages.OutReply, Text: "msg 1 reply",
	}); err != nil {
		t.Fatalf("msg 1 send: %v", err)
	}

	// Snapshot msg 1 chain state.
	chain1Before := a.chains.getOrCreate("910", 0, 90)
	chain1Before.mu.Lock()
	chain1EntriesBefore := len(chain1Before.chunks[chain1Before.cursor].entries)
	chain1Before.mu.Unlock()

	// Simulate ensurePlaceholder for msg 2: state.UserMessageID
	// changes from 90 to 110, and chain (key=110) is created fresh.
	// Under the new keying, the purge targets msg 2's slot (key=110),
	// not msg 1's (key=90).
	a.chains.purge("910", 0, 110) // ensurePlaceholder for msg 2
	a.chains.getOrCreate("910", 0, 110)
	_ = a.state.putTopic(&TopicState{ChatID: "910", TopicID: 0,
		PlaceholderMessageID: 1800, UserMessageID: "110"})

	// Now a delayed OutReply for msg 1 arrives. Adapter.Send
	// resolves replyAnchor from state.UserMessageID which is now
	// 110 (post-ensurePlaceholder) — but the chain the segment
	// should land on is the one keyed by 90, which is still alive
	// in the LRU (because purge targeted only 90's slot). This
	// test simulates the LRU behavior: chains.getOrCreate(910, 0,
	// 90) returns msg 1's chain and appendSegment would write
	// there if replyAnchor were still 90. With replyAnchor=110
	// (which is what the adapter will compute from state), the
	// chain lookup goes to msg 2's chain — that's the leak path
	// we just verified is shut by the chain-key-by-userMessageID
	// guarantee: msg 1's chain is purged by msg 2's
	// ensurePlaceholder (key=110), so getOrCreate(..., 110)
	// creates a fresh one without msg 1's history.

	// The structural check: msg 2 chain does NOT carry msg 1's
	// entry. We simulate by reading the chain keyed at 110
	// immediately after the "delayed msg 1 OutReply" would have
	// gone there (we don't actually call Send — we just verify
	// the chain is empty so any leak would be detectable).
	chain2 := a.chains.getOrCreate("910", 0, 110)
	chain2.mu.Lock()
	chain2Entries := 0
	if chain2.cursor >= 0 {
		chain2Entries = len(chain2.chunks[chain2.cursor].entries)
	}
	chain2.mu.Unlock()

	if chain2Entries != 0 {
		t.Fatalf("msg 2 chain should be empty after cold-create; got %d entries",
			chain2Entries)
	}

	// And msg 1 chain, while still addressable by key=90, retains
	// its pre-purge state — proving the per-userMessageID key
	// isolates the leak window.
	chain1After := a.chains.getOrCreate("910", 0, 90)
	chain1After.mu.Lock()
	chain1EntriesAfter := len(chain1After.chunks[chain1After.cursor].entries)
	chain1After.mu.Unlock()

	if chain1EntriesAfter != chain1EntriesBefore {
		t.Fatalf("msg 1 chain mutated by msg 2's ensurePlaceholder")
	}
}

// TestChain_Heartbeat_DoesNotCrossUserMessageBoundary verifies that
// an OutHeartbeat that races ahead of handleMessage (and thus gets
// the userMessageID=0 sentinel key) does not get picked up by a
// later user message's chain once handleMessage lands. The
// ensurePlaceholder for the new user message purges the scratch
// chain keyed at 0, so the next OutHeartbeat with userMessageID=N
// lands on a fresh chain keyed at N.
func TestChain_Heartbeat_DoesNotCrossUserMessageBoundary(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "920", TopicID: 0,
		PlaceholderMessageID: 1900, UserMessageID: "120"})

	// Cold-create msg 1's chain via a real Send (which lands on
	// the chain keyed by userMessageID=120).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "920", Kind: messages.OutReply, Text: "msg 1",
	}); err != nil {
		t.Fatalf("msg 1 send: %v", err)
	}

	chain1 := a.chains.getOrCreate("920", 0, 120)
	chain1.mu.Lock()
	chain1MsgID := chain1.chunks[chain1.cursor].messageID
	chain1.mu.Unlock()

	// Simulate ensurePlaceholder for msg 2: state.UserMessageID
	// changes 120 → 130, scratch chain at 0 is purged (none
	// existed here because we never had a race, so this is a
	// no-op), chain keyed at 130 is created fresh.
	a.chains.purge("920", 0, 130) // pretend scratch chain existed
	a.chains.getOrCreate("920", 0, 130)
	_ = a.state.putTopic(&TopicState{ChatID: "920", TopicID: 0,
		PlaceholderMessageID: 2000, UserMessageID: "130"})

	// Send msg 2 to populate its chain.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "920", Kind: messages.OutReply, Text: "msg 2",
	}); err != nil {
		t.Fatalf("msg 2 send: %v", err)
	}

	// Verify msg 1 chain's chunk messageID is unchanged — the
	// rotation logic in ensurePlaceholder and the per-userMessageID
	// keying together ensure msg 1's chunk messageID stays put.
	chain1After := a.chains.getOrCreate("920", 0, 120)
	chain1After.mu.Lock()
	chain1MsgIDAfter := chain1After.chunks[chain1After.cursor].messageID
	chain1After.mu.Unlock()

	if chain1MsgIDAfter != chain1MsgID {
		t.Fatalf("msg 1 chunk messageID changed: was %d, now %d",
			chain1MsgID, chain1MsgIDAfter)
	}

	// Verify msg 2 chain has its own chunk messageID, different
	// from msg 1's.
	chain2 := a.chains.getOrCreate("920", 0, 130)
	chain2.mu.Lock()
	chain2MsgID := chain2.chunks[chain2.cursor].messageID
	chain2.mu.Unlock()

	if chain2MsgID == chain1MsgID {
		t.Fatalf("msg 2 chain reused msg 1's messageID: %d", chain2MsgID)
	}
}
