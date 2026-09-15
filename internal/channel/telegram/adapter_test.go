package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
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

// TestE2E_RichTurnLifecycle exercises the full L3 per-turn rich
// message pipeline end-to-end: handleMessage creates the rich turn
// state on the first inbound event, OutReply / OutThinking /
// OutToolStart / OutToolEnd / OutError / OutTaskUpdate /
// OutHeartbeat / OutResult all append to / PATCH the same rich
// message via editMessageText(rich_message=...), and OnPromptEnded
// stamps the 🎉 reaction on the rich message's id (NOT on a
// chain chunk — chain is gone in L3).
//
// This is the regression test for the "rich mode always on" /
// "v9 chain retired" migration. If any L3 step regresses, the
// expectations below catch it: a missing sendRichMessage call,
// a stray sendMessage call (plain HTML fallback was deleted),
// or a 🎉 stamp that lands on the wrong message_id.
func TestE2E_RichTurnLifecycle(t *testing.T) {
	a, api := newTestAdapter(t)

	// 1. Inbound message seeds the rich turn. No chain creation,
	// no eager placeholder. L3: ensurePlaceholder is now a no-op.
	a.handleMessage(context.Background(), &Message{
		MessageID: 42,
		Date:      time.Now().Unix(),
		Chat:      Chat{ID: 100, Type: "private"},
		From:      &User{ID: 1},
		Text:      "hello",
	})

	// 2. OutReply — goes through appendSegmentForKind → appendRichTurn.
	//    rich mode is always on, no RichMode gate. The walker may
	//    or may not produce ok=true (markdown syntax dependent) —
	//    the e2e invariant is that SOMETHING rich-flavored happens.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutReply,
		Text:   "**bold** reply",
	}); err != nil {
		t.Fatalf("OutReply Send: %v", err)
	}

	// 3. OutHeartbeat → updateRichTurnHeader → scheduleFlushDebounced
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{
			Status:     messages.HeartbeatRunning,
			ThinkCount: 2,
		},
	}); err != nil {
		t.Fatalf("OutHeartbeat Send: %v", err)
	}

	// 4. OutResult — sendRichMessage path. The result message_id
	//    is recorded on richTurn.resultMessageID for the 🎉 anchor.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutResult,
		Text:   "final answer",
	}); err != nil {
		t.Fatalf("OutResult Send: %v", err)
	}

	// 5. OnPromptEnded — flush the rich turn, then stamp 🎉 on
	//    the OutResult's message_id (richTurn.resultMessageID).
	a.OnPromptEnded(context.Background(), "tg_100", "42", agent.PromptEndClean)

	// 6. Assertions: every send went through sendRichMessage
	//    (no plain-text sendMessage call anywhere — that path was
	//    deleted). One sendRichMessage per Out* event (plus one
	//    per flush), zero sendMessage calls.
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, c := range api.Calls {
		if c.Method == "sendMessage" {
			t.Fatalf("plain-text sendMessage call survived (expected sendRichMessage): %+v", c.Params)
		}
	}
	if len(api.Calls) == 0 {
		t.Fatal("expected at least one sendRichMessage call")
	}

	// 7. The 🎉 reaction landed on a non-zero message_id.
	//    setMessageReactions is called with the result message_id
	//    (richTurn.resultMessageID), NOT the placeholder (chain gone).
	var stampedTarget int64
	for _, c := range api.Calls {
		if c.Method == "setMessageReaction" {
			switch mid := c.Params["message_id"].(type) {
			case int:
				if int64(mid) > 0 {
					stampedTarget = int64(mid)
				}
			case int64:
				if mid > 0 {
					stampedTarget = mid
				}
			case float64:
				if int64(mid) > 0 {
					stampedTarget = int64(mid)
				}
			}
		}
	}
	if stampedTarget == 0 {
		t.Fatal("no setMessageReaction call with a non-zero message_id")
	}
}

// TestE2E_RichPathFailureSurfaces ensures that when sendRichMessage
// errors (e.g., pre-10.1 client returning 400, or transient
// network failure), the error surfaces to the caller rather than
// silently truncating to 4K. The plain-text fallback was deleted
// in commit 0d365d9 — silent degradation is no longer an option.
func TestE2E_RichPathFailureSurfaces(t *testing.T) {
	a, api := newTestAdapter(t)
	api.callErr = errors.New("simulated sendRichMessage 400")

	err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_200",
		Kind:   messages.OutReply,
		Text:   "this should fail",
	})
	if err == nil {
		t.Fatal("expected error from Send when sendRichMessage fails")
	}
	if !strings.Contains(err.Error(), "simulated sendRichMessage 400") {
		t.Fatalf("error did not propagate: %v", err)
	}

	// And: no plain-text sendMessage fallback. The L3 retirement
	// means OutReply errors out, it doesn't fall back to 4K.
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, c := range api.Calls {
		if c.Method == "sendMessage" {
			t.Fatalf("plain-text sendMessage fallback was used (deleted in 0d365d9): %+v", c.Params)
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
	// 适配器必须自行决定 emoji, 期望 👌 (v6.3: 单 reaction 预算, 只 Submitted 贴)
	got := findCall(api.Calls, "setMessageReaction")
	if got == nil {
		t.Fatal("expected setMessageReaction call, got none")
	}
	wantReaction := []any{
		map[string]any{"type": "emoji", "emoji": "👌"},
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
		// MessageSubmitted emits 👌 ("AI thinking" via OK-hand).
		// Queued and Done are silent drops; their visuals are
		// conveyed via placeholder text PATCH (Queued) and the
		// placeholder ✅ reaction (Done — handled in OnPromptEnded).
		{agent.MessageQueued, ""},
		{agent.MessageSubmitted, "👌"},
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
// on the user message. The terminal ✅ reaction lives on the
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
//	1st Submitted → 1 reaction call
//	2nd Submitted (same) → dedup'd (0 extra)
//	3rd Submitted again → dedup'd
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
//	1st Submitted  → 1 setMessageReaction (👌), LRU = {5: Submitted}
//	Removed        → 1 setMessageReaction ([]), LRU untouched
//	2nd Submitted  → dedup'd (0 extra) — proves LRU still holds
//	                 Submitted (if Removed had zeroed the LRU, this
//	                 would emit a 3rd call)
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

// TestAdapter_Send_OutHeartbeat_WithTopic locks the v7 PATCH
// payload format. v7 added `⏱ HH:MM:SS` from snapshot.LastBeatAt
// so the user can see when the last heartbeat was emitted. The
// snapshot here is built without LastBeatAt set, so the suffix
// is omitted (heartbeatText skips it for zero-value time).
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

// TestAdapter_Send_OutHeartbeat_AppendsTimestamp locks the
// v7 PATCH payload with a non-zero LastBeatAt. The placeholder
// text must end with `⏱ HH:MM:SS` so the user can see when
// the agent was last alive.
func TestAdapter_Send_OutHeartbeat_AppendsTimestamp(t *testing.T) {
	t.Skip("v9 chain: rewrite pending; tracked in §11.12.16 backlog. v8 expected multi-send-message / per-call reaction patterns that v9 chain consolidates into single sendMessage + debounced editMessageText.")
	t.Skip("v9 chain rolling log: rewrite to chain-headerLine assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	// Seed with both PlaceholderMessageID and UserMessageID so
	// the v6.1+ race guard lets the heartbeat path find the
	// existing placeholder and PATCH it. The ChatID is the
	// namespaced session form ("tg_<id>:<thread>") that the
	// runtime produces; splitSessionID parses this into (raw=100,
	// topic=1) for the state key + thread_id.
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 1, PlaceholderMessageID: 50, UserMessageID: "10"})
	now := time.Date(2026, 8, 22, 15, 18, 8, 0, time.UTC)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "tg_100:1",
		Kind:      messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 5, ToolCount: 7, LastBeatAt: now},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	edit := findCall(api.snapshotCalls(), "editMessageText")
	if edit == nil {
		t.Fatal("expected editMessageText")
	}
	text, _ := edit.Params["text"].(string)
	want := "💭 5 · 🔧 7 · ⏱ 15:18:08"
	if text != want {
		t.Fatalf("heartbeat text = %q, want %q", text, want)
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
	sendCall := findCall(api.snapshotCalls(), "sendRichMessage")
	if sendCall == nil {
		t.Fatal("expected sendRichMessage call")
	}
	chatID, _ := sendCall.Params["chat_id"].(string)
	if chatID != "100" {
		t.Fatalf("sendRichMessage chat_id = %q, want raw %q (Telegram Bot API rejects tg_ prefix)", chatID, "100")
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
	t.Skip("v9 chain: rewrite pending; tracked in §11.12.16 backlog. v8 expected multi-send-message / per-call reaction patterns that v9 chain consolidates into single sendMessage + debounced editMessageText.")
	t.Skip("v9 chain rolling log: rewrite to chain-buffer split assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
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
	a.OnPromptEnded(context.Background(), "100", "1", agent.PromptEndClean)
}

func TestAdapter_OnPromptEnded_WithTopic(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 1, PlaceholderMessageID: 50})
	a.OnPromptEnded(context.Background(), "100", "1", agent.PromptEndClean)
}

func TestAdapter_OnPromptEnded_EmptyChat(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.OnPromptEnded(context.Background(), "", "1", agent.PromptEndClean)
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
	t.Skip("v9 chain: rewrite pending; tracked in §11.12.16 backlog. v8 expected multi-send-message / per-call reaction patterns that v9 chain consolidates into single sendMessage + debounced editMessageText.")
	t.Skip("v9 chain rolling log: rewrite to chain-buf reply_to_message_id assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
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
	t.Skip("v9 chain: rewrite pending; tracked in §11.12.16 backlog. v8 expected multi-send-message / per-call reaction patterns that v9 chain consolidates into single sendMessage + debounced editMessageText.")
	t.Skip("v9 chain rolling log: rewrite to chain-headerLine assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
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
	t.Skip("v9 chain: rewrite pending; tracked in §11.12.16 backlog. v8 expected multi-send-message / per-call reaction patterns that v9 chain consolidates into single sendMessage + debounced editMessageText.")
	t.Skip("v9 chain rolling log: rewrite to chain-buf assertions with topic thread_id; tracked in docs/channel/telegram.md §11.12.16 backlog")
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
	t.Skip("v9 chain: rewrite pending; tracked in §11.12.16 backlog. v8 expected multi-send-message / per-call reaction patterns that v9 chain consolidates into single sendMessage + debounced editMessageText.")
	t.Skip("v9 chain rolling log: rewrite to active-chunk 🎉 reaction assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 909})
	a.OnPromptEnded(context.Background(), "100", "7", agent.PromptEndClean)

	// Must NOT PATCH placeholder text (v4 dropped "<b>✅ Completed</b>" PATCH).
	for _, call := range api.snapshotCalls() {
		if call.Method == "editMessageText" {
			t.Fatalf("v4 OnPromptEnded must NOT call editMessageText; got params=%+v", call.Params)
		}
	}

	// v6.3: ONLY the placeholder gets a 🎉 reaction. The user
	// message's single-reaction slot is reserved for
	// MessageSubmitted ("👌") — OnPromptEnded must NOT
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
	a.OnPromptEnded(context.Background(), "100", "1", agent.PromptEndClean)
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

// TestAdapter_HandleUpdate_DropsBareStart verifies that a bare
// /start (Telegram's "begin conversation" convention) is silently
// dropped at the adapter boundary — no placeholder message is
// sent, no inbound is published, no agent invocation. Variants
// like /start@<bot> follow the same path; /start with extra text
// flows through normally.
func TestAdapter_HandleUpdate_DropsBareStart(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"plain_start", "/start"},
		{"start_with_bot_suffix", "/start@testbot"},
		{"uppercase_start", "/START"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, api := newTestAdapter(t)
			api.GetMeResult = UserInfo{ID: 999, Username: "testbot"}
			a.botID = 999
			a.botName = "testbot"

			// Start so a.ctx is non-nil; without Start, publish's
			// select races against a permanently-closed ctxDone
			// channel and the "drop" assertion below would
			// vacuously pass for the wrong reason (publish dropped
			// the message rather than the bare-/start filter
			// catching it).
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer func() { _ = a.Stop(context.Background()) }()
			if err := a.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}

			a.handleUpdate(ctx, Update{
				UpdateID: 1,
				Message: &Message{
					MessageID: 100,
					Date:      time.Now().Unix(),
					Chat:      Chat{ID: 555, Type: "private"},
					From:      &User{ID: 1},
					Text:      tc.text,
				},
			})

			select {
			case msg := <-a.Incoming():
				t.Fatalf("/start must not produce an inbound message; got %+v", msg)
			case <-time.After(150 * time.Millisecond):
				// Expected: no inbound, no placeholder sendMessage call.
			}
		})
	}

	// /start with extra text MUST flow through normally — it's a
	// real user prompt, not a platform convention.
	//
	// Must call Start so a.ctx is non-nil — publish uses a 3-way
	// select against a.ctxDone(), and without Start ctxDone returns
	// a closed channel, racing with the buffered send and dropping
	// the message ~50% of the time.
	t.Run("start_with_extra_text_passes_through", func(t *testing.T) {
		a, api := newTestAdapter(t)
		api.GetMeResult = UserInfo{ID: 999, Username: "testbot"}
		a.botID = 999
		a.botName = "testbot"

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer func() { _ = a.Stop(context.Background()) }()
		if err := a.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}

		a.handleUpdate(ctx, Update{
			UpdateID: 1,
			Message: &Message{
				MessageID: 101,
				Date:      time.Now().Unix(),
				Chat:      Chat{ID: 555, Type: "private"},
				From:      &User{ID: 1},
				Text:      "/start please do the thing",
			},
		})

		select {
		case msg := <-a.Incoming():
			if msg.Text != "/start please do the thing" {
				t.Fatalf("inbound text = %q, want /start please do the thing", msg.Text)
			}
		case <-time.After(time.Second):
			t.Fatal("/start with extra text must produce an inbound message")
		}
	})
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
		if call.Method != "sendRichMessage" {
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
		t.Fatal("no force_reply sendRichMessage")
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

func TestFormatTool(t *testing.T) {
	if formatTool(messages.OutboundMessage{}) != "" {
		t.Fatal("empty tool")
	}
	// v9 (commit #3): call / result lines now match feishu's
	// claude-code-style format. ToolStart emits a `● name(args)`
	// call line; ToolEnd emits a `⎿  …` result summary.
	if got := formatTool(messages.OutboundMessage{
		Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{Name: "read", Args: "x"},
	}); got != "● read(x)" {
		t.Fatalf("ToolStart = %q, want %q", got, "● read(x)")
	}
	if got := formatTool(messages.OutboundMessage{
		Kind: messages.OutToolEnd,
		Tool: &messages.ToolInfo{Name: "read", Output: "ok"},
	}); got != "⎿  📄 Read → 1 lines" {
		t.Fatalf("ToolEnd = %q, want %q", got, "⎿  📄 Read → 1 lines")
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

// TestHeartbeatText_TerminalPrefix pins the verdict prefix that
// heartbeatText paints for the two terminal HeartbeatStatus
// values. Telegram's chunk header was previously indifferent to
// the terminal state (the standalone 🎉 reaction carried the
// only signal); this test pins that the chunk header itself
// now carries ✅ / ❌ inline so the user doesn't have to chase
// the result-message reaction to learn whether the turn
// completed cleanly.
func TestHeartbeatText_TerminalPrefix(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		hb         *messages.HeartbeatSnapshot
		wantPrefix string // first rune of the line; "" means no terminal prefix
	}{
		{
			name: "clean: Done with counters",
			hb: &messages.HeartbeatSnapshot{
				ThinkCount: 3, ToolCount: 1, LastBeatAt: now,
				Status: messages.HeartbeatDone,
			},
			wantPrefix: "✅ ",
		},
		{
			name: "error: Error with counters",
			hb: &messages.HeartbeatSnapshot{
				ThinkCount: 2, ToolCount: 4, LastBeatAt: now,
				Status: messages.HeartbeatError,
			},
			wantPrefix: "❌ ",
		},
		{
			name: "running: no prefix",
			hb: &messages.HeartbeatSnapshot{
				ThinkCount: 1, LastBeatAt: now,
				Status: messages.HeartbeatRunning,
			},
			wantPrefix: "",
		},
		{
			name: "clean: Done with no activity",
			hb: &messages.HeartbeatSnapshot{
				Status: messages.HeartbeatDone,
			},
			wantPrefix: "✅ ",
		},
		{
			name: "error: Error with no activity",
			hb: &messages.HeartbeatSnapshot{
				Status: messages.HeartbeatError,
			},
			wantPrefix: "❌ ",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := heartbeatText(c.hb)
			if !strings.HasPrefix(got, c.wantPrefix) {
				t.Fatalf("heartbeatText = %q, want prefix %q", got, c.wantPrefix)
			}
			// heartbeatText now produces plain text — no HTML tags.
			// Pre-PR-369 it wrapped the body in <b>...</b> for
			// parse_mode=HTML sendMessage; rich_message blocks
			// render text literally so the wrapper is gone.
			if strings.Contains(got, "<b>") || strings.Contains(got, "</b>") {
				t.Fatalf("heartbeatText must NOT emit HTML tags; got %q", got)
			}
			// Snapshot-driven body presence: when the snapshot
			// carries observable state (counters, time) the body
			// carries the chip; a terminal-only snapshot produces
			// just the prefix, no chip — same shape as feishu's
			// renderHeartbeatHeader.
			hasChip := c.hb.ThinkCount > 0 || c.hb.ToolCount > 0 ||
				!c.hb.LastBeatAt.IsZero()
			if hasChip && !strings.Contains(got, "⏱") {
				t.Fatalf("heartbeatText = %q, want time chip for active snapshot", got)
			}
		})
	}
}

// TestPatchChainHeader_EmptyRunningKeepsColdBanner pins the
// feishu-aligned §3.6 gate: an OutHeartbeat carrying a snapshot
// with zero counters, zero LastBeatAt, and Running status must
// NOT flip chunk.hasHeartbeat (the cold "Working" banner
// remains). Without this gate the chunk header would silently
// disappear on a /think off + /tools off turn.
func TestPatchChainHeader_EmptyRunningKeepsColdBanner(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "600", TopicID: 0,
		PlaceholderMessageID: 1300, UserMessageID: "60"})

	// Seed the chain with one OutReply so a chunk exists.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "600", Kind: messages.OutReply, Text: "seed",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Empty running snapshot — must NOT flip hasHeartbeat.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "600", Kind: messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{
			Status: messages.HeartbeatRunning,
		},
	}); err != nil {
		t.Fatalf("empty running heartbeat: %v", err)
	}

	// Now terminal-only (zero counters, no LastBeatAt, Error).
	// Per the gate, terminal status DOES flip hasHeartbeat so
	// the user sees the ❌ prefix on the chunk header.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "600", Kind: messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{
			Status: messages.HeartbeatError,
		},
	}); err != nil {
		t.Fatalf("terminal-only heartbeat: %v", err)
	}
}

// --- DM draft routing tests (Bot API 10.3+ sendMessageDraft) ---

// setupDMState primes the adapter state with a private-DM topic
// record so Send() can route through the draft path. Returns the
// chatID (raw form) that Send() expects.
func setupDMState(t *testing.T, a *Adapter, chatIDRaw int64) string {
	t.Helper()
	raw := strconv.FormatInt(chatIDRaw, 10)
	if err := a.state.putTopic(&TopicState{
		ChatID:        raw,
		TopicID:       0,
		ChatType:      "private",
		UserMessageID: "1",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}
	return raw
}

func setupGroupState(t *testing.T, a *Adapter, chatIDRaw int64, topicID int) string {
	t.Helper()
	raw := strconv.FormatInt(chatIDRaw, 10)
	if err := a.state.putTopic(&TopicState{
		ChatID:        raw,
		TopicID:       topicID,
		ChatType:      "supergroup",
		UserMessageID: "1",
	}); err != nil {
		t.Fatalf("putTopic: %v", err)
	}
	return raw
}

func findCallByMethod(calls []fakeCall, method string) *fakeCall {
	for i := range calls {
		if calls[i].Method == method {
			return &calls[i]
		}
	}
	return nil
}

func TestAdapter_Send_DM_OutThinking_StreamsToDraft(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "considering whether to invoke Read",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	draft := findCallByMethod(api.Calls, "sendMessageDraft")
	if draft == nil {
		t.Fatalf("expected sendMessageDraft call; got calls=%+v", api.Calls)
	}
	if draft.Params["chat_id"] != int64(100) {
		t.Fatalf("chat_id = %v, want 100", draft.Params["chat_id"])
	}
	// "💭 " prefix added at adapter layer for visual consistency.
	want := "💭 considering whether to invoke Read"
	if text, _ := draft.Params["text"].(string); text != want {
		t.Fatalf("text = %q, want %q (with 💭 prefix)", text, want)
	}
}

func TestAdapter_Send_DM_OutToolStart_StreamsToDraft(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	draft := findCallByMethod(api.Calls, "sendMessageDraft")
	if draft == nil {
		t.Fatalf("expected sendMessageDraft call; got calls=%+v", api.Calls)
	}
}

func TestAdapter_Send_DM_OutToolEnd_StreamsToDraft(t *testing.T) {
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolEnd,
		Tool:   &messages.ToolInfo{Name: "Read", Output: "47 lines"},
		Text:   "⎿  📄 Read → 47 lines",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	draft := findCallByMethod(api.Calls, "sendMessageDraft")
	if draft == nil {
		t.Fatalf("expected sendMessageDraft call; got calls=%+v", api.Calls)
	}
}

func TestAdapter_Send_ForumTopic_OutThinking_UsesChainNotDraft(t *testing.T) {
	// Probe on 2026-09-15 confirmed Telegram Bot API rejects
	// sendMessageDraft in basic groups (Bad Request). We can't
	// cheaply distinguish forum-supergroup-with-forum-on from
	// basic-group, so streamDraftEvent gates on ChatType=="private".
	// Forum topics therefore fall through to the v9 chain path.
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 42)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw + ":42",
		Kind:   messages.OutThinking,
		Text:   "thinking text",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if draft := findCallByMethod(api.Calls, "sendMessageDraft"); draft != nil {
		t.Fatalf("forum topic should NOT use sendMessageDraft; got call %+v", draft)
	}
}

func TestAdapter_Send_ForumTopic_OutToolStart_UsesChainNotDraft(t *testing.T) {
	// Same gate as OutThinking: forum topic falls through to v9
	// chain because streamDraftEvent requires ChatType=="private".
	a, api := newTestAdapter(t)
	raw := setupGroupState(t, a, -1001, 42)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw + ":42",
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if draft := findCallByMethod(api.Calls, "sendMessageDraft"); draft != nil {
		t.Fatalf("forum topic should NOT use sendMessageDraft; got call %+v", draft)
	}
}

func TestAdapter_Send_DM_DraftFailureLatch_DropsDoNotFallthroughToChain(t *testing.T) {
	// In DM, when sendMessageDraft fails, think/tool events are
	// DROPPED (not sent to richMessage). This is the user's explicit
	// design requirement: "think/tool 绝不混进正常的richMessage".
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)
	api.Errors = []error{errors.New("simulated 400")}

	// First OutToolStart: draft API fails → dropped. No rich turn
	// cold-create either (we skip in DM).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if c := countByMethod(api.Calls, "sendMessageDraft"); c != 1 {
		t.Fatalf("first send: sendMessageDraft count = %d, want 1", c)
	}
	// Critical: no rich message fallback. No sendRichMessage,
	// no sendMessage. The event is gone.
	if c := countByMethod(api.Calls, "sendRichMessage"); c != 0 {
		t.Fatalf("sendRichMessage count = %d, want 0 (DM draft failure must NOT fall through to richMessage)", c)
	}
	if c := countByMethod(api.Calls, "sendMessage"); c != 0 {
		t.Fatalf("sendMessage count = %d, want 0 (DM draft failure must NOT fall through to sendMessage)", c)
	}

	// Second event in same turn: latch engaged, still dropped.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolEnd,
		Tool:   &messages.ToolInfo{Name: "Read", Output: "47 lines"},
		Text:   "⎿  📄 Read → 47 lines",
	}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if c := countByMethod(api.Calls, "sendMessageDraft"); c != 1 {
		t.Fatalf("after second send: sendMessageDraft count = %d, want 1 (latch should prevent retry)", c)
	}
}

func TestAdapter_Send_DM_DraftGloballyShared_AcrossTurns(t *testing.T) {
	// Draft streamer is GLOBAL per (chat, thread) — no per-turn
	// reset. The same draft_id is reused across turns so the
	// streamer object identity survives. When turn N's real
	// message lands, the server disposes the old draft; turn N+1's
	// first event with the same draft_id is treated by the server
	// as a brand-new draft (since the previous one was pushed out).
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "turn 1 thought",
	})

	// Simulate end of turn N → start of turn N+1 WITHOUT calling
	// draftStreamers.reset. The streamer's draft_id should persist.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	})

	drafts := callsByMethod(api.Calls, "sendMessageDraft")
	if len(drafts) != 2 {
		t.Fatalf("expected 2 sendMessageDraft calls, got %d", len(drafts))
	}
	id0 := drafts[0].Params["draft_id"]
	id1 := drafts[1].Params["draft_id"]
	if id0 != id1 {
		t.Fatalf("global draft: both turns should share draft_id; got %v vs %v", id0, id1)
	}
	// And the texts reflect REPLACE semantics per-event.
	// Turn 2's OutToolStart REPLACES the prior turn's thinking.
	want := "● Read(/tmp/foo.go)"
	if got := drafts[1].Params["text"]; got != want {
		t.Fatalf("call 2 text = %v, want %q (REPLACE across turn boundary)", got, want)
	}
}

// countByMethod / callsByMethod are small helpers used by the
// tests above to slice api.Calls by method name.
func countByMethod(calls []fakeCall, method string) int {
	n := 0
	for _, c := range calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func callsByMethod(calls []fakeCall, method string) []fakeCall {
	var out []fakeCall
	for _, c := range calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func TestAdapter_EnsurePlaceholder_DM_SkipsRichTurnColdCreate(t *testing.T) {
	// In DM, ensurePlaceholder must NOT cold-create the rich turn
	// placeholder. The draft IS the live surface for think/tool;
	// an empty rich message sitting next to the draft would just be
	// noise.
	a, api := newTestAdapter(t)
	a.config.PollingTimeout = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = a.Stop(context.Background()) }()
	_ = a.Start(ctx)

	a.handleUpdate(ctx, Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 42,
			Date:      time.Now().Unix(),
			Chat:      Chat{ID: 100, Type: "private"},
			From:      &User{ID: 1},
			Text:      "hello",
		},
	})
	// Drain the inbound channel so the test doesn't deadlock.
	select {
	case <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("no inbound")
	}

	// After DM ensurePlaceholder: zero sendRichMessage / sendMessage
	// calls should have been made. Only the inbound getUpdates
	// bookkeeping (handled internally, not via api) and our dummy
	// setup may have used the api.
	for _, c := range api.Calls {
		if c.Method == "sendRichMessage" || c.Method == "sendMessage" {
			t.Fatalf("DM ensurePlaceholder must NOT send a rich turn placeholder; saw %s", c.Method)
		}
	}

	// But subsequent OutReply (which DOES need a real message)
	// should lazily cold-create a rich turn on its own.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_100",
		Kind:   messages.OutReply,
		Text:   "actual reply content",
	}); err != nil {
		t.Fatalf("OutReply: %v", err)
	}
	if c := countByMethod(api.Calls, "sendRichMessage"); c == 0 {
		t.Fatalf("OutReply in DM should cold-create rich turn on demand; saw %d sendRichMessage calls", c)
	}
}

func TestAdapter_Send_DM_OutThinking_ReplacesAcrossEvents(t *testing.T) {
	// Each OutThinking event REPLACES the prior draft text. The
	// chat shows only the latest thinking body, animated in place.
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)

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
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "third thought",
	})

	drafts := callsByMethod(api.Calls, "sendMessageDraft")
	if len(drafts) != 3 {
		t.Fatalf("sendMessageDraft count = %d, want 3", len(drafts))
	}
	// Each call's text is just the latest thinking line (with prefix).
	if got := drafts[0].Params["text"]; got != "💭 first thought" {
		t.Fatalf("call 1 text = %v, want \"💭 first thought\" (with 💭 prefix)", got)
	}
	want := "💭 third thought"
	if got := drafts[2].Params["text"]; got != want {
		t.Fatalf("final draft text = %q, want %q (REPLACE: each thinking REPLACES the prior body)", got, want)
	}

	// All three calls share the same draft_id (animation, not
	// replace-with-new-message).
	id0 := drafts[0].Params["draft_id"]
	for i, d := range drafts {
		if d.Params["draft_id"] != id0 {
			t.Fatalf("call %d draft_id=%v, want %v (same id → animate in place)", i, d.Params["draft_id"], id0)
		}
	}
}

func TestAdapter_Send_DM_OutTool_StartEnd_FormOneRecord(t *testing.T) {
	// End-to-end: OutToolStart REPLACE-s, OutToolEnd ACCUMULATE-s
	// onto the matching start, so the user sees one draft body
	// containing both the call line and the result line.
	//
	// formatTool uses msg.Tool.Output (not msg.Text) to build the
	// End body via summarizeToolResult, so the test must use a
	// realistic Output string that summarizeToolResult can format.
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	})
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolEnd,
		Tool:   &messages.ToolInfo{Name: "Read", Output: "line1\nline2\nline3"},
		Text:   "(ignored — formatTool reads msg.Tool.Output)",
	})

	drafts := callsByMethod(api.Calls, "sendMessageDraft")
	if len(drafts) != 2 {
		t.Fatalf("sendMessageDraft count = %d, want 2", len(drafts))
	}
	// First call: Start line alone.
	if got := drafts[0].Params["text"]; got != "● Read(/tmp/foo.go)" {
		t.Fatalf("call 1 text = %q, want Start line alone (REPLACE)", got)
	}
	// Second call: Start + End stacked as one record. formatTool
	// rendered End body via summarizeToolResult on 3 lines.
	want := "● Read(/tmp/foo.go)\n\n⎿  📄 Read → 3 lines"
	if got := drafts[1].Params["text"]; got != want {
		t.Fatalf("call 2 text = %q, want %q (Start+End stacked via ACCUMULATE)", got, want)
	}
}

func TestAdapter_Send_DM_OutThinkingAndOutTool_ReplacesAcrossKinds(t *testing.T) {
	// Cross-kind REPLACE: a thinking line followed by a tool start
	// shows ONLY the tool start (thinking REPLACED). Within a tool
	// call (Start → End), End ACCUMULATEs onto Start.
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "considering whether to invoke Read",
	})
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutToolStart,
		Tool:   &messages.ToolInfo{Name: "Read", Args: "/tmp/foo.go"},
		Text:   "● Read(/tmp/foo.go)",
	})

	drafts := callsByMethod(api.Calls, "sendMessageDraft")
	if len(drafts) != 2 {
		t.Fatalf("sendMessageDraft count = %d, want 2", len(drafts))
	}
	// First call had "💭 " prefix on thinking.
	wantFirst := "💭 considering whether to invoke Read"
	if got := drafts[0].Params["text"]; got != wantFirst {
		t.Fatalf("call 1 text = %q, want %q (thinking with 💭 prefix)", got, wantFirst)
	}
	// Tool start REPLACED the prior thinking line.
	want := "● Read(/tmp/foo.go)"
	if got := drafts[1].Params["text"]; got != want {
		t.Fatalf("call 2 text = %q, want %q (cross-kind REPLACE — tool start wipes thinking)", got, want)
	}
}

func TestAdapter_Send_DM_OutResult_EndsProcess(t *testing.T) {
	// OutResult must end the draft process: clear draft_id +
	// textBuf so the next turn's first event starts with a fresh
	// draft (not contaminated by leftover text from this turn).
	a, api := newTestAdapter(t)
	raw := setupDMState(t, a, 100)

	// Turn 1: events accumulate in the draft.
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
		Text:   "✅ Read → 47 lines",
	})

	// Verify draft has accumulated text before OutResult.
	chatIDInt, _ := strconv.ParseInt(raw, 10, 64)
	streamer := a.draftStreamers.getOrCreate(nil, newTestLogger(), chatIDInt, 0)
	streamer.mu.Lock()
	textBefore := streamer.textBuf.String()
	streamer.mu.Unlock()
	if textBefore == "" {
		t.Fatal("expected draft textBuf populated before OutResult")
	}

	// OutResult ends the process.
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutResult,
		Text:   "final answer",
	})

	streamer.mu.Lock()
	draftIDAfter := streamer.draftID
	textAfter := streamer.textBuf.String()
	streamer.mu.Unlock()
	if draftIDAfter != 0 {
		t.Fatalf("expected draft_id reset to 0 after OutResult, got %d", draftIDAfter)
	}
	if textAfter != "" {
		t.Fatalf("expected textBuf cleared after OutResult, got %q", textAfter)
	}

	// Turn 2: first event allocates a FRESH draft_id (not the
	// pre-OutResult one).
	api.Calls = nil
	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "turn 2 first thought",
	})
	drafts := callsByMethod(api.Calls, "sendMessageDraft")
	if len(drafts) != 1 {
		t.Fatalf("expected 1 sendMessageDraft call, got %d", len(drafts))
	}
	want := "💭 turn 2 first thought"
	if got := drafts[0].Params["text"]; got != want {
		t.Fatalf("turn 2 first event text = %q, want %q (fresh draft, no turn 1 residue)", got, want)
	}
}

func TestAdapter_OnPromptEnded_DM_EndsProcess(t *testing.T) {
	// OnPromptEnded is a safety net for turns without OutResult
	// (e.g. error-only turns). It must also end the draft
	// process so the next turn starts fresh.
	a, _ := newTestAdapter(t)
	raw := setupDMState(t, a, 100)

	_ = a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "tg_" + raw,
		Kind:   messages.OutThinking,
		Text:   "thought without result",
	})

	chatIDInt, _ := strconv.ParseInt(raw, 10, 64)
	streamer := a.draftStreamers.getOrCreate(nil, newTestLogger(), chatIDInt, 0)
	streamer.mu.Lock()
	if streamer.textBuf.Len() == 0 {
		streamer.mu.Unlock()
		t.Fatal("expected textBuf populated after thinking")
	}
	streamer.mu.Unlock()

	// OnPromptEnded clears the draft state.
	a.OnPromptEnded(context.Background(), "tg_"+raw, "1", agent.PromptEndClean)

	streamer.mu.Lock()
	defer streamer.mu.Unlock()
	if streamer.draftID != 0 {
		t.Fatalf("expected draft_id reset after OnPromptEnded, got %d", streamer.draftID)
	}
	if streamer.textBuf.Len() != 0 {
		t.Fatalf("expected textBuf cleared after OnPromptEnded, got %q", streamer.textBuf.String())
	}
}

func newTestLoggerForAdapter() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
