// print_stub_test.go — end-to-end tests of runPrintMode against
// a stub `agent` binary compiled from testdata/cursor-stub.
// Cross-platform (no build tag): the stub spawn glue uses only
// os/exec + filepath + go build, all portable.
//
// What we lock here against the prototype parser + spawn glue:
//
//   - happy path (text fallback): a simple prompt producing plain
//     stdout gives RunResult with Text, Subtype="completed",
//     DurationMs > 0; Usage / SessionID stay nil/empty (the
//     pre-fix text-only behaviour, retained for older cursor
//     versions that ignore --output-format json)
//   - json happy path: --output-format json envelope is parsed
//     and Text / Subtype / SessionID / Usage all populate from
//     the wire; channel footer can render token usage
//   - json all-zero usage: parsed but treated as nil (mirrors
//     the channel footer's "nil = not reported" contract)
//   - json missing usage: envelope without usage block also
//     yields nil Usage, no panic
//   - exit-1 with stderr: stderr is appended to the error
//   - exit-0 with empty answer: error includes "empty answer"
//   - echo mode: prompt is forwarded to the child
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

// withStubEnv runs fn with CURSOR_STUB_BEHAVIOR / TEXT set
// in the inherited process env.
func withStubEnv(t *testing.T, behavior, text string) {
	t.Helper()
	t.Setenv("CURSOR_STUB_BEHAVIOR", behavior)
	t.Setenv("CURSOR_STUB_TEXT", text)
}

func newStubStarter(t *testing.T, bin string) *Starter {
	t.Helper()
	return NewStarter("cursor-stub-test", bin, nil)
}

// TestRunPrintMode_HappyPath — simplest smoke: exit 0 with
// plain text on stdout. With --output-format json the cursor
// stub would emit a JSON envelope; this test uses the "happy"
// behaviour (plain text) so it also doubles as the fallback
// path coverage (older cursor / non-cursor binaries that
// ignore --output-format json keep working). Asserts Text /
// Subtype="completed" (the fallback default) / DurationMs;
// Usage / SessionID stay empty.
func TestRunPrintMode_HappyPath(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "happy", "READY")

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
	if result.Subtype != "completed" {
		t.Errorf("Subtype = %q, want completed", result.Subtype)
	}
	if result.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", result.DurationMs)
	}
	if result.Usage != nil {
		t.Errorf("Usage = %+v, want nil", result.Usage)
	}
	if result.SessionID != "" {
		t.Errorf("SessionID = %q, want \"\"", result.SessionID)
	}
	if result.Model != "" {
		t.Errorf("Model = %q, want \"\"", result.Model)
	}
	t.Logf("result: Text=%q Subtype=%s DurationMs=%d",
		result.Text, result.Subtype, result.DurationMs)
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
	withStubEnv(t, "json", "READY")

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
	// CURSOR_STUB_USAGE left empty → stub omits the field by
	// emitting "{}" instead of an empty usage object; override
	// the stub's default by setting the env to an empty value
	// the stub interprets as "use default" — so we patch the
	// stub to truly omit usage by setting CURSOR_STUB_USAGE to
	// a literal `null` and patching the stub if needed. To
	// keep the stub simple, this test directly exercises the
	// parser via parseCursorOutput on a hand-crafted envelope.
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"OK","session_id":"s","request_id":"r","duration_ms":1,"duration_api_ms":1}`)
	text, subtype, sid, usage := parseCursorOutput(raw)
	if text != "OK" || subtype != "success" || sid != "s" {
		t.Errorf("parsed = (%q, %q, %q), want (OK, success, s)", text, subtype, sid)
	}
	if usage != nil {
		t.Errorf("Usage = %+v, want nil for missing usage block", usage)
	}
	_ = bin
}

// TestRunPrintMode_ExitOneWithStderr — exit 1 with stderr
// output. The error must surface stderr.
func TestRunPrintMode_ExitOneWithStderr(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "exit_1", "boom")

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

// TestRunPrintMode_EmptyAnswer — process exits 0 but
// produced no output. Must return "empty answer" error.
func TestRunPrintMode_EmptyAnswer(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "empty", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil on empty answer")
	}
	if !strings.Contains(err.Error(), "empty answer") {
		t.Errorf("err = %q, want contains 'empty answer'", err.Error())
	}
}

// TestRunPrintMode_EchoPrompt — regression guard: the prompt
// must be forwarded to the child process. The stub's "echo"
// behavior prints the last positional arg verbatim.
func TestRunPrintMode_EchoPrompt(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "echo", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const userPrompt = "PROMOTE_ME_IF_FORWARDED"
	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: userPrompt}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(result.Text, userPrompt) {
		t.Errorf("Text = %q, want contains %q (prompt was NOT forwarded to child)", result.Text, userPrompt)
	}
}

// TestRunPrintMode_MissingWorkspace — guard: cfg.Workspace
// is required.
func TestRunPrintMode_MissingWorkspace(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "happy", "READY")

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
	withStubEnv(t, "happy", "READY")

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

// TestRunPrintMode_MultipleTextBlocks — multiple text blocks
// are joined with newlines into a single prompt.
func TestRunPrintMode_MultipleTextBlocks(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "echo", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "line1"},
			{Type: agent.ContentText, Text: "line2"},
		})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(result.Text, "line1") {
		t.Errorf("Text = %q, want contains 'line1'", result.Text)
	}
	if !strings.Contains(result.Text, "line2") {
		t.Errorf("Text = %q, want contains 'line2'", result.Text)
	}
}
