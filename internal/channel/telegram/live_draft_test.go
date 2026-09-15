package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// richMessageFirstBlockText extracts the first paragraph block's text
// from the rich_message JSON envelope used by sendRichMessage and
// editMessageText(rich_message=...). Returns "" if the envelope is
// malformed or empty. The new renderStacksAsBlocks emits a bare
// JSON array of paragraph blocks (no wrapping object); the helper
// accepts both the bare-array form and the legacy wrapped form
// `{"blocks":[…]}`.
func richMessageFirstBlockText(raw any) string {
	blocks := richMessageBlocks(raw)
	if len(blocks) == 0 {
		return ""
	}
	return blocks[0]
}

// richMessageBlocks decodes the rich_message envelope and returns
// every paragraph block's text in order. Returns nil if the
// envelope is malformed or empty.
func richMessageBlocks(raw any) []string {
	var data []byte
	switch v := raw.(type) {
	case json.RawMessage:
		data = []byte(v)
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		// jsontext.Value (Go 1.27+ alias for json.RawMessage) is a
		// []byte underneath but is its own type — handle by fmt.
		data = []byte(fmt.Sprintf("%v", v))
	}
	// Try the new bare-array shape first.
	var bare []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &bare); err == nil && len(bare) > 0 {
		out := make([]string, 0, len(bare))
		for _, b := range bare {
			out = append(out, b.Text)
		}
		return out
	}
	// Fall back to the legacy wrapped shape `{"blocks":[…]}`.
	var env struct {
		Blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"blocks"`
	}
	_ = json.Unmarshal(data, &env)
	out := make([]string, 0, len(env.Blocks))
	for _, b := range env.Blocks {
		out = append(out, b.Text)
	}
	return out
}

// TestAdapter_Send_Group_OutThinking_CreatesDraftMessage verifies
// the single-FIFO flush logic. The buffer holds all events
// silently — no cap-based flush. A flush fires on OnPromptEnded
// (or 10s timer), popping the LATEST 5 events. 5 events +
// OnPromptEnded → 1 sendRichMessage cold-create (5 blocks) + 1
// deleteMessage. The cold-create carries reply_to_message_id so
// the DraftMessage hangs under the user's message.
func TestAdapter_Send_Group_OutThinking_CreatesDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// 5 OutThinking events. With the FIFO model there is no
	// cap-triggered flush — every event just appends. OnPromptEnded
	// is the flush trigger.
	for i := 1; i <= 5; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("buffered thought %d", i),
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if draft := findCallByMethod(api.Calls, "sendMessageDraft"); draft != nil {
		t.Fatalf("group must NOT use sendMessageDraft; got %+v", draft)
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 0 {
		t.Fatalf("first 5 events must NOT trigger sendRichMessage (no count-based flush); got %d (calls=%+v)",
			len(sends), api.Calls)
	}

	// OnPromptEnded → endProcess → flushLocked (sendRichMessage
	// cold-create, 5 blocks) → deleteMessage.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("expected sendRichMessage cold-create on OnPromptEnded flush; got calls=%+v", api.Calls)
	}
	if cold.Params["reply_to_message_id"] != 1 {
		t.Fatalf("reply_to_message_id = %v, want 1 (userMsgID)", cold.Params["reply_to_message_id"])
	}
	blocks := richMessageBlocks(cold.Params["rich_message"])
	if len(blocks) != 5 {
		t.Fatalf("cold-create blocks = %d, want 5 (events 1..5); got %v", len(blocks), blocks)
	}
	for i, want := range []string{
		"💭 buffered thought 1", "💭 buffered thought 2",
		"💭 buffered thought 3", "💭 buffered thought 4",
		"💭 buffered thought 5",
	} {
		if blocks[i] != want {
			t.Fatalf("cold-create block[%d] = %q, want %q", i, blocks[i], want)
		}
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) != 1 {
		t.Fatalf("OnPromptEnded must delete the cold-created DraftMessage; got %d deletes", len(dels))
	}
}

// TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace
// verifies the FIFO flush's "latest 5" pop semantics. 10
// OutThinking events buffer silently; OnPromptEnded triggers the
// final flush which pops the latest 5 (events 6..10) as
// sendRichMessage. The remaining 5 events (1..5) are dropped —
// the DraftMessage is being deleted and the buffer is single-FIFO
// with no "catch up" loop in endProcess.
func TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	for i := 1; i <= 10; i++ {
		_ = a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("thought %d", i),
		})
	}

	// Before OnPromptEnded, no flush has happened — all 10 events
	// sit in the FIFO buffer.
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 0 {
		t.Fatalf("pre-OnPromptEnded: expected 0 sendRichMessage (no count-based flush); got %d", len(sends))
	}

	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendRichMessage cold-create, got %d (calls=%+v)", len(sends), api.Calls)
	}
	coldBlocks := richMessageBlocks(sends[0].Params["rich_message"])
	if len(coldBlocks) != 5 {
		t.Fatalf("cold-create blocks = %d, want 5 (latest 5 of 10 = events 6..10); got %v", len(coldBlocks), coldBlocks)
	}
	if coldBlocks[0] != "💭 thought 6" {
		t.Fatalf("cold-create block[0] = %q, want %q", coldBlocks[0], "💭 thought 6")
	}
	if coldBlocks[4] != "💭 thought 10" {
		t.Fatalf("cold-create block[4] = %q, want %q", coldBlocks[4], "💭 thought 10")
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) != 1 {
		t.Fatalf("OnPromptEnded must delete the cold-created DraftMessage; got %d deletes", len(dels))
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("endProcess only calls flushLocked once — no editMessageText expected; got %d", len(edits))
	}
}

// TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart verifies
// that OutToolStart and OutToolEnd events are simply appended to
// the FIFO buffer in arrival order — no slot composition. The
// final flush (OnPromptEnded) sends both events as 2 paragraph
// blocks: [Start block, End block] in that order.
func TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Pair: OutToolStart then OutToolEnd. Under the single-FIFO
	// model they simply buffer as two richTurnEntry rows.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Write", Args: "/tmp/bar.go"},
		Text:   "● Write(/tmp/bar.go)",
	})
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolEnd,
		Tool:   &messages.ToolInfo{Name: "Write", Output: "hello world"},
		Text:   "📝 Write → 11 bytes",
	})

	// Buffer has 2 entries; no flush yet.
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 0 {
		t.Fatalf("pre-OnPromptEnded: expected 0 sendRichMessage; got %d", len(sends))
	}

	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendRichMessage (cold-create from endProcess flush), got %d (calls=%+v)", len(sends), api.Calls)
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("no editMessageText expected (single flush per turn); got %d", len(edits))
	}
	blocks := richMessageBlocks(sends[0].Params["rich_message"])
	if len(blocks) != 2 {
		t.Fatalf("FIFO rich_message blocks = %d, want 2 (Write/Start + Write/End); got %v", len(blocks), blocks)
	}
	if blocks[0] != "● Write(/tmp/bar.go)" {
		t.Fatalf("FIFO block[0] = %q, want %q", blocks[0], "● Write(/tmp/bar.go)")
	}
	if blocks[1] != "⎿  📝 Write → 11 bytes" {
		t.Fatalf("FIFO block[1] = %q, want %q", blocks[1], "⎿  📝 Write → 11 bytes")
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) != 1 {
		t.Fatalf("endProcess must delete the cold-created DraftMessage; got %d deletes", len(dels))
	}
}

// TestAdapter_Send_Group_NextTurn_CreatesFreshDraftMessage verifies
// that endProcess drops the prior turn's entry from m.entries so
// turn N+1 can cold-create a fresh DraftMessage rather than
// editMessageText the now-deleted id. With the single-FIFO
// model, each turn's OnPromptEnded triggers exactly one
// sendRichMessage (cold-create, latest 5 of 10) + one
// deleteMessage. The 5 events that don't fit in the latest-5
// window are dropped — endProcess is the closing bracket, not a
// catch-up loop.
func TestAdapter_Send_Group_NextTurn_CreatesFreshDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: 10 OutThinking events. OnPromptEnded → endProcess
	// → flushLocked (cold-create, latest 5 = events 6..10) →
	// deleteMessage. Turn 1's entry is dropped from m.entries.
	for i := 1; i <= 10; i++ {
		_ = a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("turn 1 thought %d", i),
		})
	}
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("turn 1: expected 1 cold-create sendRichMessage, got %d (calls=%+v)", len(sends), api.Calls)
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("turn 1: endProcess flushes once (cold-create); no editMessageText expected; got %d", len(edits))
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) != 1 {
		t.Fatalf("turn 1 OnPromptEnded should delete the cold-created DraftMessage; got %d deletes", len(dels))
	}
	turn1Send := findCallByMethod(api.Calls, "sendRichMessage")
	if replyTo, _ := turn1Send.Params["reply_to_message_id"].(int); replyTo != 1 {
		t.Fatalf("turn 1 cold-create reply_to_message_id = %v, want 1", turn1Send.Params["reply_to_message_id"])
	}
	api.Calls = nil

	// Turn 2: 10 OutThinking events for the new turn. The prior
	// turn's entry was dropped, so the FIRST flush is again a
	// cold-create (sendRichMessage), not an editMessageText on
	// the deleted id.
	for i := 1; i <= 10; i++ {
		_ = a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("turn 2 thought %d", i),
		})
	}
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 2 cold-create should produce 1 sendRichMessage; got %d (calls=%+v)", len(sends), api.Calls)
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("turn 2 first flush must NOT edit prior DraftMessage (entry was dropped); got %d edits", len(edits))
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) != 1 {
		t.Fatalf("turn 2 OnPromptEnded should delete its own DraftMessage; got %d deletes", len(dels))
	}
}

// TestAdapter_Send_Group_DraftMessage_RoutedThroughTopic verifies
// that when the user is inside a forum topic (topicID > 0), the
// flush carries message_thread_id so the DraftMessage lives in
// the same topic as the conversation. With the single-FIFO
// model, OnPromptEnded triggers the flush; the cold-create must
// still carry message_thread_id.
func TestAdapter_Send_Group_DraftMessage_RoutedThroughTopic(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 42)

	// 5 events buffer silently. No cap trigger in the FIFO model.
	for i := 1; i <= 5; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw + ":42",
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("topic-scoped thought %d", i),
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	a.OnPromptEnded(context.Background(), "tg_"+raw+":42", "1", agent.PromptEndClean)

	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("expected sendRichMessage cold-create on OnPromptEnded flush; got calls=%+v", api.Calls)
	}
	if thread, _ := cold.Params["message_thread_id"].(int); thread != 42 {
		t.Fatalf("message_thread_id = %v, want 42", cold.Params["message_thread_id"])
	}
}

// TestAdapter_OnPromptEnded_Group_DeletesDraftMessage verifies the
// turn-end cleanup contract under the two-stack model: the
// OnPromptEnded path first flushes any buffered events via
// flushLocked with reason="endProcess" (sendRichMessage on the
// FIRST flush — messageID was 0), then deletes the resulting
// Telegram message (the simulated DraftMessage's auto-disappear
// analogue). There is no persisted DraftMessageID — the
// DraftMessage is purely in-memory state.
func TestAdapter_OnPromptEnded_Group_DeletesDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "thought to be deleted",
	})

	// OnPromptEnded → endProcess → flushLocked (sendRichMessage
	// cold-create) → deleteMessage.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("endProcess must flush via sendRichMessage (first flush, messageID=0); got %d (calls=%+v)",
			len(sends), api.Calls)
	}
	deletes := callsByMethod(api.Calls, "deleteMessage")
	if len(deletes) != 1 {
		t.Fatalf("expected 1 deleteMessage on OnPromptEnded, got %d (calls=%+v)", len(deletes), api.Calls)
	}
	if msgID, _ := deletes[0].Params["message_id"].(int); msgID == 0 {
		t.Fatalf("deleteMessage with message_id = 0; want the cold-created id from endProcess flush")
	}
	if chat, _ := deletes[0].Params["chat_id"].(string); chat != raw {
		t.Fatalf("deleteMessage chat_id = %q, want %q", chat, raw)
	}
}

// TestAdapter_Send_Group_OutResult_EndsDraftProcess verifies that
// OutResult's send path also cleans up the DraftMessage — without
// waiting for OnPromptEnded. Mirrors DM draft's endProcess on
// OutResult.
func TestAdapter_Send_Group_OutResult_EndsDraftProcess(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "think then result",
	})
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutResult,
		Text:   "final answer",
	})

	if del := findCallByMethod(api.Calls, "deleteMessage"); del == nil {
		t.Fatalf("OutResult must trigger deleteMessage for DraftMessage; got calls=%+v", api.Calls)
	}
}

// (Removed: TestAdapter_Send_Group_ColdCreateFailure_FallsThroughToRichTurn.
// The cold-create fast path is gone under the uniform flush logic —
// every event composes into the buffer, and on flush failure
// streamDraftEvent returns handled=true (issue #391) so the rich turn
// path never sees tool/think leakage. There is no longer a "cold-
// create fails → fall through" branch to verify.)

// TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath
// verifies the contract: when a flush's editMessageText fails (e.g.
// Telegram 429), streamDraftEvent returns handled=true so the caller
// does NOT fall through to the rich turn. Without this, tool result
// lines (`⎿ 🔧 tool → N bytes`) would leak into the final answer
// message — see issue #391.
//
// Under the two-stack model, thinkingStack cap is 5: events 1..5
// buffer, event 6 trips cap → sendRichMessage (cold-create).
// Events 7..10 buffer (stack=4), event 11 trips cap → editMessageText
// success. Events 12..15 buffer (stack=4), event 16 trips cap →
// editMessageText fail (poisoned). Buffer preserved. Event 17 re-
// trips cap → editMessageText success (poison consumed).
// TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath
// verifies the contract from #391: when a flush's editMessageText
// fails (e.g. Telegram 429), the popped entries are restored to
// the buffer so the next flush retries on the same messageID
// without re-cold-creating. Without this, the next event would
// edit a stale id and the chain would lose events.
//
// The single-FIFO model only flushes via the 10s timer or
// endProcess, and endProcess drops the entry. To exercise three
// flushes (cold-create → edit-fail → edit-recover) on the same
// entry we drive flushLocked directly through the package-level
// surface — the lock discipline and the buffer-restoration
// contract are the production-relevant invariants, not the
// specific trigger.
func TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	ctx := context.Background()

	// Seed the entry via streamDraftEvent (5 events buffer; no
	// flush because there is no count trigger).
	for i := 1; i <= 5; i++ {
		if handled, _ := a.liveDraft.streamDraftEvent(ctx, raw, 0, 1,
			fmt.Sprintf("cold-create thought %d", i), messages.OutThinking); !handled {
			t.Fatalf("seed event %d not handled", i)
		}
	}

	// Phase 1: cold-create. Drive flushLocked directly so the
	// entry stays alive (endProcess would have dropped it).
	a.liveDraft.mu.Lock()
	entry, ok := a.liveDraft.entries[liveDraftKey(raw, 0, 1)]
	a.liveDraft.mu.Unlock()
	if !ok || entry == nil {
		t.Fatalf("group draft entry missing after seed")
	}
	if _, err := a.liveDraft.flushLocked(ctx, entry, raw, 0, "test-phase1"); err != nil {
		t.Fatalf("phase 1 flushLocked: %v", err)
	}
	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("phase 1: expected sendRichMessage cold-create; got calls=%+v", api.Calls)
	}
	originalMsgID := entry.messageID
	if originalMsgID == 0 {
		t.Fatalf("cold-create did not produce a message_id")
	}

	// Phase 2: add 5 more events; trigger a flush that will be
	// poisoned. editMessageText fails → buffer restored.
	for i := 6; i <= 10; i++ {
		if handled, _ := a.liveDraft.streamDraftEvent(ctx, raw, 0, 1,
			fmt.Sprintf("warmup thought %d", i), messages.OutThinking); !handled {
			t.Fatalf("warmup event %d not handled", i)
		}
	}
	preFailEdits := len(callsByMethod(api.Calls, "editMessageText"))
	preFailSends := len(callsByMethod(api.Calls, "sendRichMessage"))

	api.Errors = []error{errors.New("simulated editMessageText 429")}
	if _, err := a.liveDraft.flushLocked(ctx, entry, raw, 0, "test-phase2-fail"); err != nil {
		t.Fatalf("phase 2 flushLocked returned err: %v", err)
	}

	// The popped slice must be restored: thinkingStack back at 5
	// (we sent 5 thinking events in warmup, sent 5 in this
	// flush, flush failed, so the popped 5 should be back).
	entry.mu.Lock()
	bufferAfterFail := len(entry.thinkingStack)
	entry.mu.Unlock()
	if bufferAfterFail != 5 {
		t.Fatalf("thinkingStack should be restored to 5 after edit failure, got %d", bufferAfterFail)
	}
	// messageID preserved — the entry stays in m.entries so the
	// next flush will retry as editMessageText on the same id.
	if entry.messageID != originalMsgID {
		t.Fatalf("entry.messageID changed on edit failure: was %d, now %d", originalMsgID, entry.messageID)
	}
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != preFailEdits+1 {
		t.Fatalf("expected %d editMessageText (pre-fail + failing), got %d (calls=%+v)",
			preFailEdits+1, len(edits), api.Calls)
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != preFailSends {
		t.Fatalf("expected %d sendRichMessage (no re-cold-create), got %d", preFailSends, len(sends))
	}

	// Phase 3: add 5 more events (to push the latest-5 window past
	// the restored entries); trigger another flush. Poison is
	// consumed, editMessageText succeeds.
	for i := 11; i <= 15; i++ {
		if handled, _ := a.liveDraft.streamDraftEvent(ctx, raw, 0, 1,
			fmt.Sprintf("recovery thought %d", i), messages.OutThinking); !handled {
			t.Fatalf("recovery event %d not handled", i)
		}
	}
	if _, err := a.liveDraft.flushLocked(ctx, entry, raw, 0, "test-phase3-recover"); err != nil {
		t.Fatalf("phase 3 flushLocked: %v", err)
	}
	edits = callsByMethod(api.Calls, "editMessageText")
	if len(edits) != preFailEdits+2 {
		t.Fatalf("expected %d editMessageText after recovery, got %d", preFailEdits+2, len(edits))
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != preFailSends {
		t.Fatalf("recovery must NOT re-cold-create; got %d sendRichMessage", len(sends))
	}
	if msgID, _ := edits[preFailEdits+1].Params["message_id"].(int); msgID != originalMsgID {
		t.Fatalf("recovery editMessageText message_id = %d, want %d (cold-create id)",
			msgID, originalMsgID)
	}
}

// TestStateStore_ChatKindMigration verifies that an old state file
// with `chat_type` (and no `chat_kind`) is migrated on load to the
// new field with the expected mapping.
func TestStateStore_ChatKindMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Write a legacy-format state file directly.
	legacy := `{
  "topics": {
    "100|0":    {"chat_id":"100","topic_id":0,"chat_type":"private"},
    "-1001|0":  {"chat_id":"-1001","topic_id":0,"chat_type":"supergroup"},
    "-1002|0":  {"chat_id":"-1002","topic_id":0,"chat_type":"group"},
    "-1003|0":  {"chat_id":"-1003","topic_id":0,"chat_type":"channel"}
  },
  "choices": {}
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	store, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	for _, tt := range []struct {
		chatID  string
		topicID int
		want    string
	}{
		{"100", 0, ChatKindPrivate},
		{"-1001", 0, ChatKindGroup},
		{"-1002", 0, ChatKindGroup},
		{"-1003", 0, ChatKindChannel},
	} {
		state, ok := store.topic(tt.chatID, tt.topicID)
		if !ok {
			t.Fatalf("topic %s|%d missing after load", tt.chatID, tt.topicID)
		}
		if state.ChatKind != tt.want {
			t.Errorf("topic %s|%d: ChatKind = %q, want %q", tt.chatID, tt.topicID, state.ChatKind, tt.want)
		}
	}
}

// (Removed: orphan-recovery tests. They exercised the contract of
// persisting DraftMessageID to state so ensurePlaceholder could
// delete orphaned messages from prior turns. state.DraftMessageID
// is gone — orphans now stay in chat until the user notices, and
// the new behavior is documented as such in group_draft.go and the
// docs/channel/telegram.md §11.12.11.2 rewrite.)

// TestAdapter_Send_Group_ConcurrentStreamDraftEvent_NoDoubleColdCreate
// fires N goroutines that all call Send with the same (chat, topic,
// userMsgID) simultaneously. With the single-FIFO model there is
// no count-based flush — the per-entry mu serialises the appends
// and the only flush trigger is OnPromptEnded, which runs to
// completion (cold-create + delete) on the same goroutine. The
// invariant under test: at most one sendRichMessage per turn and
// at most one deleteMessage per turn, regardless of how many
// concurrent streamDraftEvent calls landed.
func TestAdapter_Send_Group_ConcurrentStreamDraftEvent_NoDoubleColdCreate(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = a.Send(context.Background(), messages.OutboundMessage{
				ChatID: "tg_" + raw,
				Kind:   messages.OutThinking,
				Text:   "concurrent thought",
			})
		}()
	}
	wg.Wait()

	// Force the flush. OnPromptEnded → endProcess → flushLocked
	// (sendRichMessage cold-create, latest 5 of 10) → deleteMessage.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendRichMessage cold-create across %d concurrent events, got %d (calls=%+v)",
			N, len(sends), api.Calls)
	}
	if got := len(richMessageBlocks(sends[0].Params["rich_message"])); got != 5 {
		t.Fatalf("cold-create blocks = %d, want 5 (latest 5 of 10)", got)
	}
	deletes := callsByMethod(api.Calls, "deleteMessage")
	if len(deletes) != 1 {
		t.Fatalf("expected exactly 1 deleteMessage, got %d (calls=%+v)", len(deletes), api.Calls)
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("expected 0 editMessageText (single flush per turn); got %d", len(edits))
	}
}

// TestAdapter_Send_Group_OutThinking_UsesMsgReplyTo verifies that
// Send() derives replyAnchor from msg.ReplyTo, not from
// state.UserMessageID. Setup pins state.UserMessageID="99" but the
// event carries ReplyTo="42"; the cold-create flush must anchor to 42.
func TestAdapter_Send_Group_OutThinking_UsesMsgReplyTo(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	// Override state.UserMessageID to a different value to detect
	// any code path that still reads state.
	if err := a.state.putTopic(&TopicState{
		ChatID:        raw,
		TopicID:       0,
		ChatKind:      ChatKindGroup,
		UserMessageID: "99",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}

	// 5 events buffer silently under the FIFO model. OnPromptEnded
	// is the flush trigger. The userMsgID here is the SAME id the
	// events were anchored to (ReplyTo="42") — OnPromptEnded keys
	// liveDraft.endProcess on the caller-supplied id, not on
	// state.UserMessageID.
	for i := 1; i <= 5; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID:  "tg_" + raw,
			Kind:    messages.OutThinking,
			Text:    "anchored to msg.ReplyTo",
			ReplyTo: "42",
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	a.OnPromptEnded(context.Background(), "tg_"+raw, "42", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendRichMessage cold-create, got %d", len(sends))
	}
	if got, _ := sends[0].Params["reply_to_message_id"].(int); got != 42 {
		t.Fatalf("cold-create reply_to_message_id = %v, want 42 (msg.ReplyTo)", sends[0].Params["reply_to_message_id"])
	}
}

// TestAdapter_Send_Group_FallsBackToStateUserMessageID_WhenReplyToEmpty
// verifies the orphan-fallback contract: events without msg.ReplyTo
// (shell /gtw, startup EventAgentReady, test paths) attach to
// state.UserMessageID — same as Feishu's orphan path.
func TestAdapter_Send_Group_FallsBackToStateUserMessageID_WhenReplyToEmpty(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	if err := a.state.putTopic(&TopicState{
		ChatID:        raw,
		TopicID:       0,
		ChatKind:      ChatKindGroup,
		UserMessageID: "77",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}

	// 5 events buffer silently under the FIFO model. OnPromptEnded
	// forces the cold-create. The userMsgID here matches the
	// state.UserMessageID fallback (77) that the events used since
	// ReplyTo is empty.
	for i := 1; i <= 5; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   "orphan thought — no ReplyTo",
			// ReplyTo intentionally empty
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	a.OnPromptEnded(context.Background(), "tg_"+raw, "77", agent.PromptEndClean)

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendRichMessage cold-create, got %d", len(sends))
	}
	if got, _ := sends[0].Params["reply_to_message_id"].(int); got != 77 {
		t.Fatalf("orphan fallback reply_to_message_id = %v, want 77 (state.UserMessageID)", sends[0].Params["reply_to_message_id"])
	}
}

// TestAdapter_Send_Group_BackToBackPrompts_DraftMessagesIsolated
// verifies that two consecutive turns produce two independent
// DraftMessages anchored to their respective userMsgIDs, and that a
// late event from turn 1 (delivered after turn 2's ensurePlaceholder
// has overwritten state.UserMessageID) does NOT leak into turn 2's
// DraftMessage. The bug would manifest as turn 2's sendRichMessage
// carrying turn 1's body, or turn 1's late OutToolEnd rewriting
// turn 2's message_id.
//
// Under the single-FIFO model, each turn's OnPromptEnded is the
// flush trigger. 5 OutThinkings per turn fills the latest-5
// window exactly; any remaining events buffer. A late single
// event from turn 1 lands in turn 1's buffer (key=userMsgID=10),
// and the next turn 2 flush is anchored to userMsgID=11 (the
// state anchor AFTER turn 2's ensurePlaceholder).
func TestAdapter_Send_Group_BackToBackPrompts_DraftMessagesIsolated(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: 5 OutThinkings buffer silently; OnPromptEnded
	// forces the cold-create anchored to ReplyTo=10. The userMsgID
	// in OnPromptEnded matches the event anchor (ReplyTo="10").
	for i := 1; i <= 5; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID:  "tg_" + raw,
			Kind:    messages.OutThinking,
			Text:    "turn 1 thought",
			ReplyTo: "10",
		}); err != nil {
			t.Fatalf("turn 1 think %d: %v", i, err)
		}
	}
	a.OnPromptEnded(context.Background(), "tg_"+raw, "10", agent.PromptEndClean)
	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 1 cold-create expected 1 sendRichMessage, got %d (calls=%+v)", len(sends), api.Calls)
	}
	turn1ReplyTo, _ := sends[0].Params["reply_to_message_id"].(int)
	if turn1ReplyTo != 10 {
		t.Fatalf("turn 1 reply_to_message_id = %v, want 10", sends[0].Params["reply_to_message_id"])
	}
	turn1Blocks := richMessageBlocks(sends[0].Params["rich_message"])
	if len(turn1Blocks) != 5 {
		t.Fatalf("turn 1 blocks = %d, want 5 (latest 5); got %v", len(turn1Blocks), turn1Blocks)
	}
	if turn1Blocks[0] != "💭 turn 1 thought" {
		t.Fatalf("turn 1 block[0] = %q, want %q", turn1Blocks[0], "💭 turn 1 thought")
	}

	// Simulate turn 2's user message arriving: ensurePlaceholder
	// runs and overwrites state.UserMessageID to 11. It also
	// cold-creates turn 2's rich turn placeholder (a separate
	// sendRichMessage — different surface from the group draft).
	if err := a.ensurePlaceholder(context.Background(), raw, 0, 11, &Message{
		MessageID: 11,
		Chat:      Chat{ID: -1001, Type: "supergroup"},
	}); err != nil {
		t.Fatalf("ensurePlaceholder turn 2: %v", err)
	}

	// Turn 1 late OutToolEnd arrives with ReplyTo=10 (NOT state=11).
	// It buffers into a fresh turn-1 entry — the prior one was
	// dropped on the previous endProcess. No new flush trigger.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutToolEnd,
		Tool:    &messages.ToolInfo{Name: "Bash", Output: "ok"},
		ReplyTo: "10",
	}); err != nil {
		t.Fatalf("turn 1 late OutToolEnd: %v", err)
	}

	// Per-turn isolation: the late event goes to a fresh turn-1
	// entry (key=userMsgID=10). It does NOT cold-create a new
	// DraftMessage for turn 2 and does NOT trigger any new flush.
	//
	// Two sendRichMessage calls are expected by this point: (1)
	// turn 1's DraftMessage cold-create and (2) turn 2's eager rich
	// turn placeholder fired by ensurePlaceholder. The late event
	// must NOT add a third.
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 2 {
		t.Fatalf("turn 1 late event must NOT cold-create a new DraftMessage; sends=%d (want 2)", len(sends))
	}

	// Now turn 2's 5 OutThinkings arrive and OnPromptEnded
	// flushes them — must cold-create a fresh DraftMessage
	// anchored to 11, not 10.
	api.Calls = nil
	for i := 1; i <= 5; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID:  "tg_" + raw,
			Kind:    messages.OutThinking,
			Text:    "turn 2 thought",
			ReplyTo: "11",
		}); err != nil {
			t.Fatalf("turn 2 think %d: %v", i, err)
		}
	}
	a.OnPromptEnded(context.Background(), "tg_"+raw, "11", agent.PromptEndClean)
	sends2 := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends2) != 1 {
		t.Fatalf("turn 2 cold-create expected 1 sendRichMessage, got %d (calls=%+v)", len(sends2), api.Calls)
	}
	turn2ReplyTo, _ := sends2[0].Params["reply_to_message_id"].(int)
	if turn2ReplyTo != 11 {
		t.Fatalf("turn 2 reply_to_message_id = %v, want 11", sends2[0].Params["reply_to_message_id"])
	}
	turn2Blocks := richMessageBlocks(sends2[0].Params["rich_message"])
	if len(turn2Blocks) != 5 {
		t.Fatalf("turn 2 blocks = %d, want 5 (latest 5); got %v", len(turn2Blocks), turn2Blocks)
	}
	if turn2Blocks[0] != "💭 turn 2 thought" {
		t.Fatalf("turn 2 block[0] = %q, want %q", turn2Blocks[0], "💭 turn 2 thought")
	}
}

// TestAdapter_Send_Group_OutResult_EndProcessUsesMsgReplyTo verifies
// that Send(OutResult) calls liveDraft.endProcess with the
// msg.ReplyTo-derived userMsgID, not state.UserMessageID.
func TestAdapter_Send_Group_OutResult_EndProcessUsesMsgReplyTo(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	// Pin state.UserMessageID to a sentinel — the OutResult must
	// still end the per-ReplyTo turn.
	if err := a.state.putTopic(&TopicState{
		ChatID:        raw,
		TopicID:       0,
		ChatKind:      ChatKindGroup,
		UserMessageID: "999",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}

	// Turn with ReplyTo=42.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutThinking,
		Text:    "turn anchored to 42",
		ReplyTo: "42",
	}); err != nil {
		t.Fatalf("send think: %v", err)
	}
	api.Calls = nil

	// OutResult ends the process — must delete the DraftMessage
	// created for ReplyTo=42.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutResult,
		Text:    "final answer",
		ReplyTo: "42",
	}); err != nil {
		t.Fatalf("send result: %v", err)
	}
	if del := callsByMethod(api.Calls, "deleteMessage"); len(del) == 0 {
		t.Fatalf("OutResult must trigger deleteMessage for the per-turn DraftMessage; got calls=%+v", api.Calls)
	}
}

// TestAdapter_OnPromptEnded_Group_EndProcessSafetyNet verifies that
// OnPromptEnded cleans up the DraftMessage even when no OutResult
// was sent (error path, bridge crash, abort). Without the safety
// net, the DraftMessage orphans in chat.
func TestAdapter_OnPromptEnded_Group_EndProcessSafetyNet(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn creates a DraftMessage via think, no OutResult follows.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutThinking,
		Text:    "thinking that never finishes",
		ReplyTo: "55",
	}); err != nil {
		t.Fatalf("send think: %v", err)
	}
	api.Calls = nil

	// Prompt ends without OutResult.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "55", agent.PromptEndClean)

	if del := callsByMethod(api.Calls, "deleteMessage"); len(del) == 0 {
		t.Fatalf("OnPromptEnded safety net must trigger deleteMessage; got calls=%+v", api.Calls)
	}
}

// TestLiveDraft_EndProcess_DropsEntryFromMap locks the
// §11.12.11.3 invariant: endProcess MUST remove the per-turn
// liveDraftEntry from m.entries (not just call deleteMessage), so
// a late streamDraftEvent from the just-ended turn cannot hit a
// 400 "message not found" by editing an already-deleted Telegram
// message. After endProcess returns, m.entries must not contain
// the turn's key, and the next batch of events for the same
// userMsgID must cold-create a fresh DraftMessage (not
// editMessageText the now-deleted id).
func TestLiveDraft_EndProcess_DropsEntryFromMap(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	ctx := context.Background()

	// Turn 1: 5 streamDraftEvent calls buffer silently into the
	// FIFO (no count-based flush in this model).
	for i := 1; i <= 5; i++ {
		if handled, _ := a.liveDraft.streamDraftEvent(ctx, raw, 0, 10, "first", messages.OutThinking); !handled {
			t.Fatalf("call %d should be handled; got handled=false", i)
		}
	}

	// Confirm the entry is in m.entries and the buffer holds all 5.
	a.liveDraft.mu.Lock()
	_, present := a.liveDraft.entries[liveDraftKey(raw, 0, 10)]
	a.liveDraft.mu.Unlock()
	if !present {
		t.Fatalf("entry must be in m.entries after stream")
	}

	// Turn 1 ends. endProcess must (a) flush via flushLocked
	// (sendRichMessage cold-create, latest 5) and (b) call
	// deleteMessage and (c) drop the entry from m.entries.
	api.Calls = nil
	a.liveDraft.endProcess(ctx, raw, 0, 10)
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("endProcess: expected 1 sendRichMessage cold-create, got %d (calls=%+v)", len(sends), api.Calls)
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) == 0 {
		t.Fatalf("endProcess must trigger deleteMessage; got calls=%+v", api.Calls)
	}
	a.liveDraft.mu.Lock()
	_, present = a.liveDraft.entries[liveDraftKey(raw, 0, 10)]
	a.liveDraft.mu.Unlock()
	if present {
		t.Fatalf("endProcess MUST remove the entry from m.entries; otherwise a late event for this turn hits editMessageText against the deleted message_id")
	}

	// 5 late events for the same turn arrive. The prior entry was
	// dropped, so a fresh entry is created. No flush trigger from
	// event count — the next flush is an explicit endProcess.
	api.Calls = nil
	for i := 1; i <= 5; i++ {
		if handled, _ := a.liveDraft.streamDraftEvent(ctx, raw, 0, 10, "late event", messages.OutThinking); !handled {
			t.Fatalf("late event %d must be handled (fresh entry); got handled=false", i)
		}
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 0 {
		t.Fatalf("late events should NOT trigger any flush (no count-based trigger); got %d sendRichMessage", len(sends))
	}
	// endProcess on the fresh entry must cold-create (not edit
	// the now-deleted id).
	a.liveDraft.endProcess(ctx, raw, 0, 10)
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("late events + endProcess must cold-create; got %d sendRichMessage", len(sends))
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("late events after endProcess must NOT editMessageText (entry was dropped); got %d edits", len(edits))
	}
}

// TestAdapter_Send_Group_PatchChainHeaderUsesMsgReplyTo is the
// OutHeartbeat-side equivalent of TestAdapter_Send_Group_OutThinking_UsesMsgReplyTo:
// patchChainHeader resolves its turn anchor from msg.ReplyTo
// first, state.UserMessageID fallback second. Without this, a
// back-to-back turn can misroute a heartbeat edit onto the next
// turn's rich message (review finding #2).
func TestAdapter_Send_Group_PatchChainHeaderUsesMsgReplyTo(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	// Pin state to a different userMsgID to detect any path that
	// still reads it as primary source.
	if err := a.state.putTopic(&TopicState{
		ChatID:        raw,
		TopicID:       0,
		ChatKind:      ChatKindGroup,
		UserMessageID: "999",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}
	// First cold-create a rich turn for the ReplyTo=42 turn so
	// patchChainHeader has something to PATCH.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutReply,
		Text:    "seed reply",
		ReplyTo: "42",
	}); err != nil {
		t.Fatalf("seed reply: %v", err)
	}
	api.Calls = nil

	// Now send an OutHeartbeat anchored to ReplyTo=42. patchChainHeader
	// must route the PATCH to turn 42's rich turn, NOT turn 999's.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutHeartbeat,
		ReplyTo: "42",
		Heartbeat: &messages.HeartbeatSnapshot{
			ThinkCount: 1,
			ToolCount:  0,
			LastBeatAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	// OutHeartbeat's effective behavior in this adapter is a no-op
	// on the rich turn (see Send case OutHeartbeat comment). What
	// we care about is that patchChainHeader was called with
	// userMessageID=42, not 999. The seed reply produced a
	// sendRichMessage call; if patchChainHeader misrouted to 999
	// it would have created a fresh rich turn for 999. Verify
	// only one sendRichMessage call (the seed, for turn 42) landed.
	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 0 {
		// Heartbeat without pre-existing rich body: no new sendRichMessage
		// is the expected outcome (Compose header-skip rule + the
		// heartbeat path itself is a no-op in this adapter per the
		// OutHeartbeat case comment). If a stray sendRichMessage
		// appears, it means patchChainHeader created a rich turn
		// for the wrong userMsgID (999).
		t.Fatalf("OutHeartbeat must NOT create a new sendRichMessage; got %d (calls=%+v)", len(sends), api.Calls)
	}
}
