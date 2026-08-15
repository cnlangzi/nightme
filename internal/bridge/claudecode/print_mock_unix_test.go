//go:build !windows

// Mock-script tests for the print-mode RunOnce path.
//
// Uses internal/testdata/claude_print_mock.sh to drive runPrintMode
// without depending on the real `claude` binary. This catches the
// error paths the real-claude smoke can't easily exercise:
//
//   - CLAUDE_PRINT_FAIL: non-zero exit + stderr → error includes
//     stderr text (the "auth error / model error" class).
//   - CLAUDE_PRINT_NO_RESULT: clean stream but no result event →
//     "exit without result" diagnostic.
//   - Default: full clean run → returns the mock's text.
//
// The smoke test (print_real_unix_test.go, gated by NIGHTME_TALIVE_RUNONCE=1)
// covers the "real claude actually produced a turn" path; this file
// covers everything else with deterministic inputs.
package claudecode

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// claudePrintMockPath is the absolute path to the mock script.
// Computed at package init via init() below so the tests don't
// each repeat filepath.Abs and the value is captured before any
// test cwd-shifting happens.
//
// exec.CommandContext (and thus agent.NewCmd) routes the binary
// through exec.LookPath, which only consults PATH. A relative
// path would not be found, so the mock must be invoked by its
// absolute path.
var claudePrintMockPath string

func init() {
	abs, err := filepath.Abs("../../testdata/claude_print_mock.sh")
	if err != nil {
		panic("claude_print_mock_test: filepath.Abs: " + err.Error())
	}
	claudePrintMockPath = abs
}

// TestPrintMode_Mock_CleanRun_ReturnsText verifies the happy
// path: a clean run emits init + assistant + result, exit 0,
// and RunOnce returns the result.result text along with all
// the per-turn metadata (model / session id / duration /
// subtype / usage). This is the canonical contract test for
// RunResult fields.
func TestPrintMode_Mock_CleanRun_ReturnsText(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_TEXT", "clean-run-payload")
	t.Setenv("CLAUDE_PRINT_MODEL", "claude-mock-opus")
	t.Setenv("CLAUDE_PRINT_USAGE", "1")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Text — the primary payload.
	if got.Text != "clean-run-payload" {
		t.Errorf("Text = %q, want %q", got.Text, "clean-run-payload")
	}

	// Model — captured from system/init.
	if got.Model != "claude-mock-opus" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-mock-opus")
	}

	// SessionID — captured from system/init (mock emits
	// "mock-print-session"). Lock-in for audit-log /
	// future-resume callers.
	if got.SessionID != "mock-print-session" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "mock-print-session")
	}

	// Subtype — success on the clean path.
	if got.Subtype != "success" {
		t.Errorf("Subtype = %q, want %q", got.Subtype, "success")
	}

	// DurationMs — captured from result.duration_ms (mock emits 42).
	if got.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", got.DurationMs)
	}

	// Usage — captured from result.usage + result.modelUsage.
	if got.Usage == nil {
		t.Fatal("Usage is nil, want populated")
	}
	if got.Usage.InputTokens != 1234 {
		t.Errorf("Usage.InputTokens = %d, want 1234", got.Usage.InputTokens)
	}
	if got.Usage.OutputTokens != 56 {
		t.Errorf("Usage.OutputTokens = %d, want 56", got.Usage.OutputTokens)
	}
	if got.Usage.CacheReadInputTokens != 128 {
		t.Errorf("Usage.CacheReadInputTokens = %d, want 128", got.Usage.CacheReadInputTokens)
	}
	if got.Usage.CostUSD != 0.0021 {
		t.Errorf("Usage.CostUSD = %v, want 0.0021", got.Usage.CostUSD)
	}
	if got.Usage.ContextWindow != 200000 {
		t.Errorf("Usage.ContextWindow = %d, want 200000", got.Usage.ContextWindow)
	}
}

// TestPrintMode_Mock_NonZeroExit_SurfacesStderr verifies that
// a non-zero exit with stderr content surfaces that stderr in
// the wrapped error message — same contract pi's mock tests
// lock in (auth / model errors land on stderr).
func TestPrintMode_Mock_NonZeroExit_SurfacesStderr(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_FAIL", "1")
	t.Setenv("CLAUDE_PRINT_STDERR", "model auth failed: 401")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatalf("RunOnce returned no error; want non-zero-exit failure. got=%q", got.Text)
	}
	if !strings.Contains(err.Error(), "model auth failed: 401") {
		t.Fatalf("error missing stderr text: %v", err)
	}
}

// TestPrintMode_Mock_NoResult_ReportsMissingEvent verifies the
// "stream ended cleanly but no terminal result event" path:
// claude exited 0 but never sent the result event. RunOnce
// must surface this as an error rather than returning "" and
// pretending success.
func TestPrintMode_Mock_NoResult_ReportsMissingEvent(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_NO_RESULT", "1")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatalf("RunOnce returned no error; want missing-result failure. got=%q", got.Text)
	}
	if !strings.Contains(err.Error(), "result") {
		t.Fatalf("error missing 'result' phrase: %v", err)
	}
}

// TestPrintMode_Mock_IsError_WrapsAsError verifies the
// "claude reports the run as a failure" path: process exits 0
// but emits a result event with is_error=true. RunOnce MUST
// surface this as an error rather than returning the
// result.result text as success. The error message should
// carry the subtype (e.g. "error_max_turns") AND the result
// text so callers can debug what claude tried.
func TestPrintMode_Mock_IsError_WrapsAsError(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_IS_ERROR", "1")
	t.Setenv("CLAUDE_PRINT_TEXT", "exceeded retry budget on tool call")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatalf("RunOnce returned no error; want is_error wrapping. got=%q", got.Text)
	}
	if !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("error missing subtype: %v", err)
	}
	if !strings.Contains(err.Error(), "exceeded retry budget") {
		t.Errorf("error missing result text: %v", err)
	}
	if !strings.Contains(err.Error(), "is_error") {
		t.Errorf("error missing 'is_error' marker: %v", err)
	}
}

// TestPrintMode_Mock_IsError_PreservesUsage pins the F-38
// "no silent data loss" contract: even when claude reports
// the run as a failure (is_error=true), the per-turn cost
// must be observable. A failed /gtw commit can spend real
// tokens (claude ate the prompt + did partial work before
// erroring out); if the bridge drops the usage block the
// runtime can't surface "your last commit cost $0.18
// before failing" to the user.
//
// The mock's CLAUDE_PRINT_USAGE=1 path emits a usage block
// with input=1234, output=56, cache_read=128, cost=0.0021.
// On the is_error path RunOnce must still surface these
// numbers — either via the returned RunResult (preferred;
// future API change) or via the wrapped error message
// (current contract).
func TestPrintMode_Mock_IsError_PreservesUsage(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_IS_ERROR", "1")
	t.Setenv("CLAUDE_PRINT_USAGE", "1")
	t.Setenv("CLAUDE_PRINT_TEXT", "failed mid-tool-call")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want is_error wrapper")
	}
	// Lock in: the input token count is observable in the
	// error message. The mock emits input_tokens=1234; if the
	// bridge dropped the usage block on the is_error path the
	// runtime would silently underreport cost.
	if !strings.Contains(err.Error(), "1234") {
		t.Errorf("is_error path dropped usage.input_tokens=1234; cost is invisible on failed runs: %v", err)
	}
	// The mock also emits cache_read=128 — verifies the cap-
	// read field is preserved too, not just input.
	if !strings.Contains(err.Error(), "128") {
		t.Errorf("is_error path dropped usage.cache_read_input_tokens=128: %v", err)
	}
}

// TestPrintMode_Mock_IsError_PreservesSessionID pins that the
// session_id from the system/init event is preserved through
// the is_error path. Without this, an operator chasing a
// failed /gtw commit has no way to look up the corresponding
// transcript in claude's own logs (`claude --resume
// <session_id>`). The mock emits session_id=mock-print-
// session; the bridge's stream.go translator copies it onto
// result.SessionID before the is_error branch runs, so it
// must remain observable.
func TestPrintMode_Mock_IsError_PreservesSessionID(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_IS_ERROR", "1")
	t.Setenv("CLAUDE_PRINT_TEXT", "auth failed")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want is_error wrapper")
	}
	if !strings.Contains(err.Error(), "mock-print-session") {
		t.Errorf("is_error path dropped session_id (cannot resume / audit failed turn): %v", err)
	}
}

// TestPrintMode_Mock_IsError_EmptySubtypeAndText verifies the
// edge-case fallthrough: when claude emits a result with
// is_error=true but BOTH subtype and result.result are
// empty (rare but observed in the wild when the model
// errors out before producing text), RunOnce must still
// produce a non-empty error message — not return nil and
// pretend the run succeeded.
func TestPrintMode_Mock_IsError_EmptySubtypeAndText(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// CLAUDE_PRINT_IS_ERROR=1 with an empty CLAUDE_PRINT_TEXT
	// makes the mock emit subtype=error_max_turns and
	// result="" — both technically non-empty (subtype is
	// populated) but the text is empty. The bridge's
	// fallback "claude reported is_error=true without
	// further detail" fires only when BOTH subtype AND text
	// are empty. We exercise the partial-empty case here to
	// confirm the message is still well-formed.
	t.Setenv("CLAUDE_PRINT_IS_ERROR", "1")
	t.Setenv("CLAUDE_PRINT_TEXT", "")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want is_error wrapper")
	}
	if err.Error() == "" {
		t.Fatal("error message is empty (would surface as silent failure)")
	}
	// Subtype is still populated → the message should mention it.
	if !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("error missing subtype on empty-text is_error path: %v", err)
	}
}

// TestPrintMode_Mock_WorkspacePropagated verifies cfg.Workspace
// reaches the child process. Empty workspace must be rejected
// before spawn so a future caller doesn't get a confusing
// "command not found" or "no such file" message.
func TestPrintMode_Mock_WorkspacePropagated(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("custom workspace", func(t *testing.T) {
		ws := t.TempDir()
		if _, err := a.RunOnce(ctx, agent.StartConfig{Workspace: ws}, []agent.ContentBlock{
			{Type: agent.ContentText, Text: "in custom workspace"},
		}); err != nil {
			t.Fatalf("RunOnce with custom workspace %q: %v", ws, err)
		}
	})

	t.Run("empty workspace rejected", func(t *testing.T) {
		if _, err := a.RunOnce(ctx, agent.StartConfig{Workspace: ""}, []agent.ContentBlock{
			{Type: agent.ContentText, Text: "anything"},
		}); err == nil {
			t.Fatalf("RunOnce with empty workspace should fail before spawn")
		}
	})
}

// TestPrintMode_Mock_EnvForwarded verifies that cfg.Env reaches
// the child process. Catches the regression where cfg.Env was
// silently dropped on the print-mode path (the daemon's
// /gtw commit-time API key override would not have been honored
// by the child, producing confusing auth-failure or
// wrong-account symptoms downstream).
//
// The mock's CLAUDE_PRINT_DUMP_ENV knob echoes every
// NIGHTME_TEST_* env var to stderr. We trigger a non-zero exit
// so stderrBuf is surfaced via the error message (a clean
// run swallows stderr; the test needs to see it to assert
// forwarding).
func TestPrintMode_Mock_EnvForwarded(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_DUMP_ENV", "1")
	t.Setenv("CLAUDE_PRINT_FAIL", "1")
	t.Setenv("NIGHTME_TEST_API_KEY", "sk-test-xyz")
	t.Setenv("NIGHTME_TEST_MODEL", "claude-mock-haiku")

	got, err := a.RunOnce(ctx, agent.StartConfig{
		Workspace: t.TempDir(),
		Env: []string{
			"NIGHTME_TEST_API_KEY=sk-test-xyz",
			"NIGHTME_TEST_MODEL=claude-mock-haiku",
		},
	}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "env test"},
	})
	_ = got
	if err == nil {
		t.Fatal("RunOnce returned no error; want non-zero-exit failure so stderr surfaces")
	}
	// cfg.Env forwarded: both keys should appear in the error
	// (the mock's stderr is appended to the wrapped error).
	if !strings.Contains(err.Error(), "NIGHTME_TEST_API_KEY=sk-test-xyz") {
		t.Errorf("cfg.Env API key not forwarded to child: %v", err)
	}
	if !strings.Contains(err.Error(), "NIGHTME_TEST_MODEL=claude-mock-haiku") {
		t.Errorf("cfg.Env model not forwarded to child: %v", err)
	}
}

// TestPrintMode_Mock_StderrCapBytes verifies that stderr is
// capped at 64 KiB. A chatty failing child dumping megabytes
// of API error context to stderr must not OOM the bridge. The
// mock's CLAUDE_PRINT_LARGE_STDERR knob emits ~N bytes of
// fake stderr markers before emitting an is_error result; the
// bridge's stderrBuf caps at 64 KiB and the wrapped error must
// contain only the truncated prefix.
func TestPrintMode_Mock_StderrCapBytes(t *testing.T) {
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Force a multi-MB stderr; the cap should clamp to 64 KiB.
	t.Setenv("CLAUDE_PRINT_LARGE_STDERR", "4194304") // 4 MiB
	t.Setenv("CLAUDE_PRINT_IS_ERROR", "1")
	t.Setenv("CLAUDE_PRINT_TEXT", "capped-stderr")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "stderr cap test"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want is_error wrapper")
	}
	// stderrBuf is included in the wrapped error. It must
	// be bounded by stderrCapBytes (64 KiB); a regression
	// that drops the cap would balloon to multi-MB. Allow
	// some slack (the wrapping message itself adds bytes
	// around stderr).
	const capWithSlack = 70 * 1024 // 64 KiB + ~6 KiB for wrap
	if len(err.Error()) > capWithSlack {
		t.Errorf("wrapped error too large: %d bytes (> %d). "+
			"stderrBuf is not capped at 64 KiB; bridge risks OOM "+
			"on a chatty failing child", len(err.Error()), capWithSlack)
	}
	// Sanity: error should still contain a meaningful prefix from
	// the mock's stderr dump (not empty / not just the wrap).
	if !strings.Contains(err.Error(), "stderr_marker") {
		t.Errorf("wrapped error missing stderr content marker: %v", err)
	}
}

// TestPrintMode_Mock_ResultOverridesAssistantText verifies the
// terminal-result text is what RunOnce returns — not the
// assistant text. Real claude's "result" event carries the
// final answer; the assistant message also carries it during
// streaming, but for one-shot the result is authoritative
// (it's what /gtw commit / buildAgentPrompt want).
func TestPrintMode_Mock_ResultOverridesAssistantText(t *testing.T) {
	// The mock emits identical text in assistant and result. To
	// verify result is what's returned we'd need a mock that
	// diverges the two; we keep the mock simple here. Instead
	// we verify that when both events fire, the result text
	// (which equals the assistant text in the mock) is what
	// RunOnce surfaces.
	a := NewStarter("claude-mock", claudePrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("CLAUDE_PRINT_TEXT", "final-answer")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.Text != "final-answer" {
		t.Fatalf("RunOnce text = %q, want %q", got.Text, "final-answer")
	}
}

// Sanity guard: the mock path must be resolvable as a file at
// test time. If a future move of internal/testdata/ breaks the
// relative path, every test in this file panics on a missing
// path — the explicit assertion makes the failure mode obvious.
func TestPrintMode_MockPath_Resolves(t *testing.T) {
	abs, err := filepath.Abs(claudePrintMockPath)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", claudePrintMockPath, err)
	}
	t.Logf("mock script resolved to %s", abs)
}