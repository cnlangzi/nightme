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