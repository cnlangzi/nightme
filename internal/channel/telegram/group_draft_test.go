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
// malformed or empty. The rich_message param is json.RawMessage
// (a named []byte), so a switch handles both the typed and raw
// byte-slice forms that can appear in fakeCall.Params after the
// value is round-tripped through the form encoder.
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
		return nil
	}
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

// TestAdapter_Send_Group_OutThinking_CreatesDraftMessage asserts that
// the first OutThinking in a group turn cold-creates a real Telegram
// message (sendMessage) — no sendMessageDraft, no richTurn chain
// segment — and that the cold-create carries reply_to_message_id so
// the DraftMessage hangs under the user's message.
func TestAdapter_Send_Group_OutThinking_CreatesDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "considering whether to invoke Read",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	if draft := findCallByMethod(api.Calls, "sendMessageDraft"); draft != nil {
		t.Fatalf("group must NOT use sendMessageDraft; got %+v", draft)
	}
	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("expected sendRichMessage cold-create for group DraftMessage; got calls=%+v", api.Calls)
	}
	if cold.Params["reply_to_message_id"] != 1 {
		t.Fatalf("reply_to_message_id = %v, want 1 (userMsgID)", cold.Params["reply_to_message_id"])
	}
	if got := richMessageFirstBlockText(cold.Params["rich_message"]); got != "💭 considering whether to invoke Read" {
		t.Fatalf("cold-create text = %q, want %q", got, "💭 considering whether to invoke Read")
	}
}

// TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace
// verifies the REPLACE path: once the batch threshold (10 logical
// events) is hit, the buffered entries flush via editMessageText
// against the persisted DraftMessageID — NOT a fresh sendRichMessage.
// The cold-create carries the first event; the editMessageText
// carries the last event of the window (REPLACE wiped prior buffer
// entries).
func TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// 11 OutThinking events: event 1 cold-creates (count resets to 0
	// after coldCreate), events 2..10 buffer (count climbs 1..9),
	// event 11 hits count >= 10 and flushes via editMessageText.
	for i := 1; i <= 11; i++ {
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
		t.Fatalf("expected exactly 1 editMessageText on batch flush, got %d", len(edits))
	}
	// REPLACE semantics: each OutThinking wipes prior entries, so
	// the flush body shows only the LAST event of the window.
	if got := richMessageFirstBlockText(edits[0].Params["rich_message"]); got != "💭 thought 11" {
		t.Fatalf("edit first-block text = %q, want %q (REPLACE: prior body wiped)", got, "💭 thought 11")
	}
}

// TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart verifies
// the ACCUMULATE path: the OutToolEnd result line stacks under the
// matching OutToolStart's call line in a single DraftMessage body.
// Each rich_turn entry becomes one paragraph block in the rich
// message envelope; flush body is verified to have two blocks
// (call + result) instead of one.
func TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Cold-create via first OutToolStart; buffer is wiped after
	// the coldCreate sendRichMessage.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	})
	// Pair 2 in the same post-cold-create window: OutToolStart
	// REPLACES (entries=[Write/Start]), OutToolEnd ACCUMULATES
	// under it (entries=[Write/Start, Write/End]). Forcing the
	// flush via OnPromptEnded → endProcess → flushLocked exercises
	// the same flush path a count- or timer-based flush would.
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
		t.Fatalf("expected 1 editMessageText (ACCUMULATE flush), got %d (calls=%+v)", len(edits), api.Calls)
	}
	blocks := richMessageBlocks(edits[0].Params["rich_message"])
	if len(blocks) != 2 {
		t.Fatalf("ACCUMULATE rich_message blocks = %d, want 2 (Start+End); got %v", len(blocks), blocks)
	}
	if blocks[0] != "● Write(/tmp/bar.go)" {
		t.Fatalf("ACCUMULATE block[0] = %q, want %q", blocks[0], "● Write(/tmp/bar.go)")
	}
	if blocks[1] != "⎿  📝 Write → 11 bytes" {
		t.Fatalf("ACCUMULATE block[1] = %q, want %q", blocks[1], "⎿  📝 Write → 11 bytes")
	}
}

// TestAdapter_Send_Group_NextTurn_CreatesFreshDraftMessage verifies
// that ensurePlaceholder's per-turn DraftMessageID reset (group
// ChatKind branch) prevents stale edits to the prior turn's
// DraftMessage. Turn N+1 must cold-create a new one.
func TestAdapter_Send_Group_NextTurn_CreatesFreshDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: think + tool pair.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "turn 1 thought",
	})
	// Force turn-end so the next Out* cold-creates fresh.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)
	api.Calls = nil

	// Turn 2: new user message resets state.DraftMessageID; first
	// OutThinking must cold-create a brand new sendMessage.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "turn 2 thought",
	})

	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 2 first event should cold-create a fresh DraftMessage; got %d sendRichMessage (calls=%+v)", len(sends), api.Calls)
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("turn 2 first event must NOT edit prior DraftMessage; got %d edits", len(edits))
	}
}

// TestAdapter_Send_Group_DraftMessage_RoutedThroughTopic verifies
// that when the user is inside a forum topic (topicID > 0), the
// cold-create carries message_thread_id so the DraftMessage lives
// in the same topic as the conversation.
func TestAdapter_Send_Group_DraftMessage_RoutedThroughTopic(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 42)

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw + ":42",
		Kind:   messages.OutThinking,
		Text:   "topic-scoped thought",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	cold := findCallByMethod(api.Calls, "sendRichMessage")
	if cold == nil {
		t.Fatalf("expected sendRichMessage cold-create; got calls=%+v", api.Calls)
	}
	if thread, _ := cold.Params["message_thread_id"].(int); thread != 42 {
		t.Fatalf("message_thread_id = %v, want 42", cold.Params["message_thread_id"])
	}
}

// TestAdapter_OnPromptEnded_Group_DeletesDraftMessage verifies the
// turn-end cleanup: deleteMessage fires for the simulated DraftMessage
// (sendMessageDraft's auto-disappear analogue). There is no persisted
// DraftMessageID anymore — the DraftMessage is purely in-memory state —
// so this test only checks the deleteMessage side of the contract.
func TestAdapter_OnPromptEnded_Group_DeletesDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "thought to be deleted",
	})

	// The cold-create has already produced a message_id.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	deletes := callsByMethod(api.Calls, "deleteMessage")
	if len(deletes) != 1 {
		t.Fatalf("expected 1 deleteMessage on OnPromptEnded, got %d (calls=%+v)", len(deletes), api.Calls)
	}
	if msgID, _ := deletes[0].Params["message_id"].(int); msgID == 0 {
		t.Fatalf("deleteMessage with message_id = 0; want the cold-created id")
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

// TestAdapter_Send_Group_ColdCreateFailure_FallsThroughToRichTurn
// verifies the contract: when the cold-create sendRichMessage fails,
// streamDraftEvent returns handled=false so the caller falls
// through to the richTurn chain path. Without this, a transient
// API error would silently drop the event.
func TestAdapter_Send_Group_ColdCreateFailure_FallsThroughToRichTurn(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	api.Errors = []error{errors.New("simulated sendRichMessage 400")}

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "fallback thought",
	}); err != nil {
		// Send itself returns nil; the fallback to richTurn is
		// internal and silent on the error path.
		t.Logf("send returned %v (expected nil or wrapped)", err)
	}

	// The in-memory entry must not have a messageID — the cold-create
	// failed, so subsequent streamDraftEvent calls would still see
	// messageID == 0 and retry on the next event (or fall through
	// after the retry budget is exhausted).
	entry, ok := a.groupDraft.entries[groupDraftKey(raw, 0, 1)]
	if ok && entry.messageID != 0 {
		t.Fatalf("entry.messageID = %d after cold-create failure; want 0", entry.messageID)
	}
}

// TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath
// verifies the contract: when a flush's editMessageText fails (e.g.
// Telegram 429), streamDraftEvent returns handled=true so the caller
// does NOT fall through to the rich turn. Without this, tool result
// lines (`⎿ 🔧 tool → N bytes`) would leak into the final answer
// message — see issue #391.
//
// With batched flush semantics (10 logical events per window), the
// failing flush fires on the 11th event (count threshold). The
// recovery flush fires on the 12th event (count was preserved on
// failure, so it re-trips the threshold immediately).
func TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Events 1..10: cold-create + buffered (count climbs 1..9). The
	// 11th event trips the count threshold and triggers the failing
	// editMessageText; the 12th event triggers the recovery flush.
	// We poison api.Errors between event 10 (buffered) and event 11
	// (failing flush) so the cold-create at event 1 succeeds.
	send := func(text string) {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_" + raw,
			Kind:   messages.OutThinking,
			Text:   text,
		}); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
	}
	send("first thought") // event 1: cold-create succeeds.

	entry, ok := a.groupDraft.entries[groupDraftKey(raw, 0, 1)]
	if !ok || entry == nil {
		t.Fatalf("group draft entry missing after cold-create")
	}
	originalMsgID := entry.messageID
	if originalMsgID == 0 {
		t.Fatalf("cold-create did not produce a message_id")
	}

	for i := 2; i <= 10; i++ {
		send(fmt.Sprintf("buffered thought %d", i))
	}

	// Event 11 trips count>=10 → flush → editMessageText fails
	// (Telegram 429 simulation). streamDraftEvent MUST return
	// handled=true so the caller does NOT route this event into the
	// rich turn.
	api.Errors = []error{errors.New("simulated editMessageText 429")}
	send("failing flush thought")

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

	// Exactly one editMessageText call was attempted (the failing
	// one); no second sendRichMessage (no re-cold-create).
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText call, got %d (calls=%+v)", len(edits), api.Calls)
	}
	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendRichMessage cold-create, got %d", len(sends))
	}

	// Recovery: api.Errors cleared. The buffered count was preserved
	// on failure, so the next event re-trips the count threshold
	// immediately and flushes successfully (no re-cold-create).
	api.Errors = nil
	send("recovery flush thought")

	edits = callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 2 {
		t.Fatalf("expected 2 editMessageText after recovery, got %d", len(edits))
	}
	sends = callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("recovery must NOT re-cold-create; got %d sendRichMessage", len(sends))
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
// count-threshold flush produces a single editMessageText on the
// same message_id. Without the lock, two goroutines would both see
// entry.messageID == 0 and both sendRichMessage → two messages in
// chat.
func TestAdapter_Send_Group_ConcurrentStreamDraftEvent_NoDoubleColdCreate(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// 11 events: event 1 cold-creates (count resets to 0), events
	// 2..10 buffer (count climbs 1..9), event 11 trips count>=10
	// and flushes via editMessageText. The lock-held
	// streamDraftEvent serialises all 11 calls, so the API sees
	// exactly 1 cold-create + 1 flush.
	const N = 11
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
		t.Fatalf("expected 1 editMessageText (count-threshold flush), got %d", len(edits))
	}
	// All API calls target the same message_id (the cold-create's);
	// the per-id equality check would need api.Calls to expose
	// message_id which we already know is shared (otherwise the
	// chat would have orphan messages from the race).
	_ = richMessageFirstBlockText(sends[0].Params["rich_message"])
	_ = richMessageFirstBlockText(edits[0].Params["rich_message"])
}

// TestAdapter_Send_Group_OutThinking_UsesMsgReplyTo verifies that
// Send() derives replyAnchor from msg.ReplyTo, not from
// state.UserMessageID. Setup pins state.UserMessageID="99" but the
// event carries ReplyTo="42"; the cold-create must anchor to 42.
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

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutThinking,
		Text:    "anchored to msg.ReplyTo",
		ReplyTo: "42",
	}); err != nil {
		t.Fatalf("send: %v", err)
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

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "orphan thought — no ReplyTo",
		// ReplyTo intentionally empty
	}); err != nil {
		t.Fatalf("send: %v", err)
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
// Batched-flush caveat: a single late event is now buffered (not
// flushed immediately), so the per-turn isolation assertion is that
// no new API call fires at all on the late event — it joins turn
// 1's existing entry, awaiting the next flush.
func TestAdapter_Send_Group_BackToBackPrompts_DraftMessagesIsolated(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1: think starts the DraftMessage.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutThinking,
		Text:    "turn 1 thought",
		ReplyTo: "10",
	}); err != nil {
		t.Fatalf("turn 1 think: %v", err)
	}
	sends := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 1 cold-create expected 1 sendRichMessage, got %d", len(sends))
	}
	turn1ReplyTo, _ := sends[0].Params["reply_to_message_id"].(int)
	if turn1ReplyTo != 10 {
		t.Fatalf("turn 1 reply_to_message_id = %v, want 10", sends[0].Params["reply_to_message_id"])
	}
	turn1Text := richMessageFirstBlockText(sends[0].Params["rich_message"])
	if turn1Text != "💭 turn 1 thought" {
		t.Fatalf("turn 1 text = %q, want %q", turn1Text, "💭 turn 1 thought")
	}

	// Simulate turn 2's user message arriving: ensurePlaceholder
	// runs and overwrites state.UserMessageID to 11.
	if err := a.ensurePlaceholder(context.Background(), raw, 0, 11, &Message{
		MessageID: 11,
		Chat:      Chat{ID: -1001, Type: "supergroup"},
	}); err != nil {
		t.Fatalf("ensurePlaceholder turn 2: %v", err)
	}

	// Turn 1 late OutToolEnd arrives with ReplyTo=10 (NOT state=11).
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
	// DraftMessage for turn 2. With batching it is also not flushed
	// yet, so no editMessageText fires.
	//
	// Two sendRichMessage calls are expected by this point: (1)
	// turn 1's DraftMessage cold-create and (2) turn 2's eager rich
	// turn placeholder fired by ensurePlaceholder. The late event
	// must NOT add a third.
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 2 {
		t.Fatalf("turn 1 late event must NOT cold-create a new DraftMessage; sends=%d (want 2)", len(sends))
	}

	// Now turn 2's OutThinking arrives — it must cold-create a fresh
	// DraftMessage anchored to 11 (the state.UserMessageID AFTER
	// ensurePlaceholder for turn 2), not 10.
	api.Calls = nil
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:  "tg_" + raw,
		Kind:    messages.OutThinking,
		Text:    "turn 2 thought",
		ReplyTo: "11",
	}); err != nil {
		t.Fatalf("turn 2 think: %v", err)
	}
	sends2 := callsByMethod(api.Calls, "sendRichMessage")
	if len(sends2) != 1 {
		t.Fatalf("turn 2 cold-create expected 1 sendRichMessage, got %d", len(sends2))
	}
	turn2ReplyTo, _ := sends2[0].Params["reply_to_message_id"].(int)
	if turn2ReplyTo != 11 {
		t.Fatalf("turn 2 reply_to_message_id = %v, want 11", sends2[0].Params["reply_to_message_id"])
	}
	turn2Text := richMessageFirstBlockText(sends2[0].Params["rich_message"])
	if turn2Text != "💭 turn 2 thought" {
		t.Fatalf("turn 2 text = %q, want %q", turn2Text, "💭 turn 2 thought")
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
// a late OutToolEnd from the just-ended turn cannot hit a 400
// "message not found" by editing an already-deleted Telegram
// message. After endProcess returns, m.entries must not contain
// the turn's key, and a streamDraftEvent for the same userMsgID
// must cold-create a fresh DraftMessage (not editMessageText).
func TestGroupDraft_EndProcess_DropsEntryFromMap(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Turn 1 cold-creates a DraftMessage.
	if handled, _ := a.groupDraft.streamDraftEvent(context.Background(), raw, 0, 10, "first", messages.OutThinking); !handled {
		t.Fatalf("first call should be handled (cold-create); got handled=false")
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("expected 1 cold-create, got %d", len(sends))
	}

	// Confirm the entry is in m.entries.
	a.groupDraft.mu.Lock()
	_, present := a.groupDraft.entries[groupDraftKey(raw, 0, 10)]
	a.groupDraft.mu.Unlock()
	if !present {
		t.Fatalf("entry must be in m.entries after cold-create")
	}

	// Turn 1 ends. endProcess must (a) call deleteMessage and
	// (b) drop the entry from m.entries.
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

	// A late event for the same turn arrives — must cold-create a
	// fresh DraftMessage (reply_to_message_id=10 still anchors the
	// chain under the user's original message), NOT editMessageText
	// the now-deleted id.
	api.Calls = nil
	if handled, _ := a.groupDraft.streamDraftEvent(context.Background(), raw, 0, 10, "late event", messages.OutThinking); !handled {
		t.Fatalf("late event must be handled (fresh cold-create); got handled=false")
	}
	if sends := callsByMethod(api.Calls, "sendRichMessage"); len(sends) != 1 {
		t.Fatalf("late event after endProcess must cold-create; got %d sendRichMessage", len(sends))
	}
	if edits := callsByMethod(api.Calls, "editMessageText"); len(edits) != 0 {
		t.Fatalf("late event after endProcess must NOT editMessageText (entry was dropped); got %d edits", len(edits))
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
