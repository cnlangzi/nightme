// dispatch_test.go — eventDispatcher 单测 (F-DSH-CHAT-001 §4.4)
//
// 锁住三件事:
//   - 9 个 known event type 全部能派发到对应 handler(不 panic,产出预期 event)
//   - 未知 type 不 panic,只 dLog
//   - 提前注册的"未来 type"(todo/update、todo/delete)graceful no-op

package dsh

import (
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// collectDeliver 把 dispatch 产出的 events 收集成切片,方便断言。
type collectDeliver struct {
	events []agent.AgentEvent
}

func (c *collectDeliver) deliver(ev agent.AgentEvent) {
	c.events = append(c.events, ev)
}

// TestDispatcher_AllKnownTypesRoute_NoPanic
// 9 个 known type 全部能 dispatch,不会 panic,各自产出至少一个 AgentEvent。
// 这是"加 handler 不用改 switch"治本设计的回归锁。
func TestDispatcher_AllKnownTypesRoute_NoPanic(t *testing.T) {
	cases := []struct {
		envType string
		data    string
		minEv   int
		want    agent.EventKind
	}{
		{
			envType: "assistant/chunk",
			data:    `{"chunk":{"index":0,"text":"hello"}}`,
			minEv:   0, // chunk 本身不产出 event(F-52: tool-boundary flush 才 emit)
		},
		{
			envType: "assistant/message",
			data:    `{"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
			minEv:   0, // 同样只更新 pendingText,不 emit
		},
		{
			envType: "tool/call",
			data:    `{"callId":"c-1","name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}`,
			minEv:   1,
			want:    agent.EventAgentToolStart,
		},
		{
			envType: "tool/result",
			data:    `{"message":{"role":"user","content":[{"type":"tool-result","id":"c-1","isError":false,"content":[{"type":"text","text":"file contents"}]}]}}`,
			minEv:   1,
			want:    agent.EventAgentToolEnd,
		},
		{
			envType: "turn/start",
			data:    `{"turn":1}`,
			minEv:   0, // 状态清理,无 event
		},
		{
			envType: "turn/end",
			data:    `{"stopReason":"stop"}`,
			minEv:   1, // fresh translator.active=false → EventDone only (no phantom "Done." card per F-52)
			want:    agent.EventAgentDone,
		},
		{
			envType: "compaction/end",
			data:    `{"reason":"auto","aborted":false}`,
			minEv:   0, // debug log only
		},
		{
			envType: "todo/write",
			data:    `{"items":[{"id":"t-1","content":"x","status":"pending"}]}`,
			minEv:   1,
			want:    agent.EventAgentTaskCreate,
		},
		{
			envType: "step/start",
			data:    `{"turn":1,"step":1}`,
			minEv:   0, // inference bound, not TodoPanel (todo/write owns OutTask*)
		},
		{
			envType: "step/end",
			data:    `{"turn":1,"step":1}`,
			minEv:   0,
		},
		{
			envType: "approval/asked",
			data:    `{"toolCallId":"a-1","toolName":"Bash","action":"run","options":["approve","decline"]}`,
			minEv:   0, // d is nil here → handler returns nil; see TestDispatcher_ApprovalAsked
		},
	}

	for _, tc := range cases {
		t.Run(tc.envType, func(t *testing.T) {
			tr := newTranslator("test-agent", "/tmp/test")
			st := newWireState()
			c := &collectDeliver{}
			dispatcher := newDispatcher(tr, st, nil, c.deliver)
			muxBytes := makeMuxEvent(t, tc.envType, tc.data)
			env, view := decodeMuxEvent(t, muxBytes)

			// 不应该 panic
			dispatcher.dispatch(env, view)

			if len(c.events) < tc.minEv {
				t.Errorf("%s: got %d events, want >= %d", tc.envType, len(c.events), tc.minEv)
			}
			if tc.want != 0 && len(c.events) > 0 {
				if c.events[0].Kind != tc.want {
					t.Errorf("%s: first event kind = %v, want %v", tc.envType, c.events[0].Kind, tc.want)
				}
			}
		})
	}
}

// TestDispatcher_UnknownType_DoesNotPanic
// D4 治本:未知 type 不再静默 dLog 而消失,handler 走 lookup miss 路径,
// 无事发生,后续可观测性可以基于此做 ring buffer 计数(P4)。
func TestDispatcher_UnknownType_DoesNotPanic(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// 故意造一个不在注册表里的 type
	muxBytes := makeMuxEvent(t, "future/event/that/dsh/will/add", `{"foo":"bar"}`)
	env, view := decodeMuxEvent(t, muxBytes)

	// 必须不 panic
	dispatcher.dispatch(env, view)

	if len(c.events) != 0 {
		t.Errorf("unknown type should produce 0 events, got %d", len(c.events))
	}
}

// TestDispatcher_FutureTypesRegistered_NoPanic
// 验证"提前注册未来 type"的契约:todo/update 和 todo/delete 在注册表里,
// 即使 handler 现在返回 nil(graceful no-op),dispatcher 也不走 unknown 路径。
// 这是文档 §4.4 的关键设计:让 dsh 偷偷加事件时 bridge 自动开始处理(而不是 dLog 蒸发)。
func TestDispatcher_FutureTypesRegistered_NoPanic(t *testing.T) {
	cases := []string{"todo/update", "todo/delete"}
	for _, typ := range cases {
		t.Run(typ, func(t *testing.T) {
			tr := newTranslator("test-agent", "/tmp/test")
			st := newWireState()
			c := &collectDeliver{}
			dispatcher := newDispatcher(tr, st, nil, c.deliver)
			muxBytes := makeMuxEvent(t, typ, `{"id":"t-1"}`)
			env, view := decodeMuxEvent(t, muxBytes)
			dispatcher.dispatch(env, view)
			// 不 panic 即通过;handler 现在返回 nil 是预期
		})
	}
}

// TestDispatcher_LookupMissesAfterRegistrationRemoval
// 反向锁:从注册表里移除一个 handler 后,该 type 走 unknown 路径。
// 防止有人"优化"代码把 unknown 处理直接 panic。
func TestDispatcher_LookupMissesAfterRegistrationRemoval(t *testing.T) {
	// 这个测试是文档级契约,不写实现层 mock;真正验证在 dispatcher.go 里
	// lookup miss 必须返 (nil, false) 而不是 panic。
	// 这里只断言 lookup helper 的行为。
	d := newDispatcher(newTranslator("a", "/tmp"), newWireState(), nil, func(agent.AgentEvent) {})
	h, ok := d.registry.lookup("definitely/not/a/real/type")
	if ok {
		t.Errorf("expected lookup miss for unknown type, got handler %v", h)
	}
	if h != nil {
		t.Errorf("expected nil handler on miss, got %T", h)
	}
}

// TestDispatcher_RegistryHasAllExpectedTypes
// 文档 §7.1 Phase 1+2 DoD 锁:注册表必须包含这 11 个 type。
// 加新 type 时这个测试要同步更新,提醒 reviewer 思考 handler 实现。
func TestDispatcher_RegistryHasAllExpectedTypes(t *testing.T) {
	d := newDispatcher(newTranslator("a", "/tmp"), newWireState(), nil, func(agent.AgentEvent) {})
	want := []string{
		"assistant/chunk",
		"assistant/message",
		"tool/call",
		"tool/result",
		"turn/start",
		"turn/end",
		"compaction/end",
		"todo/write",
		"todo/update",   // 未来 type,handler 现在返回 nil
		"todo/delete",   // 同上
		"approval/asked",
		"step/start",
		"step/end",
	}
	for _, typ := range want {
		if _, ok := d.registry.lookup(typ); !ok {
			t.Errorf("registry missing type %q", typ)
		}
	}
}

// TestDispatcher_ToolCallResultPair_BackfillsNameArgs
// 关键回归:tool/call 存的 Name+Args 必须在 tool/result 时回填到
// EventAgentToolEnd,这是 dsh wire 的关键不变量(协议只传 result,
// name/args 由 bridge 维护)。
func TestDispatcher_ToolCallResultPair_BackfillsNameArgs(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// tool/call
	muxBytes := makeMuxEvent(t, "tool/call",
		`{"callId":"c-7","name":"Bash","arguments":"{\"cmd\":\"ls\"}"}`)
	env, _ := decodeMuxEvent(t, muxBytes)
	dispatcher.dispatch(env, nil)

	// tool/result
	muxBytes2 := makeMuxEvent(t, "tool/result",
		`{"message":{"role":"user","content":[{"type":"tool-result","id":"c-7","isError":false,"content":[{"type":"text","text":"file1\nfile2"}]}]}}`)
	env2, _ := decodeMuxEvent(t, muxBytes2)
	dispatcher.dispatch(env2, nil)

	// 收集到的 events: 1 个 ToolStart + 1 个 ToolEnd
	if len(c.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(c.events))
	}
	if c.events[0].Kind != agent.EventAgentToolStart {
		t.Errorf("events[0].Kind = %v, want EventAgentToolStart", c.events[0].Kind)
	}
	if c.events[1].Kind != agent.EventAgentToolEnd {
		t.Errorf("events[1].Kind = %v, want EventAgentToolEnd", c.events[1].Kind)
	}
	// ToolEnd 必须回填 Name + Args
	if c.events[1].ToolEnd.Name != "Bash" {
		t.Errorf("ToolEnd.Name = %q, want %q (backfilled)", c.events[1].ToolEnd.Name, "Bash")
	}
	if !strings.Contains(c.events[1].ToolEnd.Args, "ls") {
		t.Errorf("ToolEnd.Args = %q, want contains 'ls'", c.events[1].ToolEnd.Args)
	}
	if c.events[1].ToolEnd.ID != "c-7" {
		t.Errorf("ToolEnd.ID = %q, want c-7", c.events[1].ToolEnd.ID)
	}
}

// ─── Dashboard parity (F-DSH-DASHBOARD-PARITY 2026-08-16) ────────
//
// These tests lock the contract that nightme's dsh bridge surfaces
// the SAME three categories as the dashboard: tool, thinking, reply.
// Pre-fix the reasoning-delta text leaked into textBuf → reply
// stream, and thinking was effectively invisible.

// TestHandleAssistantChunk_TextDelta_AccumulatesOnly
// text-delta goes to textBuf, never emits an event by itself
// (per F-52 granularity contract — flush at message / tool / turn
// boundary). Reasoning buffer MUST stay empty.
func TestHandleAssistantChunk_TextDelta_AccumulatesOnly(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	muxBytes := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"text-delta","index":0,"text":"hello"}}`)
	env, _ := decodeMuxEvent(t, muxBytes)
	dispatcher.dispatch(env, nil)

	// text-delta must NOT emit anything on its own (F-52 boundary flush).
	if len(c.events) != 0 {
		t.Fatalf("text-delta should accumulate only, got %d events", len(c.events))
	}
	// textBuf[0] should have the delta.
	if b, ok := tr.textBuf[0]; !ok {
		t.Error("textBuf[0] should exist after text-delta")
	} else if b.String() != "hello" {
		t.Errorf("textBuf[0] = %q, want %q", b.String(), "hello")
	}
	// reasoningBuf must NOT have the delta.
	if _, ok := tr.reasoningBuf[0]; ok {
		t.Error("reasoningBuf[0] should NOT exist after text-delta")
	}
}

// TestHandleAssistantChunk_ReasoningDelta_GoesToReasoningBuffer
// reasoning-delta MUST accumulate into reasoningBuf (not textBuf)
// and MUST NOT emit any AgentEvent by itself — block-end is the
// single source of truth for thinking emit (to match dashboard's
// folded view).
func TestHandleAssistantChunk_ReasoningDelta_GoesToReasoningBuffer(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	muxBytes := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"reasoning-delta","index":0,"text":"让我想想"}}`)
	env, _ := decodeMuxEvent(t, muxBytes)
	dispatcher.dispatch(env, nil)

	if len(c.events) != 0 {
		t.Fatalf("reasoning-delta must NOT emit, got %d events", len(c.events))
	}
	if b, ok := tr.reasoningBuf[0]; !ok {
		t.Fatal("reasoningBuf[0] should exist after reasoning-delta")
	} else if b.String() != "让我想想" {
		t.Errorf("reasoningBuf[0] = %q, want %q", b.String(), "让我想想")
	}
	if _, ok := tr.textBuf[0]; ok {
		t.Error("textBuf[0] must NOT exist after reasoning-delta (the bug)")
	}
}

// TestHandleAssistantChunk_BlockEnd_Reasoning_EmitsThinking
// block-end{type:"reasoning"} is the single source of truth for
// thinking emit. Output must be "[思考] ..." prefixed so gateway
// translate routes it to OutThinking (not OutReply).
func TestHandleAssistantChunk_BlockEnd_Reasoning_EmitsThinking(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// First accumulate a reasoning-delta so we exercise the
	// "block-end after reasoning-delta" path (the typical case).
	muxBytes1 := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"reasoning-delta","index":0,"text":"让我想想"}}`)
	env1, _ := decodeMuxEvent(t, muxBytes1)
	dispatcher.dispatch(env1, nil)

	// Then the block-end with the assembled reasoning block.
	muxBytes2 := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"block-end","index":0,"block":{"type":"reasoning","text":"让我想想怎么写"}}}`)
	env2, _ := decodeMuxEvent(t, muxBytes2)
	dispatcher.dispatch(env2, nil)

	if len(c.events) != 1 {
		t.Fatalf("block-end{reasoning} should emit exactly 1 event, got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentText {
		t.Errorf("event kind = %v, want EventAgentText", ev.Kind)
	}
	wantPrefix := "[思考] "
	if !strings.HasPrefix(ev.Text, wantPrefix) {
		t.Errorf("thinking event Text = %q, want prefix %q (so gateway → OutThinking)", ev.Text, wantPrefix)
	}
	if !strings.Contains(ev.Text, "让我想想怎么写") {
		t.Errorf("thinking event Text = %q, want contains assembled reasoning", ev.Text)
	}
	// reasoningBuf[idx] should have been cleared after emit.
	if _, ok := tr.reasoningBuf[0]; ok {
		t.Error("reasoningBuf[0] should be deleted after block-end emit")
	}
	// reasoningEmitted[0] should be true.
	if !tr.reasoningEmitted[0] {
		t.Error("reasoningEmitted[0] should be true after block-end emit")
	}
}

// TestHandleAssistantChunk_BlockEnd_Reasoning_DoubleEmit_Suppressed
// If block-end{type:"reasoning"} fires twice for the same index,
// only the first emit lands. Defensive guard against dsh replay /
// reconnect storms.
func TestHandleAssistantChunk_BlockEnd_Reasoning_DoubleEmit_Suppressed(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	for i := 0; i < 2; i++ {
		muxBytes := makeMuxEvent(t, "assistant/chunk",
			`{"chunk":{"type":"block-end","index":0,"block":{"type":"reasoning","text":"same"}}}`)
		env, _ := decodeMuxEvent(t, muxBytes)
		dispatcher.dispatch(env, nil)
	}
	if len(c.events) != 1 {
		t.Errorf("double block-end{reasoning} should emit 1 event total, got %d", len(c.events))
	}
}

// TestHandleAssistantMessage_MixedContent_SplitsReasoningAndReply
// Dashboard parity: assistant/message.content[] is split per-block.
// text blocks → reply EventAgentText (no prefix).
// reasoning blocks → suppressed (block-end already emitted, or will).
// tool-call blocks → suppressed (independent tool/call path).
func TestHandleAssistantMessage_MixedContent_SplitsReasoningAndReply(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	muxBytes := makeMuxEvent(t, "assistant/message", `{
		"message":{
			"role":"assistant",
			"content":[
				{"type":"reasoning","text":"hidden thought"},
				{"type":"text","text":"visible reply"},
				{"type":"tool-call","id":"x","name":"Read","arguments":"{}"}
			]
		}
	}`)
	env, _ := decodeMuxEvent(t, muxBytes)
	dispatcher.dispatch(env, nil)

	if len(c.events) != 1 {
		t.Fatalf("expected exactly 1 event (the text reply), got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentText {
		t.Errorf("event kind = %v, want EventAgentText", ev.Kind)
	}
	if strings.HasPrefix(ev.Text, "[思考] ") {
		t.Errorf("reply text should NOT carry thinking prefix, got %q", ev.Text)
	}
	if ev.Text != "visible reply" {
		t.Errorf("event Text = %q, want %q", ev.Text, "visible reply")
	}
}

// TestHandleAssistantMessage_TextOnly_EmitsReply
// Sanity check: a turn with only text content (no reasoning, no
// tool-call) must produce an EventAgentText so the reply surfaces
// even without a tool-call or turn/end boundary flush.
//
// Pre-fix, this case delivered the text only at turn/end via
// EventAgentResult.Text — i.e. the user saw nothing until the turn
// settled.
func TestHandleAssistantMessage_TextOnly_EmitsReply(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	muxBytes := makeMuxEvent(t, "assistant/message", `{
		"message":{"role":"assistant","content":[{"type":"text","text":"hello world"}]}
	}`)
	env, _ := decodeMuxEvent(t, muxBytes)
	dispatcher.dispatch(env, nil)

	if len(c.events) != 1 {
		t.Fatalf("text-only assistant/message should emit 1 event, got %d", len(c.events))
	}
	if c.events[0].Text != "hello world" {
		t.Errorf("Text = %q, want %q", c.events[0].Text, "hello world")
	}
	if strings.HasPrefix(c.events[0].Text, "[思考] ") {
		t.Error("reply must not be misrouted as thinking")
	}
}

// TestHandleTurnStart_ResetsReasoningBuffer
// turn/start must wipe reasoningBuf + reasoningEmitted so the next
// turn's reasoning doesn't leak across turns.
func TestHandleTurnStart_ResetsReasoningBuffer(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// pre-fill reasoning state
	muxBytes := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"reasoning-delta","index":0,"text":"leftover"}}`)
	env, _ := decodeMuxEvent(t, muxBytes)
	dispatcher.dispatch(env, nil)
	if len(tr.reasoningBuf) == 0 {
		t.Fatal("precondition: reasoningBuf should have an entry")
	}

	// turn/start must reset it
	muxBytes2 := makeMuxEvent(t, "turn/start", `{"turn":2}`)
	env2, _ := decodeMuxEvent(t, muxBytes2)
	dispatcher.dispatch(env2, nil)

	if len(tr.reasoningBuf) != 0 {
		t.Errorf("reasoningBuf should be cleared on turn/start, got %d entries", len(tr.reasoningBuf))
	}
	if len(tr.reasoningEmitted) != 0 {
		t.Errorf("reasoningEmitted should be cleared on turn/start, got %d entries", len(tr.reasoningEmitted))
	}
}

// TestHandleAssistantMessage_ResultTextNotDone_F52RegressionLock
// Regression lock for the bug found in review (2026-08-16): after
// refactoring handleAssistantMessage to emit text per-block (for
// dashboard-parity streaming reply), the F-52 state-machine fields
// (pendingText / lastText / textDelivered) MUST still be updated so
// handleTurnEnd's Result.Text fallback carries the reply text instead
// of falling back to the "Done." placeholder.
//
// Pre-bugfix: handleAssistantMessage emitted per-block but didn't
// update pendingText/lastText → handleTurnEnd took the
// text=="" → text="Done." path on every turn. Result: receipt's
// Result.Text always said "Done." regardless of what the assistant
// said. UI visible as a phantom "Done." card in the result line.
func TestHandleAssistantMessage_ResultTextNotDone_F52RegressionLock(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	mux := makeMuxEvent(t, "assistant/message", `{
		"message":{"role":"assistant","content":[{"type":"text","text":"hello world"}]}
	}`)
	env, _ := decodeMuxEvent(t, mux)
	dispatcher.dispatch(env, nil)

	// F-52 side effects must be set
	if tr.pendingText != "hello world" {
		t.Errorf("tr.pendingText = %q, want %q (so handleTurnEnd Result.Text carries reply)", tr.pendingText, "hello world")
	}
	if tr.lastText != "hello world" {
		t.Errorf("tr.lastText = %q, want %q (handleTurnEnd fallback)", tr.lastText, "hello world")
	}
	if !tr.textDelivered {
		t.Errorf("tr.textDelivered = false; handleToolCall would re-flush the same text as a duplicate")
	}

	// Now turn/end — Result.Text must carry "hello world", not "Done."
	c2 := &collectDeliver{}
	dispatcher2 := newDispatcher(tr, st, nil, c2.deliver)
	muxEnd := makeMuxEvent(t, "turn/end", `{"stopReason":"stop"}`)
	envEnd, _ := decodeMuxEvent(t, muxEnd)
	dispatcher2.dispatch(envEnd, nil)

	var result *agent.AgentEvent
	for i := range c2.events {
		if c2.events[i].Kind == agent.EventAgentResult {
			result = &c2.events[i]
			break
		}
	}
	if result == nil {
		t.Fatal("no EventAgentResult from turn/end")
	}
	if result.Result.Text == "Done." {
		t.Errorf("EventAgentResult.Text = %q, want reply text (regression: handleAssistantMessage didn't update F-52 state)", result.Result.Text)
	}
	if result.Result.Text != "hello world" {
		t.Errorf("EventAgentResult.Text = %q, want %q", result.Result.Text, "hello world")
	}
}

// TestHandleAssistantMessage_TextDeliveredTrue_PreventsDoubleFlush
// Locks that after a per-block emit, handleToolCall's pendingText
// flush is suppressed (via textDelivered=true). Otherwise the same
// text would emit twice: once from handleAssistantMessage (per-block)
// and once from handleToolCall (F-52 boundary flush).
func TestHandleAssistantMessage_TextDeliveredTrue_PreventsDoubleFlush(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// assistant/message with text → emits per-block + sets pendingText
	mux := makeMuxEvent(t, "assistant/message", `{
		"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}
	}`)
	env, _ := decodeMuxEvent(t, mux)
	dispatcher.dispatch(env, nil)

	// Reset collector so we only observe what comes next
	c.events = nil

	// tool/call arrives — must NOT re-flush pendingText (textDelivered=true)
	muxTool := makeMuxEvent(t, "tool/call", `{"callId":"c-1","name":"Bash","arguments":"{}"}`)
	envTool, _ := decodeMuxEvent(t, muxTool)
	dispatcher.dispatch(envTool, nil)

	// Filter: should have ONE EventAgentToolStart only, NO EventAgentText
	for _, ev := range c.events {
		if ev.Kind == agent.EventAgentText {
			t.Errorf("tool/call emitted EventAgentText %q (duplicate — textDelivered guard failed)", ev.Text)
		}
	}
}

// TestHandleAssistantChunk_ReasoningThenBlockEnd_DoesNotDoubleEmit
// End-to-end dashboard parity: reasoning-delta accumulates silently,
// block-end{type:"reasoning"} emits once. A subsequent
// assistant/message with the same reasoning content MUST NOT re-emit
// (the block-end is the single source of truth).
func TestHandleAssistantChunk_ReasoningThenBlockEnd_DoesNotDoubleEmit(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// 1) reasoning-delta — accumulates
	mux1 := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"reasoning-delta","index":0,"text":"thinking..."}}`)
	env1, _ := decodeMuxEvent(t, mux1)
	dispatcher.dispatch(env1, nil)

	// 2) block-end{reasoning} — emits
	mux2 := makeMuxEvent(t, "assistant/chunk",
		`{"chunk":{"type":"block-end","index":0,"block":{"type":"reasoning","text":"thinking..."}}}`)
	env2, _ := decodeMuxEvent(t, mux2)
	dispatcher.dispatch(env2, nil)

	// 3) assistant/message carries the SAME reasoning as content
	mux3 := makeMuxEvent(t, "assistant/message", `{
		"message":{"role":"assistant","content":[
			{"type":"reasoning","text":"thinking..."},
			{"type":"text","text":"the reply"}
		]}
	}`)
	env3, _ := decodeMuxEvent(t, mux3)
	dispatcher.dispatch(env3, nil)

	// Expected emit sequence: [思考] thinking... (from block-end)
	//                            + "the reply" (text block from message)
	// The reasoning content in the message MUST be suppressed.
	if len(c.events) != 2 {
		t.Fatalf("expected 2 events ([思考] + reply), got %d: %+v", len(c.events), c.events)
	}
	if !strings.HasPrefix(c.events[0].Text, "[思考] ") {
		t.Errorf("events[0] should be thinking, got %q", c.events[0].Text)
	}
	if c.events[1].Text != "the reply" {
		t.Errorf("events[1] should be reply, got %q", c.events[1].Text)
	}
}

// TestDispatcher_SessionTitle_DoesNotDeadlock locks the handler
// contract: session/title runs under dispatcher.dispatch which
// already holds wireState.mu. Re-locking that mutex (the 2026-08-16
// handleSessionTitle bug) freezes the mux pump and history backfill
// so Feishu never receives assistant patches.
func TestDispatcher_SessionTitle_DoesNotDeadlock(t *testing.T) {
	tr := newTranslator("dsh", "/tmp")
	st := newWireState()
	dispatcher := newDispatcher(tr, st, nil, func(agent.AgentEvent) {})

	muxBytes := makeMuxEvent(t, "session/title", `{"title":"test-new-01"}`)
	env, _ := decodeMuxEvent(t, muxBytes)

	done := make(chan struct{})
	go func() {
		dispatcher.dispatch(env, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch(session/title) deadlocked; handler must not re-lock wireState.mu")
	}
	if st.title != "test-new-01" {
		t.Fatalf("title = %q, want test-new-01", st.title)
	}
}

// TestDispatcher_ApprovalAsked_DoesNotDeliverUnderLock locks the
// same contract as session/title: the handler must return events
// for post-unlock deliver. Calling handleInlineApproval (or
// driver.deliver) while dispatch holds wireState.mu deadlocks any
// deliver that re-enters that mutex — and would stall the mux
// pump the same way the 2026-08-16 title bug did.
func TestDispatcher_ApprovalAsked_DoesNotDeliverUnderLock(t *testing.T) {
	tr := newTranslator("dsh", "/tmp")
	st := newWireState()
	drv := &driver{
		pendingApprovals: map[string]chan string{},
		closed:           make(chan struct{}),
	}
	defer close(drv.closed)

	var got []agent.AgentEvent
	dispatcher := newDispatcher(tr, st, drv, func(ev agent.AgentEvent) {
		st.mu.Lock()
		st.mu.Unlock()
		got = append(got, ev)
	})

	muxBytes := makeMuxEvent(t, "approval/asked",
		`{"toolCallId":"c-1","toolName":"Bash","action":"run","options":[{"label":"approve"},{"label":"decline"}]}`)
	env, _ := decodeMuxEvent(t, muxBytes)

	done := make(chan struct{})
	go func() {
		dispatcher.dispatch(env, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch(approval/asked) deadlocked; handler must deliver after unlock")
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 permission via dispatcher deliver", len(got))
	}
	if got[0].Kind != agent.EventAgentPermission {
		t.Fatalf("kind = %v, want permission", got[0].Kind)
	}
	if got[0].Permission == nil || got[0].Permission.Tool != "Bash" {
		t.Fatalf("permission = %+v, want tool Bash", got[0].Permission)
	}
}

// TestDispatcher_StepBoundary_DoesNotEmitTask: dashboard TodoPanel
// is todo/write + todos projection, not step/*. step/start|end are
// inference-cycle bounds ({turn,step}) for sessionStats. Emitting
// synthetic "Step N" OutTask* rows was a mis-alignment.
func TestDispatcher_StepBoundary_DoesNotEmitTask(t *testing.T) {
	tr := newTranslator("dsh", "/tmp")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	for _, typ := range []string{"step/start", "step/end"} {
		env, _ := decodeMuxEvent(t, makeMuxEvent(t, typ, `{"turn":1,"step":1}`))
		dispatcher.dispatch(env, nil)
	}
	if len(c.events) != 0 {
		t.Fatalf("step/* emitted %d events %v, want none (TodoPanel is todo/write)", len(c.events), c.events)
	}

	env, _ := decodeMuxEvent(t, makeMuxEvent(t, "todo/write",
		`{"todos":[{"content":"Read docs","status":"pending"}]}`))
	dispatcher.dispatch(env, nil)
	env, _ = decodeMuxEvent(t, makeMuxEvent(t, "step/start", `{"turn":1,"step":1}`))
	dispatcher.dispatch(env, nil)
	if len(c.events) != 1 {
		t.Fatalf("got %d events, want 1 todo create", len(c.events))
	}
	if c.events[0].Kind != agent.EventAgentTaskCreate {
		t.Fatalf("kind = %v, want TaskCreate", c.events[0].Kind)
	}
	if len(c.events[0].TaskList.Items) != 1 || c.events[0].TaskList.Items[0].Subject != "Read docs" {
		t.Fatalf("todo snapshot = %+v, want subject Read docs", c.events[0].TaskList)
	}
}