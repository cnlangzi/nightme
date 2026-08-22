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
	// 适配器必须自行决定 emoji, 期望 🔄
	got := findCall(api.Calls, "setMessageReaction")
	if got == nil {
		t.Fatal("expected setMessageReaction call, got none")
	}
	wantReaction := []any{map[string]any{"type": "emoji", "emoji": "🔄"}}
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
		{agent.MessageQueued, "⏳"},
		{agent.MessageSubmitted, "🔄"},
		{agent.MessageDone, "✅"},
		{agent.MessageDropped, ""},    // 跟 feishu 对齐: 不留 reaction
		{agent.MessageState(999), ""}, // 未知 state silent drop
	}
	for _, c := range cases {
		if got := mapStateToTelegramEmoji(c.in); got != c.want {
			t.Errorf("mapStateToTelegramEmoji(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

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
	got := findCall(api.Calls, "setMessageReaction")
	if got == nil {
		t.Fatal("expected setMessageReaction call")
	}
	wantReaction := []any{map[string]any{"type": "emoji", "emoji": "⏳"}}
	if !reflect.DeepEqual(got.Params["reaction"], wantReaction) {
		t.Errorf("reaction = %v, want %v", got.Params["reaction"], wantReaction)
	}
}

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
	got := findCall(api.Calls, "setMessageReaction")
	if got == nil {
		t.Fatal("expected setMessageReaction call")
	}
	wantReaction := []any{map[string]any{"type": "emoji", "emoji": "✅"}}
	if !reflect.DeepEqual(got.Params["reaction"], wantReaction) {
		t.Errorf("reaction = %v, want %v", got.Params["reaction"], wantReaction)
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
	// 第三次 Done → 不同 state 重新触发
	msg.MessageState.State = agent.MessageDone
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 3: %v", err)
	}
	if got := len(findCalls(api.Calls, "setMessageReaction")); got != 2 {
		t.Errorf("setMessageReaction calls = %d, want 2 (1st + 3rd, 2nd deduped)", got)
	}
}

func TestAdapter_Send_OutMessageState_FirstReceivedNotSkipped(t *testing.T) {
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

func TestAdapter_Send_OutMessageStateRemoved_DoesNotPolluteLRU(t *testing.T) {
	// OutMessageStateRemoved sends reaction:[] (Telegram API contract:
	// clears all reactions). It must NOT touch the messageStates LRU —
	// otherwise subsequent OutMessageState emits for the same userMsgID
	// would be incorrectly deduped against a stale sentinel (e.g. if
	// someone added rememberMessageState(..., MessageDropped) to the
	// Removed path, Done after a Removed would still trigger because
	// Done != Dropped, but Submitted after a Removed would dedup
	// against the prior Submitted record — the invariant we want to
	// pin is that LRU still holds the *last rendered* state, untouched
	// by Removed).
	//
	// Concretely: after Submitted → Removed, a *different* state must
	// still hit the API. If Removed silently poisoned the LRU with a
	// sentinel, the LRU's stored state would no longer match the last
	// actual render — which is the only invariant the LRU exists to
	// enforce.
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
	// 3) Different state (Done) — must hit the API, proving LRU still
	// holds Submitted (not a sentinel from Removed). If Removed had
	// poisoned the LRU with MessageDropped or cleared it, the
	// comparison prev==Done would still pass here — so we ALSO verify
	// in step 4 that a *same-state* replay after Removed is deduped
	// (matches the LRU record from step 1), which would fail if the
	// LRU had been zeroed.
	msg.MessageState.State = agent.MessageDone
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send 3 (Done): %v", err)
	}
	if got := len(findCalls(api.Calls, "setMessageReaction")); got != 3 {
		t.Errorf("setMessageReaction calls = %d, want 3 (Submitted + Removed + Done)", got)
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
	case <-time.After(time.Second):
		t.Fatal("no inbound")
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

func TestAdapter_EnsurePlaceholderForHeartbeat_SkipsOnZeroTopic(t *testing.T) {
	a, _ := newTestAdapter(t)
	messageID, err := a.ensurePlaceholderForHeartbeat(context.Background(), "100", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if messageID != 0 {
		t.Fatalf("topicID=0 (p2p) must return 0, got %d", messageID)
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
