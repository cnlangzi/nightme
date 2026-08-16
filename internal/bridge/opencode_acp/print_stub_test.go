// print_stub_test.go — end-to-end tests of runPrintMode against
// a stub `opencode` binary compiled from testdata/opencode-stub.
// Cross-platform (no build tag): the stub spawn glue uses only
// os/exec + filepath + go build, all portable.
//
// Why this file is NOT `_unix_test.go`:
//
// The historical `_unix_test.go` naming in the other bridges
// (codex / claudecode / pi) reflects their tests using Unix-only
// syscalls (procfs, signal, pty). opencode's print-mode path
// runs through agent.NewCmd — which itself splits Unix/Windows
// in internal/agent/exec_{unix,windows}.go — so the spawn glue
// here is portable. We name the file after what it tests
// (stub-driven end-to-end) rather than a platform the test is
// locked to.
//
// Windows specifics:
//
//   - The stub binary must end in `.exe` on Windows. The Go
//     toolchain refuses to write a `go build -o foo` target
//     without the platform-native extension on Windows
//     (the kernel rejects PE images whose path doesn't end
//     in .exe). We branch on runtime.GOOS == "windows" rather
//     than `runtime.GOEXE` because the latter was a Go
//     proposal that was never landed — the constant doesn't
//     exist in stdlib. macOS/Linux take ""; Windows takes
//     ".exe". We intentionally don't add `.bat` / `.cmd` —
//     `go build` produces a native PE binary, never a batch
//     shim.
//
// What we lock here against the prototype parser + spawn glue:
//
//   - happy path: a simple prompt produces a RunResult with
//     Text, SessionID, Subtype="completed", DurationMs > 0
//   - exit-1 with stderr: stderr is appended to the error
//   - exit-0 with empty answer: error includes "empty answer"
//   - model_error event: error includes the captured reason
//   - image blocks: -f flag forwarded, prompt placeholder
//     concatenated into the run, response mentions the file
//
// The stub is at testdata/opencode-stub (compiled by the test
// itself if absent) and is parameterized by env vars rather
// than argv so the same binary covers all scenarios.

package opencode_acp

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

// requireStubOpencode skips the test when the stub binary is
// not available. The test harness compiles it from
// testdata/opencode-stub on demand; if that fails (no Go
// toolchain, or the source is missing), skip rather than
// fail — these tests are environment-sensitive and we don't
// want CI flakes when a developer hasn't touched opencode.
func requireStubOpencode(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("NIGHTME_OPENCODE_STUB"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Default: build from testdata into a per-test temp path.
	src := filepath.Join("testdata", "opencode-stub")
	tmp, err := os.MkdirTemp("", "opencode-stub-*")
	if err != nil {
		t.Skipf("cannot create temp dir for stub: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	// Append the platform-native executable suffix (.exe on
	// Windows, "" elsewhere). Without this, `go build -o
	// bin` on Windows fails with "Access is denied" because
	// the kernel refuses to create an executable image
	// without the .exe extension. We hard-code GOOS rather
	// than `runtime.GOEXE` (a Go proposal that was never
	// landed — the constant does not exist in stdlib).
	stubExt := ""
	if runtime.GOOS == "windows" {
		stubExt = ".exe"
	}
	bin := filepath.Join(tmp, "opencode-stub"+stubExt)
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("opencode-stub build failed (Go toolchain or source missing): %v\n%s", err, string(out))
	}
	return bin
}

// withStubEnv runs fn with OPENCODE_STUB_BEHAVIOR / TEXT set
// in the inherited process env. Caller-side this propagates
// into the spawned child via os.Environ (see runPrintMode
// cmd.Env handling).
func withStubEnv(t *testing.T, behavior, text string) {
	t.Helper()
	t.Setenv("OPENCODE_STUB_BEHAVIOR", behavior)
	t.Setenv("OPENCODE_STUB_TEXT", text)
}

func newStubStarter(t *testing.T, bin string) *Starter {
	t.Helper()
	return NewStarter("opencode-stub-test", bin, nil)
}

// TestRunPrintMode_HappyPath — simplest smoke: emit one text
// event + step_finish, exit 0. Asserts Text / SessionID /
// Subtype / DurationMs.
func TestRunPrintMode_HappyPath(t *testing.T) {
	bin := requireStubOpencode(t)
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
	if result.SessionID == "" {
		t.Errorf("SessionID is empty; expected ses_stub from stub")
	}
	if result.Subtype != "completed" {
		t.Errorf("Subtype = %q, want completed", result.Subtype)
	}
	if result.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", result.DurationMs)
	}
	if result.Usage != nil {
		t.Errorf("Usage = %+v, want nil (wire doesn't carry tokens)", result.Usage)
	}
	if result.Model != "" {
		t.Errorf("Model = %q, want \"\" (wire doesn't carry model)", result.Model)
	}
	t.Logf("result: Text=%q Subtype=%s DurationMs=%d SessionID=%s",
		result.Text, result.Subtype, result.DurationMs, result.SessionID)
}

// TestRunPrintMode_ExitOneWithStderr — exit 1 with stderr
// output. The error must surface stderr (model/auth errors
// land in stderr in real opencode).
func TestRunPrintMode_ExitOneWithStderr(t *testing.T) {
	bin := requireStubOpencode(t)
	withStubEnv(t, "exit_1", "wont-survive")

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
	if !strings.Contains(err.Error(), "opencode:") {
		t.Errorf("err = %q, want contains 'opencode:' prefix", err.Error())
	}
}

// TestRunPrintMode_EmptyAnswer — process exits 0 but
// produced no text events. Must return "empty answer" error,
// not silently succeed.
func TestRunPrintMode_EmptyAnswer(t *testing.T) {
	bin := requireStubOpencode(t)
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

// TestRunPrintMode_ModelErrorEvent — `error` event on the
// wire (CLI process exits 0 in some bash-pipe edge cases).
// The captured reason must surface.
func TestRunPrintMode_ModelErrorEvent(t *testing.T) {
	bin := requireStubOpencode(t)
	withStubEnv(t, "model_error", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil on error event")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %q, want contains 'rate limited' (from error event payload)", err.Error())
	}
	// Without an env-supplied text, the stub emits hardcoded
	// "partial". With this stub's consolidated model_error
	// behavior, that "partial" text is ALSO surfaced in the
	// error message — the regression test below uses
	// OPENCODE_STUB_TEXT to override it to a non-default value.
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("err = %q, want contains 'partial' (stub default text surfaced alongside error)", err.Error())
	}
}

// TestRunPrintMode_ModelErrorEventWithPartialText — when the
// model streams text THEN errors out, both must surface:
// the partial answer so the user can see what the model
// managed to say, and the error reason for the failure.
// Regression guard for F-OPENCODE-PRINT-001 — the previous
// implementation discarded the partial text on the errMsg
// branch.
func TestRunPrintMode_ModelErrorEventWithPartialText(t *testing.T) {
	bin := requireStubOpencode(t)
	withStubEnv(t, "model_error", "half-done answer")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("err = nil, want non-nil on error event")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rate limited") {
		t.Errorf("err = %q, want contains 'rate limited' (from error event payload)", msg)
	}
	if !strings.Contains(msg, "half-done answer") {
		t.Errorf("err = %q, want contains 'half-done answer' (partial text preserved alongside error)", msg)
	}
}

// TestRunPrintMode_ImageBlocks — content image blocks
// trigger `-f <path>` flags AND `[file: <path>]` placeholders
// in the prompt. The stub's "echo_image" behavior asserts on
// both the presence of `-f` in argv AND the trailing positional
// prompt; this locks the F-OPENCODE-PRINT-001 fix where
// buildPrintArgs appends prompt as the final argv element.
func TestRunPrintMode_ImageBlocks(t *testing.T) {
	bin := requireStubOpencode(t)
	withStubEnv(t, "echo_image", "ok")

	// Use a real (small) image file on disk so `-f` could in
	// principle resolve it. We don't validate the model sees
	// the image — that requires a real provider. We validate
	// the spawn + parser survive image-bearing prompts.
	tmpImg := filepath.Join(t.TempDir(), "tiny.png")
	if err := os.WriteFile(tmpImg, []byte("PNG-stub"), 0o644); err != nil {
		t.Fatalf("write tmp img: %v", err)
	}

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{
			{Type: agent.ContentText, Text: "describe this image"},
			{Type: agent.ContentImage, Path: tmpImg, MediaType: "image/png"},
		})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// echo_image embeds the trailing positional prompt
	// verbatim ("echo:<prompt>"). The prompt includes the
	// user's text + the [image: ...] placeholder, so we
	// assert on a substring rather than an exact match.
	if !strings.Contains(result.Text, "describe this image") {
		t.Errorf("Text = %q, want contains 'describe this image' (prompt was forwarded to child)", result.Text)
	}
	if !strings.Contains(result.Text, "echo:") {
		t.Errorf("Text = %q, want prefix 'echo:' (argv-aware stub path)", result.Text)
	}
}

// TestRunPrintMode_PromptForwarded — regression guard for
// F-OPENCODE-PRINT-001. The stub's "echo" behavior embeds the
// last positional argv element verbatim in the response, so
// a regression that drops the prompt at the spawn site (the
// bug that originally shipped in this branch) would produce
// an empty response and fail this test.
func TestRunPrintMode_PromptForwarded(t *testing.T) {
	bin := requireStubOpencode(t)
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

// TestRunPrintMode_ReasoningOnly — regression guard for the
// empty-answer guard. A stream that emits only `reasoning`
// (no `text`) must NOT be reported as "opencode: empty answer"
// — the model engaged via its thinking trace, even if it
// produced no final text. The result.Text is empty (reasoning
// is dropped by RunOnce) but the call SUCCEEDS.
func TestRunPrintMode_ReasoningOnly(t *testing.T) {
	bin := requireStubOpencode(t)
	withStubEnv(t, "reasoning_only", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runPrintMode(ctx, s,
		agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "think hard"}})
	if err != nil {
		t.Fatalf("err = %v, want nil (reasoning-only stream must NOT trip empty-answer guard)", err)
	}
	if result.Text != "" {
		t.Errorf("Text = %q, want \"\" (reasoning is dropped by RunOnce)", result.Text)
	}
	if result.Subtype != "completed" {
		t.Errorf("Subtype = %q, want completed", result.Subtype)
	}
}

// TestRunPrintMode_MissingWorkspace — guard: cfg.Workspace
// is required.
func TestRunPrintMode_MissingWorkspace(t *testing.T) {
	bin := requireStubOpencode(t)
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