// print_stub_test.go — end-to-end tests of runPrintMode against
// a stub `agent` binary compiled from testdata/cursor-stub.
// Cross-platform (no build tag): the stub spawn glue uses only
// os/exec + filepath + go build, all portable.
//
// What we lock here against the prototype parser + spawn glue:
//
//   - json happy path: --output-format json envelope is parsed
//     and Text / Subtype / SessionID / Usage all populate from
//     the wire; channel footer can render token usage
//   - json is_error envelope: cursor's failure envelope
//     (is_error=true, subtype=error_max_turns / refusal) is
//     surfaced as an error so /gtw doesn't treat the failure
//     prose as a successful body; audit fields (SessionID +
//     Usage) are attached to the error message
//   - json stderr contamination: stderr noise (update notice,
//     MCP log) does NOT break the JSON parse on stdout and
//     does NOT replace the assistant reply with a JSON+stderr
//     blob — separate stdout / stderr buffers keep them apart
//   - json all-zero usage: parsed but treated as nil (mirrors
//     the channel footer's "nil = not reported" contract)
//   - json missing usage: envelope without usage block also
//     yields nil Usage, no panic
//   - json empty result: valid envelope with empty `result`
//     hits the empty-answer guard (cursor produced nothing)
//   - parse error: non-JSON stdout is a HARD error, not a
//     silent text-mode fallback (catches cursor schema drift
//     or a binary that ignores --output-format json)
//   - unknown envelope type: valid JSON with `type` !=
//     "result" is a HARD error (cursor shipped a future
//     envelope shape we don't know about yet)
//   - exit-1 with stderr: stderr is appended to the error
//
// The stub is at testdata/cursor-stub (compiled by the test
// itself if absent) and is parameterized by env vars rather
// than argv so the same binary covers all scenarios.
package cursor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// requireStubCursor skips the test when the stub binary is
// not available. The test harness compiles it from
// testdata/cursor-stub on demand; if that fails (no Go
// toolchain, or the source is missing), skip rather than
// fail.
func requireStubCursor(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("NIGHTME_CURSOR_STUB"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	src := "./testdata/cursor-stub"
	tmp, err := os.MkdirTemp("", "cursor-stub-*")
	if err != nil {
		t.Skipf("cannot create temp dir for stub: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	stubExt := ""
	if runtime.GOOS == "windows" {
		stubExt = ".exe"
	}
	bin := filepath.Join(tmp, "cursor-stub"+stubExt)
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cursor-stub build failed (Go toolchain or source missing): %v\n%s", err, string(out))
	}
	return bin
}

func newStubStarter(t *testing.T, bin string) *Starter {
	t.Helper()
	return NewStarter("cursor-stub-test", bin, nil)
}

// TestRunPrintMode_JSON_HappyPath — cursor-agent's
// --output-format json envelope is parsed and the audit
// fields populate verbatim: Text from `result`, Subtype from
// `subtype`, SessionID from `session_id`, Usage from the
// nested `usage` object (inputTokens / outputTokens /
// cacheReadTokens / cacheWriteTokens). This is the
// production path for /gtw commit, buildAgentPrompt, etc.
// once printModeArgs switches to --output-format json.
func TestRunPrintMode_JSON_HappyPath(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json")
	t.Setenv("CURSOR_STUB_TEXT", "READY")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{
			Type: agent.ContentText,
			Text: "Reply with the single word READY.",
		}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Text != "READY" {
		t.Errorf("Text = %q, want READY", result.Text)
	}
	if result.Subtype != "success" {
		t.Errorf("Subtype = %q, want success (from envelope)", result.Subtype)
	}
	if result.SessionID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("SessionID = %q, want from envelope", result.SessionID)
	}
	if result.Usage == nil {
		t.Fatal("Usage = nil, want non-nil from envelope")
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 20 {
		t.Errorf("Usage.OutputTokens = %d, want 20", result.Usage.OutputTokens)
	}
	if result.Usage.CacheReadInputTokens != 40 {
		t.Errorf("Usage.CacheReadInputTokens = %d, want 40 (cursor cacheReadTokens)",
			result.Usage.CacheReadInputTokens)
	}
	if result.Usage.CacheCreationInputTokens != 0 {
		t.Errorf("Usage.CacheCreationInputTokens = %d, want 0 (cursor cacheWriteTokens default)",
			result.Usage.CacheCreationInputTokens)
	}
	if result.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", result.DurationMs)
	}
}

// TestRunPrintMode_JSON_AllZeroUsageTreatedAsNil — cursor
// can emit a usage block with all-zero values when the model
// refused, errored before any tokens were billed, etc. We
// must NOT surface that as a "0 tokens" footer line — the
// channel contract is "nil = not reported". This matches the
// existing nil-Usage-tolerant tests across other bridges
// (claudecode: appendAuditFields_NilUsageDoesNotPanic, codex:
// translate_UsageOverwrittenNotSummed, …).
func TestRunPrintMode_JSON_AllZeroUsageTreatedAsNil(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json")
	t.Setenv("CURSOR_STUB_TEXT", "OK")
	t.Setenv("CURSOR_STUB_USAGE", `{"inputTokens":0,"outputTokens":0,"cacheReadTokens":0,"cacheWriteTokens":0}`)

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Text != "OK" {
		t.Errorf("Text = %q, want OK", result.Text)
	}
	if result.Usage != nil {
		t.Errorf("Usage = %+v, want nil for all-zero usage", result.Usage)
	}
}

// TestRunPrintMode_JSON_NoUsageFieldIsNil — cursor can omit
// the usage block entirely (older versions, error turns where
// the result envelope was synthesised by a partial path). Same
// nil-Usage contract as the all-zero case; the parser must
// not panic on a missing field.
func TestRunPrintMode_JSON_NoUsageFieldIsNil(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json")
	t.Setenv("CURSOR_STUB_TEXT", "OK")
	// Drives the stub to emit an envelope with the default
	// usage block, then we hand-craft an envelope without a
	// usage field and run it directly through the parser —
	// the stub binary can't easily omit the field without a
	// new behaviour switch.
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"OK","session_id":"s"}`)
	env, isEnvelope, parseErr := parseCursorEnvelope(raw)
	if parseErr != nil {
		t.Fatalf("parseCursorEnvelope err = %v, want nil", parseErr)
	}
	if !isEnvelope {
		t.Error("isEnvelope = false, want true for valid type:\"result\" envelope")
	}
	if env.Result != "OK" || env.Subtype != "success" || env.SessionID != "s" {
		t.Errorf("parsed = (%q, %q, %q), want (OK, success, s)", env.Result, env.Subtype, env.SessionID)
	}
	if cursorUsageToAgentInfo(env.Usage) != nil {
		t.Errorf("Usage = %+v, want nil for missing usage block", env.Usage)
	}
	_ = bin
}

// TestRunPrintMode_JSON_IsError_ReturnsError — cursor's failure
// envelopes (is_error=true, subtype=error_max_turns / refusal
// / future error subtypes) must surface as errors so /gtw
// commit etc. don't treat cursor's failure prose as a
// successful body. The error message still carries SessionID +
// Usage as audit fields — mirrors claudecode's
// `TestPrintMode_Mock_IsError_PreservesSessionID` and
// `TestPrintMode_Mock_IsError_PreservesUsage` coverage
// (internal/bridge/claudecode/print_mock_unix_test.go).
func TestRunPrintMode_JSON_IsError_ReturnsError(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json_is_error")
	t.Setenv("CURSOR_STUB_TEXT", "hit the tool cap")
	t.Setenv("CURSOR_STUB_SUBTYPE", "error_max_turns")
	t.Setenv("CURSOR_STUB_USAGE", `{"inputTokens":10,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}`)

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil for is_error envelope")
	}
	if !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("err = %q, want contains 'error_max_turns' (subtype surfaced)", err.Error())
	}
	if !strings.Contains(err.Error(), "hit the tool cap") {
		t.Errorf("err = %q, want contains 'hit the tool cap' (failure prose surfaced)", err.Error())
	}
	if !strings.Contains(err.Error(), "cursor: result.is_error") {
		t.Errorf("err = %q, want contains 'cursor: result.is_error' (failure-path prefix)", err.Error())
	}
	// SessionID + Usage audit fields appended for log readability.
	if !strings.Contains(err.Error(), "00000000-0000-0000-0000-000000000002") {
		t.Errorf("err = %q, want contains session_id (audit field)", err.Error())
	}
	if !strings.Contains(err.Error(), "in=10") {
		t.Errorf("err = %q, want contains 'in=10' (audit usage input tokens)", err.Error())
	}
}

// TestRunPrintMode_JSON_IsError_EmptySubtypeAndText — guards
// against the claudecode `TestPrintMode_Mock_IsError_
// EmptySubtypeAndText` failure mode: cursor's failure envelope
// arrives with no subtype and no result text. The error must
// still be non-nil and carry the audit fields, with a
// fallback message so operators aren't staring at "cursor:
// result.is_error: " (empty body).
func TestRunPrintMode_JSON_IsError_EmptySubtypeAndText(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json_is_error")
	t.Setenv("CURSOR_STUB_TEXT", "")
	t.Setenv("CURSOR_STUB_SUBTYPE", "")
	t.Setenv("CURSOR_STUB_USAGE", `{"inputTokens":1,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}`)

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil for is_error envelope with empty body")
	}
	if !strings.Contains(err.Error(), "cursor reported is_error=true without further detail") {
		t.Errorf("err = %q, want contains fallback message", err.Error())
	}
}

// TestRunPrintMode_JSON_StderrDoesNotContaminateParse — cursor
// frequently writes non-JSON noise to stderr (update notices,
// MCP log lines, debug output). CombinedOutput() would have
// concatenated those onto stdout, breaking json.Unmarshal on
// the envelope and falling back to text mode — that fallback
// would then publish "{...}\n[cursor] update notice: ..." as
// RunResult.Text with Usage nil, defeating the whole purpose
// of switching to --output-format json. This test wires the
// stub to emit a valid envelope on stdout + a noise line on
// stderr and verifies the parse still succeeds and Usage is
// populated.
func TestRunPrintMode_JSON_StderrDoesNotContaminateParse(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json_with_stderr")
	t.Setenv("CURSOR_STUB_TEXT", "READY")
	t.Setenv("CURSOR_STUB_USAGE", `{"inputTokens":10,"outputTokens":2,"cacheReadTokens":0,"cacheWriteTokens":0}`)

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Text != "READY" {
		t.Errorf("Text = %q, want READY (stderr contamination must not appear in Text)", result.Text)
	}
	if strings.Contains(result.Text, "update notice") {
		t.Errorf("Text = %q, want no stderr noise (separate-capture invariant broken)", result.Text)
	}
	if result.Usage == nil {
		t.Fatal("Usage = nil, want non-nil (stderr noise must not break the JSON parse)")
	}
	if result.Usage.InputTokens != 10 {
		t.Errorf("Usage.InputTokens = %d, want 10", result.Usage.InputTokens)
	}
	if result.SessionID != "00000000-0000-0000-0000-000000000003" {
		t.Errorf("SessionID = %q, want from envelope", result.SessionID)
	}
}

// TestRunPrintMode_JSON_EmptyResult_ReturnsEmptyAnswer — valid
// envelope with an empty `result` field hits the empty-answer
// guard. The error must surface that the model produced no
// output, NOT fall back to text mode (the text-fallback path
// no longer exists in the JSON-only contract).
func TestRunPrintMode_JSON_EmptyResult_ReturnsEmptyAnswer(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json_empty_result")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil for empty result")
	}
	if !strings.Contains(err.Error(), "empty answer") {
		t.Errorf("err = %q, want contains 'empty answer'", err.Error())
	}
}

// TestRunPrintMode_MalformedJSON_ReturnsParseError — non-JSON
// stdout is a HARD error. Before the JSON-only contract, this
// would silently fall back to text mode (publishing the raw
// blob as RunResult.Text with Usage nil, defeating the
// footer). Today it surfaces as a parse error so operators
// see the actual schema mismatch instead of an empty Usage.
func TestRunPrintMode_MalformedJSON_ReturnsParseError(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "malformed_json")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil for malformed JSON stdout")
	}
	if !strings.Contains(err.Error(), "cursor: parse output") {
		t.Errorf("err = %q, want contains 'cursor: parse output' (hard-error prefix)", err.Error())
	}
	if !strings.Contains(err.Error(), "not valid json") {
		t.Errorf("err = %q, want contains underlying json error", err.Error())
	}
}

// TestRunPrintMode_UnknownEnvelopeType_ReturnsParseError —
// valid JSON but envelope `type` is something other than
// "result" (cursor shipped a future envelope shape we don't
// know about yet). Must be surfaced as an error, NOT silently
// treated as the "result" envelope (which would publish
// arbitrary fields into RunResult.Text).
func TestRunPrintMode_UnknownEnvelopeType_ReturnsParseError(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json_unknown_type")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil for unknown envelope type")
	}
	if !strings.Contains(err.Error(), "unexpected envelope type") {
		t.Errorf("err = %q, want contains 'unexpected envelope type'", err.Error())
	}
	if !strings.Contains(err.Error(), "future_envelope") {
		t.Errorf("err = %q, want contains the unknown type value", err.Error())
	}
}

// TestRunPrintMode_ExitOneWithStderr — exit 1 with stderr
// output. The error must surface stderr.
func TestRunPrintMode_ExitOneWithStderr(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "exit_1")
	t.Setenv("CURSOR_STUB_TEXT", "boom")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil on exit 1")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %q, want contains 'boom' (stderr surfaced)", err.Error())
	}
	if !strings.Contains(err.Error(), "cursor run:") {
		t.Errorf("err = %q, want contains 'cursor run:' prefix", err.Error())
	}
}

// TestRunPrintMode_MissingWorkspace — guard: cfg.Workspace
// is required.
func TestRunPrintMode_MissingWorkspace(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json")
	t.Setenv("CURSOR_STUB_TEXT", "READY")

	s := newStubStarter(t, bin)

	_, err := runPrintMode(context.Background(), s,
		agent.StartConfig{Workspace: ""},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil on empty workspace")
	}
	if !strings.Contains(err.Error(), "workspace is required") {
		t.Errorf("err = %q, want contains 'workspace is required'", err.Error())
	}
}

// TestRunPrintMode_EmptyPrompt — guard: empty prompt blocks
// must be rejected before spawning.
func TestRunPrintMode_EmptyPrompt(t *testing.T) {
	bin := requireStubCursor(t)
	t.Setenv("CURSOR_STUB_BEHAVIOR", "json")
	t.Setenv("CURSOR_STUB_TEXT", "READY")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	_, err := runPrintMode(context.Background(), s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: ""}})
	if err == nil {
		t.Fatal("err = nil, want non-nil on empty prompt")
	}
	if !strings.Contains(err.Error(), "empty prompt") {
		t.Errorf("err = %q, want contains 'empty prompt'", err.Error())
	}
}

// TestAppendCursorAuditFields_NilSessionIDAndUsageDoesNotPanic
// — parallels the claudecode
// `TestAppendAuditFields_NilUsageDoesNotPanic` family. The
// audit helper is invoked on the is_error path where Usage may
// be nil (e.g. envelope without a usage block) and SessionID
// may be empty. The helper must render empty strings without
// panicking — channels and the runtime log scraper never see
// the helper crash.
func TestAppendCursorAuditFields_NilSessionIDAndUsageDoesNotPanic(t *testing.T) {
	cases := []struct {
		name   string
		result agent.RunResult
		want   string
	}{
		// Format strings match agent.FormatSessionID and
		// agent.FormatUsage (internal/agent/audit.go): session
		// renders as " [session_id=X]" and usage renders as
		// " [usage in=N out=N cache_read=N]". The cache_write
		// half is NOT surfaced by FormatUsage today — only
		// cache_read is — so we don't assert on it.
		{"all empty", agent.RunResult{}, ""},
		{"only session", agent.RunResult{SessionID: "s"}, " [session_id=s]"},
		{"only usage", agent.RunResult{Usage: &agent.UsageInfo{InputTokens: 1}}, " [usage in=1 out=0 cache_read=0]"},
		{"both", agent.RunResult{SessionID: "s", Usage: &agent.UsageInfo{InputTokens: 2, OutputTokens: 3}}, " [session_id=s] [usage in=2 out=3 cache_read=0]"},
		{"nil usage", agent.RunResult{SessionID: "s", Usage: nil}, " [session_id=s]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendCursorAuditFields(tc.result)
			if got != tc.want {
				t.Errorf("appendCursorAuditFields = %q, want %q", got, tc.want)
			}
		})
	}
}
