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

// TestAdapter_Send_Group_OutThinking_CreatesDraftMessage verifies the
// two-stack flush logic: thinkingStack holds OutThinking events up to
// cap (5). Events 1-5 buffer silently (no API call). The 6th event
// trips the capacity threshold: streamDraftEvent flushes the buffered
// 5 events FIRST via sendRichMessage (cold-create), then appends the
// 6th event into the now-empty stack. The cold-create carries
// reply_to_message_id so the DraftMessage hangs under the user's
// message.
func TestAdapter_Send_Group_OutThinking_CreatesDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Events 1..5 fill thinkingStack to cap (5). No flush yet.
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
		t.Fatalf("first 5 events must NOT trigger sendRichMessage (batched); got %d (calls=%+v)",
			len(sends), api.Calls)
	}

	// 6th event: trips thinkingStack cap, flushes events 1..5 via
	// sendRichMessage (cold-create), then buffers event 6.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "considering whether to invoke Read",
	}); err != nil {
		t.Fatalf("send 6: %v", err)
	}
	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("expected sendRichMessage cold-create on capacity flush; got calls=%+v", api.Calls)
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
}

// TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace
// verifies the cold-create → edit transition with the two-stack
// model. thinkingStack cap is 5: events 1..5 buffer, event 6 trips
// the cap → sendRichMessage (cold-create) with 5 blocks. Events
// 7..11 buffer again, event 12 trips the cap → editMessageText
// with 5 more blocks.
func TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// 12 OutThinking events: events 1..5 buffer, event 6 trips cap
	// → sendRichMessage (cold-create). Events 7..11 buffer, event
	// 12 trips cap → editMessageText.
	for i := 1; i <= 12; i++ {
		_ = a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("thought %d", i),
		})
	}

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendRichMessage cold-create, got %d (calls=%+v)", len(sends), api.Calls)
	}
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected exactly 1 editMessageText on second capacity flush, got %d", len(edits))
	}
	coldBlocks := richMessageBlocks(sends[0].Params["rich_message"])
	if len(coldBlocks) != 5 {
		t.Fatalf("cold-create blocks = %d, want 5 (events 1..5); got %v", len(coldBlocks), coldBlocks)
	}
	if coldBlocks[0] != "💭 thought 1" {
		t.Fatalf("cold-create block[0] = %q, want %q", coldBlocks[0], "💭 thought 1")
	}
	if coldBlocks[4] != "💭 thought 5" {
		t.Fatalf("cold-create block[4] = %q, want %q", coldBlocks[4], "💭 thought 5")
	}
	editBlocks := richMessageBlocks(edits[0].Params["rich_message"])
	if len(editBlocks) != 5 {
		t.Fatalf("edit blocks = %d, want 5 (events 6..10); got %v", len(editBlocks), editBlocks)
	}
	if editBlocks[0] != "💭 thought 6" {
		t.Fatalf("edit block[0] = %q, want %q", editBlocks[0], "💭 thought 6")
	}
	if editBlocks[4] != "💭 thought 10" {
		t.Fatalf("edit block[4] = %q, want %q", editBlocks[4], "💭 thought 10")
	}
}

// TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart verifies
// the tool-slot ACCUMULATE path: OutToolStart opens a slot, the
// matching OutToolEnd appends its result line under that slot's
// call line. flushLocked renders the slot as [Start, End1, End2,
// ...] paragraph blocks.
//
// Under the two-stack model, toolsStack cap is 5 slots. The 6th
// OutToolStart trips the cap → sendRichMessage (cold-create) with
// 5 blocks (the previous slots). After the cold-create, slot 6
// sits in toolsStack; we then send Write Start + Write End, which
// append a second slot. OnPromptEnded → endProcess flushes via
// editMessageText with 3 blocks: [slot6/Start, Write/Start,
// Write/End].
func TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// 6 OutToolStart events: 5 fill toolsStack, the 6th trips the
	// cap → sendRichMessage cold-create with 5 tool-call blocks.
	for i := 1; i <= 6; i++ {
		_ = a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutToolStart,
			Tool:   &messages.ToolInfo{Name: "Read", Args: fmt.Sprintf("/tmp/file%d.go", i)},
			Text:   fmt.Sprintf("● Read(/tmp/file%d.go)", i),
		})
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("filler phase: expected 1 cold-create sendRichMessage, got %d", len(sends))
	}
	if got := len(richMessageBlocks(findCallByMethod(api.Calls, "sendRichMessage").Params["rich_message"])); got != 5 {
		t.Fatalf("cold-create blocks = %d, want 5", got)
	}

	api.Calls = nil

	// Pair: OutToolStart opens slot 7, OutToolEnd appends End to
	// the same slot. toolsStack now has [slot6/Start, slot7/[Start,
	// End]].
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
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText (ACCUMULATE endProcess flush), got %d (calls=%+v)", len(edits), api.Calls)
	}
	blocks := richMessageBlocks(edits[0].Params["rich_message"])
	if len(blocks) != 3 {
		t.Fatalf("ACCUMULATE rich_message blocks = %d, want 3 (slot6/Start + Write/Start + Write/End); got %v", len(blocks), blocks)
	}
	if blocks[0] != "● Read(/tmp/file6.go)" {
		t.Fatalf("ACCUMULATE block[0] = %q, want %q", blocks[0], "● Read(/tmp/file6.go)")
	}
	if blocks[1] != "● Write(/tmp/bar.go)" {
		t.Fatalf("ACCUMULATE block[1] = %q, want %q", blocks[1], "● Write(/tmp/bar.go)")
	}
	if blocks[2] != "⎿  📝 Write → 11 bytes" {
		t.Fatalf("ACCUMULATE block[2] = %q, want %q", blocks[2], "⎿  📝 Write → 11 bytes")
	}
}

// TestAdapter_Send_Group_NextTurn_CreatesFreshDraftMessage verifies
// that endProcess drops the prior turn's entry from m.entries so
// turn N+1 can cold-create a fresh DraftMessage rather than
// editMessageText the now-deleted id. Under the two-stack model,
// the cap trigger fires when either stack reaches 5: turn 1's
// 10 OutThinking events produce a cap flush at event 6 (cold-
// create) and the remaining 4 events stay buffered until
// OnPromptEnded → endProcess flushes them via editMessageText
// before deleting the message.
func TestAdapter_Send_Group_NextTurn_CreatesFreshDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: 10 OutThinking events. Event 6 trips thinkingStack
	// cap → cold-create sendRichMessage with events 1..5. Events
	// 7..10 buffer. OnPromptEnded → endProcess flushes the 4
	// buffered events via editMessageText, then deleteMessage.
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
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 1 {
		t.Fatalf("turn 1: expected 1 editMessageText (endProcess flush of remaining buffer), got %d", len(edits))
	}
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) != 1 {
		t.Fatalf("turn 1 OnPromptEnded should delete the cold-created DraftMessage; got %d deletes", len(dels))
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

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 2 cold-create should produce 1 sendRichMessage; got %d (calls=%+v)", len(sends), api.Calls)
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("turn 2 first flush must NOT edit prior DraftMessage (entry was dropped); got %d edits", len(edits))
	}
}

// TestAdapter_Send_Group_DraftMessage_RoutedThroughTopic verifies
// that when the user is inside a forum topic (topicID > 0), the
// first flush carries message_thread_id so the DraftMessage lives
// in the same topic as the conversation. With the two-stack model,
// the cap trigger fires at event 6 (5 fill thinkingStack, 6th
// triggers capacity flush).
func TestAdapter_Send_Group_DraftMessage_RoutedThroughTopic(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 42)

	// 6 events: 5 fill thinkingStack, 6th trips cap → cold-create.
	for i := 1; i <= 6; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw + ":42",
			Kind:   messages.OutThinking,
			Text:   fmt.Sprintf("topic-scoped thought %d", i),
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("expected sendRichMessage cold-create on capacity flush; got calls=%+v", api.Calls)
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
func TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	send := func(text string) {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   text,
		}); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
	}

	// Events 1..5 buffer, event 6 trips cap → sendRichMessage
	// cold-create succeeds.
	for i := 1; i <= 6; i++ {
		send(fmt.Sprintf("cold-create thought %d", i))
	}

	entry, ok := a.groupDraft.entries[groupDraftKey(raw, 0, 1)]
	if !ok || entry == nil {
		t.Fatalf("group draft entry missing after cold-create")
	}
	originalMsgID := entry.messageID
	if originalMsgID == 0 {
		t.Fatalf("cold-create did not produce a message_id")
	}

	// Events 7..10 buffer (stack=4), event 11 trips cap → flush
	// succeeds via editMessageText. This warms up the
	// editMessageText path before poisoning.
	for i := 7; i <= 11; i++ {
		send(fmt.Sprintf("warmup thought %d", i))
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 1 {
		t.Fatalf("warmup: expected 1 editMessageText, got %d", len(edits))
	}

	// Events 12..15 buffer (stack=4). Setting the poison now means
	// the NEXT cap flush (event 16) will be the failing one.
	for i := 12; i <= 15; i++ {
		send(fmt.Sprintf("pre-fail thought %d", i))
	}

	// Snapshot pre-failure state.
	preFailEdits := len(callsByMethod(api.Calls, "editMessageText"))
	preFailSends := len(callsByMethod(api.Calls, "sendRichMessage"))

	// Poison api.Errors so the next call (the upcoming cap flush
	// editMessageText at event 16) fails with simulated Telegram 429.
	api.Errors = []error{errors.New("simulated editMessageText 429")}

	// Event 16 trips cap → editMessageText fails. Buffer preserved.
	send("failing flush thought 16")

	// The messageID must be preserved — the entry is still alive
	// and ready for the next event to retry on top.
	entry, _ = a.groupDraft.entries[groupDraftKey(raw, 0, 1)]
	if entry == nil || entry.messageID != originalMsgID {
		got := -1
		if entry != nil {
			got = entry.messageID
		}
		t.Fatalf("entry.messageID changed on edit failure: was %d, now %d", originalMsgID, got)
	}

	// No rich turn entry should exist for this turn — if it did, the
	// tool/think line leaked into the rich turn (the #391 bug).
	if turn, ok := a.richTurns.lookup(raw, 0, 1); ok && turn != nil {
		t.Fatalf("rich turn was created for failed edit — fallback leaked; entries=%d", len(turn.entries))
	}

	// The failing editMessageText added exactly one edit call to
	// the pre-failure count; no re-cold-create.
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != preFailEdits+1 {
		t.Fatalf("expected %d editMessageText (pre-fail + failing), got %d (calls=%+v)",
			preFailEdits+1, len(edits), api.Calls)
	}
	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != preFailSends {
		t.Fatalf("expected %d sendRichMessage (no re-cold-create), got %d", preFailSends, len(sends))
	}

	// Recovery: send event 17 → cap still hit (stack=5 from
	// preserved buffer) → flush again → editMessageText succeeds
	// (poison consumed). The recovery editMessageText carries the
	// same messageID; no re-cold-create.
	send("recovery flush thought 17")

	edits = callsByMethod(api.Calls, "editMessageText")
	if len(edits) != preFailEdits+2 {
		t.Fatalf("expected %d editMessageText after recovery, got %d", preFailEdits+2, len(edits))
	}
	sends = callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != preFailSends {
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
// userMsgID) simultaneously. The per-entry lock must serialise them
// so exactly one sendRichMessage cold-create fires and only the
// second capacity flush produces a single editMessageText on the
// same message_id. Without the lock, two goroutines would both see
// entry.messageID == 0 and both sendRichMessage → two messages in
// chat.
func TestAdapter_Send_Group_ConcurrentStreamDraftEvent_NoDoubleColdCreate(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// 12 concurrent events. With thinkingStack cap=5:
	//   event 6 trips cap → sendRichMessage (cold-create) flushes
	//     5 events, then buffers the 6th.
	//   events 7..11 buffer (stack grows to 5).
	//   event 12 trips cap → editMessageText flushes 5 events, then
	//     buffers the 12th.
	// The lock-held streamDraftEvent serialises all 12 calls, so the
	// API sees exactly 1 cold-create + 1 editMessageText.
	const N = 12
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

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendRichMessage cold-create across %d concurrent events, got %d (calls=%+v)",
			N, len(sends), api.Calls)
	}
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText (second capacity flush), got %d", len(edits))
	}
	// Both flushes carried 5 blocks each.
	if got := len(richMessageBlocks(sends[0].Params["rich_message"])); got != 5 {
		t.Fatalf("cold-create blocks = %d, want 5", got)
	}
	if got := len(richMessageBlocks(edits[0].Params["rich_message"])); got != 5 {
		t.Fatalf("edit blocks = %d, want 5", got)
	}
	// The edit targets the cold-create's message_id (no re-cold-create).
	// The cold-create's id lives in the entry, not in the call Params.
	a.groupDraft.mu.Lock()
	coldMsgID := a.groupDraft.entries[groupDraftKey(raw, 0, 1)].messageID
	a.groupDraft.mu.Unlock()
	if msgID, _ := edits[0].Params["message_id"].(int); msgID != coldMsgID || coldMsgID == 0 {
		t.Fatalf("edit message_id = %d, cold-create message_id = %d (want equal & non-zero)",
			msgID, coldMsgID)
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

	// 6 events: events 1..5 buffer, event 6 trips thinkingStack cap
	// → sendRichMessage (cold-create) anchored to ReplyTo=42.
	for i := 1; i <= 6; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID:  "tg_" + raw,
			Kind:    messages.OutThinking,
			Text:    "anchored to msg.ReplyTo",
			ReplyTo: "42",
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

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

	// 6 events: events 1..5 buffer, event 6 trips thinkingStack cap
	// → sendRichMessage (cold-create) anchored to state.UserMessageID=77.
	for i := 1; i <= 6; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   "orphan thought — no ReplyTo",
			// ReplyTo intentionally empty
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

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
// Under the two-stack model, each turn's first cap flush fires at
// event 6 (5 fill thinkingStack, 6th trips cap → sendRichMessage).
// 6 OutThinkings per turn is enough to drive cold-creates; any
// remaining events buffer. A late single event from turn 1 lands
// in turn 1's buffer (key=userMsgID=10), and the next turn 2 flush
// is anchored to userMsgID=11 (the state anchor AFTER turn 2's
// ensurePlaceholder).
func TestAdapter_Send_Group_BackToBackPrompts_DraftMessagesIsolated(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: 6 OutThinkings — events 1..5 buffer, event 6 trips
	// thinkingStack cap → cold-create anchored to ReplyTo=10.
	for i := 1; i <= 6; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID:  "tg_" + raw,
			Kind:    messages.OutThinking,
			Text:    "turn 1 thought",
			ReplyTo: "10",
		}); err != nil {
			t.Fatalf("turn 1 think %d: %v", i, err)
		}
	}
	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 1 cold-create expected 1 sendRichMessage, got %d", len(sends))
	}
	turn1ReplyTo, _ := sends[0].Params["reply_to_message_id"].(int)
	if turn1ReplyTo != 10 {
		t.Fatalf("turn 1 reply_to_message_id = %v, want 10", sends[0].Params["reply_to_message_id"])
	}
	turn1Blocks := richMessageBlocks(sends[0].Params["rich_message"])
	if len(turn1Blocks) != 5 {
		t.Fatalf("turn 1 blocks = %d, want 5 (cap flush); got %v", len(turn1Blocks), turn1Blocks)
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
	// It buffers into turn 1's entry — does NOT trigger another flush
	// (cap was reset on the prior cold-create; OutToolEnd never
	// triggers a flush by itself).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutToolEnd,
		Tool:    &messages.ToolInfo{Name: "Bash", Output: "ok"},
		ReplyTo: "10",
	}); err != nil {
		t.Fatalf("turn 1 late OutToolEnd: %v", err)
	}

	// Per-turn isolation: the late event goes to turn 1's entry
	// (ReplyTo=10 → key=userMsgID=10). It does NOT cold-create a new
	// DraftMessage for turn 2 and does NOT trigger any new flush.
	//
	// Two sendRichMessage calls are expected by this point: (1)
	// turn 1's DraftMessage cold-create and (2) turn 2's eager rich
	// turn placeholder fired by ensurePlaceholder. The late event
	// must NOT add a third.
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 2 {
		t.Fatalf("turn 1 late event must NOT cold-create a new DraftMessage; sends=%d (want 2)", len(sends))
	}

	// Now turn 2's 6 OutThinkings arrive — they must cold-create a
	// fresh DraftMessage anchored to 11 (the state.UserMessageID
	// AFTER ensurePlaceholder for turn 2), not 10.
	api.Calls = nil
	for i := 1; i <= 6; i++ {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID:  "tg_" + raw,
			Kind:    messages.OutThinking,
			Text:    "turn 2 thought",
			ReplyTo: "11",
		}); err != nil {
			t.Fatalf("turn 2 think %d: %v", i, err)
		}
	}
	sends2 := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends2) != 1 {
		t.Fatalf("turn 2 cold-create expected 1 sendRichMessage, got %d", len(sends2))
	}
	turn2ReplyTo, _ := sends2[0].Params["reply_to_message_id"].(int)
	if turn2ReplyTo != 11 {
		t.Fatalf("turn 2 reply_to_message_id = %v, want 11", sends2[0].Params["reply_to_message_id"])
	}
	turn2Blocks := richMessageBlocks(sends2[0].Params["rich_message"])
	if len(turn2Blocks) != 5 {
		t.Fatalf("turn 2 blocks = %d, want 5 (cap flush); got %v", len(turn2Blocks), turn2Blocks)
	}
	if turn2Blocks[0] != "💭 turn 2 thought" {
		t.Fatalf("turn 2 block[0] = %q, want %q", turn2Blocks[0], "💭 turn 2 thought")
	}
}

// TestAdapter_Send_Group_OutResult_EndProcessUsesMsgReplyTo verifies
// that Send(OutResult) calls groupDraft.endProcess with the
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

// TestGroupDraft_EndProcess_DropsEntryFromMap locks the
// §11.12.11.3 invariant: endProcess MUST remove the per-turn
// groupDraftEntry from m.entries (not just call deleteMessage), so
// a late streamDraftEvent from the just-ended turn cannot hit a
// 400 "message not found" by editing an already-deleted Telegram
// message. After endProcess returns, m.entries must not contain
// the turn's key, and the next batch of events for the same
// userMsgID must cold-create a fresh DraftMessage (not
// editMessageText the now-deleted id).
func TestGroupDraft_EndProcess_DropsEntryFromMap(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: 6 streamDraftEvent calls. Events 1..5 fill
	// thinkingStack; event 6 trips cap → cold-create via
	// sendRichMessage.
	for i := 1; i <= 6; i++ {
		if handled, _ := a.groupDraft.streamDraftEvent(context.Background(), raw, 0, 10, "first", messages.OutThinking); !handled {
			t.Fatalf("call %d should be handled; got handled=false", i)
		}
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("expected 1 cold-create after 6 events, got %d", len(sends))
	}

	// Confirm the entry is in m.entries.
	a.groupDraft.mu.Lock()
	_, present := a.groupDraft.entries[groupDraftKey(raw, 0, 10)]
	a.groupDraft.mu.Unlock()
	if !present {
		t.Fatalf("entry must be in m.entries after cold-create")
	}

	// Turn 1 ends. endProcess must (a) call deleteMessage and
	// (b) drop the entry from m.entries. Buffer is empty after the
	// cap flush, so endProcess skips the inner flushLocked call.
	api.Calls = nil
	a.groupDraft.endProcess(context.Background(), raw, 0, 10)
	if dels := callsByMethod(api.Calls, "deleteMessage"); len(dels) == 0 {
		t.Fatalf("endProcess must trigger deleteMessage; got calls=%+v", api.Calls)
	}
	a.groupDraft.mu.Lock()
	_, present = a.groupDraft.entries[groupDraftKey(raw, 0, 10)]
	a.groupDraft.mu.Unlock()
	if present {
		t.Fatalf("endProcess MUST remove the entry from m.entries; otherwise a late event for this turn hits editMessageText against the deleted message_id")
	}

	// 6 late events for the same turn arrive — must cold-create a
	// fresh DraftMessage (reply_to_message_id=10 still anchors the
	// chain under the user's original message), NOT editMessageText
	// the now-deleted id.
	api.Calls = nil
	for i := 1; i <= 6; i++ {
		if handled, _ := a.groupDraft.streamDraftEvent(context.Background(), raw, 0, 10, "late event", messages.OutThinking); !handled {
			t.Fatalf("late event %d must be handled (fresh cold-create); got handled=false", i)
		}
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("late events after endProcess must cold-create; got %d sendRichMessage", len(sends))
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
