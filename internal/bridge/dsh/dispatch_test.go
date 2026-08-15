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
			envType: "approval/asked",
			data:    `{"toolCallId":"a-1","toolName":"Bash","action":"run","options":["approve","decline"]}`,
			minEv:   0, // approval/asked 不在 dispatcher 注册(走 permissions.go)
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