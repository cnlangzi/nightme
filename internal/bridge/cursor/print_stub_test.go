// print_stub_test.go — end-to-end tests of runPrintMode against
// a stub `agent` binary compiled from testdata/cursor-stub.
// Cross-platform (no build tag): the stub spawn glue uses only
// os/exec + filepath + go build, all portable.
//
// What we lock here against the prototype parser + spawn glue:
//
//   - happy path: a simple prompt produces a RunResult with
//     Text, Subtype="completed", DurationMs > 0
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
// text output. Asserts Text / Subtype / DurationMs.
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
	if result.Model != "" {
		t.Errorf("Model = %q, want \"\"", result.Model)
	}
	t.Logf("result: Text=%q Subtype=%s DurationMs=%d",
		result.Text, result.Subtype, result.DurationMs)
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
