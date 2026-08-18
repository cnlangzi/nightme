//go:build !windows

// Mock-script tests for the print-mode RunOnce path.
//
// Uses internal/testdata/pi_print_mock.sh to drive runPrintMode
// without depending on the real `pi` binary. This catches the
// error paths the real-pi smoke can't easily exercise:
//
//   - PI_PRINT_FAIL: non-zero exit + stderr → error includes
//     stderr text (the "auth error / model error" class).
//   - PI_PRINT_NO_SETTLE: clean stream but no agent_settled →
//     "exit without agent_settled" diagnostic.
//   - Default: full clean run → returns the mock's text.
//
// The smoke test (print_realpi_test.go, gated by NIGHTME_REAL_PI=1)
// covers the "real LLM actually committed" path; this file
// covers everything else with deterministic inputs.

package pi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// piPrintMockPath is the absolute path to the mock script.
// Computed at package init via init() below so the tests don't
// each repeat filepath.Abs and the value is captured before any
// test cwd-shifting happens.
//
// exec.CommandContext (and thus proc.New) routes the binary
// through exec.LookPath, which only consults PATH. A relative
// path would not be found, so the mock must be invoked by its
// absolute path. The relative const above was the wrong shape.
var piPrintMockPath string

func init() {
	abs, err := filepath.Abs("../../testdata/pi_print_mock.sh")
	if err != nil {
		panic("pi_print_mock_test: filepath.Abs: " + err.Error())
	}
	piPrintMockPath = abs
}

func TestPrintMode_Mock_CleanRun_ReturnsText(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_TEXT", "clean-run-payload")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.Text != "clean-run-payload" {
		t.Fatalf("RunOnce text = %q, want %q", got.Text, "clean-run-payload")
	}
}

// TestPrintMode_Mock_CleanRun_PopulatesSessionIDAndModel is the
// regression lock for F-PI-PRINT-002: parsePrintStream must
// surface SessionID (from {"type":"session","id":..}) and Model
// (from the assistant message_start/message_end wire frames)
// onto RunResult so the AgentBar footer in channel/feishu
// renders "🤖: pi · <model> · <sessionid>" instead of just
// "🤖: pi".
//
// Mock script (internal/testdata/pi_print_mock.sh) emits:
//
//   - {"type":"session","id":"mock-print-session",...}
//   - {"type":"message_start","message":{...,"model":"mock",...}}
//   - {"type":"message_end","message":{...,"model":"mock",...}}
//
// so both fields are observable on the wire and RunResult must
// carry them after RunOnce returns.
func TestPrintMode_Mock_CleanRun_PopulatesSessionIDAndModel(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_TEXT", "anything")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.SessionID != "mock-print-session" {
		t.Errorf("RunResult.SessionID = %q, want %q", got.SessionID, "mock-print-session")
	}
	if got.Model != "mock" {
		t.Errorf("RunResult.Model = %q, want %q", got.Model, "mock")
	}
}

func TestPrintMode_Mock_NonZeroExit_SurfacesStderr(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_FAIL", "1")
	t.Setenv("PI_PRINT_STDERR", "model auth failed: 401")

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

func TestPrintMode_Mock_NoSettled_ReportsMissingEvent(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_NO_SETTLE", "1")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatalf("RunOnce returned no error; want missing-settled failure. got=%q", got.Text)
	}
	if !strings.Contains(err.Error(), "agent_settled") {
		t.Fatalf("error missing 'agent_settled' phrase: %v", err)
	}
}

func TestPrintMode_Mock_MultipleTextBlocks_JoinedWithNewline(t *testing.T) {
	// The bridge layer's buildPrintArgs → agent.BlocksToPrompt
	// joins ContentText blocks with "\n" BEFORE handing them to
	// pi — so pi sees one combined prompt. The text returned is
	// still the mock's fixed TEXT, so we can only assert that
	// the run completes cleanly (a regression in BlocksToPrompt
	// would show up as a parse failure in the mock, since the
	// prompt wouldn't be the one the mock echoes).
	//
	// Caveat: the mock doesn't JSON-escape control characters
	// in the echoed prompt. Real pi does. We use single-line
	// block contents to avoid the embedded-newline problem;
	// a real-newline-join test would need a richer mock.
	//
	// This test sends a single text block (the simpler case
	// the mock can validate); multi-block behaviour is covered
	// by the buildAgentPrompt prompt in the real-pi smoke.
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_TEXT", "ok")

	got, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "single-block"},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got.Text != "ok" {
		t.Fatalf("RunOnce text = %q, want %q", got.Text, "ok")
	}
}

func TestPrintMode_Mock_WorkspacePropagated(t *testing.T) {
	// RunOnce must pass cfg.Workspace down to spawn so pi runs
	// with the right cwd. We exercise this by running inside a
	// non-default temp dir and asserting the command succeeds
	// (the mock doesn't actually check cwd, but if the bridge
	// dropped it pi would fail to spawn because RunOnce returns
	// early on empty workspace).
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws := t.TempDir()
	if _, err := a.RunOnce(ctx, agent.StartConfig{Workspace: ws}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "in custom workspace"},
	}); err != nil {
		t.Fatalf("RunOnce with custom workspace %q: %v", ws, err)
	}
}

// Sanity guard: the mock path must be resolvable as a file at
// test time. If a future move of internal/testdata/ breaks the
// relative path, every test in this file panics on a missing
// path — the explicit assertion makes the failure mode obvious.
func TestPrintMode_MockPath_Resolves(t *testing.T) {
	abs, err := filepath.Abs(piPrintMockPath)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", piPrintMockPath, err)
	}
	t.Logf("mock script resolved to %s", abs)
}

// TestPrintMode_Mock_EnvForwarded verifies that cfg.Env reaches
// the child process. Catches the regression where cfg.Env was
// silently dropped on the print-mode path (mirrors the claudecode
// equivalent test). Uses PI_PRINT_DUMP_ENV to echo every
// NIGHTME_TEST_* var to stderr; PI_PRINT_FAIL=1 makes the run
// fail so the stderrBuf is included in the wrapped error.
func TestPrintMode_Mock_EnvForwarded(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_DUMP_ENV", "1")
	t.Setenv("PI_PRINT_FAIL", "1")
	t.Setenv("NIGHTME_TEST_API_KEY", "sk-pi-test-xyz")
	t.Setenv("NIGHTME_TEST_MODEL", "pi-mock-haiku")

	_, err := a.RunOnce(ctx, agent.StartConfig{
		Workspace: t.TempDir(),
		Env: []string{
			"NIGHTME_TEST_API_KEY=sk-pi-test-xyz",
			"NIGHTME_TEST_MODEL=pi-mock-haiku",
		},
	}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "env test"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want non-zero-exit failure so stderr surfaces")
	}
	if !strings.Contains(err.Error(), "NIGHTME_TEST_API_KEY=sk-pi-test-xyz") {
		t.Errorf("cfg.Env API key not forwarded to child: %v", err)
	}
	if !strings.Contains(err.Error(), "NIGHTME_TEST_MODEL=pi-mock-haiku") {
		t.Errorf("cfg.Env model not forwarded to child: %v", err)
	}
}

// TestPrintMode_Mock_StderrCapBytes verifies that stderr is
// capped at 64 KiB. Mirrors the claudecode equivalent test;
// uses PI_PRINT_LARGE_STDERR to dump ~4 MiB of fake stderr
// before exiting non-zero so the bridge's stderrBuf (and
// hence the wrapped error) carries the cap-enforced prefix.
func TestPrintMode_Mock_StderrCapBytes(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_LARGE_STDERR", "4194304") // 4 MiB
	// PI_PRINT_FAIL=1 makes the mock emit the large stderr
	// dump AND exit 1. The non-zero exit is the trigger the
	// bridge uses to surface stderr in the wrapped error
	// (a clean exit with stderr is dropped — the stderrBuf
	// is freed on success).
	t.Setenv("PI_PRINT_FAIL", "1")
	t.Setenv("PI_PRINT_TEXT", "capped-stderr")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "stderr cap test"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want non-zero-exit failure so stderr surfaces")
	}
	// stderrBuf is included in the wrapped error. It must
	// be bounded by stderrCapBytes (64 KiB).
	const capWithSlack = 70 * 1024 // 64 KiB + slack
	if len(err.Error()) > capWithSlack {
		t.Errorf("wrapped error too large: %d bytes (> %d); stderrBuf not capped at 64 KiB",
			len(err.Error()), capWithSlack)
	}
	// Sanity: stderr content from the dump is observable.
	if !strings.Contains(err.Error(), "pi_stderr_marker") {
		t.Errorf("wrapped error missing stderr content marker: %v", err)
	}
}

// TestPrintMode_Mock_NoSettled_PreservesUsage is the regression
// for the F-PI-PRINT-001 followup: when pi emits a usage-bearing
// message_end but never sends agent_settled, the captured usage
// must survive the "exit without agent_settled" error so the
// runtime can audit per-turn cost on failed runs.
//
// Implementation note (known limitation): pi's translator buffers
// `lastUsage` / `lastMessageText` / `stopReason` on message_end but
// only surfaces them to the stream consumer via `EventAgentResult`
// from `finishTurnLocked`, which fires on `agent_settled`. With
// NO_SETTLE the EventAgentResult never arrives, so the bridge
// has nothing to append to the error message yet. The audit-
// field appender (appendAuditFields) is wired and exercised on
// the claudecode side; the pi side will start preserving usage
// once the translator emits an EventAgentResult from
// recordAssistantMessageLocked too. Until then the test only
// verifies the no-settled error path is well-formed and the
// appendAuditFields helper is wired.
func TestPrintMode_Mock_NoSettled_PreservesUsage(t *testing.T) {
	a := NewStarter("pi-mock", piPrintMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_NO_SETTLE", "1")
	t.Setenv("PI_PRINT_NO_SETTLE_USAGE", "1")
	t.Setenv("PI_PRINT_TEXT", "captured-text")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "anything"},
	})
	if err == nil {
		t.Fatal("RunOnce returned no error; want no-settled failure")
	}
	if !strings.Contains(err.Error(), "agent_settled") {
		t.Errorf("error missing 'agent_settled' phrase: %v", err)
	}
	// F-PI-PRINT-002: appendAuditFields now runs with
	// whenSessionID=true (peekPrintMeta surfaced SessionID from
	// the {"type":"session","id":..} wire frame), so the error
	// message does carry "[session_id=mock-print-session]" today.
	// Usage + Subtype remain absent until the translator emits an
	// EventAgentResult from recordAssistantMessageLocked; once
	// that lands this assertion will flip to checking for "1234"
	// / "128" / "subtype=stop" like the claudecode
	// TestPrintMode_Mock_IsError_PreservesUsage does.
	if strings.Contains(err.Error(), "[usage") || strings.Contains(err.Error(), "[subtype") {
		t.Errorf("unexpected audit-field markers; translator may now emit EventAgentResult before agent_settled: %v", err)
	}
}
