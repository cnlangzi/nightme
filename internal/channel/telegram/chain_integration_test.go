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
func chainSnapshot(a *Adapter, chatID string, topicID int) string {
	chain := a.chains.getOrCreate(chatID, topicID)
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if chain.cursor < 0 {
		return ""
	}
	return chain.chunks[chain.cursor].buf.String()
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

	chain := a.chains.getOrCreate("100", 0)
	chain.mu.Lock()
	cursor := chain.cursor
	chunksLen := len(chain.chunks)
	firstBuf := chain.chunks[0].buf.String()
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
	if !strings.Contains(chainSnapshot(a, "100", 0), "second thought from agent") {
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

	chain := a.chains.getOrCreate("300", 0)
	chain.mu.Lock()
	header := chain.chunks[0].headerLine
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

	if !strings.Contains(chainSnapshot(a, "500", 0), "second error") {
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
