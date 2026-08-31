
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
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
// F-54 dropped `ContextWindow` from UsageInfo; F-55 re-introduced
// it so the footer can render `(window)` alongside X% (CLI Agent
// 报什么就显示什么,错了让用户自己计算 — see
// docs/feat/F-55-footer-show-context-window.md). The window
// value is bridge-local and passes through to UsageInfo verbatim;
// runtime / channel do not recompute, catalog, or clamp on it.
// Companion TestDecodeUsage_ForwardsContextWindow (below)
// pins the re-introduced forwarding path so a future refactor
// can't silently drop the field again.
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

// TestDecodeUsage_ForwardsContextWindow pins F-55: the wire
// `modelUsage[<model>].contextWindow` value is copied onto
// `out.ContextWindow` so the channel footer can render
// `X% (window)` alongside the percentage. Without this
// assertion, a future refactor that drops the `out.ContextWindow
// = contextWindow` line would still pass
// TestDecodeUsage_ComputesContextWindowPct (which only checks
// the percentage) — but the footer would silently render
// `X% (0)` for every turn, defeating the whole point of F-55.
//
// Scenarios: window present (forwarded verbatim, both 200K and
// 1M), window absent in modelUsage (stays 0, footer omits the
// segment), no modelUsage payload at all (stays 0).
func TestDecodeUsage_ForwardsContextWindow(t *testing.T) {
	cases := []struct {
		name      string
		modelJSON string
		wantWin   int
	}{
		{
			name:      "200k window — forwarded verbatim",
			modelJSON: `{"claude-opus-4-5":{"contextWindow":200000}}`,
			wantWin:   200_000,
		},
		{
			name:      "1M window — M-unit footer renders correctly",
			modelJSON: `{"claude-opus-4-8":{"contextWindow":1000000}}`,
			wantWin:   1_000_000,
		},
		{
			name:      "no contextWindow in modelUsage — stays 0 (footer omits)",
			modelJSON: `{"claude-opus-4-5":{"costUSD":0.01}}`,
			wantWin:   0,
		},
		{
			name:      "no modelUsage payload at all — stays 0",
			modelJSON: ``,
			wantWin:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := decodeUsage(
				json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
				json.RawMessage(tc.modelJSON),
			)
			if u == nil {
				t.Fatalf("decodeUsage returned nil")
			}
			if u.ContextWindow != tc.wantWin {
				t.Errorf("ContextWindow = %d, want %d", u.ContextWindow, tc.wantWin)
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
		// armPendingAskFn is left nil: every existing fixture
		// drives (a) tool_use (which uses askHandler, not
		// armPendingAskFn). The text-fallback path requires a
		// non-nil armPendingAskFn, and is exercised separately
		// by TestPumpStream_AskUserQuestion_TextFallback.
		pumpStream(strings.NewReader(string(data)), events, askHandler, nil, "claude", "/tmp", "main", nil)
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
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("event kind = %v, want EventAgentReady", evs[0].Kind)
	}
	// Init no longer has a sub-struct payload — context fields
	// are flattened on AgentEvent. Use SessionID == "" as the
	// "not initialised" sentinel.
	if evs[0].SessionID == "" {
		t.Fatal("Init SessionID is empty")
	}
	if evs[0].SessionID != "s_test_001" {
		t.Errorf("SessionID = %q, want 's_test_001'", evs[0].SessionID)
	}
	if evs[0].Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want 'claude-sonnet-4-5'", evs[0].Model)
	}
}

func TestPumpStream_TextChunk(t *testing.T) {
	evs := streamFromFixture(t, "text_chunk.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventAgentText {
		t.Errorf("event kind = %v, want EventAgentText", evs[0].Kind)
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
	if evs[0].Kind != agent.EventAgentToolStart {
		t.Errorf("event kind = %v, want EventAgentToolStart", evs[0].Kind)
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
	if evs[0].Kind != agent.EventAgentToolEnd {
		t.Errorf("event kind = %v, want EventAgentToolEnd", evs[0].Kind)
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
		t.Fatalf("got %d events, want at least 2 (tool_use handleToolUse + tool_result EventAgentToolEnd)", len(evs))
	}
	// Last event should be the tool_result EventAgentToolEnd with args.
	last := evs[len(evs)-1]
	if last.Kind != agent.EventAgentToolEnd || last.ToolEnd == nil {
		t.Fatalf("last event = %+v, want EventAgentToolEnd", last)
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
	if evs[0].Kind != agent.EventAgentPermission {
		t.Errorf("event kind = %v, want EventAgentPermission", evs[0].Kind)
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
	// Co-located usage design: a single EventAgentResult with Usage
	// attached to AgentResultEvent, then EventAgentDone. The legacy
	// EventAgentResult + EventUsage pair no longer exists.
	evs := streamFromFixture(t, "result.json", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result with co-located Usage + Done)", len(evs))
	}
	// EventAgentResult — carries Text, DurationMs, Subtype, AND Usage.
	if evs[0].Kind != agent.EventAgentResult {
		t.Errorf("evs[0].Kind = %v, want EventAgentResult", evs[0].Kind)
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
	if evs[0].Err != nil {
		t.Error("IsError = true, want false")
	}
	// Usage is now on the same AgentResultEvent (not a separate event).
	if evs[0].Result.Usage == nil {
		t.Fatal("AgentResultEvent.Usage is nil; bridge should populate from result.usage")
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
	// EventAgentDone
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
	if evs[1].Done == nil || evs[1].Done.ExitCode != 0 {
		t.Errorf("done = %+v, want ExitCode 0", evs[1].Done)
	}
}

// TestPumpStream_SyntheticNoContent_Dropped guards the /new
// regression. driver.New writes
// `{"type":"user","message":{"role":"user","content":"/clear"}}` to
// the CLI's stdin; the CLI answers with conversation_reset +
// system/init + a synthetic "(no content)" assistant message +
// result{result:""}. Only the assistant message has user-visible
// text, but it says nothing — forwarding it posts a content-free
// bubble to the chat right after the /new receipt card.
//
// The fix drops the synthetic placeholder in the assistant/text
// branch. The gate requires BOTH the "<synthetic>" model marker
// AND the exact "(no content)" text so a real model that
// legitimately answers "(no content)" still reaches the user.
func TestPumpStream_SyntheticNoContent_Dropped(t *testing.T) {
	// Just the assistant message (the only one with user-visible
	// text). Reproduced verbatim from a live run of
	// `claude --output-format stream-json --input-format stream-json`
	// fed a /clear line on stdin.
	input := `{"type":"assistant","message":{"id":"1cd25b3f-37cb-48e0-b1cc-72260a2c305c","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"(no content)"}]}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 0 {
		t.Errorf("got %d events, want 0 — synthetic \"(no content)\" must be dropped", len(evs))
		for i, ev := range evs {
			t.Logf("  evs[%d] = %+v", i, ev)
		}
	}
}

// TestPumpStream_RealModelSayingNoContent_Kept is the negative
// half of the guard. A non-synthetic model that produces the
// literal text "(no content)" (e.g. as part of an answer) must
// still be forwarded. The gate is AND-ed, not OR-ed.
func TestPumpStream_RealModelSayingNoContent_Kept(t *testing.T) {
	input := `{"type":"assistant","message":{"model":"claude-opus-4-1","role":"assistant","content":[{"type":"text","text":"(no content)"}]}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 — real model saying \"(no content)\" must be forwarded", len(evs))
	}
	if evs[0].Kind != agent.EventAgentText {
		t.Errorf("kind = %v, want EventAgentText", evs[0].Kind)
	}
	if evs[0].Text != "(no content)" {
		t.Errorf("text = %q, want \"(no content)\"", evs[0].Text)
	}
}

// TestPumpStream_PlaceholderMatchIsStrict pins the byte-exact
// match contract of isSyntheticNoContent. Whitespace-padded
// variants of "(no content)" must NOT be dropped — the guard
// only swallows the literal byte sequence Claude Code emits.
// Review feedback: a previous revision used strings.TrimSpace,
// which silently widened the match to " (no content)" and
// "(no content)\n". That is the wrong failure mode (silent data
// loss beats a stray character), so this test guards against
// future "be lenient" drift.
func TestPumpStream_PlaceholderMatchIsStrict(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		drop  bool
	}{
		{name: "literal placeholder", text: "(no content)", drop: true},
		{name: "leading space", text: " (no content)", drop: false},
		{name: "trailing space", text: "(no content) ", drop: false},
		{name: "leading and trailing space", text: " (no content) ", drop: false},
		{name: "trailing newline", text: "(no content)\n", drop: false},
		{name: "tab padding", text: "\t(no content)\t", drop: false},
		{name: "embedded in larger text", text: "agent decided: (no content)", drop: false},
		{name: "different phrasing", text: "(no content yet)", drop: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":` + mustJSONString(tc.text) + `}]}}` + "\n"
			evs := streamFromString(input)
			switch {
			case tc.drop && len(evs) != 0:
				t.Errorf("text=%q: got %d events, want 0 (must be dropped)", tc.text, len(evs))
			case !tc.drop && len(evs) != 1:
				t.Errorf("text=%q: got %d events, want 1 (must be forwarded)", tc.text, len(evs))
			case !tc.drop && len(evs) == 1:
				if evs[0].Text != tc.text {
					t.Errorf("text round-trip: got %q, want %q", evs[0].Text, tc.text)
				}
			}
		})
	}
}

// mustJSONString returns the JSON-encoded form of s (quoted, with
// the necessary escapes for ", \, control characters). Used by
// strict-match tests that build wire envelopes from Go strings —
// string concatenation would break on inputs containing " or \.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // json.Marshal on a string cannot fail
	}
	return string(b)
}

// TestPumpStream_OtherSyntheticMessages_Kept covers the rest of
// the synthetic class. Claude Code also uses "<synthetic>" for
// interrupt notices and API-error surfaces that DO carry real
// user-visible text. The drop is narrow on purpose — only the
// exact zero-content placeholder.
func TestPumpStream_OtherSyntheticMessages_Kept(t *testing.T) {
	input := `{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"Request interrupted by user."}]}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 — non-placeholder synthetic text must be forwarded", len(evs))
	}
	if evs[0].Kind != agent.EventAgentText {
		t.Errorf("kind = %v, want EventAgentText", evs[0].Kind)
	}
	if evs[0].Text != "Request interrupted by user." {
		t.Errorf("text = %q, want interrupt notice verbatim", evs[0].Text)
	}
}

// TestPumpStream_ConversationReset_LoggedNoEvents covers the
// /new observability upgrade: a conversation_reset event from
// the CLI must be surfaced in logs at info level (not buried in
// debug) and must NOT produce agent events — the authoritative
// SessionID + Model arrive in the immediately-following system/init,
// which is wired separately. A future protocol revision that swaps
// or drops that pairing would otherwise silently break /new; this
// test pins both halves of the contract.
//
// Two cases: with-id (current CLI behavior) and absent-id
// (defensive — a future revision that drops the field must still
// log something, but distinguishable from a successful reset so an
// operator looking at the log can tell them apart).
func TestPumpStream_ConversationReset_LoggedNoEvents(t *testing.T) {
	t.Run("with new_conversation_id", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		input := `{"type":"conversation_reset","new_conversation_id":"acd2bd4e-6261-45b4-ad93-bf2792d062ca","uuid":"c78637d4-0373-4bf0-9c32-0dc5d4e17d67"}` + "\n"

		got := streamFromStringWithLogger(input, logger)
		assertResetLoggedNoEvents(t, got, buf.Bytes(), "acd2bd4e-6261-45b4-ad93-bf2792d062ca", false)
	})

	t.Run("without new_conversation_id (future protocol drift)", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		// uuid is the only other field Claude Code emits; new_conversation_id is gone.
		input := `{"type":"conversation_reset","uuid":"c78637d4-0373-4bf0-9c32-0dc5d4e17d67"}` + "\n"

		got := streamFromStringWithLogger(input, logger)
		assertResetLoggedNoEvents(t, got, buf.Bytes(), "", true)
	})
}

// assertResetLoggedNoEvents consolidates the per-case assertions so
// both subtests stay focused on their scenario (present vs absent id).
//
// Parses the log line rather than substring-matching raw JSON:
// slog's handler field order, indentation, and key set are not
// contractual, and a brittle Contains check would fail the next
// time someone adds an attr or switches handler.
func assertResetLoggedNoEvents(t *testing.T, got []agent.AgentEvent, raw []byte, wantID string, wantAbsent bool) {
	t.Helper()
	if len(got) != 0 {
		t.Errorf("got %d events, want 0 — conversation_reset is informational only", len(got))
		for i, ev := range got {
			t.Logf("  evs[%d] = %+v", i, ev)
		}
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nraw: %s", err, string(raw))
	}
	if entry["msg"] != "claudecode: conversation_reset (post /clear)" {
		t.Errorf("msg = %q, want %q\nfull entry: %+v", entry["msg"],
			"claudecode: conversation_reset (post /clear)", entry)
	}
	if level, _ := entry["level"].(string); level != "INFO" {
		t.Errorf("level = %q, want INFO\nfull entry: %+v", level, entry)
	}

	switch {
	case wantAbsent:
		// Absent-id marker must be present; new_conversation_id must NOT be
		// (the field is absent in the JSON object, not just "").
		if entry["new_conversation_id_absent"] != true {
			t.Errorf("expected new_conversation_id_absent=true marker, full entry: %+v", entry)
		}
		if _, has := entry["new_conversation_id"]; has {
			t.Errorf("did not expect new_conversation_id field on absent-id path, full entry: %+v", entry)
		}
	default:
		if entry["new_conversation_id"] != wantID {
			t.Errorf("new_conversation_id = %q, want %q\nfull entry: %+v",
				entry["new_conversation_id"], wantID, entry)
		}
	}
}

func TestPumpStream_Result_EmptyText_NoResultEvent(t *testing.T) {
	// When the result has no text AND is_error=false, the entire
	// result branch is dropped (text + usage are useless). Only
	// EventAgentDone fires. Previously we emitted EventUsage + Done;
	// now usage is co-located so it goes with the dropped Result.
	input := `{"type":"result","subtype":"success","usage":{"input_tokens":50,"output_tokens":25},"session_id":"s_test_001"}` + "\n"
	events := make(chan agent.AgentEvent, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (Done only — text empty so Result dropped)", len(got))
	}
	if got[0].Kind != agent.EventAgentDone {
		t.Errorf("got[0].Kind = %v, want EventAgentDone", got[0].Kind)
	}
}

func TestPumpStream_Result_NoUsagePayload(t *testing.T) {
	// When usage is absent on the wire, AgentResultEvent.Usage stays
	// nil — the runtime is a passive pass-through, so a nil Usage
	// just means the channel footer omits Line 2 for this event.
	// Result + Done still fire.
	input := `{"type":"result","subtype":"success","result":"done","session_id":"s_test_001"}` + "\n"
	events := make(chan agent.AgentEvent, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(got))
	}
	if got[0].Kind != agent.EventAgentResult {
		t.Errorf("got[0].Kind = %v, want EventAgentResult", got[0].Kind)
	}
	if got[0].Result.Usage != nil {
		t.Errorf("AgentResultEvent.Usage = %+v, want nil (no usage on the wire)", got[0].Result.Usage)
	}
	if got[1].Kind != agent.EventAgentDone {
		t.Errorf("got[1].Kind = %v, want EventAgentDone", got[1].Kind)
	}
}

// TestPumpStream_Compact removed: F-49 compaction tracking was
// deleted across the runtime. The bridge no longer emits
// EventAgentCompaction; subtype:"compact" is silently dropped
// (Pi suppresses its transient `compaction_start`; the runtime
// no longer tracks cycles).
// TestPumpStream_Compaction removed: F-49 compaction tracking was
// deleted across the runtime. The bridge no longer emits
// EventAgentCompaction; the kind does not exist.

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
	if evs[0].Kind != agent.EventAgentText {
		t.Errorf("event kind = %v, want EventAgentText", evs[0].Kind)
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
		pumpStream(strings.NewReader(input), events, nil, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (invalid line skipped)", len(got))
	}
	if got[0].Kind != agent.EventAgentDone {
		t.Errorf("kind = %v, want EventAgentDone", got[0].Kind)
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
	a := NewStarter("claude", "claude", nil)
	if a.Info().Name != "claude" {
		t.Errorf("Name = %q, want 'claude'", a.Info().Name)
	}
}

func TestAgent_Mode(t *testing.T) {
	a := NewStarter("claude", "claude", nil)
	if a.Info().Mode != agent.ModeJSONIO {
		t.Errorf("Mode = %v, want ModeJSONIO", a.Info().Mode)
	}
}

func TestAgent_Detect_MissingBinary(t *testing.T) {
	a := NewStarter("claude", "this-binary-does-not-exist-12345", nil)
	if err := a.Detect(); err == nil {
		t.Error("Detect should fail for missing binary")
	}
}

// --- Session tests (no real Claude Code binary needed) ---

func TestSession_Start_NoProcess(t *testing.T) {
	// newSession requires a real binary; we test the Start-failure
	// path indirectly here.
	a := NewStarter("claude", "this-binary-does-not-exist-12345", nil)
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

func TestStart_EmptyWorkspace(t *testing.T) {
	a := NewStarter("echo", "echo", nil)
	_, err := a.Start(context.Background(), agent.StartConfig{Workspace: ""})
	if err == nil {
		t.Fatal("Start with empty workspace should fail")
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

// TestAgent_BuildArgs_ResumeIDAppended asserts that cfg.SessionID, when
// non-empty, lands at the tail of argv as `--resume <id>` so the
// bridge can resume the previous Claude Code session.
func TestAgent_BuildArgs_ResumeIDAppended(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{SessionID: "sess-xyz-123"})
	if !containsSeq(got, "--resume", "sess-xyz-123") {
		t.Errorf("--resume <id> not appended at tail; got=%v", got)
	}
	// Should appear AFTER user-supplied cfg.Args.
	indexOfResume := indexOfSeq(got, "--resume")
	if indexOfResume <= 0 || indexOfResume >= len(got)-1 {
		t.Errorf("--resume missing the resume-id argument; got=%v", got)
	}
}

// TestAgent_BuildArgs_EmptyResumeID asserts that an empty SessionID
// does NOT add a `--resume ""` to the argv (which would be a no-op
// or, worse, ambiguous).
func TestAgent_BuildArgs_EmptyResumeID(t *testing.T) {
	got := buildArgs(nil, agent.StartConfig{SessionID: ""})
	for _, s := range got {
		if s == "--resume" {
			t.Errorf("empty SessionID should not add --resume flag; got=%v", got)
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
	return streamFromStringWithLogger(stream, nil)
}

// streamFromStringWithLogger is the logger-aware variant. Tests
// that need to assert on log output (e.g. the conversation_reset
// observability test) pass a *slog.Logger writing into a buffer;
// the rest go through streamFromString with nil and don't pay the
// allocation cost.
//
// The buffered chan size 64 matches streamFromString: enough headroom
// for the parser's internal emits on a multi-message burst, small
// enough to surface accidental back-pressure stalls as test hangs
// rather than silent drops. Tests that emit more than 64 events
// should bump this.
func streamFromStringWithLogger(stream string, logger *slog.Logger) []agent.AgentEvent {
	events := make(chan agent.AgentEvent, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(stream), events, nil, nil, "claude", "/tmp", "main", logger)
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
	// an EventAgentResult (for the final assistant text) and an
	// EventAgentDone (terminal). So per user turn we get four
	// AgentEvents: replay + assistant + result + done. Total
	// across `messages` turns is 4 * messages.
	wantEvents := messages * 4
	if len(evs) != wantEvents {
		t.Fatalf("events = %d, want %d (replay + assistant + result + done per message)", len(evs), wantEvents)
	}

	var replays, assistants, results, dones int
	for _, ev := range evs {
		switch ev.Kind {
		case agent.EventAgentText:
			// The bridge prefixes user-role events with "[你] "
			// before forwarding to the channel; assistant text
			// goes through unprefixed.
			switch {
			case strings.HasPrefix(ev.Text, "[你] [replay] "):
				replays++
			case strings.HasPrefix(ev.Text, "got: "):
				assistants++
			}
		case agent.EventAgentResult:
			results++
		case agent.EventAgentDone:
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
		t.Errorf("EventAgentResult count = %d, want %d", results, messages)
	}
	if dones != messages {
		t.Errorf("EventAgentDone count = %d, want %d", dones, messages)
	}
}

