package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestDecodeUsage_ComputesContextWindowPct pins the bridge's
// contract: claudecode computes the Doc 1 context-window-pct
// formula from the wire `modelUsage.<model>.contextWindow` +
// the per-turn usage counters. Runtime does NOT recompute pct;
// bridge-computed value flows verbatim through the channel
// footer (X% segment).
//
// Formula: pct = (input + output + cache_creation + cache_read) /
// contextWindow * 100.  Skipped when either operand is 0 (footer
// would otherwise render a misleading "0.0%").
//
// F-54: `contextWindow` is bridge-local — this test pins the
// pct output but no longer asserts any `ContextWindow` field on
// UsageEvent (the field was deleted).
func TestDecodeUsage_ComputesContextWindowPct(t *testing.T) {
	cases := []struct {
		name      string
		usageJSON string
		modelJSON string
		wantPct   float64
	}{
		{
			name:      "typical — 21100 of 200k → 10.55%",
			usageJSON: `{"input_tokens":100,"output_tokens":1000,"cache_creation_input_tokens":20000,"cache_read_input_tokens":0}`,
			modelJSON: `{"claude-opus-4-5":{"contextWindow":200000,"costUSD":0.01}}`,
			wantPct:   10.55, // (100+1000+20000+0)/200000*100 = 10.55
		},
		{
			name:      "near ceiling — 199200 / 200000 → 99.6%",
			usageJSON: `{"input_tokens":200,"output_tokens":1000,"cache_creation_input_tokens":198000,"cache_read_input_tokens":0}`,
			modelJSON: `{"claude-opus-4-5":{"contextWindow":200000,"costUSD":0.5}}`,
			wantPct:   99.6, // 199200/200000*100 = 99.6
		},
		{
			name:      "at ceiling — 200000 / 200000 → 100.0%",
			usageJSON: `{"input_tokens":200000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`,
			modelJSON: `{"claude-opus-4-5":{"contextWindow":200000,"costUSD":0.5}}`,
			wantPct:   100.0,
		},
		{
			name:      "no contextWindow in modelUsage → pct omitted",
			usageJSON: `{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`,
			modelJSON: `{"claude-opus-4-5":{"costUSD":0.01}}`,
			wantPct:   0,
		},
		{
			name:      "no modelUsage payload at all → pct omitted",
			usageJSON: `{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`,
			modelJSON: ``,
			wantPct:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := decodeUsage(
				json.RawMessage(tc.usageJSON),
				json.RawMessage(tc.modelJSON),
			)
			if u == nil {
				t.Fatalf("decodeUsage returned nil; want non-nil")
			}
			// F-54: bridge-local contextWindow no longer stored on
			// UsageEvent. The compile-time guard is implicit — if
			// anyone re-adds `ContextWindow` to UsageEvent without
			// re-introducing the assertion below, `agent.UsageEvent`
			// itself will fail to compile against this test file's
			// previous assertions (the `wantContext` field was
			// removed in F-54). Only ContextWindowPct crosses the
			// struct boundary.
			if tc.wantPct == 0 {
				if u.ContextWindowPct != 0 {
					t.Errorf("ContextWindowPct = %v, want 0", u.ContextWindowPct)
				}
			} else {
				// Tolerate tiny float rounding (0.05% tolerance).
				diff := u.ContextWindowPct - tc.wantPct
				if diff < -0.05 || diff > 0.05 {
					t.Errorf("ContextWindowPct = %.4f, want %.4f (±0.05)",
						u.ContextWindowPct, tc.wantPct)
				}
			}
		})
	}
}

// readFixture loads a JSON fixture from testdata/.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// collectEvents drains `events` until `count` events arrive or timeout.
// Returns the collected events in arrival order.
func collectEvents(t *testing.T, events <-chan agent.AgentEvent, count int, timeout time.Duration) []agent.AgentEvent {
	t.Helper()
	out := make([]agent.AgentEvent, 0, count)
	deadline := time.After(timeout)
	for len(out) < count {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timeout: got %d/%d events", len(out), count)
		}
	}
	return out
}

// streamFromFixture runs pumpStream against a fixture as if it were
// the child's stdout. Captures the events it emits.
func streamFromFixture(t *testing.T, name string, askHandler askHandlerFunc) []agent.AgentEvent {
	t.Helper()
	data := readFixture(t, name)
	events := make(chan agent.AgentEvent, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(string(data)), events, askHandler, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	wg.Wait()
	return got
}

// --- Stream translation tests ---

func TestPumpStream_Init(t *testing.T) {
	evs := streamFromFixture(t, "init.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventAgentConnected {
		t.Errorf("event kind = %v, want EventAgentConnected", evs[0].Kind)
	}
	if evs[0].Connected == nil {
		t.Fatal("Init payload is nil")
	}
	if evs[0].Connected.SessionID != "s_test_001" {
		t.Errorf("SessionID = %q, want 's_test_001'", evs[0].Connected.SessionID)
	}
	if evs[0].Connected.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want 'claude-sonnet-4-5'", evs[0].Connected.Model)
	}
}

func TestPumpStream_TextChunk(t *testing.T) {
	evs := streamFromFixture(t, "text_chunk.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventText {
		t.Errorf("event kind = %v, want EventText", evs[0].Kind)
	}
	if evs[0].Text != "让我看一下这段代码" {
		t.Errorf("text = %q, want '让我看一下这段代码'", evs[0].Text)
	}
}

func TestPumpStream_ToolUse(t *testing.T) {
	evs := streamFromFixture(t, "tool_use.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventToolStart {
		t.Errorf("event kind = %v, want EventToolStart", evs[0].Kind)
	}
	if evs[0].ToolStart.Name != "Read" {
		t.Errorf("tool name = %q, want 'Read'", evs[0].ToolStart.Name)
	}
	if evs[0].ToolStart.ID != "toolu_001" {
		t.Errorf("tool id = %q, want 'toolu_001'", evs[0].ToolStart.ID)
	}
	if !strings.Contains(evs[0].ToolStart.Args, "/tmp/foo.py") {
		t.Errorf("args = %q, want to contain '/tmp/foo.py'", evs[0].ToolStart.Args)
	}
}

func TestPumpStream_ToolResult(t *testing.T) {
	evs := streamFromFixture(t, "tool_result.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventToolEnd {
		t.Errorf("event kind = %v, want EventToolEnd", evs[0].Kind)
	}
	// Output must carry the tool_result content so the renderer
	// can show "tool → result" instead of a blank "tool done".
	if evs[0].ToolEnd.Output == "" {
		t.Errorf("ToolEnd.Output is empty; renderer would show a useless 'tool done' line")
	}
	if !strings.Contains(evs[0].ToolEnd.Output, "print") {
		t.Errorf("ToolEnd.Output = %q, want it to contain the tool result text", evs[0].ToolEnd.Output)
	}
}

// TestPumpStream_ToolResultArgsCorrelatedAcrossMessages — F-34
// review P0-2 regression guard. Claude Code's stream-json emits
// tool_use in assistant-role messages and the matching
// tool_result in user-role messages, correlated by tool_use_id.
// The args recorded on the tool_use block must survive into the
// later tool_result handler so the Feishu adapter's type-aware
// summary can render "Read /a/b.go" instead of "Read".
func TestPumpStream_ToolResultArgsCorrelatedAcrossMessages(t *testing.T) {
	// Real protocol: two separate messages, assistant then user.
	assistant := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_001","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}` + "\n"
	user := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_001","name":"Read","content":"line 1\nline 2"}]}}` + "\n"
	evs := streamFromString(assistant + user)
	if len(evs) < 2 {
		t.Fatalf("got %d events, want at least 2 (tool_use handleToolUse + tool_result EventToolEnd)", len(evs))
	}
	// Last event should be the tool_result EventToolEnd with args.
	last := evs[len(evs)-1]
	if last.Kind != agent.EventToolEnd || last.ToolEnd == nil {
		t.Fatalf("last event = %+v, want EventToolEnd", last)
	}
	if last.ToolEnd.Args != `{"file_path":"/tmp/foo.go"}` {
		t.Errorf("Args = %q, want raw tool_use input %q", last.ToolEnd.Args, `{"file_path":"/tmp/foo.go"}`)
	}
	if last.ToolEnd.ID != "toolu_001" {
		t.Errorf("ID = %q, want %q", last.ToolEnd.ID, "toolu_001")
	}
	if last.ToolEnd.Name != "Read" {
		t.Errorf("Name = %q, want %q", last.ToolEnd.Name, "Read")
	}
}

func TestPumpStream_ToolResultArgsMissingMatch(t *testing.T) {
	input := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_missing","name":"Read","content":"line 1"}]}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].ToolEnd == nil {
		t.Fatal("ToolEnd is nil")
	}
	if evs[0].ToolEnd.Args != "" {
		t.Errorf("Args = %q, want empty for unmatched tool_result", evs[0].ToolEnd.Args)
	}
}

// TestStringifyToolResult covers the three payload shapes Claude
// Code emits for tool_result.content: a plain JSON string, an
// array of content blocks (multi-modal), and a non-string non-array
// payload (defensive).
func TestStringifyToolResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain string",
			raw:  `"hello world"`,
			want: "hello world",
		},
		{
			name: "string with surrounding whitespace",
			raw:  `"\n  multi\n  line\n"`,
			want: "multi\n  line",
		},
		{
			name: "array of text blocks",
			raw:  `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`,
			want: "first | second",
		},
		{
			name: "array mixing text and image blocks",
			raw:  `[{"type":"text","text":"caption"},{"type":"image","source":{}}]`,
			want: "caption | [image]",
		},
		{
			name: "empty content",
			raw:  `""`,
			want: "",
		},
		{
			name: "unknown shape falls back to raw JSON",
			raw:  `{"foo":42}`,
			want: `{"foo":42}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stringifyToolResult(json.RawMessage(c.raw))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestPumpStream_AskUserQuestion(t *testing.T) {
	handler := defaultAskHandler
	evs := streamFromFixture(t, "ask_question.json", handler)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventPermission {
		t.Errorf("event kind = %v, want EventPermission", evs[0].Kind)
	}
	pr := evs[0].Permission
	if pr == nil {
		t.Fatal("permission is nil")
	}
	if pr.Tool != "AskUserQuestion" {
		t.Errorf("tool = %q, want 'AskUserQuestion'", pr.Tool)
	}
	// 3 original options + "Other"
	if len(pr.Options) != 4 {
		t.Errorf("options count = %d, want 4 (3 + Other)", len(pr.Options))
	}
	if pr.Options[0] != "PostgreSQL" {
		t.Errorf("first option = %q, want 'PostgreSQL' (Recommended suffix stripped)", pr.Options[0])
	}
	if pr.Options[3] != "Other" {
		t.Errorf("last option = %q, want 'Other'", pr.Options[3])
	}
	if pr.ResponseCh == nil {
		t.Error("ResponseCh is nil")
	}
	if !strings.Contains(pr.Action, "Which database?") {
		t.Errorf("action = %q, want to contain 'Which database?'", pr.Action)
	}
}

func TestPumpStream_Result(t *testing.T) {
	// result.json fixture carries result/usage/duration_ms/subtype.
	// Co-located usage design: a single EventResult with Usage
	// attached to ResultEvent, then EventDone. The legacy
	// EventResult + EventUsage pair no longer exists.
	evs := streamFromFixture(t, "result.json", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result with co-located Usage + Done)", len(evs))
	}
	// EventResult — carries Text, DurationMs, Subtype, AND Usage.
	if evs[0].Kind != agent.EventResult {
		t.Errorf("evs[0].Kind = %v, want EventResult", evs[0].Kind)
	}
	if evs[0].Result == nil || evs[0].Result.Text != "完成" {
		t.Errorf("evs[0].Result = %+v, want Text '完成'", evs[0].Result)
	}
	if evs[0].Result.DurationMs != 12345 {
		t.Errorf("DurationMs = %d, want 12345", evs[0].Result.DurationMs)
	}
	if evs[0].Result.Subtype != "success" {
		t.Errorf("Subtype = %q, want 'success'", evs[0].Result.Subtype)
	}
	if evs[0].Result.IsError {
		t.Error("IsError = true, want false")
	}
	// Usage is now on the same ResultEvent (not a separate event).
	if evs[0].Result.Usage == nil {
		t.Fatal("ResultEvent.Usage is nil; bridge should populate from result.usage")
	}
	if evs[0].Result.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", evs[0].Result.Usage.InputTokens)
	}
	if evs[0].Result.Usage.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", evs[0].Result.Usage.OutputTokens)
	}
	if evs[0].Result.Usage.CostUSD != 0.001 {
		t.Errorf("CostUSD = %f, want 0.001", evs[0].Result.Usage.CostUSD)
	}
	// EventDone
	if evs[1].Kind != agent.EventDone {
		t.Errorf("evs[1].Kind = %v, want EventDone", evs[1].Kind)
	}
	if evs[1].Done == nil || evs[1].Done.ExitCode != 0 {
		t.Errorf("done = %+v, want ExitCode 0", evs[1].Done)
	}
}

func TestPumpStream_Result_EmptyText_NoResultEvent(t *testing.T) {
	// When the result has no text AND is_error=false, the entire
	// result branch is dropped (text + usage are useless). Only
	// EventDone fires. Previously we emitted EventUsage + Done;
	// now usage is co-located so it goes with the dropped Result.
	input := `{"type":"result","subtype":"success","usage":{"input_tokens":50,"output_tokens":25},"session_id":"s_test_001"}` + "\n"
	events := make(chan agent.AgentEvent, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (Done only — text empty so Result dropped)", len(got))
	}
	if got[0].Kind != agent.EventDone {
		t.Errorf("got[0].Kind = %v, want EventDone", got[0].Kind)
	}
}

func TestPumpStream_Result_NoUsagePayload(t *testing.T) {
	// When usage is absent on the wire, ResultEvent.Usage stays
	// nil — the runtime is a passive pass-through, so a nil Usage
	// just means the channel footer omits Line 2 for this event.
	// Result + Done still fire.
	input := `{"type":"result","subtype":"success","result":"done","session_id":"s_test_001"}` + "\n"
	events := make(chan agent.AgentEvent, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(got))
	}
	if got[0].Kind != agent.EventResult {
		t.Errorf("got[0].Kind = %v, want EventResult", got[0].Kind)
	}
	if got[0].Result.Usage != nil {
		t.Errorf("ResultEvent.Usage = %+v, want nil (no usage on the wire)", got[0].Result.Usage)
	}
	if got[1].Kind != agent.EventDone {
		t.Errorf("got[1].Kind = %v, want EventDone", got[1].Kind)
	}
}

func TestPumpStream_Compact(t *testing.T) {
	// subtype:"compact" is a MID-TURN compaction. Emit EventCompaction
	// and DO NOT emit EventDone — subsequent events continue the turn.
	evs := streamFromFixture(t, "compact.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventCompaction {
		t.Errorf("event kind = %v, want EventCompaction", evs[0].Kind)
	}
	// F-49: Compaction payload is now an empty marker struct; the
	// Kind alone discriminates. See
	// docs/feat/F-49-compaction-counter.md §1.3.
}

func TestPumpStream_Compaction(t *testing.T) {
	// Older CLI used subtype:"compaction" — same handling as "compact".
	evs := streamFromFixture(t, "compaction.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventCompaction {
		t.Errorf("event kind = %v, want EventCompaction", evs[0].Kind)
	}
}

func TestPumpStream_ControlRequest(t *testing.T) {
	// control_request is not implemented under bypassPermissions —
	// it's only logged at debug. The bridge emits zero events; the
	// channel pump simply drains the empty stream.
	evs := streamFromFixture(t, "control_request.json", nil)
	if len(evs) != 0 {
		t.Fatalf("got %d events, want 0 (control_request is logged-only)", len(evs))
	}
}

func TestPumpStream_ReplayUserMessage(t *testing.T) {
	// --replay-user-messages echoes user-role text blocks back so the
	// channel can render "[你] <text>" alongside agent activity.
	evs := streamFromFixture(t, "user_replay.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventText {
		t.Errorf("event kind = %v, want EventText", evs[0].Kind)
	}
	if evs[0].Text != "[你] hello" {
		t.Errorf("text = %q, want '[你] hello'", evs[0].Text)
	}
}

func TestPumpStream_InvalidJSON_Skipped(t *testing.T) {
	input := "not json\n{\"type\":\"result\",\"subtype\":\"success\"}\n"
	events := make(chan agent.AgentEvent, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (invalid line skipped)", len(got))
	}
	if got[0].Kind != agent.EventDone {
		t.Errorf("kind = %v, want EventDone", got[0].Kind)
	}
}

// --- AskUserQuestion answer encoding tests ---

func TestEncodeUserAnswer_SingleSelect(t *testing.T) {
	data, err := encodeUserAnswer("toolu_002", []string{"PostgreSQL"}, false)
	if err != nil {
		t.Fatal(err)
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if msg.Type != "user" {
		t.Errorf("type = %q, want 'user'", msg.Type)
	}
	if msg.Message.Role != "user" {
		t.Errorf("role = %q, want 'user'", msg.Message.Role)
	}
	if len(msg.Message.Content) != 1 {
		t.Fatalf("content count = %d, want 1", len(msg.Message.Content))
	}
	c := msg.Message.Content[0]
	if c.Type != "tool_result" {
		t.Errorf("content type = %q, want 'tool_result'", c.Type)
	}
	if c.ToolUseID != "toolu_002" {
		t.Errorf("tool_use_id = %q, want 'toolu_002'", c.ToolUseID)
	}
	// Single-select → string form
	if string(c.Content) != `"PostgreSQL"` {
		t.Errorf("content = %s, want \"PostgreSQL\" (string)", c.Content)
	}
}

func TestEncodeUserAnswer_MultiSelect_Array(t *testing.T) {
	data, err := encodeUserAnswer("toolu_002", []string{"PostgreSQL", "Auth"}, true)
	if err != nil {
		t.Fatal(err)
	}

	var msg struct {
		Message struct {
			Content []struct {
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.Message.Content[0].Content) != `["PostgreSQL","Auth"]` {
		t.Errorf("content = %s, want array form", msg.Message.Content[0].Content)
	}
}

func TestEncodeUserAnswer_MultiSelect_LegacyString(t *testing.T) {
	// Even with multi=true, if only one option was selected, we fall
	// back to the string form (no commas needed).
	data, err := encodeUserAnswer("toolu_002", []string{"PostgreSQL"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Message struct {
			Content []struct {
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.Message.Content[0].Content) != `"PostgreSQL"` {
		t.Errorf("content = %s, want string form for single pick", msg.Message.Content[0].Content)
	}
}

func TestEncodeUserAnswer_Empty_NoOp(t *testing.T) {
	data, err := encodeUserAnswer("toolu_002", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("data = %q, want empty", data)
	}
}

// --- Agent descriptor tests ---

func TestAgent_Name(t *testing.T) {
	a := New("claude", "claude", nil)
	if a.Name() != "claude" {
		t.Errorf("Name = %q, want 'claude'", a.Name())
	}
}

func TestAgent_Mode(t *testing.T) {
	a := New("claude", "claude", nil)
	if a.Mode() != agent.ModeJSONIO {
		t.Errorf("Mode = %v, want ModeJSONIO", a.Mode())
	}
}

func TestAgent_Detect_MissingBinary(t *testing.T) {
	a := New("claude", "this-binary-does-not-exist-12345", nil)
	if err := a.Detect(); err == nil {
		t.Error("Detect should fail for missing binary")
	}
}

// --- Session tests (no real Claude Code binary needed) ---

func TestSession_SendText_NoProcess(t *testing.T) {
	// newSession requires a real binary; we test the JSON encoding
	// path indirectly via SendText/EncodeUserAnswer.
	a := New("claude", "this-binary-does-not-exist-12345", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := a.Start(ctx, agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("Start should fail for missing binary")
	}
	if !strings.Contains(err.Error(), "this-binary-does-not-exist") {
		t.Errorf("err = %v, want to mention the binary name", err)
	}
}

func TestNewSession_EmptyWorkspace(t *testing.T) {
	_, err := newSession(context.Background(), "echo", "echo", nil, nil, "")
	if err == nil {
		t.Fatal("newSession with empty workspace should fail")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("err = %v, want to mention 'workspace'", err)
	}
}

// --- buildArgs unit tests (no process spawn) ---

func TestAgent_BuildArgs_Default(t *testing.T) {
	// Empty cfg.PermissionMode falls back to bypassPermissions,
	// which is the placeholder value baked into DefaultArgs — the
	// net result should match DefaultArgs + extras + cfg.Args.
	//
	// --replay-user-messages was removed from DefaultArgs as part
	// of the F-25 v1.1 rolling-log fix: the chat surface pairs
	// each receipt with its user message via Feishu's ReplyMessage
	// API, so the channel doesn't need to re-render user text.
	got := buildArgs(nil, agent.StartConfig{Args: []string{"--resume", "abc"}})
	if containsSeq(got, "--replay-user-messages") {
		t.Error("DefaultArgs should not contain --replay-user-messages (removed for F-25 v1.1)")
	}
	if !containsSeq(got, "--permission-mode", PermissionBypass) {
		t.Errorf("--permission-mode placeholder not rewritten; got=%v", got)
	}
	if !containsSeq(got, "--resume", "abc") {
		t.Error("cfg.Args not appended at the tail")
	}
}

func TestAgent_BuildArgs_OverridesPermissionMode(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{PermissionMode: "default"})
	if !containsSeq(got, "--permission-mode", "default") {
		t.Errorf("PermissionMode override did not land in args; got=%v", got)
	}
	// And the placeholder bypassPermissions should NOT appear twice.
	if countSeq(got, "--permission-mode") != 1 {
		t.Errorf("expected exactly one --permission-mode flag; got %d in %v", countSeq(got, "--permission-mode"), got)
	}
}

func TestAgent_BuildArgs_Auto(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{PermissionMode: PermissionAuto})
	if !containsSeq(got, "--permission-mode", PermissionAuto) {
		t.Errorf("PermissionAuto override did not land in args; got=%v", got)
	}
}

// TestAgent_BuildArgs_ResumeIDAppended asserts that cfg.ResumeID, when
// non-empty, lands at the tail of argv as `--resume <id>` so the
// bridge can resume the previous Claude Code session.
func TestAgent_BuildArgs_ResumeIDAppended(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{ResumeID: "sess-xyz-123"})
	if !containsSeq(got, "--resume", "sess-xyz-123") {
		t.Errorf("--resume <id> not appended at tail; got=%v", got)
	}
	// Should appear AFTER user-supplied cfg.Args.
	indexOfResume := indexOfSeq(got, "--resume")
	if indexOfResume <= 0 || indexOfResume >= len(got)-1 {
		t.Errorf("--resume missing the resume-id argument; got=%v", got)
	}
}

// TestAgent_BuildArgs_EmptyResumeID asserts that an empty ResumeID
// does NOT add a `--resume ""` to the argv (which would be a no-op
// or, worse, ambiguous).
func TestAgent_BuildArgs_EmptyResumeID(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{ResumeID: ""})
	for _, s := range got {
		if s == "--resume" {
			t.Errorf("empty ResumeID should not add --resume flag; got=%v", got)
		}
	}
}

// indexOfSeq returns the first index in got where seq starts as a
// contiguous subsequence, or -1 if not found. Used by buildArgs
// tests to assert position-relative ordering.
func indexOfSeq(got []string, seq ...string) int {
	if len(seq) == 0 || len(got) < len(seq) {
		return -1
	}
	for i := 0; i+len(seq) <= len(got); i++ {
		match := true
		for j := range seq {
			if got[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// containsSeq returns true when seq appears as a contiguous subsequence
// of got. Used by buildArgs tests to assert flag presence / ordering.
func containsSeq(got []string, seq ...string) bool {
	if len(seq) == 0 || len(got) < len(seq) {
		return len(seq) == 0
	}
	for i := 0; i+len(seq) <= len(got); i++ {
		match := true
		for j := range seq {
			if got[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// countSeq counts how many positions in got start with the contiguous
// sequence seq. Used to assert "exactly one" flag occurrences.
func countSeq(got []string, seq ...string) int {
	if len(seq) == 0 {
		return 0
	}
	n := 0
	for i := 0; i+len(seq) <= len(got); i++ {
		match := true
		for j := range seq {
			if got[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	return n
}

// streamFromString runs pumpStream against an in-memory stream and
// collects the AgentEvents it emits. Used by tests that want to
// drive pumpStream with multi-event input that no single fixture
// covers — e.g. several user messages in a row, each emitting
// replay + assistant + result, to verify the parser doesn't
// drop frames under burst input.
func streamFromString(stream string) []agent.AgentEvent {
	events := make(chan agent.AgentEvent, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(stream), events, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	wg.Wait()
	return got
}

// TestPumpStream_MultiMessageBurst exercises pumpStream with a
// burst of stream-json envelopes: three user turns back-to-back,
// each consisting of one user-role echo + one assistant text +
// one terminal result. The parser must produce nine events
// (three of each kind) without dropping or duplicating any.
//
// This is the parser-side companion to
// TestRun_ConsecutiveMessagesDoNotDeadlock (gateway) and
// TestSendUserMessage_EvictionDoesNotDeadlock (Feishu adapter):
// the burst arrives at the bridge through pumpStream, the parser
// translates each event, and the AgentEvents flow to the session
// manager. A regression in the parser's state machine that
// causes it to drop events between user turns would surface here
// as a count mismatch, even if the bridge's stdin write side
// were healthy.
func TestPumpStream_MultiMessageBurst(t *testing.T) {
	const messages = 3
	lines := make([]string, 0, messages*3)
	for i := 0; i < messages; i++ {
		text := "hello-" + string(rune('a'+i))
		lines = append(lines,
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[replay] `+text+`"}]}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"got: `+text+`"}]}}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"duration_api_ms":1,"num_turns":1,"result":"done"}`,
		)
	}
	stream := strings.Join(lines, "\n") + "\n"

	evs := streamFromString(stream)
	// Each result event in Claude Code's stream-json emits both
	// an EventResult (for the final assistant text) and an
	// EventDone (terminal). So per user turn we get four
	// AgentEvents: replay + assistant + result + done. Total
	// across `messages` turns is 4 * messages.
	wantEvents := messages * 4
	if len(evs) != wantEvents {
		t.Fatalf("events = %d, want %d (replay + assistant + result + done per message)", len(evs), wantEvents)
	}

	var replays, assistants, results, dones int
	for _, ev := range evs {
		switch ev.Kind {
		case agent.EventText:
			// The bridge prefixes user-role events with "[你] "
			// before forwarding to the channel; assistant text
			// goes through unprefixed.
			switch {
			case strings.HasPrefix(ev.Text, "[你] [replay] "):
				replays++
			case strings.HasPrefix(ev.Text, "got: "):
				assistants++
			}
		case agent.EventResult:
			results++
		case agent.EventDone:
			dones++
		}
	}
	if replays != messages {
		t.Errorf("replay events = %d, want %d", replays, messages)
	}
	if assistants != messages {
		t.Errorf("assistant events = %d, want %d", assistants, messages)
	}
	if results != messages {
		t.Errorf("EventResult count = %d, want %d", results, messages)
	}
	if dones != messages {
		t.Errorf("EventDone count = %d, want %d", dones, messages)
	}
}

// claudeMockScript is the path to a sh wrapper that absorbs the
// bridge's args (--print, --input-format stream-json, ...) and
// runs the Python mock. The bridge's DefaultArgs are flagged
// for the real claude binary; Python would reject them as
// unknown options, so we round-trip through sh.
const claudeMockScript = "../../testdata/claude_mock.sh"

// claudeMockCommand returns the argv that spawns the mock. The
// bridge passes DefaultArgs (--print, --input-format stream-json,
// ...) which the underlying Python interpreter rejects as
// unknown options. The sh wrapper around the Python mock absorbs
// those args via shebang semantics so the bridge can pass them
// unchanged.
func claudeMockCommand(t *testing.T) (string, []string) {
	t.Helper()
	// Resolve the mock script to an absolute path so the
	// bridge's exec.LookPath doesn't fail when starting
	// from the test's working directory.
	abs, err := filepath.Abs(claudeMockScript)
	if err != nil {
		t.Fatalf("resolve mock script %q: %v", claudeMockScript, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("claude mock script not present at %s: %v (skipping integration test)", abs, err)
	}
	return abs, nil
}

// TestClaudeCodeBridge_RealSubprocess drives the claudecode bridge
// through a real subprocess (a Python mock that mimics the Claude
// Code CLI's stream-json surface) and verifies that three
// back-to-back SendText calls produce the expected event trio
// per message. With the pre-fix eviction code the third SendText
// would deadlock; with the post-fix code all three messages
// flow through the bridge's read path and reach the Events
// channel.
//
// The mock is a Python script (not sh -c) so the test exercises
// a real OS subprocess with all the pipe / fd / buffering
// semantics that the production Claude Code session uses.
// Failure modes the test catches:
//   - The bridge writes the wrong stream-json envelope shape
//     (the mock would fail to extract text and emit "empty").
//   - The bridge fails to multiplex concurrent SendText calls
//     onto the child's stdin pipe (one of the writes would block).
//   - The bridge's pumpStream drops frames between messages
//     (the per-message event count would be wrong).
func TestClaudeCodeBridge_RealSubprocess(t *testing.T) {
	cmd, args := claudeMockCommand(t)

	// Bump the default slog level so the bridge's drainStderr
	// surfaces the mock's stderr (the mock logs each step at
	// its own stderr; the bridge forwards them via slog.Debug).
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := New("mock-claude", cmd, args)
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace:      t.TempDir(),
		PermissionMode: PermissionBypass,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if pid := sess.PID(); pid <= 0 {
		t.Fatalf("PID = %d, want > 0 (mock child should be running)", pid)
	}

	// Three messages back-to-back. The bridge's writeLine
	// serializes the writes via stdinMu so each envelope lands
	// atomically on the child's stdin.
	const messages = 3
	for i := 0; i < messages; i++ {
		text := fmt.Sprintf("hello-%d", i)
		if err := sess.SendText(text); err != nil {
			t.Fatalf("SendText[%d] (%q): %v", i, text, err)
		}
	}

	// Collect events. Per message we expect (post F-25 v1.1 rolling-
	// log fix; --replay-user-messages was removed from DefaultArgs):
	//   - 1 "got: hello-N"     EventText (assistant response)
	//   - 1 EventResult (final assistant text)
	//   - 1 EventDone  (terminal)
	// Total: 3 * messages.
	//
	// The loop drains every event from the channel until either
	// the channel closes (pumpStream hit EOF) or the deadline
	// trips. Counting only the categories we care about avoids
	// the trap of `assistants == messages && dones < messages` —
	// the && would short-circuit as soon as assistants reached 3
	// even when EventDone for the third message was still in
	// the channel buffer.
	var replays, assistants, results, dones int
	deadline := time.After(5 * time.Second)
drain:
	for {
		// Fast-path exit: once we've seen the expected number of
		// every event kind on the closed channel, the bridge's
		// pumpStream hasn't hit EOF yet (the mock is a long-
		// lived process waiting for stdin) so we must break out
		// ourselves. The explicit check here also proves the
		// counts add up before the loop's deadline-fallback
		// ever fires.
		if assistants >= messages &&
			results >= messages && dones >= messages {
			break drain
		}
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break drain
			}
			switch ev.Kind {
			case agent.EventText:
				switch {
				case strings.HasPrefix(ev.Text, "[你] [replay] "):
					replays++
				case strings.HasPrefix(ev.Text, "got: "):
					assistants++
				}
			case agent.EventResult:
				results++
			case agent.EventDone:
				dones++
			}
		case <-deadline:
			t.Fatalf("deadline reached with replays=%d assistants=%d results=%d dones=%d (want assistants/results/dones == %d, replays == 0)",
				replays, assistants, results, dones, messages)
		}
	}

	if replays != 0 {
		t.Errorf("replay events = %d, want 0 (--replay-user-messages removed from DefaultArgs)", replays)
	}
	if assistants != messages {
		t.Errorf("assistant events = %d, want %d", assistants, messages)
	}
	if results != messages {
		t.Errorf("EventResult count = %d, want %d", results, messages)
	}
	if dones != messages {
		t.Errorf("EventDone count = %d, want %d", dones, messages)
	}
}

// TestSession_New_SendsClearUserMessage verifies F-34 §3.2.1 final
// (live binary test 2026-08-04): claudecode.New writes a properly-
// structured user-typed JSON envelope whose content is literally
// "/clear". The mock recognizes this content and replies with a
// fresh system/init event carrying a new session_id; the test
// asserts the bridge surfaces that contract end-to-end.
//
// We don't mock the stdin pipe directly — the bridge runs against
// the full mock CLI binary so writeLine → JSON-line → parser is
// exercised end-to-end (F-34 §3.2.1 final).
func TestSession_New_SendsClearUserMessage(t *testing.T) {
	cmd, args := claudeMockCommand(t)

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := New("mock-claude", cmd, args)
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace:      t.TempDir(),
		PermissionMode: PermissionBypass,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// Drive sess.New(); the mock will respond with system/init +
	// a terminal result.
	if err := sess.New(context.Background()); err != nil {
		t.Fatalf("New: %v", err)
	}

	// Drain events. Expect at least:
	//   - 1 EventAgentConnected (session_id == "sess-after-clear-mock")
	//   - 1 EventDone (terminal)
	deadline := time.After(5 * time.Second)
	var sawInit bool
	var initSessionID string
	var sawDone bool
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			switch ev.Kind {
			case agent.EventAgentConnected:
				sawInit = true
				if ev.Connected != nil {
					initSessionID = ev.Connected.SessionID
				}
			case agent.EventDone:
				sawDone = true
			}
			if sawInit && sawDone {
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	if !sawInit {
		t.Fatalf("expected EventAgentConnected from mock's /clear handling")
	}
	if initSessionID != "sess-after-clear-mock" {
		t.Fatalf("Init.SessionID = %q, want %q (mock's post-clear session id)",
			initSessionID, "sess-after-clear-mock")
	}
}
