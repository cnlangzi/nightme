// sink_test.go — unit tests for the per-call WithEventSink wiring
// on Starter.RunOnce / Starter.Review.
//
// Background: prior to fix-codex-runonce-review-event the codex
// bridge's print-mode and review paths (`runPrintMode` +
// `runCodexReviewPlain`) silently dropped the `opts` argument. The
// /review dispatcher installed a sink via `agent.WithEventSink`, but
// the codex bridge never read it, so the chat channel saw 30s of
// silence followed by a single text dump. These tests lock the
// post-fix contract: bridge delivers Ready → Text → Result on the
// success path, Error on the failure path, and is a no-op when the
// sink is nil.
//
// We use a fake "codex" binary (a small shell script) instead of the
// real `codex` CLI. This keeps the tests:
//
//   - fast (sub-second, no API key, no network),
//   - deterministic (script emits a fixed payload, not model output),
//   - portable (no `NIGHTME_REAL_CODEX=1` opt-in guard needed).
//
// The fake-binary approach also matches the existing pattern in
// `print_internal_unix_test.go` (which uses a fake stderr/stdout
// producer via `proc.New` with `echo`).

//go:build !windows

package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// eventRecorder is a thread-safe AgentEvent collector used by the
// sink tests. We can't use a plain slice because the bridge may emit
// from a goroutine (stderrDrain, NDJSON scanner) — even though the
// sink itself is synchronous on the bridge's goroutine, tests want
// to assert on the observed order.
type eventRecorder struct {
	mu     sync.Mutex
	events []agent.AgentEvent
}

func (r *eventRecorder) record(ev agent.AgentEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) sink() func(agent.AgentEvent) {
	return func(ev agent.AgentEvent) { r.record(ev) }
}

func (r *eventRecorder) snapshot() []agent.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agent.AgentEvent, len(r.events))
	copy(out, r.events)
	return out
}

// kinds extracts the EventKind of each recorded event for concise
// assertion messages.
func kinds(evs []agent.AgentEvent) []agent.EventKind {
	out := make([]agent.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// gitInit seeds a minimal git repository at dir with an initial
// commit on the named branch. Lets detectBranch (which calls
// git symbolic-ref) return a stable value across the test run
// without depending on the host's git config.
func gitInit(t *testing.T, dir, branch string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "-C", dir, "init", "--initial-branch=" + branch},
		{"git", "-C", dir, "config", "user.email", "test@test"},
		{"git", "-C", dir, "config", "user.name", "Test"},
		{"git", "-C", dir, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("gitInit %v: %v\n%s", args, err, out)
		}
	}
}

// writeFakeCodex stages a shell script that emulates a codex CLI
// (review or exec surface) and returns its absolute path. The
// script's behaviour is parameterised by `mode`:
//
//   - "review": emulates `codex exec review --json -o <file>`
//     — finds the `-o` flag, writes `stdout` (the final review
//     answer) there; emits a thread.started + turn.completed NDJSON
//     pair on stdout so runCodexReviewPlin's NDJSON parser
//     exercises the SessionID path. Stderr + exit code are
//     propagated from the env vars.
//   - "exec":   emulates `codex exec --json -o <file>` — same
//     shape as review-mode minus the review-tuned argv (see
//     runPrintMode). Item.completed[error] is omitted — the
//     model field on the Result is allowed to be empty per
//     print_real_unix_test.go's tolerance.
//
// The script uses POSIX `sh` and only depends on `echo`, `printf`,
// and shell redirection — no codex binary required.
func writeFakeCodex(t *testing.T, mode, stdout, stderr string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake-binary tests are POSIX-only; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-codex")
	script := ""
	switch mode {
	case "review", "exec":
		// Both surfaces share the same wire shape now: NDJSON on
		// stdout with thread.started + turn.completed, plus the
		// -o <tmpfile> carrying the final agent message.
		//
		// argv can be either:
		//   `codex exec …` (plain exec — argv[1]=exec)
		//   `codex -C <ws> exec review …` (exec review — argv[1]=-C,
		//                                   argv[2]=<ws>, argv[3]=exec,
		//                                   argv[4]=review)
		// so the script walks the whole argv looking for the first
		// `-o <file>` pair. Liberal by design — robust to future
		// codex flag-order tweaks.
		script = "#!/bin/sh\n" +
			"# Skip argv[0] ($0 = script path).\n" +
			"shift\n" +
			"# Walk argv looking for -o <file>.\n" +
			"while [ $# -gt 0 ]; do\n" +
			"  if [ \"$1\" = \"-o\" ] && [ $# -ge 2 ]; then\n" +
			"    shift\n" +
			"    printf '%s' \"$CODEX_FAKE_STDOUT\" > \"$1\"\n" +
			"  fi\n" +
			"  shift\n" +
			"done\n" +
			// Emit thread.started + turn.completed so the parser
			// sees the canonical wire shape and populates
			// SessionID / Usage. Item.completed[error] is omitted
			// — model field may stay empty per print_real_unix_test.go's
			// tolerance.
			"  printf '{\"type\":\"thread.started\",\"thread_id\":\"thread-fake-123\"}\\n'\n" +
			"  printf '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":22,\"cached_input_tokens\":3}}\\n'\n" +
			"  printf '%s' \"$CODEX_FAKE_STDERR\" 1>&2\n" +
			"  exit \"$CODEX_FAKE_EXIT\"\n" +
			"fi\n" +
			"echo 'fake-codex: unsupported subcommand' 1>&2\n" +
			"exit 2\n"
	default:
		t.Fatalf("writeFakeCodex: unknown mode %q", mode)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	t.Setenv("CODEX_FAKE_STDOUT", stdout)
	t.Setenv("CODEX_FAKE_STDERR", stderr)
	t.Setenv("CODEX_FAKE_EXIT", strconv.Itoa(exitCode))
	return path
}

// TestRunCodexReviewPlain_SinkReadyResult — happy path: the
// review-mode fake emits "REVIEW OK" via thread.started +
// turn.completed + writes "REVIEW OK" to the -o tempfile. The sink
// must see Ready → Ready(thread_id) → Result(non-nil, Text=="REVIEW
// OK") in that order. F-CODEX-DOUBLE-RENDER fix: NO Text emit —
// Result is the single point of prose delivery (dsh gates Result.Text
// the same way; see internal/bridge/dsh/dispatch.go).
func TestRunCodexReviewPlain_SinkReadyResult(t *testing.T) {
	fake := writeFakeCodex(t, "review", "REVIEW OK\n", "", 0)

	rec := &eventRecorder{}
	// Use a temp dir as workspace and seed a git repo with a
	// known branch so detectBranch returns " main " — locks the
	// AgentName / Workspace / Branch stamping contract (P2-#4
	// follow-up; dsh drains emit all five fields).
	ws := t.TempDir()
	gitInit(t, ws, "main")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := runCodexReviewPlain(ctx, NewStarter("codex-test", fake, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"}, agent.WithEventSink(rec.sink()))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "REVIEW OK" {
		t.Errorf("RunResult.Text = %q, want REVIEW OK", res.Text)
	}
	if res.SessionID != "thread-fake-123" {
		t.Errorf("RunResult.SessionID = %q, want thread-fake-123", res.SessionID)
	}

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("sink observed %d events %v, want 3 [Ready Ready Result] (F-CODEX-DOUBLE-RENDER fix + thread.started-driven Ready)",
			len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("ev[0] = %s, want Ready (up-front)", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentReady {
		t.Errorf("ev[1] = %s, want Ready (thread.started-driven)", evs[1].Kind)
	}
	if evs[1].SessionID != "thread-fake-123" {
		t.Errorf("ev[1].SessionID = %q, want thread-fake-123", evs[1].SessionID)
	}
	if evs[2].Kind != agent.EventAgentResult {
		t.Errorf("ev[2] = %s, want Result", evs[2].Kind)
	}
	if evs[2].Result == nil {
		t.Fatalf("ev[2].Result is nil")
	}
	if evs[2].Result.Text != "REVIEW OK" {
		t.Errorf("ev[2].Result.Text = %q, want REVIEW OK", evs[2].Result.Text)
	}
	if evs[2].SessionID != "thread-fake-123" {
		t.Errorf("ev[2].SessionID = %q, want thread-fake-123", evs[2].SessionID)
	}
	// P2-#4 follow-up: every codex sink event must stamp
	// AgentName / Workspace / Branch so statusbar renders
	// the full three-line footer (dsh's drain shape).
	for i, ev := range evs {
		if ev.AgentName != "codex-test" {
			t.Errorf("ev[%d].AgentName = %q, want codex-test", i, ev.AgentName)
		}
		if ev.Workspace != ws {
			t.Errorf("ev[%d].Workspace = %q, want %q", i, ev.Workspace, ws)
		}
		if ev.Branch != "main" {
			t.Errorf("ev[%d].Branch = %q, want main", i, ev.Branch)
		}
	}
}

// TestRunCodexReviewPlain_SinkErrorOnExitFailure — failure path:
// the fake exits 2 with stderr "boom". The sink must see Ready
// and then Error(non-nil Err). It MUST NOT see Text or Result
// (the review answer was empty / process died, and the /review
// dispatcher renders the formatted failure via emitter.Send).
func TestRunCodexReviewPlain_SinkErrorOnExitFailure(t *testing.T) {
	fake := writeFakeCodex(t, "review", "", "boom\n", 2)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runCodexReviewPlain(ctx, NewStarter("codex-test", fake, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"}, agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("sink observed %d events %v, want 3 [Ready Ready Error] (up-front + thread.started + failure)", len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("ev[0] = %s, want Ready (up-front)", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentReady {
		t.Errorf("ev[1] = %s, want Ready (thread.started-driven)", evs[1].Kind)
	}
	if evs[2].Kind != agent.EventAgentError {
		t.Errorf("ev[2] = %s, want Error", evs[2].Kind)
	}
	if evs[2].Err == nil {
		t.Errorf("ev[2].Err is nil; want non-nil")
	}
	// Diagnostic is REQUIRED — outbound.Translate:188-202 silently
	// drops EventAgentError events with nil Diagnostic, which would
	// leave the chat receipt stuck at 🔄 even though the dispatcher
	// surfaces a separate ❌ OutReply. BridgeExitNonZeroExit because
	// the fake exits with code 2.
	if evs[2].Diagnostic == nil {
		t.Fatalf("ev[2].Diagnostic is nil; outbound.Translate would drop this error")
	}
	if evs[2].Diagnostic.ExitKind != agent.BridgeExitNonZeroExit {
		t.Errorf("ev[2].Diagnostic.ExitKind = %s, want non-zero-exit",
			evs[2].Diagnostic.ExitKind)
	}
	if evs[2].Diagnostic.AgentName != "codex" {
		t.Errorf("ev[2].Diagnostic.AgentName = %q, want codex",
			evs[2].Diagnostic.AgentName)
	}
	if !strings.Contains(evs[2].Diagnostic.StderrTail, "boom") {
		t.Errorf("ev[2].Diagnostic.StderrTail = %q, want contains 'boom'",
			evs[2].Diagnostic.StderrTail)
	}
}

// TestRunCodexReviewPlain_SinkErrorOnEmptyAnswer — `codex review`
// exits 0 cleanly but produces no stdout (e.g. running against
// an empty branch with no diff). formatCodexExitError returns
// non-nil because finalText is empty, so the bridge still emits
// EventAgentError — but the title "codex bridge died (clean-exit)"
// is misleading; the bridge didn't die. Verify the Diagnostic
// uses BridgeExitUnknown (NOT BridgeExitCleanExit) so the
// rendered card title stays consistent with the body "codex:
// empty review answer".
func TestRunCodexReviewPlain_SinkErrorOnEmptyAnswer(t *testing.T) {
	fake := writeFakeCodex(t, "review", "", "", 0)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runCodexReviewPlain(ctx, NewStarter("codex-test", fake, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"}, agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatalf("expected error from empty answer, got nil")
	}
	if !strings.Contains(err.Error(), "empty review answer") {
		t.Errorf("err = %q, want contains 'empty review answer'", err)
	}

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("sink observed %d events %v, want 3 [Ready Ready Error] (up-front + thread.started + empty-answer failure)", len(evs), kinds(evs))
	}
	if evs[2].Kind != agent.EventAgentError {
		t.Fatalf("ev[2].Kind = %s, want Error", evs[2].Kind)
	}
	if evs[2].Diagnostic == nil {
		t.Fatalf("ev[2].Diagnostic is nil")
	}
	// CRITICAL: must NOT be BridgeExitCleanExit. The Feishu
	// renderer titles the error card with the ExitKind string
	// ("clean-exit", "non-zero-exit", etc.); CleanExit here
	// would say "⚠️ codex bridge died (clean-exit)" while the
	// body says "codex: empty review answer" — contradiction.
	if evs[2].Diagnostic.ExitKind == agent.BridgeExitCleanExit {
		t.Errorf("ev[2].Diagnostic.ExitKind = clean-exit; want anything-but-clean-exit " +
			"so the card title doesn't claim the bridge died")
	}
	if evs[2].Diagnostic.ExitKind != agent.BridgeExitUnknown {
		t.Errorf("ev[2].Diagnostic.ExitKind = %s, want unknown (empty-answer fallback)",
			evs[2].Diagnostic.ExitKind)
	}
	if evs[2].Diagnostic.AgentName != "codex" {
		t.Errorf("ev[2].Diagnostic.AgentName = %q, want codex",
			evs[2].Diagnostic.AgentName)
	}
}

// TestRunCodexReviewPlain_NilSink — sink=nil must not panic and
// must NOT fabricate events. The contract is "no observer, behave
// as before this option existed" (agent.go:1211-1213). On success
// we still expect the underlying RunResult to be correct.
func TestRunCodexReviewPlain_NilSink(t *testing.T) {
	fake := writeFakeCodex(t, "review", "REVIEW OK", "", 0)

	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := runCodexReviewPlain(ctx, NewStarter("codex-test", fake, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "REVIEW OK" {
		t.Errorf("RunResult.Text = %q, want REVIEW OK", res.Text)
	}
}

// TestRunCodexReviewPlain_NilSinkOnFailure — nil sink + non-zero
// exit must also not panic. Locks the "no observer" branch on the
// error path.
func TestRunCodexReviewPlain_NilSinkOnFailure(t *testing.T) {
	fake := writeFakeCodex(t, "review", "", "boom\n", 2)

	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runCodexReviewPlain(ctx, NewStarter("codex-test", fake, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestStarterReview_ForwardsSink — end-to-end through the public
// Starter.Review API: it must forward opts to runCodexReview,
// which forwards to runCodexReviewPlain. We assert by inspecting
// the sink that Starter.Review installs.
func TestStarterReview_ForwardsSink(t *testing.T) {
	fake := writeFakeCodex(t, "review", "REVIEW OK", "", 0)
	s := NewStarter("codex-test", fake, nil)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := s.Review(ctx, agent.StartConfig{Workspace: ws}, agent.WithEventSink(rec.sink()))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "REVIEW OK" {
		t.Errorf("RunResult.Text = %q, want REVIEW OK", res.Text)
	}
	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("sink observed %d events %v, want 3 [Ready Ready Result] (F-CODEX-DOUBLE-RENDER fix + thread.started-driven Ready)",
			len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady ||
		evs[1].Kind != agent.EventAgentReady ||
		evs[2].Kind != agent.EventAgentResult {
		t.Errorf("event order = %v, want [Ready Ready Result]", kinds(evs))
	}
}

// TestRunPrintMode_SinkReadyReadyResult — exec path emits
// thread.started so the bridge re-emits Ready with the now-known
// SessionID. Sequence: Ready(empty) → Ready(thread-fake-123) →
// Result(Text=EXEC OK, Usage populated). F-CODEX-DOUBLE-RENDER
// regression: pre-fix this was [Ready Ready Text Result] — see
// TestRunCodexReviewPlain_SinkReadyResult for the rationale. The
// two Readys are a deliberate design choice (see the comment in
// runPrintMode's NDJSON callback): the up-front Ready flips the
// StatusBar, the thread.started-driven Ready lets the channel
// receipt header render the session id.
func TestRunPrintMode_SinkReadyReadyResult(t *testing.T) {
	fake := writeFakeCodex(t, "exec", "EXEC OK", "", 0)
	s := NewStarter("codex-test", fake, nil)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := runPrintMode(ctx, s, agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
		agent.WithEventSink(rec.sink()))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "EXEC OK" {
		t.Errorf("RunResult.Text = %q, want EXEC OK", res.Text)
	}
	if res.SessionID != "thread-fake-123" {
		t.Errorf("RunResult.SessionID = %q, want thread-fake-123", res.SessionID)
	}

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("sink observed %d events %v, want 3 [Ready Ready Result] (F-CODEX-DOUBLE-RENDER fix)",
			len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("ev[0] = %s, want Ready", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentReady {
		t.Errorf("ev[1] = %s, want Ready (thread.started-driven)", evs[1].Kind)
	}
	if evs[1].SessionID != "thread-fake-123" {
		t.Errorf("ev[1].SessionID = %q, want thread-fake-123", evs[1].SessionID)
	}
	if evs[2].Kind != agent.EventAgentResult {
		t.Errorf("ev[2] = %s, want Result", evs[2].Kind)
	}
	if evs[2].Result == nil {
		t.Fatalf("ev[2].Result is nil")
	}
	if evs[2].Result.Text != "EXEC OK" {
		t.Errorf("ev[2].Result.Text = %q, want EXEC OK", evs[2].Result.Text)
	}
	if evs[2].Result.Usage == nil {
		t.Fatalf("ev[2].Result.Usage is nil; want populated from turn.completed")
	}
	if evs[2].Result.Usage.InputTokens != 11 || evs[2].Result.Usage.OutputTokens != 22 {
		t.Errorf("ev[2].Result.Usage = %+v, want in=11 out=22", evs[2].Result.Usage)
	}
	if evs[2].SessionID != "thread-fake-123" {
		t.Errorf("ev[2].SessionID = %q, want thread-fake-123", evs[2].SessionID)
	}
}

// TestStarterRunOnce_ForwardsSink — public Starter.RunOnce must
// forward opts to runPrintMode. Asserts the Ready →
// Ready(thread-fake-123) → Result sequence through the external
// API (F-CODEX-DOUBLE-RENDER fix: no Text emit).
func TestStarterRunOnce_ForwardsSink(t *testing.T) {
	fake := writeFakeCodex(t, "exec", "EXEC OK", "", 0)
	s := NewStarter("codex-test", fake, nil)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := s.RunOnce(ctx, agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
		agent.WithEventSink(rec.sink()))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("sink observed %d events %v, want 3", len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady ||
		evs[1].Kind != agent.EventAgentReady ||
		evs[2].Kind != agent.EventAgentResult {
		t.Errorf("event order = %v, want [Ready Ready Result]", kinds(evs))
	}
}

// TestRunPrintMode_NilSink — runPrintMode with no sink must
// neither panic nor fabricate events. Locks the "no observer,
// behave as before" contract on the exec path.
func TestRunPrintMode_NilSink(t *testing.T) {
	fake := writeFakeCodex(t, "exec", "EXEC OK", "", 0)
	s := NewStarter("codex-test", fake, nil)

	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := runPrintMode(ctx, s, agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "EXEC OK" {
		t.Errorf("RunResult.Text = %q, want EXEC OK", res.Text)
	}
}

// TestRunPrintMode_SinkErrorOnStartFailure — when the configured
// `command` resolves to a path that does not exist, `child.Start`
// fails before any pipes / NDJSON work happens. The sink contract
// requires every Ready to be paired with a terminal event, so the
// sink MUST observe Ready followed by Error (and no Text /
// Result). Locks the early-return fix at print.go:163-168 + 188-
// 198 + 201-211.
func TestRunPrintMode_SinkErrorOnStartFailure(t *testing.T) {
	// Path that definitely does not exist. Using an absolute
	// /tmp-anchored name so the test is hermetic.
	missing := "/tmp/definitely-not-a-binary-1234567890"
	s := NewStarter("codex-test", missing, nil)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s, agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
		agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	evs := rec.snapshot()
	if len(evs) != 2 {
		t.Fatalf("sink observed %d events %v, want 2 [Ready Error]", len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("ev[0] = %s, want Ready", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentError {
		t.Errorf("ev[1] = %s, want Error", evs[1].Kind)
	}
	if evs[1].Err == nil {
		t.Errorf("ev[1].Err is nil; want non-nil")
	}
	// Diagnostic is REQUIRED — outbound.Translate:188-202 silently
	// drops EventAgentError events with nil Diagnostic, which would
	// leave the chat receipt stuck at 🔄 even though the dispatcher
	// surfaces a separate ❌ OutReply. BridgeExitUnknown because
	// the subprocess never started (Start() failure).
	if evs[1].Diagnostic == nil {
		t.Fatalf("ev[1].Diagnostic is nil; outbound.Translate would drop this error")
	}
	if evs[1].Diagnostic.ExitKind != agent.BridgeExitUnknown {
		t.Errorf("ev[1].Diagnostic.ExitKind = %s, want unknown (spawn failure)",
			evs[1].Diagnostic.ExitKind)
	}
	if evs[1].Diagnostic.AgentName != "codex" {
		t.Errorf("ev[1].Diagnostic.AgentName = %q, want codex",
			evs[1].Diagnostic.AgentName)
	}
}
func TestRunPrintMode_NilSinkOnStartFailure(t *testing.T) {
	missing := "/tmp/definitely-not-a-binary-1234567890"
	s := NewStarter("codex-test", missing, nil)

	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s, agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestRunCodexReviewPlain_SinkErrorOnStartFailure — same shape as
// TestRunPrintMode_SinkErrorOnStartFailure but on the review path.
// Locks the early-return fix at print.go:858-880.
func TestRunCodexReviewPlain_SinkErrorOnStartFailure(t *testing.T) {
	missing := "/tmp/definitely-not-a-binary-1234567890"

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runCodexReviewPlain(ctx, NewStarter("codex-test", missing, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"}, agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	evs := rec.snapshot()
	if len(evs) != 2 {
		t.Fatalf("sink observed %d events %v, want 2 [Ready Error]", len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("ev[0] = %s, want Ready", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentError {
		t.Errorf("ev[1] = %s, want Error", evs[1].Kind)
	}
	if evs[1].Err == nil {
		t.Errorf("ev[1].Err is nil; want non-nil")
	}
	// Diagnostic is REQUIRED — outbound.Translate:188-202 silently
	// drops EventAgentError events with nil Diagnostic. Bridge-
	// ExitUnknown because the review subprocess never started.
	if evs[1].Diagnostic == nil {
		t.Fatalf("ev[1].Diagnostic is nil; outbound.Translate would drop this error")
	}
	if evs[1].Diagnostic.ExitKind != agent.BridgeExitUnknown {
		t.Errorf("ev[1].Diagnostic.ExitKind = %s, want unknown",
			evs[1].Diagnostic.ExitKind)
	}
	if evs[1].Diagnostic.AgentName != "codex" {
		t.Errorf("ev[1].Diagnostic.AgentName = %q, want codex",
			evs[1].Diagnostic.AgentName)
	}
}

// TestRunCodexReviewPlain_NilSinkOnStartFailure — nil sink +
// missing binary must not panic on the review early-return
// failure path.
func TestRunCodexReviewPlain_NilSinkOnStartFailure(t *testing.T) {
	missing := "/tmp/definitely-not-a-binary-1234567890"
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runCodexReviewPlain(ctx, NewStarter("codex-test", missing, nil), agent.StartConfig{Workspace: ws}, []string{"--uncommitted"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestRunPrintMode_SinkErrorOnCreateTempFailure — force
// `os.CreateTemp` to fail by pointing TMPDIR at a path that is a
// file (not a directory). CreateTemp opens its target via
// `os.OpenFile(name, …)` and gets ENOTDIR. We then assert the sink
// sees Ready + Error and never sees Text / Result. The TMPDIR
// override is scoped to this test via t.Setenv.
func TestRunPrintMode_SinkErrorOnCreateTempFailure(t *testing.T) {
	// Make TMPDIR point at a path that exists but is a regular
	// file. CreateTemp's `os.OpenFile(…, O_CREATE|O_EXCL)` will
	// fail with ENOTDIR on Linux/macOS. t.TempDir gives us a
	// hermetic parent; the file lives next to it.
	parent := t.TempDir()
	tmpfile := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(tmpfile, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("TMPDIR", tmpfile)
	t.Setenv("TMP", tmpfile)
	t.Setenv("TEMP", tmpfile)

	fake := writeFakeCodex(t, "exec", "EXEC OK", "", 0)
	s := NewStarter("codex-test", fake, nil)

	rec := &eventRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s, agent.StartConfig{Workspace: t.TempDir()},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
		agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatalf("expected error from CreateTemp failure, got nil")
	}

	evs := rec.snapshot()
	// Must be exactly [Ready Error]. CreateTemp fails BEFORE
	// spawn, so neither pipe nor Start runs.
	if len(evs) != 2 {
		t.Fatalf("sink observed %d events %v, want 2 [Ready Error]", len(evs), kinds(evs))
	}
	if evs[0].Kind != agent.EventAgentReady {
		t.Errorf("ev[0] = %s, want Ready", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentError {
		t.Errorf("ev[1] = %s, want Error", evs[1].Kind)
	}
	if evs[1].Err == nil {
		t.Errorf("ev[1].Err is nil; want non-nil")
	}
	// Diagnostic required — see translate.go:188-202. The
	// CreateTemp-failure path never spawned a subprocess so
	// stderr is empty; the renderer falls back to the
	// synthesized body (translate.go:216-227).
	if evs[1].Diagnostic == nil {
		t.Fatalf("ev[1].Diagnostic is nil; outbound.Translate would drop this error")
	}
	if evs[1].Diagnostic.ExitKind != agent.BridgeExitUnknown {
		t.Errorf("ev[1].Diagnostic.ExitKind = %s, want unknown",
			evs[1].Diagnostic.ExitKind)
	}
	if evs[1].Diagnostic.AgentName != "codex" {
		t.Errorf("ev[1].Diagnostic.AgentName = %q, want codex",
			evs[1].Diagnostic.AgentName)
	}
}

// TestRunPrintMode_SinkErrorOnEmptyAnswer — `codex exec` exits 0
// cleanly but produces no stdout AND no -o file (subprocess ran,
// model produced no answer). formatCodexExitError returns non-nil
// because finalText is empty; verify the Diagnostic uses
// BridgeExitUnknown (NOT BridgeExitCleanExit) so the rendered card
// title doesn't claim the bridge died when it just had nothing to
// say. Mirror of TestRunCodexReviewPlain_SinkErrorOnEmptyAnswer.
func TestRunPrintMode_SinkErrorOnEmptyAnswer(t *testing.T) {
	fake := writeFakeCodex(t, "exec", "", "", 0)
	s := NewStarter("codex-test", fake, nil)

	rec := &eventRecorder{}
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := runPrintMode(ctx, s, agent.StartConfig{Workspace: ws},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
		agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatalf("expected error from empty answer, got nil")
	}
	if !strings.Contains(err.Error(), "empty answer") {
		t.Errorf("err = %q, want contains 'empty answer'", err)
	}

	evs := rec.snapshot()
	if len(evs) < 2 {
		t.Fatalf("sink observed %d events %v, want >=2 (Ready + Error)",
			len(evs), kinds(evs))
	}
	last := evs[len(evs)-1]
	if last.Kind != agent.EventAgentError {
		t.Fatalf("last event = %s, want Error", last.Kind)
	}
	if last.Diagnostic == nil {
		t.Fatalf("last.Diagnostic is nil")
	}
	if last.Diagnostic.ExitKind == agent.BridgeExitCleanExit {
		t.Errorf("Diagnostic.ExitKind = clean-exit; want anything-but-clean-exit " +
			"so the card title doesn't claim the bridge died")
	}
	if last.Diagnostic.ExitKind != agent.BridgeExitUnknown {
		t.Errorf("Diagnostic.ExitKind = %s, want unknown (empty-answer fallback)",
			last.Diagnostic.ExitKind)
	}
	if last.Diagnostic.AgentName != "codex" {
		t.Errorf("Diagnostic.AgentName = %q, want codex",
			last.Diagnostic.AgentName)
	}
}
