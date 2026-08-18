package shell

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestParseShell_Matrix locks in the 13-row normalization contract
// shared between shell.parseShell and commander.parseCommand. The
// parallel test matrix in commander_test.go (TestParseCommand_Matrix)
// encodes the same cases for the slash side; if you change the
// rules, update both parsers AND both test matrices in lock-step.
func TestParseShell_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantBody string
		wantOK   bool
	}{
		{"empty", "", "", false},
		{"whitespace_only", "   ", "", false},
		{"plain_text", "hello", "", false},
		{"half_bang_cmd", "!cmd", "cmd", true},
		{"full_width_bang_cmd", "！cmd", "cmd", true},
		{"leading_whitespace_bang", "   !cmd", "cmd", true},
		{"bang_followed_by_whitespace", "!   cmd", "cmd", true},
		{"lone_bang", "!", "", false},
		{"bang_only_whitespace", "!   ", "", false},
		{"first_char_is_slash", "/cmd", "", false}, // parseShell only handles !
		{"bang_inside_string", "echo !hi", "", false},
		{"fw_bang_with_trailing_space", "！  hi", "hi", true},
		{"tab_separated", "!cmd\tfix", "cmd\tfix", true}, // trim, not all whitespace-collapse
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBody, gotOK := parseShell(tc.input)
			if gotBody != tc.wantBody {
				t.Errorf("parseShell(%q) body = %q, want %q", tc.input, gotBody, tc.wantBody)
			}
			if gotOK != tc.wantOK {
				t.Errorf("parseShell(%q) ok = %v, want %v", tc.input, gotOK, tc.wantOK)
			}
		})
	}
}

func TestDispatch_NonBangText_FallsThrough(t *testing.T) {
	// Plain text, slash command, full-width slash — none of these
	// are shell dispatches. Consumed must be false so the gateway
	// falls through to message dispatch.
	cases := []string{
		"hello",
		"/cmd",
		"／cmd",
		"   hello",
		"echo !hi", // bang not at start
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			r := dispatch(context.Background(), Request{Text: text, Cwd: t.TempDir()})

			if r.Consumed {
				t.Errorf("dispatch(%q): expected Consumed=false, got true (reply=%q)", text, r.Reply)
			}
			if r.Reply != "" {
				t.Errorf("dispatch(%q): expected empty Reply for fall-through, got %q", text, r.Reply)
			}
		})
	}
}

func TestDispatch_LoneBang_FallsThrough(t *testing.T) {
	// 防呆: ! alone or ! followed only by whitespace should not
	// dispatch. Returns Consumed=false so gateway can fall through.
	for _, text := range []string{"!", "!   ", "！", "！  "} {
		t.Run(text, func(t *testing.T) {
			r := dispatch(context.Background(), Request{Text: text, Cwd: t.TempDir()})

			if r.Consumed {
				t.Errorf("dispatch(%q): lone bang should NOT consume (防呆), got reply=%q", text, r.Reply)
			}
		})
	}
}

func TestDispatch_EmptyCwd_FriendlyError(t *testing.T) {
	r := dispatch(context.Background(), Request{Text: "!ls", Cwd: ""})

	if !r.Consumed {
		t.Fatal("empty CWD should still be consumed (with friendly error)")
	}
	if !strings.Contains(r.Reply, "no CWD") {
		t.Errorf("expected friendly no-CWD message, got %q", r.Reply)
	}
}

func TestDispatch_EchoHello_StdoutAndSummary(t *testing.T) {
	if runtime.GOOS == "windows" {
		// sh is not available on Windows; the cmd /c path is
		// exercised by dispatch_windows_test.go separately.
		t.Skip("echo path uses sh -c; skip on Windows (covered by dispatch_windows_test.go)")
	}
	r := dispatch(context.Background(), Request{Text: "!echo hello", Cwd: t.TempDir()})

	if !r.Consumed {
		t.Fatal("expected Consumed=true")
	}
	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("expected stdout to contain 'hello', got %q", r.Stdout)
	}
	if !strings.Contains(r.Reply, "✅") {
		t.Errorf("expected summary to have ✅, got %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "echo hello") {
		t.Errorf("expected summary to include command, got %q", r.Reply)
	}
}

func TestDispatch_False_ExitCodeOne_AndCrossMark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`false` is a Unix builtin; skip on Windows")
	}
	r := dispatch(context.Background(), Request{Text: "!false", Cwd: t.TempDir()})

	if r.ExitCode != 1 {
		t.Errorf("expected exit 1, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Reply, "❌") {
		t.Errorf("expected summary to have ❌ on non-zero exit, got %q", r.Reply)
	}
	if strings.Contains(r.Reply, "✅") {
		t.Errorf("non-zero exit should NOT have ✅, got %q", r.Reply)
	}
}

func TestDispatch_NotFoundCommand_Exit127(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exit-127 semantic; skip on Windows")
	}
	r := dispatch(context.Background(), Request{Text: "!definitely-not-a-real-command-xyzzy", Cwd: t.TempDir()})

	if r.ExitCode != 127 {
		t.Errorf("expected exit 127 for missing command, got %d", r.ExitCode)
	}
}

func TestDispatch_Pwd_MatchesCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`pwd` is a Unix builtin; skip on Windows")
	}
	dir := t.TempDir()
	r := dispatch(context.Background(), Request{Text: "!pwd", Cwd: dir})

	if r.ExitCode != 0 {
		t.Fatalf("pwd failed: exit %d, stderr=%q", r.ExitCode, r.Stderr)
	}
	// On macOS /var is a symlink to /private/var — pwd may resolve
	// the real path. Compare EvalSymlinks on both sides so the test
	// works on both Linux and macOS.
	if !pathsEquivalent(r.Stdout, dir) {
		t.Errorf("pwd stdout %q does not match CWD %q", strings.TrimSpace(r.Stdout), dir)
	}
}

func TestDispatch_LongOutput_Truncated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`seq` is a Unix utility; skip on Windows")
	}
	dir := t.TempDir()
	// seq 1 200 emits 200 lines, well over MaxStdoutLines=50.
	r := dispatch(context.Background(), Request{Text: "!seq 1 200", Cwd: dir})

	if !strings.Contains(r.Reply, "truncated") {
		t.Errorf("expected summary to mention truncation, got %q", r.Reply)
	}
	// Verify the head IS preserved (first line "1" should be in the card).
	if !strings.Contains(r.Reply, "\n  1\n") {
		t.Errorf("expected first line of truncated output to appear, got %q", r.Reply)
	}
}

func TestDispatch_ContextCancel_SignalExit(t *testing.T) {
	// When the parent context is cancelled mid-execution,
	// CommandContext kills the child. On Unix this is SIGKILL;
	// exec.Run returns *exec.ExitError with ExitCode() == -1
	// (Go's signal-exit sentinel). The summary card should
	// surface that as ❌ with exit -1.
	if runtime.GOOS == "windows" {
		t.Skip("signal-exit semantics differ on Windows; covered by dispatch_windows_test.go")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the child starts → instant SIGKILL

	r := dispatch(ctx, Request{Text: "!sleep 30", Cwd: t.TempDir()})
	if !r.Consumed {
		t.Fatal("expected Consumed=true")
	}
	if r.ExitCode != -1 {
		t.Errorf("expected ExitCode=-1 after ctx cancel, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Reply, "❌") {
		t.Errorf("expected ❌ on signal-exit, got %q", r.Reply)
	}
	if strings.Contains(r.Reply, "✅") {
		t.Errorf("signal-exit must not have ✅, got %q", r.Reply)
	}
}

func TestRenderSummary_SuccessShape(t *testing.T) {
	r := &result{
		Consumed: true,
		Cmd:      "ls -la",
		Cwd:      "/tmp",
		ExitCode: 0,
		Duration: 23 * time.Millisecond,
		Stdout:   "file1\nfile2\n",
	}
	got := renderSummary(r)
	want := []string{"✅", "$ ls -la", "exit 0", "23ms", "/tmp", "stdout:", "  file1", "  file2"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("renderSummary missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "stderr:") {
		t.Errorf("renderSummary should not include stderr section when stderr empty:\n%s", got)
	}
}

func TestRenderSummary_FailureShape(t *testing.T) {
	r := &result{
		Consumed: true,
		Cmd:      "false",
		Cwd:      "/tmp",
		ExitCode: 1,
		Duration: 5 * time.Millisecond,
		Stderr:   "boom\n",
	}
	got := renderSummary(r)
	for _, w := range []string{"❌", "$ false", "exit 1", "5ms", "stderr:", "  boom"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderSummary missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "✅") {
		t.Errorf("non-zero exit must not have ✅:\n%s", got)
	}
}

func TestRenderSummary_TruncationNotice(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxStdoutLines+10; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.Repeat("x", 5))
	}
	r := &result{
		Consumed: true,
		Cmd:      "seq 1 60",
		Cwd:      "/tmp",
		ExitCode: 0,
		Stdout:   b.String(),
	}
	got := renderSummary(r)
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation notice, got:\n%s", got)
	}
	if !strings.Contains(got, "first") {
		t.Errorf("expected 'first N of M lines' framing, got:\n%s", got)
	}
}

// pathsEquivalent compares two filesystem paths, resolving any
// symlinks first (relevant on macOS where t.TempDir() returns a
// /var/... path that pwd resolves to /private/var/...).
func pathsEquivalent(a, b string) bool {
	return evalLinks(a) == evalLinks(b)
}

func evalLinks(p string) string {
	if resolved, err := filepath.EvalSymlinks(strings.TrimSpace(p)); err == nil {
		return resolved
	}
	return strings.TrimSpace(p)
}

// ---------------------------------------------------------------------------
// Dispatcher.Handle + framework ⏳→✅ contract
//
// F-XX (Sender→Emitter refactor) introduced a new Handle signature
// (cs + InboundRequest + *ShellOutput + bool) and wired framework
// ⏳→✅ MessageState emissions. The tests below cover:
//
//   - Handle's fall-through path (non-!cmd)
//   - Handle's consumption path (matched !cmd posts summary card)
//   - Framework ⏳ is emitted BEFORE the goroutine spawns
//   - Framework ✅ is emitted AFTER the goroutine completes
//     (success, failure, panic — all paths covered by runShell's
//     LIFO defer order)
//   - nil / empty guards on cs and MessageID don't crash and
//     don't emit
// ---------------------------------------------------------------------------

// fakeEmitter records each Send call. Implements outbound.Emitter
// via the Send method.
//
// sendErr, when non-nil, is returned from Send — used to verify
// the "reply Send failed → goroutine still completes and emits
// MessageDone via LIFO defer" contract.
type fakeEmitter struct {
	mu       sync.Mutex
	calls    []messages.OutboundMessage
	sendErr  error // optional — inject to test "reply Send failed" path
	gotReply atomic.Bool
}

func (f *fakeEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	f.mu.Lock()
	f.calls = append(f.calls, msg)
	f.mu.Unlock()
	f.gotReply.Store(true)
	return f.sendErr
}

func (f *fakeEmitter) callsCopy() []messages.OutboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]messages.OutboundMessage, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeEmitter) didReceiveReply() bool {
	return f.gotReply.Load()
}

// stateCapture subscribes to a ChatSession's MessageStateBus and
// records every event for assertions on the framework ⏳→✅ contract.
// Mirrors the captureHandler pattern in internal/chatsession/message_state_test.go
// but stays in the shell package to avoid an import cycle on the
// unexported MessageStateEvent type.
type stateCapture struct {
	mu    sync.Mutex
	calls []stateCall
}

type stateCall struct {
	chatID, userMsgID string
	state             agent.MessageState
}

func (c *stateCapture) handler(e chatsession.MessageStateEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, stateCall{chatID: e.ChatID, userMsgID: e.UserMsgID, state: e.State})
	return false
}

func (c *stateCapture) snapshot() []stateCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]stateCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// newWiredCS builds a ChatSession via chatsession.New so its
// MessageStateBus is wired, then subscribes cap. Production
// always uses chatsession.New; bare &chatsession.ChatSession{}
// would skip the framework emit (the nil-bus guard in
// emitShellState short-circuits) — covered separately below.
func newWiredCS(t *testing.T, cap *stateCapture) *chatsession.ChatSession {
	t.Helper()
	cs, err := chatsession.New("oc_test", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.MessageStateBus.Subscribe(cap.handler)
	return cs
}

// awaitReply polls up to 10s for the goroutine to call Send. Returns
// the recorded messages (may be empty if timeout fires).
func awaitReply(t *testing.T, em *fakeEmitter) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if em.didReceiveReply() {
			return em.callsCopy()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return em.callsCopy()
}

// TestDispatcherHandle_NonShellText covers the fall-through path.
// Plain text / slash commands / non-leading bang — none of these
// are shell commands. Handle must return (nil, false) so the
// gateway falls through to tryMessageDispatch, AND must NOT spawn
// a goroutine (no MessageStateBus emission either).
func TestDispatcherHandle_NonShellText(t *testing.T) {
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	for _, text := range []string{
		"hello",
		"/cmd",
		"／cmd",
		"   hello",
		"echo !hi", // bang not at start
	} {
		t.Run(text, func(t *testing.T) {
			cap.calls = nil
			em.calls = nil
			em.gotReply.Store(false)

			out, handled := d.Handle(testMgr(t), cs, InboundRequest{
				Request:   Request{Text: text, Cwd: t.TempDir()},
				ChatID:    "oc_test",
				MessageID: "om_test",
			})
			if handled {
				t.Errorf("Handle(%q): expected handled=false, got true (out=%+v)", text, out)
			}
			if out != nil {
				t.Errorf("Handle(%q): expected nil output, got %+v", text, out)
			}
			if got := cap.snapshot(); len(got) != 0 {
				t.Errorf("Handle(%q): non-shell text should not emit MessageState, got %+v", text, got)
			}
			if got := em.callsCopy(); len(got) != 0 {
				t.Errorf("Handle(%q): non-shell text should not call Emitter, got %d calls", text, len(got))
			}
		})
	}
}

// TestDispatcherHandle_ShellCommand covers the consumption path.
// For a `!cmd` text, Handle returns (&ShellOutput{Consumed: true},
// true) and (eventually) calls Emitter with the rendered summary
// card as a messages.OutboundMessage{Kind: OutCommandReply}.
func TestDispatcherHandle_ShellCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	out, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!echo hello-shell", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_test",
	})
	if !handled || out == nil || !out.Consumed {
		t.Fatalf("expected (ShellOutput{Consumed:true}, true), got handled=%v out=%+v", handled, out)
	}

	calls := awaitReply(t, em)
	if len(calls) == 0 {
		t.Fatal("expected Emitter to be called with the result card")
	}
	c := calls[0]
	if c.Kind != messages.OutCommandReply {
		t.Errorf("Kind = %v, want OutCommandReply", c.Kind)
	}
	if c.ChatID != "oc_test" {
		t.Errorf("ChatID = %q, want oc_test", c.ChatID)
	}
	if c.ReplyTo != "om_test" {
		t.Errorf("ReplyTo = %q, want om_test", c.ReplyTo)
	}
	if !strings.Contains(c.Text, "✅") || !strings.Contains(c.Text, "echo hello-shell") {
		t.Errorf("expected summary card with ✅ and command, got %q", c.Text)
	}
}

// TestDispatcherHandle_NilEmitter verifies that NewDispatcher()
// doesn't panic — the dispatcher still consumes shell commands
// (returns Consumed=true) but silently drops the reply. Lets the
// runtime stay wired during channel outages.
func TestDispatcherHandle_NilEmitter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	d := NewDispatcher() // emitter = nil

	out, handled := d.Handle(nil, &chatsession.ChatSession{}, InboundRequest{
		Request: Request{Text: "!echo ok", Cwd: t.TempDir()},
	})
	if !handled || out == nil || !out.Consumed {
		t.Fatalf("expected consumed=true even with nil emitter, got handled=%v out=%+v", handled, out)
	}
	// The goroutine's `d.emitter == nil` short-circuit returns
	// before any Send call. No panic occurs, no reply is
	// dispatched. The point of this test is to confirm
	// NewDispatcher() doesn't panic and Handle still returns
	// the consumed=true contract.
}

// ---------------------------------------------------------------------------
// Framework ⏳→✅ reactions
// ---------------------------------------------------------------------------

// TestDispatcherHandle_EmitsQueuedThenDone verifies the framework
// ⏳→✅ sequence. MessageQueued fires synchronously BEFORE the
// goroutine spawns; MessageDone fires inside the goroutine's
// LIFO defer (after the reply Send). This is the same contract
// as commander.Dispatch — see docs/feat/slash-command-reactions.md.
func TestDispatcherHandle_EmitsQueuedThenDone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	out, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!echo queued-done", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_qd",
	})
	if !handled || out == nil {
		t.Fatalf("expected consumed, got handled=%v out=%+v", handled, out)
	}

	// Wait for the goroutine to finish (Send → defer → Done).
	calls := awaitReply(t, em)
	if len(calls) == 0 {
		t.Fatal("expected Emitter to be called")
	}

	states := cap.snapshot()
	if len(states) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued + Done)", len(states))
	}
	if states[0].state != agent.MessageQueued {
		t.Errorf("states[0] = %v, want MessageQueued", states[0].state)
	}
	if states[1].state != agent.MessageDone {
		t.Errorf("states[1] = %v, want MessageDone", states[1].state)
	}
	if states[0].userMsgID != "om_qd" || states[1].userMsgID != "om_qd" {
		t.Errorf("MessageID mismatch: states=%+v", states)
	}
}

// TestDispatcherHandle_FallThroughEmitsNothing guards the
// framework contract: non-!cmd text must NOT trigger any
// MessageState emission. Otherwise `/etc/passwd`-style inputs
// would flash ⏳/✅ on the user message for no reason.
func TestDispatcherHandle_FallThroughEmitsNothing(t *testing.T) {
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	d := NewDispatcher()

	for _, text := range []string{"hello", "/etc/passwd", "!   ", "echo !hi"} {
		cap.calls = nil
		_, handled := d.Handle(testMgr(t), cs, InboundRequest{
			Request:   Request{Text: text, Cwd: t.TempDir()},
			ChatID:    "oc_test",
			MessageID: "om_x",
		})
		if handled {
			t.Errorf("Handle(%q): expected handled=false on fall-through", text)
		}
		if got := cap.snapshot(); len(got) != 0 {
			t.Errorf("Handle(%q): expected zero MessageState events, got %+v", text, got)
		}
	}
}

// TestDispatcherHandle_NilMessageIDSkipsEmit covers the
// empty-MessageID guard. Matches commander.Dispatch's contract —
// runtime subscriber at cmd/nightme/run.go already drops empty
// UserMsgID silently, but the framework-level guard keeps the
// contract local and test-friendly.
func TestDispatcherHandle_NilMessageIDSkipsEmit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	_, handled := d.Handle(testMgr(t), cs, InboundRequest{
		Request: Request{Text: "!echo empty-id", Cwd: t.TempDir()},
		ChatID:  "oc_test",
		// MessageID: "" — explicitly empty
	})
	if !handled {
		t.Fatal("expected handled=true even with empty MessageID")
	}
	// Wait for the goroutine, then assert no emits happened.
	_ = awaitReply(t, em)
	if got := cap.snapshot(); len(got) != 0 {
		t.Errorf("empty MessageID should suppress both Queued + Done, got %+v", got)
	}
}

// TestDispatcherHandle_NilCSSkipsEmit covers the nil-cs guard.
// Match commander.Dispatch: nil cs must not crash, must not emit.
func TestDispatcherHandle_NilCSSkipsEmit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	em := &fakeEmitter{}
	d := NewDispatcher()

	out, handled := d.Handle(nil, nil, InboundRequest{
		Request:   Request{Text: "!echo nil-cs", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_nil",
	})
	if !handled || out == nil {
		t.Fatalf("expected consumed=true even with nil cs, got handled=%v out=%+v", handled, out)
	}
	// Wait for goroutine completion so any deferred MessageDone
	// would have fired if it had a cs to call into. The
	// emitShellState nil-guard means this is a no-op.
	_ = awaitReply(t, em)
}

// TestDispatcherHandle_ZeroValueCSDoesNotPanic covers the bare
// &chatsession.ChatSession{} (zero-value, MessageStateBus == nil)
// case — emitShellState's nil-bus guard short-circuits both
// emits, so the goroutine completes cleanly without panic. This
// mirrors commander.Dispatch's zero-value-CS test; both packages
// share the same framework contract.
func TestDispatcherHandle_ZeroValueCSDoesNotPanic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	em := &fakeEmitter{}
	d := NewDispatcher()

	out, handled := d.Handle(testMgrWithEmitter(t, em), &chatsession.ChatSession{}, InboundRequest{
		Request:   Request{Text: "!echo zero-cs", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_zv",
	})
	if !handled || out == nil {
		t.Fatalf("expected consumed with zero-value cs, got handled=%v out=%+v", handled, out)
	}
	// Wait for goroutine — if the nil-bus guard is broken the
	// emit call would panic and the goroutine would die before
	// calling Send. The assertion below catches that.
	calls := awaitReply(t, em)
	if len(calls) == 0 {
		t.Fatal("expected Emitter to be called even with zero-value cs (nil-bus emit is a no-op)")
	}
}

// TestDispatcherHandle_ReplySendFailed verifies that when the
// wired emitter's Send returns an error, the goroutine still
// completes and the framework still emits MessageDone via the
// LIFO defer. This is the "reply Send failed → user still sees
// ✅" contract — without it the user has no idea their command
// "ran successfully" inside the daemon but the reply vanished.
// Mirrors the post-recovery defer order in shell.runShell.
func TestDispatcherHandle_ReplySendFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{sendErr: errors.New("emitter down")}
	d := NewDispatcher()

	// v1.3+ multi-channel: mgr carries the per-channel Emitter.
	// Build a real chatsession.Manager, wire em, and pass it.
	mgr := chatsession.NewManager().WithEmitter(em)

	_, handled := d.Handle(mgr, cs, InboundRequest{
		Request:   Request{Text: "!echo send-fails", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_sendfail",
	})
	if !handled {
		t.Fatal("expected handled=true")
	}
	// Wait for the goroutine to attempt Send (it will fail).
	calls := awaitReply(t, em)
	if len(calls) != 1 {
		t.Fatalf("expected exactly one Send call, got %d", len(calls))
	}
	if calls[0].Kind != messages.OutCommandReply {
		t.Errorf("Send Kind = %v, want OutCommandReply", calls[0].Kind)
	}

	// The framework must still emit MessageDone even though
	// Send failed — that's the LIFO defer contract.
	states := cap.snapshot()
	if len(states) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued + Done despite Send failure)", len(states))
	}
	if states[1].state != agent.MessageDone {
		t.Errorf("states[1] = %v, want MessageDone (Send-fail must not skip Done)", states[1].state)
	}
}

// panickingEmitter panics inside Send so runShell's recover
// defer fires. Used by TestDispatcherHandle_PanicStillEmitsDone
// to lock the LIFO defer ordering.
type panickingEmitter struct{}

func (panickingEmitter) Send(_ context.Context, _ messages.OutboundMessage) error {
	panic("panickingEmitter: simulating dispatcher-side panic")
}

// TestDispatcherHandle_PanicStillEmitsDone verifies that even
// when the async goroutine panics, the framework ⏳→✅ contract
// is preserved. The defer ordering in runShell is:
//
//	defer emitShellState(MessageDone)   // outer (LIFO last)
//	defer func() { recover() ... }()     // inner (LIFO first)
//
// LIFO means a panic in dispatch() or emitter.Send is recovered
// first (logged, swallowed), then MessageDone fires. A future
// refactor that drops either defer would silently break this
// contract — this test is the regression guard.
func TestDispatcherHandle_PanicStillEmitsDone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	d := NewDispatcher()

	_, handled := d.Handle(testMgr(t), cs, InboundRequest{
		Request:   Request{Text: "!echo panic-me", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_panic",
	})
	if !handled {
		t.Fatal("expected handled=true")
	}

	// Wait long enough for the goroutine to finish — it will
	// panic inside emitter.Send, recover, then emit MessageDone.
	// The recover-defer swallows the panic so the goroutine
	// exits cleanly; the outer Done-defer fires last.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(cap.snapshot()) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	states := cap.snapshot()
	if len(states) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued sync + Done from LIFO defer)", len(states))
	}
	if states[0].state != agent.MessageQueued {
		t.Errorf("states[0] = %v, want MessageQueued", states[0].state)
	}
	if states[1].state != agent.MessageDone {
		t.Errorf("states[1] = %v, want MessageDone (panic must not skip Done)", states[1].state)
	}
}

// TestDispatcherHandle_ShellCommandFails covers the non-zero-exit
// path. !false exits 1, the summary card flips to ❌, and the
// reply still routes through emitter.Send with OutCommandReply
// kind. Verifies the framework ⏳→✅ contract holds on the error
// path (Done must fire even when the command fails).
func TestDispatcherHandle_ShellCommandFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`false` is a Unix builtin; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	_, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!false", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_fail",
	})
	if !handled {
		t.Fatal("expected handled=true")
	}

	calls := awaitReply(t, em)
	if len(calls) == 0 {
		t.Fatal("expected Emitter.Send to be called even on command failure")
	}
	c := calls[0]
	if c.Kind != messages.OutCommandReply {
		t.Errorf("Kind = %v, want OutCommandReply", c.Kind)
	}
	if c.ReplyTo != "om_fail" {
		t.Errorf("ReplyTo = %q, want om_fail", c.ReplyTo)
	}
	if !strings.Contains(c.Text, "❌") {
		t.Errorf("failure reply should contain ❌, got %q", c.Text)
	}
	if strings.Contains(c.Text, "✅") {
		t.Errorf("failure reply must NOT contain ✅, got %q", c.Text)
	}

	// Framework ⏳→✅ on the failure path — Done must fire even
	// when the command exits non-zero, otherwise the user sees
	// ⏳ "stuck" until the next inbound.
	states := cap.snapshot()
	if len(states) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued + Done on failure)", len(states))
	}
	if states[1].state != agent.MessageDone {
		t.Errorf("states[1] = %v, want MessageDone (failure path must still emit Done)", states[1].state)
	}
}

// testMgr returns a real *chatsession.Manager for shell.Handle.
// v1.3+ multi-channel: Handle takes a per-channel Manager that
// carries the per-channel Emitter used for outbound. The shell
// tests need a Manager; most don't wire an Emitter because the
// test only checks Consumed/Reply (the synchronous return),
// not the async reply post.
func testMgr(t *testing.T) *chatsession.Manager {
	t.Helper()
	return chatsession.NewManager()
}

// testMgrWithEmitter returns a real *chatsession.Manager with
// the given Emitter wired. v1.3+ multi-channel: shell.Handle
// uses mgr.Emitter() for outbound, so the test Manager must
// carry the Emitter for the reply to be sent.
func testMgrWithEmitter(t *testing.T, em outbound.Emitter) *chatsession.Manager {
	t.Helper()
	return chatsession.NewManager().WithEmitter(em)
}
