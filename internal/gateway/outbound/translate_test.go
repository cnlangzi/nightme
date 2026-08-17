package outbound

import (
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestTranslate covers the AgentEvent → messages.OutboundMessage mapping
// for every EventKind that has its own OutboundKind (i.e. events
// that actually emit something). Terminal / dropped kinds
// (EventAgentDone, EventAgentError, default) are tested implicitly
// by the existing tests in cmd/gateway_test.go.
//
// Tests follow the pattern: build AgentEvent, call Translate,
// assert Kind + Text + key fields. We don't assert on every field —
// the bridge / translator contract is that producers + consumers
// agree on field names; consumers are the source of truth for which
// fields are mandatory (see feishu/adapter_test.go).

func TestTranslate_EventText(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"})
	if !ok || msg.Kind != messages.OutReply || msg.Text != "hello" {
		t.Errorf("got (%v, %v) text=%q, want (OutReply=true, hello)", msg.Kind, ok, msg.Text)
	}
}

func TestTranslate_EventText_ThinkingPrefix(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] thinking…"})
	if !ok || msg.Kind != messages.OutThinking || msg.Text != "thinking…" {
		t.Errorf("got kind=%v text=%q, want OutThinking / thinking…", msg.Kind, msg.Text)
	}
}

func TestTranslate_EventText_EmptyDropped(t *testing.T) {
	_, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "  "})
	if ok {
		t.Error("empty / whitespace-only text should drop, not emit")
	}
}

func TestTranslate_EventResult(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:       "完成",
			DurationMs: 1234,
			Subtype:    "success",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("expected translate to emit")
	}
	if msg.Kind != messages.OutResult {
		t.Errorf("Kind = %v, want OutResult", msg.Kind)
	}
	if msg.Text != "完成" {
		t.Errorf("Text = %q, want 完成", msg.Text)
	}
	// §1.4 cleanup: Result fields flow through the typed
	// OutboundMessage.Result payload, not Meta.
	if msg.Result == nil {
		t.Fatal("msg.Result is nil; outbound should populate the typed AgentResultEvent payload")
	}
	if msg.Result.DurationMs != 1234 {
		t.Errorf("Result.DurationMs = %v, want 1234", msg.Result.DurationMs)
	}
	if msg.Result.Subtype != "success" {
		t.Errorf("Result.Subtype = %v, want success", msg.Result.Subtype)
	}
	if msg.Err != nil {
		t.Error("Result.IsError = true; want false")
	}
}

func TestTranslate_EventResult_EmptyDropped(t *testing.T) {
	// Empty text + IsError=false → no useful content; drop.
	in := agent.AgentEvent{
		Kind:   agent.EventAgentResult,
		Result: &agent.AgentResultEvent{Text: "", Subtype: "success"},
	}
	if _, ok := Translate("chat1", in); ok {
		t.Error("empty Result with !IsError should drop")
	}
}

func TestTranslate_EventResult_ErrorKept(t *testing.T) {
	// Empty text + IsError=true → kept so channels can flip header
	// to error state.
	in := agent.AgentEvent{
		Kind:   agent.EventAgentResult,
		Err:    errors.New("error"),
		Result: &agent.AgentResultEvent{Text: "", Subtype: "error_max_turns"},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != messages.OutResult {
		t.Errorf("IsError=true should keep the event; got kind=%v ok=%v", msg.Kind, ok)
	}
}

// TestTranslate_EventResult_CoLocatesUsage verifies that an
// EventAgentResult carrying usage (via AgentResultEvent.Usage, the
// single-event design that replaced the EventAgentResult + EventUsage
// pair) gets Translated into an OutboundMessage with Usage
// populated on the SAME out, not a separate OutUsage kind.
func TestTranslate_EventResult_CoLocatesUsage(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "完成",
			Subtype: "success",
			Usage: &agent.UsageInfo{
				InputTokens:          100,
				OutputTokens:         200,
				CacheReadInputTokens: 30,
				CostUSD:              0.001,
			},
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("expected translate to emit")
	}
	if msg.Kind != messages.OutResult {
		t.Errorf("Kind = %v, want OutResult", msg.Kind)
	}
	if msg.Usage == nil {
		t.Fatal("msg.Usage is nil; outbound should populate it from AgentResultEvent.Usage")
	}
	if msg.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %v, want 100", msg.Usage)
	}
	if msg.Usage.OutputTokens != 200 {
		t.Errorf("Usage.OutputTokens = %v, want 200", msg.Usage)
	}
	if msg.Usage.CacheReadInputTokens != 30 {
		t.Errorf("Usage.CacheReadInputTokens = %v, want 30", msg.Usage)
	}
	if msg.Usage.CostUSD != 0.001 {
		t.Errorf("Usage.CostUSD = %v, want 0.001", msg.Usage)
	}
	if msg.Text != "完成" {
		t.Errorf("Text = %q, want 完成", msg.Text)
	}
}

// TestTranslate_EventResult_NilUsageFine: a Result event with no
// usage (zero-usage turn / synthetic message) still translates.
// OutboundMessage.Usage stays nil — the runtime is a passive
// pass-through, so a nil Usage just means the channel footer
// omits Line 2 for this event.
func TestTranslate_EventResult_NilUsageFine(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "ok",
			Subtype: "success",
			// Usage intentionally nil
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("expected translate to emit")
	}
	if msg.Usage != nil {
		t.Errorf("msg.Usage = %+v, want nil (Result.Usage was nil)", msg.Usage)
	}
}

func TestTranslate_EventToolEnd_CarriesArgs(t *testing.T) {
	// F-34 review fix (architecture feedback 2026-08-04):
	// AgentToolEndEvent.Args flows through outbound into
	// OutboundMessage.Tool.Args (typed ToolInfo) so channel
	// renderers can produce type-aware thread-reply summaries
	// without mining Meta for implicit per-tool keys.
	in := agent.AgentEvent{
		Kind: agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{
			Name:   "Read",
			Args:   "/foo.go",
			Output: "47 lines",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != messages.OutToolEnd {
		t.Fatalf("got (%v, %v), want (OutToolEnd, true)", msg.Kind, ok)
	}
	if msg.Tool == nil {
		t.Fatal("msg.Tool is nil; outbound should populate the unified ToolInfo")
	}
	if msg.Tool.Name != "Read" {
		t.Errorf("Tool.Name = %q, want %q", msg.Tool.Name, "Read")
	}
	if msg.Tool.Args != "/foo.go" {
		t.Errorf("Tool.Args = %q, want %q", msg.Tool.Args, "/foo.go")
	}
	if msg.Tool.Output != "47 lines" {
		t.Errorf("Tool.Output = %q, want %q", msg.Tool.Output, "47 lines")
	}
}

// TestTranslate_EventAgentConnected verifies that an EventAgentReady
// translates to OutInit with the 5 context fields populated.
func TestTranslate_EventAgentConnected(t *testing.T) {
	in := agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "s_001",
		Model:     "claude-sonnet-4-5",
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != messages.OutInit {
		t.Fatalf("got (%v, %v), want (OutInit, true)", msg.Kind, ok)
	}
	// §1.4 cleanup: init fields flow through the typed
	// OutboundMessage.SessionID/Model/... flat fields (was
	// msg.Ready before event flattening).
	if msg.SessionID == "" {
		t.Fatal("msg.SessionID is empty; outbound should populate from AgentEvent.SessionID")
	}
	if msg.SessionID != "s_001" {
		t.Errorf("Init.SessionID = %v, want 's_001'", msg.SessionID)
	}
	if msg.Model != "claude-sonnet-4-5" {
		t.Errorf("Init.Model = %v, want 'claude-sonnet-4-5'", msg.Model)
	}
}

func TestTranslate_EventDone_Dropped(t *testing.T) {
	// EventAgentDone is reflected in the receipt's terminal header — no
	// separate outbound message.
	if _, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}}); ok {
		t.Error("EventAgentDone should drop (no OutboundMessage)")
	}
}

func TestTranslate_EventPermission_ActionNeeded(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:    "question",
			Action:  "Which trigger?",
			Options: []string{"仅 REPL 启动(裸 nightme)", "REPL + 所有 CLI 子命令"},
			Kind:    agent.PermissionKindQuestion,
		},
	})
	if !ok || msg.Kind != messages.OutChoice {
		t.Fatalf("got (%v, %v), want (OutChoice, true)", msg.Kind, ok)
	}
	if msg.Choice == nil {
		t.Fatal("Choice is nil")
	}
	if msg.Choice.Title != "Action Needed" {
		t.Errorf("Title = %q, want Action Needed", msg.Choice.Title)
	}
	if !strings.Contains(msg.Choice.Body, "Which trigger?") {
		t.Errorf("Body = %q, want question text", msg.Choice.Body)
	}
	if len(msg.Choice.Options) != 2 {
		t.Errorf("Options = %v, want 2", msg.Choice.Options)
	}
	if msg.Choice.Kind != messages.ChoiceKindQuestion {
		t.Errorf("Kind = %v, want Question", msg.Choice.Kind)
	}
	if msg.Choice.RequestID == "" {
		t.Error("RequestID should be stamped")
	}
}

func TestTranslate_EventPermission_QuestionBatch(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:    "question",
			Action:  "Trigger — 何时检查?\nSource — 怎么查?",
			Options: []string{"仅 REPL 启动(裸 nightme)", "REPL + 所有 CLI 子命令"},
			Questions: []agent.AgentPermissionQuestion{
				{
					ID:       "q-trigger",
					Header:   "Trigger",
					Question: "何时检查?",
					Options:  []string{"仅 REPL 启动(裸 nightme)", "REPL + 所有 CLI 子命令"},
				},
				{
					ID:       "q-source",
					Header:   "Source",
					Question: "怎么查?",
					Options:  []string{"GitHub Releases API", "go-github-selfupdate"},
				},
			},
		},
	})
	if !ok || msg.Choice == nil {
		t.Fatalf("got (%v, %v), want OutChoice", msg.Kind, ok)
	}
	if len(msg.Choice.Questions) != 2 {
		t.Fatalf("Questions = %d, want 2", len(msg.Choice.Questions))
	}
	if msg.Choice.Questions[0].ID != "q-trigger" || msg.Choice.Questions[1].ID != "q-source" {
		t.Errorf("question ids = %+v", msg.Choice.Questions)
	}
	if len(msg.Choice.Options) != 2 || msg.Choice.Options[0].ID != "仅 REPL 启动(裸 nightme)" {
		t.Errorf("Options = %v, want first question only", msg.Choice.Options)
	}
	if msg.Choice.Kind != messages.ChoiceKindQuestion {
		t.Errorf("Kind = %v, want Question", msg.Choice.Kind)
	}
	if msg.Choice.RequestID == "" {
		t.Error("RequestID should be stamped on permission create")
	}
}

func TestTranslate_EventPermission_ApprovalCard(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:    "Bash",
			Action:  "escalate sandbox to danger-full-access",
			Options: []string{"Allow once", "Reject"},
			Kind:    agent.PermissionKindApproval,
		},
	})
	if !ok || msg.Choice == nil {
		t.Fatal("want OutChoice")
	}
	if msg.Choice.Title != "Waiting for approval" {
		t.Errorf("Title = %q, want Waiting for approval", msg.Choice.Title)
	}
	if len(msg.Choice.Questions) != 0 {
		t.Errorf("Questions = %d, want 0 (approval is not AskUserQuestion)", len(msg.Choice.Questions))
	}
}

func TestTranslate_EventPermissionSettled_PatchesCard(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind: agent.EventAgentPermissionSettled,
		PermissionSettled: &agent.AgentPermissionSettled{
			Outcome: "allowed-once",
			Source:  "dashboard",
		},
	})
	if !ok || msg.Kind != messages.OutChoicePatch {
		t.Fatalf("got (%v, %v), want OutChoicePatch", msg.Kind, ok)
	}
	if msg.ReplyTo != "" {
		t.Errorf("ReplyTo = %q, want empty (channel looks up last opt card)", msg.ReplyTo)
	}
	if msg.Choice == nil || !strings.Contains(msg.Choice.Body, "allowed-once") {
		t.Errorf("Body = %+v, want dashboard outcome", msg.Choice)
	}
	if !msg.Choice.Settled {
		t.Error("PermissionSettled should mark Choice.Settled")
	}
}

// TestTranslate_EventAgentTaskCreate_ToOutTaskCreate pins the
// EventAgentTaskCreate → OutTaskCreate translation. This is the
// bridge-side contract that the dsh bridge (and claudecode) rely on:
// if this translation breaks, every todo list sent by the agent
// silently disappears from the channel. REGRESSION GUARD against
// someone deleting the case (or changing the OutboundKind value)
// and only noticing when users report "my todo list vanished".
func TestTranslate_EventAgentTaskCreate_ToOutTaskCreate(t *testing.T) {
	items := []agent.AgentTaskItem{
		{ID: "t-1", Subject: "Read README", ActiveForm: "Reading README", Status: agent.TaskCompleted},
		{ID: "t-2", Subject: "Write code", ActiveForm: "Writing code", Status: agent.TaskInProgress},
		{ID: "t-3", Subject: "Run tests", Status: agent.TaskPending},
	}
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind:     agent.EventAgentTaskCreate,
		TaskList: &agent.AgentTaskListEvent{Items: items},
	})
	if !ok {
		t.Fatal("EventAgentTaskCreate should translate to a message")
	}
	if msg.Kind != messages.OutTaskCreate {
		t.Errorf("Kind = %v, want %v", msg.Kind, messages.OutTaskCreate)
	}
	if msg.TaskList == nil {
		t.Fatal("TaskList payload is nil (channel will treat this as no-op)")
	}
	if len(msg.TaskList.Items) != 3 {
		t.Errorf("items = %d, want 3 (channel-side checklist won't match bridge snapshot)", len(msg.TaskList.Items))
	}
	// Field-level fidelity: every AgentTaskItem field must survive
	// the translate hop. Subject / ActiveForm / Status are what the
	// Feishu adapter renders — if any of them are zeroed out by a
	// future refactor, the checklist UI shows blank rows.
	want := []agent.AgentTaskItem{
		{ID: "t-1", Subject: "Read README", ActiveForm: "Reading README", Status: agent.TaskCompleted},
		{ID: "t-2", Subject: "Write code", ActiveForm: "Writing code", Status: agent.TaskInProgress},
		{ID: "t-3", Subject: "Run tests", Status: agent.TaskPending},
	}
	for i, w := range want {
		got := msg.TaskList.Items[i]
		if got.ID != w.ID || got.Subject != w.Subject ||
			got.ActiveForm != w.ActiveForm || got.Status != w.Status {
			t.Errorf("items[%d] = %+v, want %+v", i, got, w)
		}
	}
	// ChatID must be preserved so the channel routes to the right chat.
	if msg.ChatID != "chat1" {
		t.Errorf("ChatID = %q, want chat1", msg.ChatID)
	}
}

// TestTranslate_EventAgentTaskUpdate_ToOutTaskUpdate mirrors
// TestTranslate_EventAgentTaskCreate_ToOutTaskCreate for the
// update path. Empty Items is a valid "clear the checklist"
// signal — pin that the empty slice is preserved (not nil-ed
// out by an over-zealous "drop empty" branch).
func TestTranslate_EventAgentTaskUpdate_ToOutTaskUpdate(t *testing.T) {
	items := []agent.AgentTaskItem{
		{ID: "t-2", Subject: "Write code", Status: agent.TaskCompleted},
	}
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind:     agent.EventAgentTaskUpdate,
		TaskList: &agent.AgentTaskListEvent{Items: items},
	})
	if !ok {
		t.Fatal("EventAgentTaskUpdate should translate to a message")
	}
	if msg.Kind != messages.OutTaskUpdate {
		t.Errorf("Kind = %v, want %v", msg.Kind, messages.OutTaskUpdate)
	}
	if len(msg.TaskList.Items) != 1 || msg.TaskList.Items[0].ID != "t-2" {
		t.Errorf("TaskList items = %+v, want one row t-2", msg.TaskList.Items)
	}
}

// TestTranslate_EventAgentTaskCreate_NilTaskList_Dropped pins
// that a malformed EventAgentTaskCreate with no TaskList payload
// is silently dropped (rather than rendered as an empty checklist
// — which would be indistinguishable from "user explicitly cleared
// the checklist"). The bridge contract is "emit TaskList or
// nothing".
func TestTranslate_EventAgentTaskCreate_NilTaskList_Dropped(t *testing.T) {
	if _, ok := Translate("chat1", agent.AgentEvent{
		Kind: agent.EventAgentTaskCreate,
		// TaskList deliberately nil
	}); ok {
		t.Error("EventAgentTaskCreate with nil TaskList should drop (not emit empty checklist)")
	}
}

// TestTranslate_EventAgentTaskUpdate_EmptyItems_EmptiesChecklist
// pins the "clear the checklist" signal: an EventAgentTaskUpdate
// with an empty (but non-nil) Items slice must translate to
// OutTaskUpdate with the same empty Items — NOT be dropped. The
// Feishu adapter treats this as "user's tasks all done / cleared
// the list".
func TestTranslate_EventAgentTaskUpdate_EmptyItems_EmptiesChecklist(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{
		Kind:     agent.EventAgentTaskUpdate,
		TaskList: &agent.AgentTaskListEvent{Items: []agent.AgentTaskItem{}},
	})
	if !ok {
		t.Fatal("EventAgentTaskUpdate with empty Items should emit (clear signal)")
	}
	if msg.Kind != messages.OutTaskUpdate {
		t.Errorf("Kind = %v, want OutTaskUpdate", msg.Kind)
	}
	if msg.TaskList == nil {
		t.Fatal("TaskList nil — empty Items must be preserved on the wire")
	}
	if len(msg.TaskList.Items) != 0 {
		t.Errorf("Items = %d, want 0", len(msg.TaskList.Items))
	}
}
func TestTranslate_EventError_WithDiagnostic_EmitsOutError(t *testing.T) {
	// Bridge death with a populated Diagnostic must surface as a
	// dedicated OutError card so the user sees a clear "dsh
	// died because X" signal — pre-fix this was silently dropped.
	in := agent.AgentEvent{
		Kind:      agent.EventAgentError,
		AgentName: "dsh",
		Workspace: "/code",
		Err:       errors.New("dsh: lifecycle exit signal_killed: signal: killed\n--- stderr tail ---\nfoo bar"),
		Diagnostic: &agent.BridgeDiagnostic{
			ExitKind:   agent.BridgeExitSignalKilled,
			WaitErr:    errors.New("signal: killed"),
			StderrTail: "foo bar",
			SessionID:  "session-x",
			AgentName:  "dsh",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("EventAgentError with Diagnostic should emit, got dropped")
	}
	if msg.Kind != messages.OutError {
		t.Errorf("kind = %v, want OutError", msg.Kind)
	}
	// Body should be the FIRST line of Err — long stderr tails
	// go via Diagnostic, not Text, so the card stays scannable.
	if msg.Text == "" {
		t.Error("OutError Text must be non-empty")
	}
	if strings.Contains(msg.Text, "stderr tail") {
		t.Errorf("OutError Text should be first-line only, got %q", msg.Text)
	}
	if msg.Diagnostic == nil || msg.Diagnostic.ExitKind != agent.BridgeExitSignalKilled {
		t.Errorf("Diagnostic should be propagated, got %+v", msg.Diagnostic)
	}
	if msg.AgentName != "dsh" {
		t.Errorf("AgentName = %q, want dsh (channel uses it for the card title)", msg.AgentName)
	}
}

func TestTranslate_EventError_NoDiagnostic_Drops(t *testing.T) {
	// Pre-Diagnostic-era bridges (and EventAgentError events
	// where the lifecycle couldn't classify the exit) keep the
	// legacy silent-drop behavior — we don't want to start
	// surfacing blank "bridge died" cards without the exit
	// kind.
	in := agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  errors.New("plain error"),
	}
	if _, ok := Translate("chat1", in); ok {
		t.Error("EventAgentError without Diagnostic should drop (legacy behavior)")
	}
}

func TestTranslate_EventError_FallbackBodyFromDiagnostic(t *testing.T) {
	// Err is nil but Diagnostic is present — we synthesize a
	// short body from AgentName + ExitKind so the card is
	// never blank.
	in := agent.AgentEvent{
		Kind:      agent.EventAgentError,
		AgentName: "dsh",
		Diagnostic: &agent.BridgeDiagnostic{
			ExitKind:  agent.BridgeExitNonZeroExit,
			AgentName: "dsh",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("EventAgentError with Diagnostic should emit")
	}
	if msg.Text == "" {
		t.Error("body must be synthesized from Diagnostic when Err is nil")
	}
	if !strings.Contains(msg.Text, "dsh") || !strings.Contains(msg.Text, "non-zero-exit") {
		t.Errorf("synthesized body should mention agent + exit kind, got %q", msg.Text)
	}
}

// TestTranslate_EventTaskCreate pins the F-38 happy path: a
// non-empty TaskList from a bridge (dsh todo/write, claudecode
// TaskCreate, etc.) must translate to OutTaskCreate with the
// full snapshot preserved on the typed payload. The Feishu
// adapter reads out.TaskList directly to render the checklist;
// losing the typed payload here would silently render an empty
// receipt card.
//
// This is the regression test that protects the
// "todo list → OutTaskCreate" pipeline. If a future refactor
// breaks the translation, this test catches it before the
// channel sees a corrupt outbound message.
func TestTranslate_EventTaskCreate(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentTaskCreate,
		TaskList: &agent.AgentTaskListEvent{
			Items: []agent.AgentTaskItem{
				{
					ID:         "t-1",
					Subject:    "Read docs",
					ActiveForm: "Reading docs",
					Status:     agent.TaskCompleted,
				},
				{
					ID:         "t-2",
					Subject:    "Write code",
					ActiveForm: "Writing code",
					Status:     agent.TaskInProgress,
				},
				{
					ID:      "t-3",
					Subject: "Run tests",
					Status:  agent.TaskPending,
				},
			},
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("EventAgentTaskCreate with non-nil TaskList should emit")
	}
	if msg.Kind != messages.OutTaskCreate {
		t.Errorf("Kind = %v, want OutTaskCreate", msg.Kind)
	}
	if msg.TaskList == nil {
		t.Fatal("TaskList payload is nil; outbound must preserve the typed snapshot")
	}
	if len(msg.TaskList.Items) != 3 {
		t.Fatalf("len(msg.TaskList.Items) = %d, want 3", len(msg.TaskList.Items))
	}
	// Field-level assertions protect against accidental field
	// renames in agent.AgentTaskItem.
	if msg.TaskList.Items[0].ID != "t-1" || msg.TaskList.Items[0].Subject != "Read docs" {
		t.Errorf("items[0] = %+v, want ID=t-1 Subject=Read docs", msg.TaskList.Items[0])
	}
	if msg.TaskList.Items[0].ActiveForm != "Reading docs" {
		t.Errorf("items[0].ActiveForm = %q, want Reading docs", msg.TaskList.Items[0].ActiveForm)
	}
	if msg.TaskList.Items[0].Status != agent.TaskCompleted {
		t.Errorf("items[0].Status = %v, want TaskCompleted", msg.TaskList.Items[0].Status)
	}
	if msg.TaskList.Items[1].Status != agent.TaskInProgress {
		t.Errorf("items[1].Status = %v, want TaskInProgress", msg.TaskList.Items[1].Status)
	}
	if msg.TaskList.Items[2].Status != agent.TaskPending {
		t.Errorf("items[2].Status = %v, want TaskPending", msg.TaskList.Items[2].Status)
	}
}

// TestTranslate_EventTaskUpdate — same payload semantics as
// EventAgentTaskCreate; the only difference is the OutboundKind
// (OutTaskUpdate for subsequent mutations, including delete
// emitting an empty Items snapshot).
func TestTranslate_EventTaskUpdate(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentTaskUpdate,
		TaskList: &agent.AgentTaskListEvent{
			Items: []agent.AgentTaskItem{
				{ID: "t-1", Subject: "Marked done", Status: agent.TaskCompleted},
			},
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("EventAgentTaskUpdate with non-nil TaskList should emit")
	}
	if msg.Kind != messages.OutTaskUpdate {
		t.Errorf("Kind = %v, want OutTaskUpdate", msg.Kind)
	}
	if msg.TaskList == nil || len(msg.TaskList.Items) != 1 {
		t.Fatalf("TaskList payload lost: %+v", msg.TaskList)
	}
}

// TestTranslate_EventTaskCreate_EmptySnapshot pins the
// "clear the checklist" signal — an empty Items slice is a
// valid payload. The Feishu adapter reads this as a clear
// instruction (drop the checklist section). If a future
// refactor accidentally drops empty snapshots, the channel
// would re-render the previous checklist forever.
func TestTranslate_EventTaskCreate_EmptySnapshot(t *testing.T) {
	in := agent.AgentEvent{
		Kind:     agent.EventAgentTaskCreate,
		TaskList: &agent.AgentTaskListEvent{Items: []agent.AgentTaskItem{}},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("empty TaskList is the valid 'clear checklist' signal; must emit")
	}
	if msg.Kind != messages.OutTaskCreate {
		t.Errorf("Kind = %v, want OutTaskCreate", msg.Kind)
	}
	if msg.TaskList == nil {
		t.Fatal("TaskList must be non-nil even when empty")
	}
	if len(msg.TaskList.Items) != 0 {
		t.Errorf("len = %d, want 0", len(msg.TaskList.Items))
	}
}

// TestTranslate_EventTaskCreate_NilTaskList pins the
// defensive guard: a TaskList==nil payload (a bridge bug or
// a malformed event) must drop, not emit an empty-message
// Whisper to the channel. Feishu also rejects nil TaskList
// payloads in postOrphanTaskCard / ensureReceiptForTask; this
// is the upstream guard.
func TestTranslate_EventTaskCreate_NilTaskList(t *testing.T) {
	in := agent.AgentEvent{
		Kind:     agent.EventAgentTaskCreate,
		TaskList: nil,
	}
	if _, ok := Translate("chat1", in); ok {
		t.Error("nil TaskList should drop, not emit (defensive against bridge bugs)")
	}
}
