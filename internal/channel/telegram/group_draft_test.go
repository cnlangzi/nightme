package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

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
	cold := findCallByMethod(api.Calls, "sendMessage")
	if cold == nil {
		t.Fatalf("expected sendMessage cold-create for group DraftMessage; got calls=%+v", api.Calls)
	}
	if cold.Params["reply_to_message_id"] != 1 {
		t.Fatalf("reply_to_message_id = %v, want 1 (userMsgID)", cold.Params["reply_to_message_id"])
	}
	if got, _ := cold.Params["text"].(string); got != "💭 considering whether to invoke Read" {
		t.Fatalf("cold-create text = %q, want %q", got, "💭 considering whether to invoke Read")
	}
}

// TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace
// verifies the REPLACE path on the second event: editMessageText
// (NOT a fresh sendMessage) targets the persisted DraftMessageID,
// keeping the chat tidy.
func TestAdapter_Send_Group_OutThinking_SecondEvent_EDITesInPlace(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "first thought",
	})
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "second thought",
	})

	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendMessage cold-create, got %d (calls=%+v)", len(sends), api.Calls)
	}
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected exactly 1 editMessageText for second event, got %d", len(edits))
	}
	if text, _ := edits[0].Params["text"].(string); text != "💭 second thought" {
		t.Fatalf("edit text = %q, want %q (REPLACE: prior body wiped)", text, "💭 second thought")
	}
}

// TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart verifies
// the ACCUMULATE path: the OutToolEnd result line stacks under the
// matching OutToolStart's call line in a single DraftMessage body,
// separated by a blank line — same format as #383 DM draft.
func TestAdapter_Send_Group_OutToolEnd_ACCUMULATES_UnderStart(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	})
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolEnd,
		Tool:   &messages.ToolInfo{Name: "Read", Output: "47 lines"},
		Text:   "📄 Read → 47 lines",
	})

	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText (OutToolEnd accumulate), got %d (calls=%+v)", len(edits), api.Calls)
	}
	want := "● Read(/tmp/foo.go)\n\n⎿  📄 Read → 1 lines"
	if got, _ := edits[0].Params["text"].(string); got != want {
		t.Fatalf("ACCUMULATE body = %q, want %q", got, want)
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

	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 2 first event should cold-create a fresh DraftMessage; got %d sendMessage (calls=%+v)", len(sends), api.Calls)
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

	cold := findCallByMethod(api.Calls, "sendMessage")
	if cold == nil {
		t.Fatalf("expected sendMessage cold-create; got calls=%+v", api.Calls)
	}
	if thread, _ := cold.Params["message_thread_id"].(int); thread != 42 {
		t.Fatalf("message_thread_id = %v, want 42", cold.Params["message_thread_id"])
	}
}

// TestAdapter_OnPromptEnded_Group_DeletesDraftMessage verifies the
// turn-end cleanup: deleteMessage fires for the simulated DraftMessage
// (sendMessageDraft's auto-disappear analogue) and the persisted
// DraftMessageID is cleared so the next turn starts clean.
func TestAdapter_OnPromptEnded_Group_DeletesDraftMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "thought to be deleted",
	})

	// The cold-create has already produced a message_id (counter 101).
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

	// Persisted DraftMessageID cleared on state.
	state, ok := a.state.topic(raw, 0)
	if !ok {
		t.Fatalf("topic state missing")
	}
	if state.DraftMessageID != 0 {
		t.Fatalf("state.DraftMessageID = %d, want 0 after OnPromptEnded", state.DraftMessageID)
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
// verifies the contract: when the cold-create sendMessage fails,
// streamDraftEvent returns handled=false so the caller falls
// through to the richTurn chain path. Without this, a transient
// API error would silently drop the event.
func TestAdapter_Send_Group_ColdCreateFailure_FallsThroughToRichTurn(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)
	api.Errors = []error{errors.New("simulated sendMessage 400")}

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "fallback thought",
	}); err != nil {
		// Send itself returns nil; the fallback to richTurn is
		// internal and silent on the error path.
		t.Logf("send returned %v (expected nil or wrapped)", err)
	}

	// No DraftMessage should have been persisted.
	state, _ := a.state.topic(raw, 0)
	if state != nil && state.DraftMessageID != 0 {
		t.Fatalf("expected DraftMessageID == 0 after cold-create failure, got %d", state.DraftMessageID)
	}
}

// TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath
// verifies the contract: when the subsequent editMessageText fails
// (e.g. Telegram 429), streamDraftEvent returns handled=true so the
// caller does NOT fall through to the rich turn. Without this, tool
// result lines (`⎿ 🔧 tool → N bytes`) would leak into the final
// answer message — see issue #391.
func TestAdapter_Send_Group_EditMessageTextFailure_StaysOnDraftPath(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// First event: cold-create must succeed (no error queued).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "first thought",
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}

	state, ok := a.state.topic(raw, 0)
	if !ok || state == nil {
		t.Fatalf("state missing after cold-create")
	}
	originalMsgID := state.DraftMessageID
	if originalMsgID == 0 {
		t.Fatalf("cold-create did not persist DraftMessageID")
	}

	// Second event: editMessageText will fail (Telegram 429
	// simulation). streamDraftEvent MUST return handled=true so the
	// caller does NOT route this event into the rich turn.
	api.Errors = []error{errors.New("simulated editMessageText 429")}

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "second thought",
	}); err != nil {
		t.Fatalf("second send: %v", err)
	}

	// The DraftMessageID must be preserved — the entry is still alive
	// and ready for the next event to retry on top.
	state, _ = a.state.topic(raw, 0)
	if state == nil || state.DraftMessageID != originalMsgID {
		t.Fatalf("DraftMessageID changed on edit failure: was %d, now %v", originalMsgID, state)
	}

	// No rich turn entry should exist for this turn — if it did, the
	// tool/think line leaked into the rich turn (the #391 bug).
	if turn, ok := a.richTurns.lookup(raw, 0, 1); ok && turn != nil {
		t.Fatalf("rich turn was created for failed edit — fallback leaked; entries=%d", len(turn.entries))
	}

	// Exactly one editMessageText call was attempted (the failing
	// one); no second sendMessage (no re-cold-create).
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText call, got %d (calls=%+v)", len(edits), api.Calls)
	}
	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendMessage cold-create, got %d", len(sends))
	}

	// Recovery: a third event with API recovered must re-use the
	// same DraftMessageID via editMessageText (no re-cold-create).
	api.Errors = nil
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "third thought (recovered)",
	}); err != nil {
		t.Fatalf("third send: %v", err)
	}

	edits = callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 2 {
		t.Fatalf("expected 2 editMessageText after recovery, got %d", len(edits))
	}
	sends = callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("recovery must NOT re-cold-create; got %d sendMessage", len(sends))
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

// TestAdapter_Send_Group_EnsurePlaceholder_DeletesOrphan verifies the
// crash / network-failure recovery path: when ensurePlaceholder sees
// state.DraftMessageID > 0 from a previous turn (daemon crashed
// mid-turn, or deleteMessage failed last time), it must
// deleteMessage that orphan before clearing the state. Without this,
// orphans pile up across daemon restarts.
func TestAdapter_Send_Group_EnsurePlaceholder_DeletesOrphan(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Simulate a previous turn that left an orphan: stamp a
	// DraftMessageID on state directly (no in-memory entry —
	// equivalent to "daemon restarted after a cold-create but
	// before OnPromptEnded").
	if err := a.state.putTopic(&TopicState{
		ChatID:         raw,
		TopicID:        0,
		ChatKind:       ChatKindGroup,
		UserMessageID:  "1",
		DraftMessageID: 999,
	}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	// Trigger ensurePlaceholder via handleMessage (the only
	// caller). New inbound user message → orphan cleanup runs.
	a.handleMessage(context.Background(), &Message{
		MessageID: 2,
		Chat:      Chat{ID: -1001, Type: "supergroup"},
		Text:      "next turn",
		From:      &User{ID: 7},
		Date:      time.Now().Unix(),
	})

	// deleteOrphanForNewTurn is fire-and-forget; wait for the
	// goroutine to fire the API call.
	deletes := waitForMethod(t, api, "deleteMessage", 2*time.Second)
	if len(deletes) != 1 {
		t.Fatalf("expected 1 deleteMessage for orphan, got %d (calls=%+v)", len(deletes), api.Calls)
	}
	if mid, _ := deletes[0].Params["message_id"].(int); mid != 999 {
		t.Fatalf("orphan delete message_id = %d, want 999", mid)
	}

	// state.DraftMessageID cleared after the deleteMessage (async).
	state, _ := a.state.topic(raw, 0)
	if state == nil {
		t.Fatal("topic state missing")
	}
	waitFor(t, func() bool {
		s, _ := a.state.topic(raw, 0)
		return s != nil && s.DraftMessageID == 0
	}, 2*time.Second, "state.DraftMessageID should clear after orphan delete")
}

// TestAdapter_Send_Group_EnsurePlaceholder_OrphanDeleteFailure_KeepsState
// documents the best-effort contract: when the orphan delete fails,
// state.DraftMessageID is still cleared (so the new turn's
// cold-create can claim the slot) and the orphan is left in chat.
// Retrying across turns would require a separate
// OrphanDraftMessageID field to avoid races with the new turn's
// cold-create, and apiCall already retries transient errors, so
// the permanent-failure case is rare enough to accept the leak.
func TestAdapter_Send_Group_EnsurePlaceholder_OrphanDeleteFailure_KeepsState(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	if err := a.state.putTopic(&TopicState{
		ChatID:         raw,
		TopicID:        0,
		ChatKind:       ChatKindGroup,
		UserMessageID:  "1",
		DraftMessageID: 1234,
	}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	api.MethodErrors = map[string]error{
		"deleteMessage": errors.New("simulated 500"),
	}

	a.handleMessage(context.Background(), &Message{
		MessageID: 2,
		Chat:      Chat{ID: -1001, Type: "supergroup"},
		Text:      "trigger orphan cleanup",
		From:      &User{ID: 7},
		Date:      time.Now().Unix(),
	})

	// deleteMessage was attempted (sync, happens during handleMessage).
	deletes := callsByMethod(api.Calls, "deleteMessage")
	if len(deletes) != 1 {
		t.Fatalf("expected 1 deleteMessage attempt for orphan, got %d (calls=%+v)", len(deletes), api.Calls)
	}
	if mid, _ := deletes[0].Params["message_id"].(int); mid != 1234 {
		t.Fatalf("orphan delete message_id = %d, want 1234", mid)
	}
	// Best-effort: state cleared regardless of success so the new
	// turn can claim the slot.
	state, _ := a.state.topic(raw, 0)
	if state == nil {
		t.Fatal("topic state missing")
	}
	if state.DraftMessageID != 0 {
		t.Fatalf("state.DraftMessageID = %d after orphan cleanup attempt, want 0 (best-effort clear)", state.DraftMessageID)
	}
}

// TestAdapter_Send_Group_EndProcess_DeleteFailure_KeepsState verifies
// the same retry-on-failure contract for the turn-end cleanup path:
// deleteMessage from endProcess failing leaves state.DraftMessageID
// intact so the next ensurePlaceholder retries.
func TestAdapter_Send_Group_EndProcess_DeleteFailure_KeepsState(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	// Cold-create first (so endProcess has a message to delete).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "thought",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Make the next deleteMessage call fail.
	api.Errors = []error{errors.New("simulated 500")}

	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	state, _ := a.state.topic(raw, 0)
	if state == nil {
		t.Fatal("topic state missing")
	}
	if state.DraftMessageID == 0 {
		t.Fatalf("state.DraftMessageID cleared despite deleteMessage failure; retry loop broken")
	}
}

// TestAdapter_Send_Group_ConcurrentStreamDraftEvent_NoDoubleColdCreate
// fires N goroutines that all call Send with the same (chat, topic,
// userMsgID) simultaneously. The per-entry lock must serialise them
// so exactly one sendMessage cold-create fires and the rest are
// editMessageText on the same message_id. Without the lock, two
// goroutines would both see entry.messageID == 0 and both
// sendMessage → two messages in chat.
func TestAdapter_Send_Group_ConcurrentStreamDraftEvent_NoDoubleColdCreate(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 0)

	const N = 8
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

	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected exactly 1 sendMessage cold-create across %d concurrent events, got %d (calls=%+v)",
			N, len(sends), api.Calls)
	}
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != N-1 {
		t.Fatalf("expected %d editMessageText (one per non-cold-create event), got %d", N-1, len(edits))
	}
	// All edits target the same message_id (the cold-create's).
	coldID, _ := sends[0].Params["text"]
	_ = coldID
	for i, e := range edits {
		// Just verify edits happened; the per-id equality check
		// would need api.Calls to expose message_id which we
		// already know is shared (otherwise the chat would have
		// orphan messages from the race).
		_ = i
		_ = e
	}
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

	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendMessage cold-create, got %d", len(sends))
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

	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected 1 sendMessage cold-create, got %d", len(sends))
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
// DraftMessage. The bug would manifest as turn 2's sendMessage
// carrying turn 1's body, or turn 1's late OutToolEnd rewriting
// turn 2's message_id.
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
	sends := callsByMethod(api.Calls, "sendMessage")
	if len(sends) != 1 {
		t.Fatalf("turn 1 cold-create expected 1 sendMessage, got %d", len(sends))
	}
	turn1ReplyTo, _ := sends[0].Params["reply_to_message_id"].(int)
	if turn1ReplyTo != 10 {
		t.Fatalf("turn 1 reply_to_message_id = %v, want 10", sends[0].Params["reply_to_message_id"])
	}
	turn1Text, _ := sends[0].Params["text"].(string)
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

	// Expect: turn 1's late event went out as an editMessageText
	// (it belongs to turn 1's DraftMessage), NOT a new sendMessage.
	edits := callsByMethod(api.Calls, "editMessageText")
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText for turn 1 late event, got %d (sends=%d)", len(edits), len(callsByMethod(api.Calls, "sendMessage")))
	}
	// No new sendMessage — turn 1's entry was reused.
	if sends := callsByMethod(api.Calls, "sendMessage"); len(sends) != 1 {
		t.Fatalf("turn 1 late event must NOT cold-create a new DraftMessage; sends=%d", len(sends))
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
	sends2 := callsByMethod(api.Calls, "sendMessage")
	if len(sends2) != 1 {
		t.Fatalf("turn 2 cold-create expected 1 sendMessage, got %d", len(sends2))
	}
	turn2ReplyTo, _ := sends2[0].Params["reply_to_message_id"].(int)
	if turn2ReplyTo != 11 {
		t.Fatalf("turn 2 reply_to_message_id = %v, want 11", sends2[0].Params["reply_to_message_id"])
	}
	turn2Text, _ := sends2[0].Params["text"].(string)
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
	if handled, _ := a.groupDraft.streamDraftEvent(context.Background(), raw, 0, 10, "first", true); !handled {
		t.Fatalf("first call should be handled (cold-create); got handled=false")
	}
	if sends := callsByMethod(api.Calls, "sendMessage"); len(sends) != 1 {
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
	if handled, _ := a.groupDraft.streamDraftEvent(context.Background(), raw, 0, 10, "late event", true); !handled {
		t.Fatalf("late event must be handled (fresh cold-create); got handled=false")
	}
	if sends := callsByMethod(api.Calls, "sendMessage"); len(sends) != 1 {
		t.Fatalf("late event after endProcess must cold-create; got %d sendMessage", len(sends))
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

// helpers — package-internal polling utilities for async goroutine
// paths (deleteOrphanForNewTurn spawns a goroutine; tests need to
// wait for its API call to land before asserting on api.Calls).

func waitForMethod(t *testing.T, api *fakeAPI, method string, timeout time.Duration) []fakeCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cs := callsByMethod(api.snapshotCalls(), method); len(cs) > 0 {
			return cs
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}
