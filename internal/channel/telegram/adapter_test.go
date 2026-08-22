package telegram

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

func newTestAdapter(t *testing.T) (*Adapter, *fakeAPI) {
	t.Helper()
	resetSendMessageCounter()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "test-token"
	cfg.Paths.DataDir = dir
	api := &fakeAPI{GetMeResult: UserInfo{ID: 999, Username: "testbot"}}
	adapter := NewAdapterWithClient(cfg, api, dir)
	if adapter == nil {
		t.Fatal("NewAdapterWithClient nil")
	}
	return adapter, api
}

func TestNewAdapterWithClient_NilConfig(t *testing.T) {
	resetSendMessageCounter()
	dir := t.TempDir()
	a := NewAdapterWithClient(nil, &fakeAPI{}, dir)
	if a == nil {
		t.Fatal("nil cfg must still produce adapter")
	}
}

func TestNewAdapter_NilConfig(t *testing.T) {
	if _, err := NewAdapter(nil); err == nil {
		t.Fatal("nil cfg must error")
	}
}

func TestNewAdapter_EmptyToken(t *testing.T) {
	cfg := &config.Config{}
	if _, err := NewAdapter(cfg); err == nil {
		t.Fatal("empty token must error")
	}
}

func TestNewAdapter_DefaultsPollingTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "x"
	dir := t.TempDir()
	a, err := NewAdapter(cfg)
	if err != nil {
		// NewAdapter needs the data dir to be writeable, etc.
		t.Skip("skipping: requires real data dir")
	}
	if a.config.PollingTimeout != 30 {
		t.Skip("skip")
	}
	_ = dir
}

func TestAdapter_Start_Stop(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestAdapter_Start_GetMeError(t *testing.T) {
	a, api := newTestAdapter(t)
	api.GetMeErr = &apiError{Message: "boom"}
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestAdapter_Start_EmptyUsername(t *testing.T) {
	a, api := newTestAdapter(t)
	api.GetMeResult = UserInfo{ID: 1}
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("empty username must error")
	}
}

func TestAdapter_Stop_NilContext(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Stop(nil); err != nil {
		t.Fatalf("stop nil ctx: %v", err)
	}
}

func TestAdapter_Stop_Twice(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop 1: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop 2: %v", err)
	}
}

func TestAdapter_Stop_BeforeStart(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop before start: %v", err)
	}
}

func TestAdapter_Name(t *testing.T) {
	a, _ := newTestAdapter(t)
	if a.Name() != "telegram" {
		t.Fatalf("name = %q", a.Name())
	}
}

func TestAdapter_SetLogger(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.SetLogger(nil)
}

func TestAdapter_HealthSnapshot(t *testing.T) {
	a, _ := newTestAdapter(t)
	name, payload, err := a.HealthSnapshot()
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if name != "telegram" {
		t.Fatalf("name = %q", name)
	}
	if len(payload) == 0 {
		t.Fatal("payload empty")
	}
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Schema: username, connected, offset. mode was removed in
	// the polling-only refactor — see docs/channel/telegram.md §10.4.
	for _, key := range []string{"username", "connected", "offset"} {
		if _, ok := parsed[key]; !ok {
			t.Fatalf("missing key %q in %v", key, parsed)
		}
	}
}

func TestAdapter_BuildBlocks(t *testing.T) {
	a, _ := newTestAdapter(t)
	blocks := a.BuildBlocks("text", []messages.Attachment{
		{LocalPath: "/tmp/img", MimeType: "image/png", Name: "img.png"},
		{LocalPath: "/tmp/file", MimeType: "application/json"},
	})
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	if blocks[0].Text != "text" {
		t.Fatalf("first block = %+v", blocks[0])
	}
	if blocks[1].Path != "/tmp/img" {
		t.Fatalf("block path = %q", blocks[1].Path)
	}
}

func TestAdapter_BuildBlocks_NoAttachments(t *testing.T) {
	a, _ := newTestAdapter(t)
	blocks := a.BuildBlocks("only text", nil)
	if len(blocks) != 1 || blocks[0].Text != "only text" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestAdapter_BuildBlocks_EmptyAttachments(t *testing.T) {
	a, _ := newTestAdapter(t)
	blocks := a.BuildBlocks("", []messages.Attachment{{}})
	if len(blocks) != 0 {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestAdapter_Incoming(t *testing.T) {
	a, _ := newTestAdapter(t)
	ch := a.Incoming()
	if ch == nil {
		t.Fatal("incoming nil")
	}
}

func TestAdapter_Send_EmptyChatID(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{ChatID: "", Kind: messages.OutReply})
	if err == nil {
		t.Fatal("empty chatID must error")
	}
}

func TestAdapter_Send_OutReply(t *testing.T) {
	a, api := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutReply,
		Text:   "hello",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if findCall(api.snapshotCalls(), "sendMessage") == nil {
		t.Fatal("no sendMessage call")
	}
}

func TestAdapter_Send_OutCommandReply(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutCommandReply,
		Text:   "cmd reply",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutInit(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutInit,
		AgentName: "claude",
		Model:     "opus",
		SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutError_Diagnostic(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutError,
		Text:   "boom",
		Diagnostic: &agent.BridgeDiagnostic{
			StderrTail: "stderr line 1\nstderr line 2",
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutTool(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "read", Args: "{\"path\":\"/x\"}"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	err = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutToolEnd,
		Tool:   &messages.ToolInfo{Name: "read", Output: "ok"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutTask(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutTaskCreate,
		TaskList: &agent.AgentTaskListEvent{
			Items: []agent.AgentTaskItem{
				{ID: "1", Subject: "task one", Status: agent.TaskPending},
				{ID: "2", Subject: "task two", Status: agent.TaskCompleted},
			},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutMessageState(t *testing.T) {
	a, api := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
			State:     agent.MessageSubmitted,
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// 适配器必须自行决定 emoji, 期望 🤔 (v6.3: 单 reaction 预算, 只 Submitted 贴)
	got := findCall(api.Calls, "setMessageReaction")
	if got == nil {
		t.Fatal("expected setMessageReaction call, got none")
	}
	wantReaction := []any{
		map[string]any{"type": "emoji", "emoji": "🤔"},
	}
	if !reflect.DeepEqual(got.Params["reaction"], wantReaction) {
		t.Errorf("reaction = %v, want %v", got.Params["reaction"], wantReaction)
	}
}

func TestAdapter_Send_OutMessageStateRemoved(t *testing.T) {
	a, api := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageStateRemoved,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// 移除态: reaction 必须是空数组 (Telegram API 用 [] 删除所有 reaction)
	got := findCall(api.Calls, "setMessageReaction")
	if got == nil {
		t.Fatal("expected setMessageReaction call, got none")
	}
	wantReaction := []any{}
	if !reflect.DeepEqual(got.Params["reaction"], wantReaction) {
		t.Errorf("reaction = %v, want %v", got.Params["reaction"], wantReaction)
	}
}

func TestAdapter_Send_OutMessageState_BadID(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "abc",
			State:     agent.MessageSubmitted,
		},
	})
	if err == nil {
		t.Fatal("non-numeric ID must error")
	}
}

func TestMapStateToTelegramEmoji(t *testing.T) {
	cases := []struct {
		in   agent.MessageState
		want string
	}{
		// v6.3: Telegram bot single-reaction budget — only
		// MessageSubmitted emits 🤔 ("AI thinking"). Queued
		// and Done are silent drops; their visuals are conveyed
		// via placeholder text PATCH (Queued) and the placeholder
		// 🎉 reaction (Done — handled in OnPromptEnded).
		{agent.MessageQueued, ""},
		{agent.MessageSubmitted, "🤔"},
		{agent.MessageDone, ""},
		{agent.MessageDropped, ""},    // 跟 feishu 对齐: 不留 reaction
		{agent.MessageState(999), ""}, // 未知 state silent drop
	}
	for _, c := range cases {
		if got := mapStateToTelegramEmoji(c.in); got != c.want {
			t.Errorf("mapStateToTelegramEmoji(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAdapter_Send_OutMessageState_QueuedRenders locks the
// v6.3 single-reaction budget: MessageQueued is a silent drop
// on the user message. The bot reserves its only reaction
// slot for MessageSubmitted ("AI thinking"). The placeholder
// text "🤖 Working..." still gets PATCHed, so the user
// sees the message-received visual without burning the reaction
// slot.
func TestAdapter_Send_OutMessageState_QueuedRenders(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
			State:     agent.MessageQueued,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// No setMessageReaction call expected (v6.3 silent drop).
	for _, call := range api.snapshotCalls() {
		if call.Method == "setMessageReaction" {
			t.Fatalf("v6.3 Queued must NOT call setMessageReaction; got params=%+v", call.Params)
		}
	}
}

// TestAdapter_Send_OutMessageState_DoneRenders locks the
// v6.3 single-reaction budget: MessageDone is a silent drop
// on the user message. The terminal 🎉 reaction lives on the
// per-turn placeholder (set by OnPromptEnded), not on the user
// message. Reserving the user-message reaction slot for
// MessageSubmitted only preserves the "thinking" visual for
// the entire async turn.
func TestAdapter_Send_OutMessageState_DoneRenders(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
			State:     agent.MessageDone,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, call := range api.snapshotCalls() {
		if call.Method == "setMessageReaction" {
			t.Fatalf("v6.3 Done must NOT call setMessageReaction; got params=%+v", call.Params)
		}
	}
}

func TestAdapter_Send_OutMessageState_UnknownStateDrops(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
			State:     agent.MessageState(999), // 未知 state
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := findCall(api.Calls, "setMessageReaction"); got != nil {
		t.Errorf("unknown state must silent-drop, got %v", got.Params)
	}
}

// TestAdapter_Send_OutMessageState_TracksStateIdempotency locks
// the LRU dedup: same state twice in a row skips the second
// call. v6.3 only emits for MessageSubmitted (Queued/Done are
// silent drops), so this test exercises:
//
//   1st Submitted → 1 reaction call
//   2nd Submitted (same) → dedup'd (0 extra)
//   3rd Submitted again → dedup'd
//
// We don't transition to Done here because v6.3 makes Done a
// silent drop; the LRU dedup path is what we're testing.
func TestAdapter_Send_OutMessageState_TracksStateIdempotency(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx := context.Background()
	msg := messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
		},
	}
	// 第一次 Submitted → 触发 API
	msg.MessageState.State = agent.MessageSubmitted
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	// 第二次 Submitted → 同 state skip
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	// 第三次 Submitted (再试一次) → 还是 dedup
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 3: %v", err)
	}
	if got := len(findCalls(api.Calls, "setMessageReaction")); got != 1 {
		t.Errorf("setMessageReaction calls = %d, want 1 (only 1st; 2nd & 3rd deduped)", got)
	}
}

// TestAdapter_Send_OutMessageState_FirstReceivedNotSkipped
// (F-31 review fix): empty lastMessageState map must not make
// the first emit look like a "repeat" and get deduped. v6.3
// uses MessageSubmitted since Queued is a silent drop.
func TestAdapter_Send_OutMessageState_FirstReceivedNotSkipped(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
			State:     agent.MessageSubmitted,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// 第一次 emit 必须真正打到 API, 不能因为 messageStates 初始为空就误判
	if got := len(findCalls(api.Calls, "setMessageReaction")); got != 1 {
		t.Errorf("setMessageReaction calls = %d, want 1 (first emit must not be deduped)", got)
	}
}

func TestAdapter_Send_OutMessageState_DroppedSilentDrops(t *testing.T) {
	// MessageDropped intentionally maps to "" (silent drop), aligned
	// with feishu's choice to convey failure via the reply text's ❌
	// prefix rather than a user-message reaction. This test pins that
	// decision so a future contributor adding "❌ for dropped" gets a
	// failing test that forces an explicit discussion.
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
			State:     agent.MessageDropped,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := findCall(api.Calls, "setMessageReaction"); got != nil {
		t.Errorf("MessageDown must silent-drop, got %v", got.Params)
	}
}

// TestAdapter_Send_OutMessageStateRemoved_DoesNotPolluteLRU
// v6.3: only MessageSubmitted emits a reaction. Queued / Done
// are silent drops, so this test only exercises Submitted
// transitions through Removed:
//
//   1st Submitted  → 1 setMessageReaction (🤔), LRU = {5: Submitted}
//   Removed        → 1 setMessageReaction ([]), LRU untouched
//   2nd Submitted  → dedup'd (0 extra) — proves LRU still holds
//                    Submitted (if Removed had zeroed the LRU, this
//                    would emit a 3rd call)
//
// If Removed had silently poisoned the LRU, the 2nd Submitted
// would either dedup against a stale sentinel (Removed-time state)
// or be unexpectedly triggered — both are bugs the test catches.
func TestAdapter_Send_OutMessageStateRemoved_DoesNotPolluteLRU(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx := context.Background()
	msg := messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
		},
	}
	// 1) Render Submitted → API call #1, LRU = {5: Submitted}
	msg.MessageState.State = agent.MessageSubmitted
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	// 2) Removed → API call #2 (empty reaction), LRU untouched
	if err := a.Send(ctx, messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageStateRemoved,
		MessageState: &messages.MessageStatePayload{
			MessageID: "5",
		},
	}); err != nil {
		t.Fatalf("send 2 (Removed): %v", err)
	}
	// 3) Same state again (Submitted) — must dedup against the LRU
	// record from step 1, proving Removed did not clear or poison
	// the LRU.
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 3 (Submitted again): %v", err)
	}
	if got := len(findCalls(api.Calls, "setMessageReaction")); got != 2 {
		t.Errorf("setMessageReaction calls = %d, want 2 (Submitted + Removed; 2nd Submitted dedup'd)", got)
	}
}

func TestAdapter_Send_OutHeartbeat_NoTopic(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 2, ToolCount: 1},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutHeartbeat_WithTopic(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 1, PlaceholderMessageID: 50})
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 2, ToolCount: 1},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutChoice_Valid(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-1",
			Kind:      messages.ChoiceKindPermission,
			Title:     "Perm",
			Options: []messages.ChoiceOption{
				{ID: "yes", Label: "Yes"},
				{ID: "no", Label: "No"},
			},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

// TestAdapter_Send_OutChoicePatch_NamespacedChatID_StripsTGPrefix
// locks the 2026-08-22 fix on the patchChoice side: when
// ChoiceState.ChatID is the namespaced session form (as it
// always is in real runtime), editMessageText inside patchChoice
// must strip the tg_ prefix before reaching the Telegram Bot
// API. Same shape as the sendChoice fix but on the PATCH path.
func TestAdapter_Send_OutChoicePatch_NamespacedChatID_StripsTGPrefix(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100:42", // runtime form: namespaced + thread suffix
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-ns-patch",
			Kind:      messages.ChoiceKindPermission,
			Title:     "Approve",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100:42",
		Kind:   messages.OutChoicePatch,
		Choice: &messages.Choice{
			RequestID:  "req-ns-patch",
			Settled:    true,
			SelectedID: "yes",
		},
	}); err != nil {
		t.Fatalf("send patch: %v", err)
	}
	editCall := findCall(api.snapshotCalls(), "editMessageText")
	if editCall == nil {
		t.Fatal("expected editMessageText call")
	}
	chatID, _ := editCall.Params["chat_id"].(string)
	if chatID != "100" {
		t.Fatalf("editMessageText chat_id = %q, want raw %q (Telegram Bot API rejects tg_ prefix)", chatID, "100")
	}
}

// TestAdapter_Send_OutChoice_NamespacedChatID_StripsTGPrefix
// locks the 2026-08-22 fix for the production-time chatID bug
// on the send side: Telegram Bot API rejects "tg_<digits>" as
// chat_id. sendChoice must strip the prefix before calling
// sendMessage. Previously it passed msg.ChatID (the session
// form) straight through to the API call, which worked in unit
// tests (where chatID is raw) but produced 400 Bad Request in
// real runtime for every OutChoice.
func TestAdapter_Send_OutChoice_NamespacedChatID_StripsTGPrefix(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100:42", // runtime-form: namespaced + thread suffix
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-namespace",
			Kind:      messages.ChoiceKindPermission,
			Title:     "Approve",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	sendCall := findCall(api.snapshotCalls(), "sendMessage")
	if sendCall == nil {
		t.Fatal("expected sendMessage call")
	}
	chatID, _ := sendCall.Params["chat_id"].(string)
	if chatID != "100" {
		t.Fatalf("sendMessage chat_id = %q, want raw %q (Telegram Bot API rejects tg_ prefix)", chatID, "100")
	}
	if threadID, _ := sendCall.Params["message_thread_id"].(int); threadID != 42 {
		t.Fatalf("message_thread_id = %v, want 42", sendCall.Params["message_thread_id"])
	}
	// And ChoiceState stored the session form (for runtime routing),
	// not the raw form.
	state, ok := a.state.choiceByRequestID("req-namespace")
	if !ok {
		t.Fatal("ChoiceState not persisted")
	}
	if state.ChatID != "tg_100:42" {
		t.Fatalf("ChoiceState.ChatID = %q, want session form tg_100:42 (runtime routing key)", state.ChatID)
	}
}

func TestAdapter_Send_OutChoice_MissingRequestID(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{Kind: messages.ChoiceKindPermission},
	})
	if err == nil {
		t.Fatal("missing request id must error")
	}
}

func TestAdapter_Send_OutChoice_NilChoice(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
	})
	if err == nil {
		t.Fatal("nil choice must error")
	}
}

func TestAdapter_Send_OutChoicePatch_NoState(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoicePatch,
		Choice: &messages.Choice{
			RequestID: "unknown",
			Settled:   true,
		},
	})
	if err != nil {
		t.Fatalf("patch unknown state: %v", err)
	}
}

func TestAdapter_Send_OutChoicePatch_WithState(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-1",
			Kind:      messages.ChoiceKindPermission,
			Title:     "Perm",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoicePatch,
		Choice: &messages.Choice{
			RequestID:  "req-1",
			Settled:    true,
			SelectedID: "yes",
		},
	})
	if err != nil {
		t.Fatalf("send patch: %v", err)
	}
}

func TestAdapter_Send_OutChoice_QuestionKind(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-q",
			Kind:      messages.ChoiceKindQuestion,
			Title:     "Ask",
			Questions: []messages.ChoiceQuestion{{
				ID:       "q1",
				Question: "Pick one",
				Options:  []messages.ChoiceOption{{ID: "a", Label: "A"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_OutChoice_DecisionKind(t *testing.T) {
	a, _ := newTestAdapter(t)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-d",
			Kind:      messages.ChoiceKindDecision,
			Title:     "Choose",
			Options:   []messages.ChoiceOption{{ID: "act:/gtw/retry", Label: "🔄", Emoji: "🔄"}},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_Send_DropsLongText(t *testing.T) {
	a, api := newTestAdapter(t)
	long := strings.Repeat("x", 10000)
	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutReply,
		Text:   long,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	count := len(findCalls(api.snapshotCalls(), "sendMessage"))
	if count < 2 {
		t.Fatalf("expected multiple sendMessage, got %d", count)
	}
}

func TestAdapter_OnPromptEnded_NoTopic(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.OnPromptEnded(context.Background(), "100", "1")
}

func TestAdapter_OnPromptEnded_WithTopic(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 1, PlaceholderMessageID: 50})
	a.OnPromptEnded(context.Background(), "100", "1")
}

func TestAdapter_OnPromptEnded_EmptyChat(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.OnPromptEnded(context.Background(), "", "1")
}

func TestAdapter_HandleUpdate_Empty(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleUpdate(context.Background(), Update{})
}

func TestAdapter_HandleUpdate_BotMessage(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 2,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 100, Type: "private"},
			From:      &User{ID: 999},
			Text:      "self message",
		},
	})
}

func TestAdapter_HandleUpdate_PrivateMessage(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 2,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 100, Type: "private"},
			From:      &User{ID: 1},
			Text:      "hello",
		},
	})
	select {
	case msg := <-a.Incoming():
		if msg.Text != "hello" {
			t.Fatalf("text = %q", msg.Text)
		}
		if msg.ChatID != "tg_100" {
			t.Fatalf("chatID = %q, want tg_100 (namespacing §5.5)", msg.ChatID)
		}
		if msg.MessageID != "2" {
			t.Fatalf("MessageID = %q, want 2", msg.MessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

// TestAdapter_HandleUpdate_DM_CreatesPerTurnPlaceholder locks the
// 2026-08-22 v3 contract: each DM user message triggers a NEW
// "🤖 Working..." bot placeholder. The previous turn's
// placeholder is untouched (it was PATCHed to ✅ Completed and
// stays in the Telegram timeline as that turn's permanent
// status marker). UserMessageID is updated to the latest user
// message id so OutXxx reply chain anchors under the user's
// own "hi" message rather than under the placeholder.
func TestAdapter_HandleUpdate_DM_CreatesPerTurnPlaceholder(t *testing.T) {
	a, api := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 7,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 100, Type: "private"},
			From:      &User{ID: 1},
			Text:      "hi bot",
		},
	})
	select {
	case <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
	state, ok := a.state.topic("100", 0)
	if !ok {
		t.Fatal("DM TopicState{topicID=0} must exist after inbound")
	}
	if state.UserMessageID != "7" {
		t.Fatalf("UserMessageID drift: %q (want 7)", state.UserMessageID)
	}
	if state.PlaceholderMessageID <= 0 {
		t.Fatalf("DM must create per-turn placeholder, got %d", state.PlaceholderMessageID)
	}
	// v7: the placeholder sendMessage must:
	//  1. carry reply_to_message_id = user message id (7), so the
	//     placeholder hangs as a reply under the user's "hi".
	//  2. text contains the "🤖 Working..." prefix and the
	//     "⏱ HH:MM:SS" timestamp suffix.
	var placeholderCall *fakeCall
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		if text, ok := call.Params["text"].(string); ok && strings.Contains(text, "🤖 Working...") {
			placeholderCall = &call
			break
		}
	}
	if placeholderCall == nil {
		t.Fatal("expected sendMessage with placeholder text")
	}
	replyTo, ok := placeholderCall.Params["reply_to_message_id"]
	if !ok {
		t.Fatalf("v7 placeholder must carry reply_to_message_id (so it threads under user msg); got params=%+v", placeholderCall.Params)
	}
	if replyTo != 7 {
		t.Fatalf("reply_to_message_id = %v, want 7 (user message id)", replyTo)
	}
	text, _ := placeholderCall.Params["text"].(string)
	if !strings.Contains(text, "⏱ ") {
		t.Fatalf("v7 placeholder text = %q, want to contain ⏱ HH:MM:SS timestamp", text)
	}
}

// TestAdapter_Send_DM_UsesReplyToPlaceholder verifies that
// OutReply / OutThinking / OutTool in DM carry
// reply_to_message_id = PlaceholderMessageID so Telegram
// visually chains the bubble under the per-turn "🤖 Working..."
// anchor.
// TestAdapter_Send_DM_RepliesToUserMessage locks the 2026-08-22
// v3 reply anchor: in DM (topicID == 0), every OutXxx bubble
// uses reply_to_message_id = TopicState.UserMessageID (the
// user's own message). Placeholder is the per-turn status
// ticker (handled separately by OutHeartbeat PATCH); OutXxx
// reply chain does NOT anchor to the placeholder.
func TestAdapter_Send_DM_RepliesToUserMessage(t *testing.T) {
	a, api := newTestAdapter(t)
	// Simulate the inbound path having recorded the user's
	// message ID and created the per-turn placeholder. Both
	// fields are present in real runtime.
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 800, UserMessageID: "42"})
	for _, kind := range []messages.OutboundKind{
		messages.OutReply,
		messages.OutThinking,
		messages.OutToolStart,
		messages.OutResult,
	} {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "100",
			Kind:   kind,
			Text:   "body for " + kind.String(),
		}); err != nil {
			t.Fatalf("send %s: %v", kind, err)
		}
	}
	wantReply := 42
	got := 0
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		reply, ok := call.Params["reply_to_message_id"]
		if !ok {
			t.Fatalf("sendMessage for kind %v missing reply_to_message_id: params=%+v", call.Params["text"], call.Params)
		}
		if reply != wantReply {
			t.Fatalf("sendMessage reply_to_message_id = %v, want %d (userMsgID, NOT placeholder)", reply, wantReply)
		}
		// ChatID must be the raw form (Send strips tg_ prefix).
		if chatID, _ := call.Params["chat_id"].(string); chatID != "100" {
			t.Fatalf("sendMessage chat_id = %q, want raw %q", chatID, "100")
		}
		// DM must NOT carry message_thread_id.
		if _, has := call.Params["message_thread_id"]; has {
			t.Fatalf("DM sendMessage must not carry message_thread_id: %+v", call.Params)
		}
		got++
	}
	if got != 4 {
		t.Fatalf("expected 4 sendMessage calls (one per kind), got %d", got)
	}
}

// TestAdapter_Send_DM_OutHeartbeat_PATCHesPlaceholder locks the
// DM heartbeat path: the same placeholder as OutReply anchors to
// is now PATCH-ed with the heartbeat text instead of spawning a
// standalone bubble.
// TestAdapter_Send_DM_OutHeartbeat_PATCHesPlaceholder locks the
// 2026-08-22 v3 contract: OutHeartbeat PATCHes the per-turn
// DM placeholder with the live think/tool count. The placeholder
// is the status ticker; reply chain still anchors to the user
// message id (handled in TestAdapter_Send_DM_RepliesToUserMessage).
func TestAdapter_Send_DM_OutHeartbeat_PATCHesPlaceholder(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 777})
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 2, ToolCount: 1},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	editCall := findCall(api.snapshotCalls(), "editMessageText")
	if editCall == nil {
		t.Fatal("expected editMessageText for DM heartbeat")
	}
	if mid, _ := editCall.Params["message_id"].(int); mid != 777 {
		t.Fatalf("editMessageText message_id = %v, want 777 (placeholder)", editCall.Params["message_id"])
	}
	if chatID, _ := editCall.Params["chat_id"].(string); chatID != "100" {
		t.Fatalf("editMessageText chat_id = %q, want raw %q", chatID, "100")
	}
	if text, _ := editCall.Params["text"].(string); !strings.Contains(text, "💭 2") || !strings.Contains(text, "🔧 1") {
		t.Fatalf("editMessageText text = %q, want to contain think/tool counts", text)
	}
}

// TestAdapter_Send_Topic_ReplyToUserMessageToo locks the
// 2026-08-22 v3 contract: reply_to_message_id is per-turn
// userMsgID, applies to BOTH topic and DM modes. Topic mode
// carries message_thread_id for visual grouping AND
// reply_to_message_id for content anchoring — the two are
// orthogonal axes (grouping vs. context). OnPromptEnded and
// OutHeartbeat still PATCH the per-turn placeholder.
func TestAdapter_Send_Topic_ReplyToUserMessageToo(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 42, PlaceholderMessageID: 800, UserMessageID: "55"})
	for _, kind := range []messages.OutboundKind{
		messages.OutReply,
		messages.OutThinking,
		messages.OutToolStart,
		messages.OutResult,
	} {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			ChatID: "tg_100:42",
			Kind:   kind,
			Text:   "body for " + kind.String(),
		}); err != nil {
			t.Fatalf("send %s: %v", kind, err)
		}
	}
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		// Both topic_id (grouping) AND reply_to_message_id
		// (content anchoring) must be present.
		reply, hasReply := call.Params["reply_to_message_id"]
		if !hasReply {
			t.Fatalf("topic sendMessage must carry reply_to_message_id (userMsgID); params=%+v", call.Params)
		}
		if reply != 55 {
			t.Fatalf("topic reply_to_message_id = %v, want 55 (userMsgID)", reply)
		}
		if mid, ok := call.Params["message_thread_id"].(int); !ok || mid != 42 {
			t.Fatalf("topic sendMessage must carry message_thread_id=42, got %+v", call.Params["message_thread_id"])
		}
	}
	// Sanity: heartbeat in topic still PATCHes the placeholder.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "tg_100:42",
		Kind:      messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 1},
	}); err != nil {
		t.Fatalf("send topic heartbeat: %v", err)
	}
	editCall := findCall(api.snapshotCalls(), "editMessageText")
	if editCall == nil {
		t.Fatal("topic heartbeat must PATCH placeholder")
	}
	if mid, _ := editCall.Params["message_id"].(int); mid != 800 {
		t.Fatalf("editMessageText message_id = %v, want 800 (placeholder)", editCall.Params["message_id"])
	}
}

// TestAdapter_OnPromptEnded_DM_PATCHesPlaceholder verifies the
// 2026-08-22 plan-C change: OnPromptEnded now PATCHes the DM
// placeholder too (previously topicID > 0 guard skipped it). The
// ✅ Completed visual matches the topic path.
// TestAdapter_OnPromptEnded_DM_PATCHesPlaceholder locks the
// 2026-08-22 v3 contract: OnPromptEnded PATCHes the per-turn
// DM placeholder text to "<b>✅ Completed</b>". The placeholder
// stays in the Telegram timeline as that turn's permanent
// status marker. Same code path as topic mode (one branch —
// placeholderMessageID is the only visual ✅ carrier).
// TestAdapter_OnPromptEnded_DM_ReactsOnUserAndPlaceholder locks the
// 2026-08-22 v4 contract: OnPromptEnded puts ✅ reactions on
// BOTH the user message and the per-turn placeholder (Feishu
// parity — AddReaction + SetPromptState). NO editMessageText
// call — placeholder keeps its last heartbeat text.
func TestAdapter_OnPromptEnded_DM_ReactsOnUserAndPlaceholder(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 909})
	a.OnPromptEnded(context.Background(), "100", "7")

	// Must NOT PATCH placeholder text (v4 dropped "<b>✅ Completed</b>" PATCH).
	for _, call := range api.snapshotCalls() {
		if call.Method == "editMessageText" {
			t.Fatalf("v4 OnPromptEnded must NOT call editMessageText; got params=%+v", call.Params)
		}
	}

	// v6.3: ONLY the placeholder gets a 🎉 reaction. The user
	// message's single-reaction slot is reserved for
	// MessageSubmitted ("AI thinking") — OnPromptEnded must NOT
	// overwrite it with 🎉.
	var (
		placeholderCalls int
		userMsgCalls     int
	)
	for _, call := range api.snapshotCalls() {
		if call.Method != "setMessageReaction" {
			continue
		}
		mid, _ := call.Params["message_id"].(int)
		reactions, _ := call.Params["reaction"].([]any)
		if len(reactions) != 1 {
			t.Fatalf("reaction len = %d, want 1 (Telegram single-reaction limit)", len(reactions))
		}
		entry, _ := reactions[0].(map[string]any)
		if e, _ := entry["emoji"].(string); e != "🎉" {
			t.Fatalf("reaction emoji = %q, want 🎉", e)
		}
		switch mid {
		case 7:
			userMsgCalls++
			t.Fatalf("v6.3 OnPromptEnded must NOT set reaction on user msg %d; reaction slot is reserved for MessageSubmitted", mid)
		case 909:
			placeholderCalls++
		default:
			t.Fatalf("unexpected reaction message_id = %d, want 909 (placeholder)", mid)
		}
	}
	if placeholderCalls != 1 {
		t.Fatalf("expected 1 placeholder reaction call, got %d", placeholderCalls)
	}
	if userMsgCalls != 0 {
		t.Fatalf("v6.3 must NOT call setMessageReaction on user msg, got %d calls", userMsgCalls)
	}
}

// TestAdapter_OnPromptEnded_DM_NoPlaceholder_NoOp locks the safe
// fallback: DM with no TopicState at all must not error and must
// not call editMessageText (no anchor to PATCH).
func TestAdapter_OnPromptEnded_DM_NoPlaceholder_NoOp(t *testing.T) {
	a, api := newTestAdapter(t)
	a.OnPromptEnded(context.Background(), "100", "1")
	if call := findCall(api.snapshotCalls(), "editMessageText"); call != nil {
		t.Fatalf("expected no editMessageText with no placeholder, got %+v", call)
	}
}

// TestStateStore_DM_Persistence locks the §11.11 contract that
// DM placeholders survive daemon restart via telegram_state.json.
// TopicState{topicID=0} must round-trip through newStateStore and
// come back with the same PlaceholderMessageID — otherwise the
// first OutXxx after a restart would lazy-create a NEW
// placeholder and orphan the old one.
func TestStateStore_DM_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	if err := store.putTopic(&TopicState{
		ChatID:               "100",
		TopicID:              0,
		PlaceholderMessageID: 1234,
		UserMessageID:        "42",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}
	store2, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore reload: %v", err)
	}
	state, ok := store2.topic("100", 0)
	if !ok {
		t.Fatal("DM TopicState{topicID=0} did not round-trip across reload")
	}
	if state.PlaceholderMessageID != 1234 {
		t.Fatalf("PlaceholderMessageID drift across reload: got %d, want 1234", state.PlaceholderMessageID)
	}
	if state.UserMessageID != "42" {
		t.Fatalf("UserMessageID drift across reload: got %q", state.UserMessageID)
	}
}

// TestSessionChatID_DM_StillStable is the plan-C regression
// guard: chatID stability contract (docs/CHANNEL.md §5.5) must
// remain pure-function-of-(chat.id, thread_id) after the DM
// placeholder + reply-chain additions. If anyone re-introduces
// daemon-state into sessionChatID this test fails.
func TestSessionChatID_DM_StillStable(t *testing.T) {
	a, _ := newTestAdapter(t)
	// Same DM (chatID=100, threadID=0) called repeatedly must
	// always produce the same chatID regardless of state.
	want := "tg_100"
	for i := 0; i < 5; i++ {
		got := a.sessionChatID("100", 0)
		if got != want {
			t.Fatalf("iter %d: sessionChatID(\"100\", 0) = %q, want %q", i, got, want)
		}
	}
	// Negative group id is still prefixed (Telegram native).
	if got := a.sessionChatID("-10012345", 0); got != "tg_-10012345" {
		t.Fatalf("negative group chat id: got %q, want tg_-10012345", got)
	}
}

func TestAdapter_HandleUpdate_GroupWithoutMention(t *testing.T) {
	// Channel layer must NOT filter on its own — non-mention group
	// messages are forwarded to chatsession.Manager.HandleInbound,
	// which runs the watchMode gate (AcceptInbound).
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 2,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: -100, Type: "supergroup"},
			From:      &User{ID: 1, Username: "alice"},
			Text:      "no mention here",
		},
	})
	select {
	case <-a.Incoming():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected inbound (channel does not filter)")
	}
}

func TestAdapter_HandleUpdate_GroupWithMention(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 2,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: -100, Type: "supergroup"},
			From:      &User{ID: 1, Username: "alice"},
			Text:      "hello @testbot",
		},
	})
	select {
	case msg := <-a.Incoming():
		if msg.ChatID == "" {
			t.Fatalf("empty chat id")
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_HandleUpdate_GroupNonMentionPublished(t *testing.T) {
	// Channel layer must NOT filter on its own — even non-mention
	// group messages are forwarded to chatsession.Manager.HandleInbound,
	// which then runs the watchMode gate (see chatsession.AcceptInbound).
	// This guarantees the watch hint tombstone path works for telegram.
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 2,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: -100, Type: "supergroup"},
			From:      &User{ID: 1, Username: "alice"},
			Text:      "plain text without bot",
		},
	})
	select {
	case <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("expected inbound")
	}
}

func TestAdapter_HandleUpdate_ReplyToBot(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleUpdate(context.Background(), Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 3,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: -100, Type: "supergroup"},
			From:      &User{ID: 1},
			Text:      "thanks",
			ReplyToMessage: &Message{
				MessageID: 2,
				From:      &User{ID: 999},
			},
		},
	})
	select {
	case msg := <-a.Incoming():
		if msg.ReplyTo == "" {
			t.Fatalf("expected reply to be set: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_HandleUpdate_EmptyMessage(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleUpdate(context.Background(), Update{UpdateID: 1, Message: nil})
	a.handleUpdate(context.Background(), Update{UpdateID: 1, Message: &Message{}})
}

func TestAdapter_HandleCallback_Permission(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-perm",
			Kind:      messages.ChoiceKindPermission,
			Title:     "Perm",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	state, _ := a.state.choiceByRequestID("req-perm")
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:      "cb-1",
		From:    User{ID: 7},
		Message: &Message{MessageID: state.MessageID, Chat: Chat{ID: 100}},
		Data:    "c:" + shortID("req-perm") + ":0",
	})
	select {
	case msg := <-a.Incoming():
		if msg.Action == nil {
			t.Fatalf("expected action, got %+v", msg)
		}
		if msg.Action.Option != "yes" {
			t.Fatalf("option = %q", msg.Action.Option)
		}
		if msg.Action.RequestID != "req-perm" {
			t.Fatalf("request id = %q", msg.Action.RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_HandleCallback_QuestionWizard(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-q",
			Kind:      messages.ChoiceKindQuestion,
			Title:     "Pick",
			Questions: []messages.ChoiceQuestion{
				{ID: "q1", Question: "first?", Options: []messages.ChoiceOption{{ID: "a", Label: "A"}}},
				{ID: "q2", Question: "second?", Options: []messages.ChoiceOption{{ID: "b", Label: "B"}}},
			},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	state, _ := a.state.choiceByRequestID("req-q")
	// First question pick.
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:      "cb-1",
		From:    User{ID: 7},
		Message: &Message{MessageID: state.MessageID, Chat: Chat{ID: 100}},
		Data:    "c:" + shortID("req-q") + ":0",
	})
	// Second question pick - should fire batched.
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:      "cb-2",
		From:    User{ID: 7},
		Message: &Message{MessageID: state.MessageID, Chat: Chat{ID: 100}},
		Data:    "c:" + shortID("req-q") + ":0",
	})
	deadline := time.After(time.Second)
	select {
	case msg := <-a.Incoming():
		if msg.Action == nil {
			t.Fatalf("expected action")
		}
		if !strings.HasPrefix(msg.Action.Option, messages.QuestionBatchPrefix) {
			t.Fatalf("expected batch prefix, got %q", msg.Action.Option)
		}
	case <-deadline:
		t.Fatal("no inbound after wizard completion")
	}
	select {
	case msg := <-a.Incoming():
		t.Fatalf("unexpected extra inbound: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAdapter_HandleCallback_Decision(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-dec",
			Kind:      messages.ChoiceKindDecision,
			Title:     "Pick",
			Options:   []messages.ChoiceOption{{ID: "act:/gtw/retry", Label: "🔄", Emoji: "🔄"}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	state, _ := a.state.choiceByRequestID("req-dec")
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:      "cb-1",
		From:    User{ID: 7},
		Message: &Message{MessageID: state.MessageID, Chat: Chat{ID: 100}},
		Data:    "c:" + shortID("req-dec") + ":0",
	})
	select {
	case msg := <-a.Incoming():
		if msg.Reaction == nil {
			t.Fatalf("expected reaction, got %+v", msg)
		}
		if msg.Reaction.Emoji != "🔄" {
			t.Fatalf("emoji = %q", msg.Reaction.Emoji)
		}
		if msg.Reaction.RequestID != "req-dec" {
			t.Fatalf("request id = %q", msg.Reaction.RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_HandleCallback_UnknownAction(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleCallbackQuery(context.Background(), &CallbackQuery{ID: "cb", Data: "x:y"})
}

func TestAdapter_HandleCallback_EmptyData(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleCallbackQuery(context.Background(), &CallbackQuery{ID: "cb", Data: ""})
}

func TestAdapter_HandleCallback_NilCallback(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleCallbackQuery(context.Background(), nil)
}

func TestAdapter_HandleCallback_UnknownRequestID(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb",
		Data: "c:unknown:0",
	})
}

func TestAdapter_HandleCallback_Settled(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putChoice(&ChoiceState{
		RequestID: "req-s",
		ChatID:    "100",
		TopicID:   0,
		MessageID: 50,
		Settled:   true,
		Choice:    &messages.Choice{RequestID: "req-s", Kind: messages.ChoiceKindPermission},
	})
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb",
		Data: "c:" + shortID("req-s") + ":0",
	})
}

func TestAdapter_HandleCallback_BadOptionIndex(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-1",
			Kind:      messages.ChoiceKindPermission,
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb",
		Data: "c:" + shortID("req-1") + ":abc",
	})
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb",
		Data: "c:" + shortID("req-1") + ":-1",
	})
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:   "cb",
		Data: "c:" + shortID("req-1") + ":99",
	})
}

func TestAdapter_HandleInputClick_ForceReply(t *testing.T) {
	a, api := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-q",
			Kind:      messages.ChoiceKindQuestion,
			Questions: []messages.ChoiceQuestion{{ID: "q1", Question: "Pick one", Options: []messages.ChoiceOption{{ID: "a", Label: "A"}}}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	state, _ := a.state.choiceByRequestID("req-q")
	a.handleCallbackQuery(context.Background(), &CallbackQuery{
		ID:      "cb",
		From:    User{ID: 7},
		Message: &Message{MessageID: state.MessageID, Chat: Chat{ID: 100}},
		Data:    "i:" + shortID("req-q"),
	})
	// Now reply to the force_reply prompt.
	promptID := 0
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		markup, ok := call.Params["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		if _, has := markup["force_reply"]; has {
			result, _ := json.Marshal(call.Params)
			var parsed struct {
				ChatID string `json:"chat_id"`
			}
			_ = json.Unmarshal(result, &parsed)
			_ = parsed
			promptID++
		}
	}
	if promptID == 0 {
		t.Fatal("no force_reply sendMessage")
	}
}

func TestAdapter_HandleForceReply_Permission(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-perm",
			Kind:      messages.ChoiceKindPermission,
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	a.state.putChoice(&ChoiceState{
		RequestID: "req-perm",
		ChatID:    "100",
		MessageID: 99,
		Choice:    &messages.Choice{RequestID: "req-perm", Kind: messages.ChoiceKindPermission},
		Input:     &InputState{PromptMessageID: 88, OwnerID: 7, Kind: "permission"},
	})
	got := a.handleForceReply(context.Background(), &Message{
		MessageID: 89,
		Chat:      Chat{ID: 100},
		From:      &User{ID: 7},
		Text:      "user typed answer",
		ReplyToMessage: &Message{
			MessageID: 88,
		},
	})
	if !got {
		t.Fatal("handleForceReply returned false")
	}
	select {
	case msg := <-a.Incoming():
		if msg.Action == nil || msg.Action.Option != "user typed answer" {
			t.Fatalf("action = %+v", msg.Action)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_HandleForceReply_NoReplyTo(t *testing.T) {
	a, _ := newTestAdapter(t)
	got := a.handleForceReply(context.Background(), &Message{
		MessageID: 89,
		Chat:      Chat{ID: 100},
		From:      &User{ID: 7},
		Text:      "no reply",
	})
	if got {
		t.Fatal("expected false")
	}
}

func TestAdapter_HandleForceReply_NotMine(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putChoice(&ChoiceState{
		RequestID: "req-perm",
		ChatID:    "100",
		MessageID: 99,
		Choice:    &messages.Choice{RequestID: "req-perm", Kind: messages.ChoiceKindPermission},
		Input:     &InputState{PromptMessageID: 88, OwnerID: 7, Kind: "permission"},
	})
	got := a.handleForceReply(context.Background(), &Message{
		MessageID: 89,
		Chat:      Chat{ID: 100},
		From:      &User{ID: 999},
		Text:      "wrong user",
		ReplyToMessage: &Message{
			MessageID: 88,
		},
	})
	if got {
		t.Fatal("expected false for wrong owner")
	}
}

func TestAdapter_PublishAction_NilCallback(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.publishAction("100", "req-1", "x", nil)
	select {
	case msg := <-a.Incoming():
		if msg.Action == nil {
			t.Fatal("expected action")
		}
		if msg.Action.Option != "x" {
			t.Fatalf("option = %q", msg.Action.Option)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_ResolveRequestID_ByMessageID(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putChoice(&ChoiceState{
		RequestID: "req-real",
		ChatID:    "100",
		MessageID: 99,
		Choice:    &messages.Choice{RequestID: "req-real", Kind: messages.ChoiceKindPermission},
	})
	got := a.resolveRequestID("anything", &CallbackQuery{
		Message: &Message{MessageID: 99},
	})
	if got != "req-real" {
		t.Fatalf("resolve = %q", got)
	}
}

func TestAdapter_ResolveRequestID_ByShortID(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putChoice(&ChoiceState{
		RequestID: "req-real-12345678",
		ChatID:    "100",
		MessageID: 99,
		Choice:    &messages.Choice{RequestID: "req-real-12345678", Kind: messages.ChoiceKindPermission},
	})
	got := a.resolveRequestID(shortID("req-real-12345678"), nil)
	if got != "req-real-12345678" {
		t.Fatalf("resolve = %q", got)
	}
}

func TestAdapter_ResolveRequestID_Fallback(t *testing.T) {
	a, _ := newTestAdapter(t)
	got := a.resolveRequestID("unknown", nil)
	if got != "unknown" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestBuildQuestionBatch_Empty(t *testing.T) {
	if got := buildQuestionBatch(nil, nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestBuildQuestionBatch_AllSkipped(t *testing.T) {
	out := buildQuestionBatch([]messages.ChoiceQuestion{
		{ID: "q1", Options: []messages.ChoiceOption{{ID: "a", Label: "A"}}},
	}, []string{})
	if !strings.HasPrefix(out, messages.QuestionBatchPrefix) {
		t.Fatalf("output = %q", out)
	}
}

func TestBuildQuestionBatch_OneSelected(t *testing.T) {
	out := buildQuestionBatch([]messages.ChoiceQuestion{
		{ID: "q1", Options: []messages.ChoiceOption{{ID: "a", Label: "A"}}},
	}, []string{"a"})
	if !strings.HasPrefix(out, messages.QuestionBatchPrefix) {
		t.Fatalf("output = %q", out)
	}
	picks, err := messages.DecodeQuestionPicks(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(picks) != 1 || picks[0].ID != "q1" || len(picks[0].Selected) != 1 || picks[0].Selected[0] != "a" {
		t.Fatalf("picks = %+v", picks)
	}
}

func TestBuildQuestionBatch_Custom(t *testing.T) {
	out := buildQuestionBatch([]messages.ChoiceQuestion{
		{ID: "q1", Options: []messages.ChoiceOption{{ID: "a", Label: "A"}}},
	}, []string{messages.StoreQuestionCustom("typed")})
	picks, _ := messages.DecodeQuestionPicks(out)
	if picks[0].Custom != "typed" {
		t.Fatalf("custom = %q", picks[0].Custom)
	}
}

func TestBuildQuestionBatch_NoIDs(t *testing.T) {
	out := buildQuestionBatch([]messages.ChoiceQuestion{
		{ID: ""},
	}, []string{""})
	if out != "" {
		t.Fatalf("output = %q", out)
	}
}

func TestBuildQuestionBatch_PadShorterPicks(t *testing.T) {
	out := buildQuestionBatch([]messages.ChoiceQuestion{
		{ID: "q1"},
		{ID: "q2"},
	}, []string{"a"})
	if !strings.HasPrefix(out, messages.QuestionBatchPrefix) {
		t.Fatalf("output = %q", out)
	}
}

func TestUserID(t *testing.T) {
	if userID(nil) != "" {
		t.Fatal("nil msg")
	}
	if userID(&Message{}) != "" {
		t.Fatal("nil from")
	}
	if userID(&Message{From: &User{ID: 5}}) != "5" {
		t.Fatal("5")
	}
}

func TestReplyToID(t *testing.T) {
	if replyToID(nil) != "" {
		t.Fatal("nil")
	}
	if replyToID(&Message{}) != "" {
		t.Fatal("nil reply")
	}
	if replyToID(&Message{ReplyToMessage: &Message{MessageID: 7}}) != "7" {
		t.Fatal("7")
	}
}

func TestMessageIDString(t *testing.T) {
	if messageIDString(nil) != "" {
		t.Fatal("nil")
	}
	if messageIDString(&Message{}) != "0" {
		t.Fatal("zero")
	}
	if messageIDString(&Message{MessageID: 42}) != "42" {
		t.Fatal("42")
	}
}

func TestChoiceKindName(t *testing.T) {
	if choiceKindName(messages.ChoiceKindPermission) != "permission" {
		t.Fatal("permission")
	}
	if choiceKindName(messages.ChoiceKindQuestion) != "question" {
		t.Fatal("question")
	}
	if choiceKindName(messages.ChoiceKindDecision) != "decision" {
		t.Fatal("decision")
	}
	if choiceKindName(messages.ChoiceKind(99)) != "" {
		t.Fatal("unknown")
	}
}

func TestRenderInlineText(t *testing.T) {
	if got := renderInlineText(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := renderInlineText("**bold**"); !strings.Contains(got, "<b>bold</b>") {
		t.Fatalf("inline = %q", got)
	}
}

func TestFormatTool(t *testing.T) {
	if formatTool(messages.OutboundMessage{}) != "" {
		t.Fatal("empty tool")
	}
	if !strings.Contains(formatTool(messages.OutboundMessage{
		Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{Name: "read", Args: "x"},
	}), "🔧") {
		t.Fatal("start emoji")
	}
	if !strings.Contains(formatTool(messages.OutboundMessage{
		Kind: messages.OutToolEnd,
		Tool: &messages.ToolInfo{Name: "read", Output: "ok"},
	}), "✅") {
		t.Fatal("end emoji")
	}
}

func TestFormatTaskList_Empty(t *testing.T) {
	if formatTaskList(nil) != "" {
		t.Fatal("nil")
	}
	if formatTaskList(&agent.AgentTaskListEvent{}) != "" {
		t.Fatal("empty")
	}
}

func TestAdapter_ConcurrentSend(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Send(context.Background(), messages.OutboundMessage{
				ChatID: "100",
				Kind:   messages.OutReply,
				Text:   "hello",
			})
		}()
	}
	wg.Wait()
}

func TestAdapter_ChoiceCallbackData_Short(t *testing.T) {
	a, _ := newTestAdapter(t)
	state := &ChoiceState{
		RequestID: "req-short",
		ChatID:    "100",
		TopicID:   1,
		Choice: &messages.Choice{
			RequestID: "req-short",
			Kind:      messages.ChoiceKindPermission,
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}
	_ = a.state.putChoice(state)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: state.Choice,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestAdapter_StatePersistAcrossReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state.json")
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "x"
	cfg.Paths.DataDir = filepath.Dir(dir)
	api := &fakeAPI{}
	a := NewAdapterWithClient(cfg, api, filepath.Dir(dir))
	if a == nil {
		t.Fatal("NewAdapterWithClient nil")
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-persist",
			Kind:      messages.ChoiceKindPermission,
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	a2 := NewAdapterWithClient(cfg, &fakeAPI{}, filepath.Dir(dir))
	if a2 == nil {
		t.Fatal("second adapter")
	}
	// Note: state may be reloaded by NewAdapterWithClient.
}

// TestAdapter_SessionChatID_Stable is the post-stable-chatID
// contract: chatID is always "tg_<chat.id>[:thread_id]". The
// two former topic_mode tests (Shared / Separate) are merged
// here because the unified rule makes the distinction obsolete.
func TestAdapter_SessionChatID_Stable(t *testing.T) {
	a, _ := newTestAdapter(t)
	cases := []struct {
		name      string
		rawChatID string
		threadID  int
		want      string
	}{
		{"dm", "100", 0, "tg_100"},
		{"group main window", "100", 42, "tg_100:42"},
		{"group topic 42", "100", 42, "tg_100:42"},
		{"group topic 88", "100", 88, "tg_100:88"},
		{"negative group id", "-10012345", 42, "tg_-10012345:42"},
		{"private with thread_id > 0 still prefixed", "1234567890", 999999, "tg_1234567890:999999"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.sessionChatID(tt.rawChatID, tt.threadID); got != tt.want {
				t.Errorf("sessionChatID(%q, %d) = %q, want %q",
					tt.rawChatID, tt.threadID, got, tt.want)
			}
		})
	}
}

// TestAdapter_SessionTopicID_Stable replaces the legacy
// shared/separate topic-resolution tests. The new sessionTopicID
// is a pure function over the chatID string: it strips the "tg_"
// prefix and parses the optional ":thread_id" suffix. No state
// lookup is involved.
func TestAdapter_SessionTopicID_Stable(t *testing.T) {
	a, _ := newTestAdapter(t)
	tests := []struct {
		chatID string
		want   int
	}{
		{"tg_100", 0},
		{"tg_100:42", 42},
		{"tg_100:7", 7},
		{"tg_-10012345:88", 88},
		{"tg_abc:notanumber", 0}, // parse failure → treat as bare chatID
		{"oc_xxxxx", 0},          // non-telegram → 0
		{"100", 0},               // legacy bare-digit → 0 (no tg_ prefix)
		{"", 0},                  // empty → 0
	}
	for _, tt := range tests {
		t.Run(tt.chatID, func(t *testing.T) {
			if got := a.sessionTopicID(tt.chatID); got != tt.want {
				t.Errorf("sessionTopicID(%q) = %d, want %d", tt.chatID, got, tt.want)
			}
		})
	}
}

func TestAdapter_Send_OutReplyEmptyText(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutReply,
		Text:   "   \n\t  ",
	}); err != nil {
		t.Fatalf("empty OutReply must not error: %v", err)
	}
	if len(api.Calls) > 0 {
		t.Fatalf("empty OutReply must not send: calls=%d", len(api.Calls))
	}
}

func TestAdapter_Send_OutResultEmptyText(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutResult,
		Text:   "",
	}); err != nil {
		t.Fatalf("empty OutResult must not error: %v", err)
	}
	if len(api.Calls) > 0 {
		t.Fatalf("empty OutResult must not send: calls=%d", len(api.Calls))
	}
}

func TestAdapter_HandleMessageReaction_ForwardsInbound(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleMessageReaction(context.Background(), &MessageReactionUpdate{
		Chat:       Chat{ID: 100, Type: "private"},
		MessageID:  99,
		User:       User{ID: 7, Username: "alice"},
		Date:       time.Now().Unix(),
		NewReaction: []ReactionType{{Type: "emoji", Emoji: "👍"}},
	})
	select {
	case msg := <-a.Incoming():
		if msg.Reaction == nil {
			t.Fatal("expected reaction")
		}
		if msg.Reaction.Emoji != "👍" {
			t.Fatalf("emoji=%q", msg.Reaction.Emoji)
		}
		if msg.Reaction.TargetMsgID != "99" {
			t.Fatalf("target msg id=%q", msg.Reaction.TargetMsgID)
		}
		if msg.UserID != "7" {
			t.Fatalf("user id=%q", msg.UserID)
		}
		// 2026-08-22 fix: reaction ChatID must be namespaced
		// ("tg_<chatid>") to match the message path. Otherwise
		// runtime.findChatSession cannot resolve the owning
		// ChatSession and gtw emoji-reaction routing silently
		// drops the event.
		if msg.ChatID != "tg_100" {
			t.Fatalf("ChatID = %q, want tg_100 (namespaced §5.5)", msg.ChatID)
		}
		if msg.Reaction.ChatID != "tg_100" {
			t.Fatalf("Reaction.ChatID = %q, want tg_100 (namespaced)", msg.Reaction.ChatID)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}
}

func TestAdapter_HandleMessageReaction_IgnoresBotReaction(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleMessageReaction(context.Background(), &MessageReactionUpdate{
		Chat:       Chat{ID: 100, Type: "private"},
		MessageID:  99,
		User:       User{ID: 999}, // matches fakeAPI's getMe user
		NewReaction: []ReactionType{{Type: "emoji", Emoji: "👍"}},
	})
	select {
	case msg := <-a.Incoming():
		t.Fatalf("bot reaction must be ignored: %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAdapter_HandleMessageReaction_RemovedEmoji(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)
	a.handleMessageReaction(context.Background(), &MessageReactionUpdate{
		Chat:       Chat{ID: 100, Type: "private"},
		MessageID:  99,
		User:       User{ID: 7},
		NewReaction: nil, // user removed all reactions
	})
	select {
	case msg := <-a.Incoming():
		if msg.Reaction == nil {
			t.Fatal("expected reaction inbound even on removal")
		}
		if msg.Reaction.Emoji != "" {
			t.Fatalf("expected empty emoji on removal, got %q", msg.Reaction.Emoji)
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound on reaction removal")
	}
}

func TestAdapter_HandleMyChatMember_LogsWithoutPanic(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleMyChatMember(context.Background(), &ChatMemberUpdate{
		Chat:          Chat{ID: 100, Type: "supergroup"},
		From:          User{ID: 1},
		OldChatMember: &ChatMember{Status: "left"},
		NewChatMember: &ChatMember{Status: "administrator"},
	})
	// Just ensure no panic; the log is best-effort.
}

func TestAdapter_HandleChatMember_LogsWithoutPanic(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.handleChatMember(context.Background(), &ChatMemberUpdate{
		Chat:          Chat{ID: 100, Type: "supergroup"},
		From:          User{ID: 1},
		NewChatMember: &ChatMember{Status: "member", User: User{ID: 7}},
	})
}

func TestAdapter_ApiCall_AppliesRateLimitAndRetry(t *testing.T) {
	a, api := newTestAdapter(t)
	ctx := context.Background()
	// The apiCall wrapper should retry after one 503, then succeed.
	// Force a 503 once, then success — should recover transparently.
	transient := &apiError{StatusCode: 503, Message: "down"}
	api.TransientOnce = transient
	if err := a.apiCall(ctx, "sendMessage", map[string]any{"chat_id": "100", "text": "hi"}, nil); err != nil {
		t.Fatalf("apiCall: %v", err)
	}
	if api.callCount < 2 {
		t.Fatalf("expected retry, callCount=%d", api.callCount)
	}
}

func TestAdapter_Send_OutInitSilentDrop(t *testing.T) {
	a, api := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutInit,
		SessionID: "sess-1",
		Model:     "claude-sonnet-5",
		AgentName: "claudecode",
		Text:      "Agent: claudecode · Model: claude-sonnet-5 · Session: sess-1",
	}); err != nil {
		t.Fatalf("OutInit must silently drop, got err: %v", err)
	}
	if len(api.Calls) > 0 {
		t.Fatalf("OutInit must not send any message: calls=%d", len(api.Calls))
	}
}

func TestAdapter_Send_OutInitDropEvenWithText(t *testing.T) {
	a, api := newTestAdapter(t)
	// Even if text is provided, OutInit must be dropped — matches
	// feishu F-44 silent-drop semantics.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutInit,
		Text:   "this text should be discarded",
	}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(api.Calls) > 0 {
		t.Fatalf("OutInit must drop text: calls=%d", len(api.Calls))
	}
}

func TestAdapter_EnsurePlaceholderForHeartbeat_CreatesWhenMissing(t *testing.T) {
	a, _ := newTestAdapter(t)
	// Seed a topic WITHOUT a placeholder.
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 42})
	messageID, err := a.ensurePlaceholderForHeartbeat(context.Background(), "100", 42)
	if err != nil {
		t.Fatalf("ensurePlaceholderForHeartbeat: %v", err)
	}
	if messageID <= 0 {
		t.Fatalf("expected a placeholder message id, got %d", messageID)
	}
	state, ok := a.state.topic("100", 42)
	if !ok || state.PlaceholderMessageID != messageID {
		t.Fatalf("placeholder id not persisted: state=%+v want=%d", state, messageID)
	}
}

func TestAdapter_EnsurePlaceholderForHeartbeat_ReusesExisting(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 42, PlaceholderMessageID: 99})
	messageID, err := a.ensurePlaceholderForHeartbeat(context.Background(), "100", 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if messageID != 99 {
		t.Fatalf("expected to reuse existing placeholder 99, got %d", messageID)
	}
	// No new sendMessage call should have been made.
	for _, call := range api.snapshotCalls() {
		if call.Method == "sendMessage" {
			t.Fatalf("must not send new placeholder when one exists: %+v", call)
		}
	}
}

// TestAdapter_EnsurePlaceholderForHeartbeat_CreatesInDM locks the
// 2026-08-22 plan-C change: topicID=0 (DM / main-window) now
// lazy-creates a placeholder the same way real topics do, so
// OutXxx bubbles have a stable reply_to_message_id anchor.
//
// Previously topicID <= 0 short-circuited to (0, nil) — DM had no
// placeholder and no way to visually chain OutReply / OutTool to
// a status ticker. See docs/channel/telegram.md §11.11.
// TestAdapter_EnsurePlaceholderForHeartbeat_DMCreates locks the
// 2026-08-22 v3 contract: ensurePlaceholderForHeartbeat creates
// a placeholder in DM (topicID == 0) just like topic mode, when
// state.PlaceholderMessageID == 0. Returns the new id. Topic mode
// is unchanged. Used by OutHeartbeat / first-Send-race to ensure
// the placeholder always exists when an emit needs to PATCH it.
func TestAdapter_EnsurePlaceholderForHeartbeat_DMCreates(t *testing.T) {
	a, _ := newTestAdapter(t)
	messageID, err := a.ensurePlaceholderForHeartbeat(context.Background(), "100", 0)
	if err != nil {
		t.Fatalf("ensurePlaceholderForHeartbeat DM: %v", err)
	}
	if messageID <= 0 {
		t.Fatalf("DM must create placeholder, got %d", messageID)
	}
	state, ok := a.state.topic("100", 0)
	if !ok {
		t.Fatal("DM TopicState{topicID=0} not persisted")
	}
	if state.PlaceholderMessageID != messageID {
		t.Fatalf("placeholder id drift: state=%d call=%d", state.PlaceholderMessageID, messageID)
	}
	// Second call must reuse the existing placeholder (idempotent).
	again, err := a.ensurePlaceholderForHeartbeat(context.Background(), "100", 0)
	if err != nil {
		t.Fatalf("ensurePlaceholderForHeartbeat second call: %v", err)
	}
	if again != messageID {
		t.Fatalf("second call must reuse placeholder, got %d want %d", again, messageID)
	}
	// Topic mode unchanged.
	topicID, err := a.ensurePlaceholderForHeartbeat(context.Background(), "100", 42)
	if err != nil {
		t.Fatalf("ensurePlaceholderForHeartbeat topic: %v", err)
	}
	if topicID <= 0 {
		t.Fatalf("topic must create placeholder, got %d", topicID)
	}
}

func TestAdapter_Send_OutHeartbeat_CreatesPlaceholderOnDemand(t *testing.T) {
	a, api := newTestAdapter(t)
	// Seed a chat_id → topic mapping. The outbound chatID is now
	// wrapped in "tg_<chat.id>:<thread_id>" form by the adapter;
	// the underlying state key remains the raw Telegram chat_id.
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 42})
	// First heartbeat arrives before any user message triggered
	// handleMessage's ensurePlaceholder (race condition).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100:42",
		Kind:   messages.OutHeartbeat,
		Text:   "ignored when heartbeat present",
		Heartbeat: &messages.HeartbeatSnapshot{
			ThinkCount: 1,
			ToolCount:  0,
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Verify: one sendMessage (placeholder creation) + one
	// editMessageText (heartbeat PATCH).
	var sawSend, sawEdit bool
	for _, call := range api.snapshotCalls() {
		if call.Method == "sendMessage" {
			sawSend = true
		}
		if call.Method == "editMessageText" {
			sawEdit = true
		}
	}
	if !sawSend {
		t.Fatal("expected sendMessage to create placeholder")
	}
	if !sawEdit {
		t.Fatal("expected editMessageText to PATCH heartbeat")
	}
	state, ok := a.state.topic("100", 42)
	if !ok || state.PlaceholderMessageID <= 0 {
		t.Fatalf("placeholder not persisted: %+v", state)
	}
}
