// state_test.go — wireState 单测 (F-DSH-CHAT-001 §4.3)
//
// wireState 是 dsh bridge 内部的真值镜像(由 View + Projection + raw event
// 三路喂数据),通过 applyEvent / applyProjection 产出 AgentEvent 序列。
// 本文件锁住:
//   - todo/write 含 ID 字段时,wireState.tasks 能按 ID 索引
//   - todo/write 不含 ID 时不 panic(graceful),降级按 Content 作 ID
//   - applyProjection 覆盖 wireState.tasks
//   - applyEvent 返回的 AgentEvent 序列包含正确的 EventAgentTaskCreate

package dsh

import (
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// decodeMuxEvent 把 muxSessionEvent 的 raw bytes 解析成 (env, view) 对。
// 拆 envelope 让 handler 直接拿到内部 sessionEventEnvelope,跳过中间层。
func decodeMuxEvent(t *testing.T, muxBytes []byte) (sessionEventEnvelope, json.RawMessage) {
	t.Helper()
	var muxEv muxSessionEvent
	if err := json.Unmarshal(muxBytes, &muxEv); err != nil {
		t.Fatalf("decode muxSessionEvent: %v", err)
	}
	var env sessionEventEnvelope
	if err := json.Unmarshal(muxEv.Event, &env); err != nil {
		t.Fatalf("decode inner envelope: %v", err)
	}
	return env, muxEv.View
}

// makeMuxEvent 构建一帧 muxSessionEvent 的 raw JSON,带内嵌 sessionEventEnvelope。
func makeMuxEvent(t *testing.T, envType string, dataJSON string) []byte {
	t.Helper()
	outer := `{"sessionId":"s-1","event":{"type":"` + envType + `","data":` + dataJSON + `}}`
	return []byte(outer)
}

// TestWireState_ApplyEvent_TodoWritePopulatesIDAndActiveForm
// 验证 D1(AgentTaskItem.ID 缺失)被修复。
// WIRE-PROBE-REQUIRED: 字段名 `id` / `activeForm` 是从 dsh TodoItem 注释
// 推断的,需要真实 wire probe 校准。如果 probe 后字段名不同,改 todoItem
// struct 的 json tag 即可,本测试的断言也要相应调整。
func TestWireState_ApplyEvent_TodoWritePopulatesIDAndActiveForm(t *testing.T) {
	st := newWireState()
	data := `{"items":[
		{"id":"t-1","content":"Read docs","activeForm":"Reading docs","status":"completed"},
		{"id":"t-2","content":"Write code","activeForm":"Writing code","status":"in_progress"},
		{"id":"t-3","content":"Run tests","activeForm":"","status":"pending"}
	]}`
	muxBytes := makeMuxEvent(t, "todo/write", data)
	env, view := decodeMuxEvent(t, muxBytes)
	if len(view) != 0 {
		t.Fatalf("expected empty View for todo/write, got %d bytes", len(view))
	}

	events := st.applyEvent(env, view)
	if len(events) != 1 {
		t.Fatalf("expected 1 AgentEvent, got %d", len(events))
	}
	if events[0].Kind != agent.EventAgentTaskCreate {
		t.Fatalf("expected EventAgentTaskCreate, got %v", events[0].Kind)
	}
	items := events[0].TaskList.Items
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	// D1 验证: ID 必填
	wantIDs := []string{"t-1", "t-2", "t-3"}
	for i, want := range wantIDs {
		if items[i].ID != want {
			t.Errorf("items[%d].ID = %q, want %q", i, items[i].ID, want)
		}
	}
	// Subject
	if items[0].Subject != "Read docs" {
		t.Errorf("items[0].Subject = %q, want %q", items[0].Subject, "Read docs")
	}
	// ActiveForm 透传
	if items[0].ActiveForm != "Reading docs" {
		t.Errorf("items[0].ActiveForm = %q, want %q", items[0].ActiveForm, "Reading docs")
	}
	if items[1].ActiveForm != "Writing code" {
		t.Errorf("items[1].ActiveForm = %q, want %q", items[1].ActiveForm, "Writing code")
	}
	// ActiveForm 空字符串 OK
	if items[2].ActiveForm != "" {
		t.Errorf("items[2].ActiveForm = %q, want empty", items[2].ActiveForm)
	}
	// Status 映射
	if items[0].Status != agent.TaskCompleted {
		t.Errorf("items[0].Status = %v, want TaskCompleted", items[0].Status)
	}
	if items[1].Status != agent.TaskInProgress {
		t.Errorf("items[1].Status = %v, want TaskInProgress", items[1].Status)
	}
	if items[2].Status != agent.TaskPending {
		t.Errorf("items[2].Status = %v, want TaskPending", items[2].Status)
	}

	// wireState.tasks map 应该能按 ID 索引
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.tasks) != 3 {
		t.Errorf("wireState.tasks size = %d, want 3", len(st.tasks))
	}
	if st.tasks["t-1"].Content != "Read docs" {
		t.Errorf("st.tasks[t-1].Content = %q", st.tasks["t-1"].Content)
	}
}

// TestWireState_ApplyEvent_TodoWriteWithoutID_FallsBackGracefully
// 验证 B3 缓解措施:TodoItem 真没 ID 时不 panic,按 Content 作 fallback ID。
func TestWireState_ApplyEvent_TodoWriteWithoutID_FallsBackGracefully(t *testing.T) {
	st := newWireState()
	// 故意没 id / activeForm 字段(模拟老版 wire 或不同 dsh 版本)
	data := `{"items":[
		{"content":"Task A","status":"pending"},
		{"content":"Task B","status":"completed"}
	]}`
	muxBytes := makeMuxEvent(t, "todo/write", data)
	env, view := decodeMuxEvent(t, muxBytes)

	events := st.applyEvent(env, view)
	if len(events) != 1 {
		t.Fatalf("expected 1 AgentEvent, got %d", len(events))
	}
	items := events[0].TaskList.Items
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// fallback ID 应该用 Content 作 key
	if items[0].ID != "Task A" {
		t.Errorf("items[0].ID (fallback) = %q, want %q", items[0].ID, "Task A")
	}
	if items[1].ID != "Task B" {
		t.Errorf("items[1].ID (fallback) = %q, want %q", items[1].ID, "Task B")
	}
	// Subject / Status 正常映射
	if items[0].Subject != "Task A" {
		t.Errorf("items[0].Subject = %q", items[0].Subject)
	}
	if items[1].Status != agent.TaskCompleted {
		t.Errorf("items[1].Status = %v", items[1].Status)
	}
}

// TestWireState_ApplyProjection_UpdatesTasks
// 验证 D3(session/projection 接通):
// projection 帧带 todo 快照,wireState 收到后产出 EventAgentTaskCreate。
//
// WIRE-PROBE-REQUIRED: projectionEnvelope 字段名(projection / name / value)
// 是从 dsh.md 推断的,需要真实 wire probe 校准。
func TestWireState_ApplyProjection_UpdatesTasks(t *testing.T) {
	st := newWireState()
	proj := projectionEnvelope{
		Projection: "todo",
		Value: []byte(`{
			"items":[
				{"id":"p-1","content":"From projection","status":"completed"}
			]
		}`),
	}
	events := st.applyProjection(proj)
	if len(events) != 1 {
		t.Fatalf("expected 1 AgentEvent, got %d", len(events))
	}
	if events[0].Kind != agent.EventAgentTaskCreate {
		t.Fatalf("expected EventAgentTaskCreate, got %v", events[0].Kind)
	}
	items := events[0].TaskList.Items
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "p-1" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "p-1")
	}
	if items[0].Subject != "From projection" {
		t.Errorf("items[0].Subject = %q", items[0].Subject)
	}

	// 验证 projection 的 items 已写入 wireState.tasks(后续 state.diff 可计算)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tasks["p-1"].Content != "From projection" {
		t.Errorf("st.tasks[p-1].Content = %q", st.tasks["p-1"].Content)
	}
}

// TestWireState_ApplyProjection_UnknownProjection_NoOp
// 未知 projection 名字(如 "title" / "experiment")不 panic,返 nil。
// WIRE-PROBE-REQUIRED: 真 wire 可能下发 "title" 等,目前返回 nil 是 graceful,
// P3 会接 title → EventAgentTaskCreate(用于 session header 刷新)。
func TestWireState_ApplyProjection_UnknownProjection_NoOp(t *testing.T) {
	st := newWireState()
	proj := projectionEnvelope{
		Projection: "title",
		Value:      []byte(`{"title":"My Session"}`),
	}
	events := st.applyProjection(proj)
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown projection, got %d", len(events))
	}
	// P3 之前 title 不更新;若 P3 已落,这里改为 st.title == "My Session"
}

// TestWireState_ApplyTodoWrite_DoesNotSilentlyEmitEmptyItems
//
// REGRESSION LOCK for the "todo list isn't converted to OutTaskCreate"
// report: when dsh wire's todoItem field names DON'T match what
// todoItem struct expects (per protocol.go's WIRE-PROBE-REQUIRED
// comments — fields are inferred, not validated), every item gets
// silently skipped (ID="" and Content="" both empty), and
// applyTodoWriteLocked still returns EventAgentTaskCreate with
// Items=[] — which the Feishu adapter then interprets as
// "clear the checklist" and wipes the user's existing tasks.
//
// This test pins the wire format the bridge was DESIGNED for
// (id/content/activeForm/status). If dsh's real wire uses different
// field names (e.g. uuid/subject/taskStatus per the dsh probe
// notes), applyTodoWriteLocked MUST be fixed to match — silent
// skip + phantom clear is the failure mode.
//
// Each table entry below is a candidate real-world dsh wire
// shape; the "expected" entry is what the code was built against.
// If a probe updates the wire shape, update the struct tags AND
// the table entry together, or this test will (correctly) fail
// and force the issue into the open instead of letting the user
// discover "my todo list vanished" in production.
func TestWireState_ApplyTodoWrite_DoesNotSilentlyEmitEmptyItems(t *testing.T) {
	cases := []struct {
		name      string
		wireItems string // raw JSON for the items[] array
		wantIDs   []string
		wantSubj  []string
	}{
		{
			name: "designed_schema",
			wireItems: `[
				{"id":"t-1","content":"Read README","activeForm":"Reading README","status":"completed"},
				{"id":"t-2","content":"Write code","activeForm":"Writing code","status":"in_progress"},
				{"id":"t-3","content":"Run tests","status":"pending"}
			]`,
			wantIDs:  []string{"t-1", "t-2", "t-3"},
			wantSubj: []string{"Read README", "Write code", "Run tests"},
		},
		{
			// dsh.md §6.3 lists wire shape as
			// {event:{items:[{content, status}]}} — no id field.
			// Graceful fallback should use content as key.
			name: "doc_quoted_schema_no_id",
			wireItems: `[
				{"content":"Read README","status":"pending"},
				{"content":"Write code","status":"in_progress"}
			]`,
			wantIDs:  []string{"Read README", "Write code"},
			wantSubj: []string{"Read README", "Write code"},
		},
		{
			// Defensive: status field might be absent or use a
			// different name. todoStatusToTaskStatus already maps
			// unknown status -> TaskPending, so this is graceful.
			name: "unknown_status_falls_to_pending",
			wireItems: `[
				{"id":"x-1","content":"Task X","status":"weird_status_value"}
			]`,
			wantIDs:  []string{"x-1"},
			wantSubj: []string{"Task X"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newWireState()
			data := `{"items":` + tc.wireItems + `}`
			muxBytes := makeMuxEvent(t, "todo/write", data)
			env, view := decodeMuxEvent(t, muxBytes)

			events := st.applyEvent(env, view)

			// Every entry must produce at least one event.
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if events[0].Kind != agent.EventAgentTaskCreate {
				t.Fatalf("event kind = %v, want EventAgentTaskCreate",
					events[0].Kind)
			}
			if events[0].TaskList == nil {
				t.Fatal("TaskList payload is nil")
			}

			// CRITICAL: every item in the wire must end up in the
			// emitted TaskList. An empty Items slice here means the
			// Feishu adapter will interpret this as "clear the
			// checklist" and wipe whatever was previously shown.
			if len(events[0].TaskList.Items) != len(tc.wantIDs) {
				t.Fatalf("emitted items = %d, want %d — this is the\n"+
					"silent-skip failure mode: every item in the wire\n"+
					"was zero-valued (e.g. field names don't match the\n"+
					"struct tags), so applyTodoWriteLocked skipped them\n"+
					"all and still emitted an empty EventAgentTaskCreate,\n"+
					"which the Feishu adapter will treat as 'clear the\n"+
					"checklist'. Check that dsh wire's todoItem fields\n"+
					"match the json tags on internal/bridge/dsh/protocol.go\n"+
					"todoItem (WIRE-PROBE-REQUIRED).",
					len(events[0].TaskList.Items), len(tc.wantIDs))
			}
			for i, want := range tc.wantIDs {
				if events[0].TaskList.Items[i].ID != want {
					t.Errorf("items[%d].ID = %q, want %q",
						i, events[0].TaskList.Items[i].ID, want)
				}
				if events[0].TaskList.Items[i].Subject != tc.wantSubj[i] {
					t.Errorf("items[%d].Subject = %q, want %q",
						i, events[0].TaskList.Items[i].Subject,
						tc.wantSubj[i])
				}
			}
		})
	}
}

// TestWireState_ApplyTodoProjection_FieldNameDrift_SurfacesFailure
//
// Same failure mode as TestWireState_ApplyTodoWrite_FieldNameDrift_SurfacesFailure
// but for the session/projection frame's todo snapshot path. The
// projection handler used to silently continue on drifted fields
// (no dLog at all) — even worse than the todo/write path. Pin
// the fix so a future wire probe validates both paths together.
func TestWireState_ApplyTodoProjection_FieldNameDrift_SurfacesFailure(t *testing.T) {
	st := newWireState()
	proj := projectionEnvelope{
		Projection: "todo",
		Value: []byte(`{
			"items":[
				{"uuid":"u-1","subject":"Read README","taskStatus":"pending"},
				{"uuid":"u-2","subject":"Write code","taskStatus":"in_progress"}
			]
		}`),
	}
	events := st.applyProjection(proj)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := len(events[0].TaskList.Items); got != 0 {
		t.Fatalf("with drifted wire fields, projection items should be 0; got %d", got)
	}
	// UnknownCount MUST bump so ops sees the failure via DumpWireStats.
	unknownTotal, _ := st.DumpWireStats()
	if unknownTotal == 0 {
		t.Fatal("expected unknownCount > 0 after drifted-projection skip")
	}
}

// TestWireState_ApplyTodoWrite_FieldNameDrift_SurfacesFailure
//
// The hard scenario: the dsh wire uses field names that DON'T match
// what todoItem struct expects. Per protocol.go's WIRE-PROBE-REQUIRED
// comments, the json tags below are inferred from the dsh source
// comment, not validated against a real wire. If the real wire uses
// `uuid` instead of `id`, or `subject` instead of `content`, every
// item silently zero-fills and gets skipped — and the function still
// emits EventAgentTaskCreate with Items=[].
//
// This test simulates that failure mode and verifies:
//   1. The items DO get skipped (so the production failure mode
//      is detectable in tests, not hidden behind a passing test).
//   2. wireState.unknownCount is bumped, so DumpWireStats surfaces
//      the failure to ops via `nightme debug dsh dump-wire`.
//
// If dsh's real wire is later probed and the field names are confirmed
// wrong, the struct tags MUST be updated — keeping this test
// passing-by-accident would re-introduce the silent-skip production
// failure.
func TestWireState_ApplyTodoWrite_FieldNameDrift_SurfacesFailure(t *testing.T) {
	st := newWireState()
	// Hypothetical real dsh wire with different field names.
	driftedItems := `[
		{"uuid":"u-1","subject":"Read README","taskStatus":"pending"},
		{"uuid":"u-2","subject":"Write code","taskStatus":"in_progress"}
	]`
	data := `{"items":` + driftedItems + `}`
	muxBytes := makeMuxEvent(t, "todo/write", data)
	env, view := decodeMuxEvent(t, muxBytes)

	events := st.applyEvent(env, view)

	// With drifted field names, all items zero-fill and get
	// skipped (ID="" AND Content=""), so the emitted
	// EventAgentTaskCreate has Items=[] — the EXACT failure mode
	// that wipes the user's checklist. Pin this so anyone who
	// tries to "fix" the test to ignore the failure will see the
	// failure mode spelled out loud.
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := len(events[0].TaskList.Items); got != 0 {
		t.Fatalf("with drifted wire fields, items should be 0 (silent\n"+
			"skip + phantom clear is the failure mode); got %d.\n"+
			"This means someone updated the test fixtures to match a\n"+
			"probed real wire — which is good — but please verify\n"+
			"the production code path also got the struct tag\n"+
			"update, otherwise users will see their todo list\n"+
			"vanish on every todo/write.", got)
	}

	// The fix: when items get skipped due to wire drift, the
	// silent failure must be visible in DumpWireStats so ops can
	// see "bridge isn't picking up dsh's todos" via
	// `nightme debug dsh dump-wire` (unknownCount column).
	unknownTotal, _ := st.DumpWireStats()
	if unknownTotal == 0 {
		t.Fatal("expected unknownCount > 0 after drifted-wire skip —\n"+
			"the skip path must surface the failure mode, otherwise\n"+
			"the only signal is 'user reports empty todo list' with\n"+
			"zero ops-side telemetry.")
	}

	// Sanity: a real-shape todo/write on the same state should
	// NOT bump unknownCount (only drift triggers it).
	realData := `{"items":[{"id":"r-1","content":"Real","status":"pending"}]}`
	realMuxBytes := makeMuxEvent(t, "todo/write", realData)
	realEnv, realView := decodeMuxEvent(t, realMuxBytes)
	st.applyEvent(realEnv, realView)
	unknownAfterReal, _ := st.DumpWireStats()
	if unknownAfterReal != unknownTotal {
		t.Errorf("unknownCount bumped on a real-shape todo/write\n"+
			"(before=%d, after=%d) — the counter should only fire\n"+
			"on wire drift, not on every applyTodoWrite call.",
			unknownTotal, unknownAfterReal)
	}
}

// TestWireState_TasksMap_UpdateOverwritesByID
// 验证 todo/write 多次下发,wireState.tasks 按 ID 更新而不是堆叠。
func TestWireState_TasksMap_UpdateOverwritesByID(t *testing.T) {
	st := newWireState()
	// 第一帧:写 2 个 task
	data1 := `{"items":[
		{"id":"x-1","content":"First","status":"pending"},
		{"id":"x-2","content":"Second","status":"pending"}
	]}`
	muxBytes := makeMuxEvent(t, "todo/write", data1)
	env, view := decodeMuxEvent(t, muxBytes)
	st.applyEvent(env, view)

	// 第二帧:更新 x-1 状态
	data2 := `{"items":[
		{"id":"x-1","content":"First (renamed)","status":"completed"},
		{"id":"x-2","content":"Second","status":"pending"}
	]}`
	muxBytes2 := makeMuxEvent(t, "todo/write", data2)
	env2, view2 := decodeMuxEvent(t, muxBytes2)
	st.applyEvent(env2, view2)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.tasks) != 2 {
		t.Errorf("tasks map size = %d, want 2 (not stacked)", len(st.tasks))
	}
	if st.tasks["x-1"].Content != "First (renamed)" {
		t.Errorf("x-1 not updated: %q", st.tasks["x-1"].Content)
	}
	if st.tasks["x-1"].Status != "completed" {
		t.Errorf("x-1 status = %q", st.tasks["x-1"].Status)
	}
}