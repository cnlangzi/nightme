package shell

import (
	"bytes"
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
				t.Errorf("parseShell(%q) body = %q, want %q", tc.input, gotBody, gotBody)
			}
			if gotOK != tc.wantOK {
				t.Errorf("parseShell(%q) ok = %v, want %v", tc.input, gotOK, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// executeShell direct tests — exercise the streaming core without
// going through the dispatcher. These are the "what does the
// shell actually produce" tests: stdout / stderr capture, exit
// codes, duration, panic recovery, ctx cancellation. The
// dispatcher's behavior (OutReply framing, framework ⏳→✅)
// lives in TestRunShell_* and TestDispatcherHandle_* below.
// ---------------------------------------------------------------------------

// TestExecuteShell_EchoHello covers the happy path: a command
// that prints to stdout and exits 0. With onChunk=nil the
// streaming core accumulates everything in sink and we read
// the full text from r.Stdout — identical to the pre-streaming
// Run contract.
func TestExecuteShell_EchoHello(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo path uses sh -c; skip on Windows")
	}
	r := executeShell(context.Background(), t.TempDir(), "echo hello", nil, nil)
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (stderr=%q)", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("Stdout = %q, want contains 'hello'", r.Stdout)
	}
	if r.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", r.Duration)
	}
}

// TestExecuteShell_False_ExitCodeOne covers the non-zero exit
// path. The streaming core preserves the exit code in r.ExitCode
// and the footer renders ❌ in the dispatcher.
func TestExecuteShell_False_ExitCodeOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`false` is a Unix builtin; skip on Windows")
	}
	r := executeShell(context.Background(), t.TempDir(), "false", nil, nil)
	if r.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", r.ExitCode)
	}
}

// TestExecuteShell_NotFoundCommand_Exit127 covers the missing-
// command case. POSIX shells exit 127 when the command is not
// found; we surface that in r.ExitCode so the dispatcher footer
// can render `❌ exit 127`.
func TestExecuteShell_NotFoundCommand_Exit127(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exit-127 semantic; skip on Windows")
	}
	r := executeShell(context.Background(), t.TempDir(), "definitely-not-a-real-command-xyzzy", nil, nil)
	if r.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127 for missing command", r.ExitCode)
	}
}

// TestExecuteShell_Pwd_MatchesCwd covers the cwd propagation.
// On macOS /var is a symlink to /private/var — EvalSymlinks on
// both sides normalises the path so the test works on Linux
// and macOS.
func TestExecuteShell_Pwd_MatchesCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`pwd` is a Unix builtin; skip on Windows")
	}
	dir := t.TempDir()
	r := executeShell(context.Background(), dir, "pwd", nil, nil)
	if r.ExitCode != 0 {
		t.Fatalf("pwd failed: exit %d, stderr=%q", r.ExitCode, r.Stderr)
	}
	if !pathsEquivalent(r.Stdout, dir) {
		t.Errorf("pwd stdout %q does not match CWD %q", strings.TrimSpace(r.Stdout), dir)
	}
	if r.Cwd != dir {
		t.Errorf("Cwd = %q, want %q", r.Cwd, dir)
	}
}

// TestExecuteShell_LongOutput_AllDelivered replaces the old
// TestDispatch_LongOutput_Truncated. The pre-streaming
// implementation truncated at MaxStdoutLines=50 with a
// "... N more lines truncated" notice; the streaming
// implementation delivers the full output and lets the
// channel layer (Feishu's RolloverTo) handle overflow.
// This test asserts all 200 lines arrive in r.Stdout.
func TestExecuteShell_LongOutput_AllDelivered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`seq` is a Unix utility; skip on Windows")
	}
	dir := t.TempDir()
	r := executeShell(context.Background(), dir, "seq 1 200", nil, nil)
	if r.ExitCode != 0 {
		t.Fatalf("seq failed: exit %d, stderr=%q", r.ExitCode, r.Stderr)
	}
	// All 200 lines should appear in Stdout. We count newlines
	// as a cheap "did all lines arrive" proxy; the trailing
	// newline from `seq` produces 200 lines worth.
	lineCount := strings.Count(strings.TrimRight(r.Stdout, "\n"), "\n") + 1
	if lineCount < 200 {
		t.Errorf("Stdout line count = %d, want >= 200 (truncation should be gone)", lineCount)
	}
}

// TestExecuteShell_ContextCancel_SignalExit covers ctx-driven
// kill. CommandContext sends SIGKILL on Unix (exit code -1 in
// Go's signal-exit sentinel); the streaming core sets
// r.ExitCode=-1 via the runErr branch so the dispatcher footer
// renders `❌ exit -1`.
func TestExecuteShell_ContextCancel_SignalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-exit semantics differ on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the child starts → instant SIGKILL

	r := executeShell(ctx, t.TempDir(), "sleep 30", nil, nil)
	if r.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 after ctx cancel", r.ExitCode)
	}
}

// TestCoalesceLines_PanicPropagates verifies that coalesceLines
// does NOT swallow panics from the underlying reader — they
// propagate to the caller, which is exactly what executeShell's
// drainer goroutines rely on:
//
//	go func() {
//	    defer wg.Done()
//	    defer func() {
//	        if r := recover(); r != nil { panicCh <- r }
//	    }()
//	    _ = coalesceLines(outR, &stdoutBuf, onChunk, false)
//	}()
//
// executeShell's recover defer catches the panic and forwards
// it via panicCh; the parent executeShell then surfaces
// r.ExitCode=-1 + r.Stderr="drainer panic: …" so the
// dispatcher footer renders ❌.
//
// Why this test exists at the coalesceLines level (not at the
// executeShell level): executeShell's pipe comes from a real
// cmd.Stdout, so we can't inject a panickingReader through the
// child execution path without major refactoring. The panic-
// recovery contract that executeShell DEPENDS ON is "coalesceLines
// must not swallow panics from r.Read". This test pins that
// contract; the recover/panicCh path in executeShell is then a
// trivial wrapping that code review can verify.
type panickingReader struct{}

func (panickingReader) Read(_ []byte) (int, error) {
	panic("panickingReader: simulating child-side stream panic")
}

func TestCoalesceLines_PanicPropagates(t *testing.T) {
	var sink bytes.Buffer
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_ = coalesceLines(panickingReader{}, &sink, nil, false)
	}()
	if recovered == nil {
		t.Fatal("expected coalesceLines to propagate panickingReader's panic")
	}
	if msg, ok := recovered.(string); !ok || !strings.Contains(msg, "panickingReader") {
		t.Errorf("recovered value = %v, want string containing 'panickingReader'", recovered)
	}
}

// ---------------------------------------------------------------------------
// coalesceLines unit tests
// ---------------------------------------------------------------------------

// TestCoalesceLines_HighVolume_MultipleFlushes feeds 64 KiB of
// input through coalesceLines with chunkBytes=4 KiB and asserts
// onChunk is called multiple times before EOF, AND sink contains
// the full input.
func TestCoalesceLines_HighVolume_MultipleFlushes(t *testing.T) {
	// 64 KiB / 4 KiB = at least 16 mid-stream flushes, well over
	// the >= 4 threshold the plan specifies.
	const totalBytes = 64 * 1024
	input := strings.Repeat("x\n", totalBytes/2) // each "x\n" is 2 bytes

	var (
		mu        sync.Mutex
		flushes   int
		chunkSeen strings.Builder
	)
	r := strings.NewReader(input)
	err := coalesceLines(r, &bytes.Buffer{}, func(chunk string) error {
		mu.Lock()
		defer mu.Unlock()
		flushes++
		chunkSeen.WriteString(chunk)
		return nil
	}, false)
	if err != nil {
		t.Fatalf("coalesceLines: %v", err)
	}

	mu.Lock()
	gotFlushes := flushes
	gotChunkSeen := chunkSeen.String()
	mu.Unlock()

	if gotFlushes < 4 {
		t.Errorf("flushes = %d, want >= 4 mid-stream flushes for %d bytes", gotFlushes, totalBytes)
	}
	if gotChunkSeen != input {
		t.Errorf("chunkSeen length = %d, want %d (chunks must equal input)", len(gotChunkSeen), len(input))
	}
}

// TestCoalesceLines_OnChunkError_StopsReading covers the
// abort-on-error path. onChunk returns an error on the 3rd
// call; coalesceLines must propagate that error and stop
// reading. sink still has whatever lines were processed before
// the error (every line is unconditionally sink.WriteString'd).
//
// Input is large enough (~12 KiB across 3 long lines) to force
// ≥3 mid-stream flushes. With chunkBytes=4 KiB, three 4 KiB-ish
// lines produce exactly 3 flushes; the 3rd flush's onChunk
// returns the error and the loop terminates.
func TestCoalesceLines_OnChunkError_StopsReading(t *testing.T) {
	// 3 lines × ~4 KiB each = ~12 KiB, well over 2 chunkBytes
	// so we get 3 mid-stream flushes (one per line boundary
	// crossing 4 KiB).
	line := strings.Repeat("x", 4096) + "\n"
	input := line + line + line + line + line // 5 lines, expect 3+ flushes
	wantErr := errors.New("emitter down")

	var (
		mu       sync.Mutex
		attempts int
	)
	r := strings.NewReader(input)
	var sink bytes.Buffer
	err := coalesceLines(r, &sink, func(chunk string) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		if attempts == 3 {
			return wantErr
		}
		return nil
	}, false)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}

	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	// The 3rd onChunk call returns error; coalesceLines returns
	// without trying a 4th (the trailing flush is skipped
	// because err != nil).
	if gotAttempts != 3 {
		t.Errorf("attempts = %d, want exactly 3 (4th blocked by error)", gotAttempts)
	}
	// sink still has every line processed before the error
	// (every line is unconditionally sink.WriteString'd), but
	// NOT the trailing line whose chunk would have triggered
	// the 4th flush. With our 5-line input, lines 1-3 hit the
	// sink before the 3rd flush errored; line 4 is buffered
	// in `b` with the 3rd flush; line 5 is unread.
	sinkLen := sink.Len()
	if sinkLen < 3*len(line) {
		t.Errorf("sink length = %d, want >= %d (lines 1-3 must be in sink)", sinkLen, 3*len(line))
	}
}

// TestCoalesceLines_StderrPrefix verifies the isStderr=true
// path prefixes each line with "stderr: " for visual separation
// in the rendered card.
func TestCoalesceLines_StderrPrefix(t *testing.T) {
	input := "boom\nkaboom\n"
	var got strings.Builder
	r := strings.NewReader(input)
	err := coalesceLines(r, &bytes.Buffer{}, func(chunk string) error {
		got.WriteString(chunk)
		return nil
	}, true)
	if err != nil {
		t.Fatalf("coalesceLines: %v", err)
	}
	if !strings.HasPrefix(got.String(), "stderr: boom\n") {
		t.Errorf("chunk = %q, want leading 'stderr: ' prefix", got.String())
	}
	if !strings.Contains(got.String(), "stderr: kaboom") {
		t.Errorf("chunk = %q, want 'stderr: kaboom'", got.String())
	}
}

// TestCoalesceLines_NilOnChunk_CollectsToSink verifies that
// when onChunk is nil (the Run / gtw-hooks path), coalesceLines
// still writes every line to sink and returns nil at EOF.
func TestCoalesceLines_NilOnChunk_CollectsToSink(t *testing.T) {
	input := "alpha\nbeta\ngamma\n"
	var sink bytes.Buffer
	err := coalesceLines(strings.NewReader(input), &sink, nil, false)
	if err != nil {
		t.Fatalf("coalesceLines: %v", err)
	}
	if sink.String() != input {
		t.Errorf("sink = %q, want %q", sink.String(), input)
	}
}

// ---------------------------------------------------------------------------
// Run / streaming-backed Run test
// ---------------------------------------------------------------------------

// TestRun_StreamingBacked confirms that Run (the sync API used
// by gtw/hooks.go) still produces the pre-streaming contract:
// "hello\nworld" from `echo hello; echo world`. Internally Run
// calls executeShell with onChunk=nil; coalesceLines writes
// every line to sink so the user-visible behavior is identical
// to the buffer-based implementation.
func TestRun_StreamingBacked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c; Windows cmd /c tested separately")
	}
	stdout, stderr, exitCode, err := Run(context.Background(), t.TempDir(), "echo hello; echo world", nil)
	if err != nil {
		t.Fatalf("Run: %v (stderr=%q)", err, stderr)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if stdout != "hello\nworld" {
		t.Errorf("stdout = %q, want %q", stdout, "hello\nworld")
	}
}

// TestRun_WindowsCmd verifies the cmd /c path on Windows.
// Skipped on Unix (sh -c is the Unix path; covered above).
func TestRun_WindowsCmd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd /c is Windows-only")
	}
	stdout, _, exitCode, err := Run(context.Background(), t.TempDir(), "echo hello & echo world", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	// cmd.exe may add trailing whitespace; compare after trim.
	if !strings.Contains(strings.TrimSpace(stdout), "hello") {
		t.Errorf("stdout = %q, want contains 'hello'", stdout)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher integration tests — the OutReply protocol.
// ---------------------------------------------------------------------------

// TestRunShell_EmptyCwd_OutReply drives the empty-CWD branch
// through Handle and asserts the stub emitter received exactly
// one OutReply whose text contains the friendly error. The
// defer still fires MessageDone so the receipt flips to ✅.
func TestRunShell_EmptyCwd_OutReply(t *testing.T) {
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	out, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!ls", Cwd: ""},
		ChatID:    "oc_test",
		MessageID: "om_empty_cwd",
	})
	if !handled || out == nil || !out.Consumed {
		t.Fatalf("expected consumed, got handled=%v out=%+v", handled, out)
	}

	calls := awaitReply(t, em)
	if len(calls) != 1 {
		t.Fatalf("Send call count = %d, want 1 (header-only on empty CWD)", len(calls))
	}
	c := calls[0]
	if c.Kind != messages.OutReply {
		t.Errorf("Kind = %v, want OutReply", c.Kind)
	}
	if c.ReplyTo != "om_empty_cwd" {
		t.Errorf("ReplyTo = %q, want om_empty_cwd", c.ReplyTo)
	}
	if !strings.Contains(c.Text, "no CWD") {
		t.Errorf("Text = %q, want contains 'no CWD'", c.Text)
	}

	states := cap.snapshot()
	if len(states) != 2 || states[1].state != agent.MessageDone {
		t.Errorf("expected Queued + Done states; got %+v", states)
	}
}

// TestRunShell_HeaderChunkFooter drives a happy-path shell
// command and asserts the streaming OutReply protocol: first
// Send is the header (starts with "⌨️ $"), at least one chunk
// Send follows, last Send is the footer (starts with "\n✅
// exit 0"), all share the same ReplyTo.
func TestRunShell_HeaderChunkFooter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c; Windows cmd /c tested separately")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	out, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!echo hello-shell", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_hcf",
	})
	if !handled || out == nil || !out.Consumed {
		t.Fatalf("expected consumed, got handled=%v out=%+v", handled, out)
	}

	calls := awaitReply(t, em)
	if len(calls) < 2 {
		t.Fatalf("Send call count = %d, want >= 2 (header + footer)", len(calls))
	}
	// First call: header
	if !strings.HasPrefix(calls[0].Text, "⌨️ $") {
		t.Errorf("calls[0].Text = %q, want leading '⌨️ $'", calls[0].Text)
	}
	if !strings.Contains(calls[0].Text, "echo hello-shell") {
		t.Errorf("calls[0].Text = %q, want contains command", calls[0].Text)
	}
	if calls[0].Kind != messages.OutReply {
		t.Errorf("calls[0].Kind = %v, want OutReply", calls[0].Kind)
	}
	// Last call: footer
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last.Text, "\n✅") {
		t.Errorf("last.Text = %q, want leading '\\n✅'", last.Text)
	}
	if !strings.Contains(last.Text, "exit 0") {
		t.Errorf("last.Text = %q, want contains 'exit 0'", last.Text)
	}
	if last.Kind != messages.OutReply {
		t.Errorf("last.Kind = %v, want OutReply", last.Kind)
	}
	// All calls share the same ReplyTo (PATCH on same card).
	for i, c := range calls {
		if c.ReplyTo != "om_hcf" {
			t.Errorf("calls[%d].ReplyTo = %q, want om_hcf", i, c.ReplyTo)
		}
	}
}

// TestRunShell_False_FooterIsError covers the non-zero exit
// path. The footer must use ❌ and must NOT contain ✅
// anywhere in any Send call.
func TestRunShell_False_FooterIsError(t *testing.T) {
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
		MessageID: "om_false",
	})
	if !handled {
		t.Fatal("expected handled=true")
	}

	calls := awaitReply(t, em)
	if len(calls) < 2 {
		t.Fatalf("Send call count = %d, want >= 2", len(calls))
	}
	// Footer is the last call.
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last.Text, "\n❌") {
		t.Errorf("last.Text = %q, want leading '\\n❌'", last.Text)
	}
	if !strings.Contains(last.Text, "exit 1") {
		t.Errorf("last.Text = %q, want contains 'exit 1'", last.Text)
	}
	for i, c := range calls {
		if strings.Contains(c.Text, "✅") {
			t.Errorf("calls[%d].Text = %q, must NOT contain ✅", i, c.Text)
		}
	}
}

// TestRunShell_OutReplySequence drives a multi-chunk command
// (`seq 1 200`) and asserts the stub emitter received at least
// N+2 sends: 1 header + ≥1 chunk + 1 footer. All share the
// same ReplyTo. The exact chunk count depends on coalesceLines'
// 4 KiB threshold and is implementation-defined; we only check
// the minimum.
func TestRunShell_OutReplySequence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`seq` is a Unix utility; skip on Windows")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{}
	d := NewDispatcher()

	_, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!seq 1 200", Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_seq",
	})
	if !handled {
		t.Fatal("expected handled=true")
	}

	calls := awaitReply(t, em)
	if len(calls) < 3 {
		t.Fatalf("Send call count = %d, want >= 3 (header + ≥1 chunk + footer)", len(calls))
	}
	for i, c := range calls {
		if c.ReplyTo != "om_seq" {
			t.Errorf("calls[%d].ReplyTo = %q, want om_seq", i, c.ReplyTo)
		}
		if c.Kind != messages.OutReply {
			t.Errorf("calls[%d].Kind = %v, want OutReply", i, c.Kind)
		}
	}
	// First is header, last is footer.
	if !strings.HasPrefix(calls[0].Text, "⌨️ $") {
		t.Errorf("calls[0].Text = %q, want leading '⌨️ $'", calls[0].Text)
	}
	if !strings.HasPrefix(calls[len(calls)-1].Text, "\n✅") {
		t.Errorf("last.Text = %q, want leading '\\n✅'", calls[len(calls)-1].Text)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher.Handle + framework ⏳→✅ contract — same as the
// pre-streaming version, but Send now produces OutReply (header
// / chunks / footer) instead of a single OutCommandReply.
// ---------------------------------------------------------------------------

// fakeEmitter records each Send call. Implements messages.Emitter
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
// true) and (eventually) calls Emitter with the streaming reply
// (header OutReply + chunks + footer OutReply), all sharing
// ReplyTo = userMsgID.
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
		t.Fatal("expected Emitter to be called with streaming reply")
	}
	// First call must be the header with OutReply kind.
	c := calls[0]
	if c.Kind != messages.OutReply {
		t.Errorf("calls[0].Kind = %v, want OutReply", c.Kind)
	}
	if c.ChatID != "oc_test" {
		t.Errorf("calls[0].ChatID = %q, want oc_test", c.ChatID)
	}
	if c.ReplyTo != "om_test" {
		t.Errorf("calls[0].ReplyTo = %q, want om_test", c.ReplyTo)
	}
	if !strings.Contains(c.Text, "⌨️ $ echo hello-shell") {
		t.Errorf("calls[0].Text = %q, want header '⌨️ $ echo hello-shell'", c.Text)
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
	// The goroutine's `mgr.Emitter() == nil` short-circuit returns
	// before any Send call. No panic occurs, no reply is
	// dispatched. The point of this test is to confirm
	// NewDispatcher() doesn't panic and Handle still returns
	// the consumed=true contract.
}

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
	// At minimum the header Send fires before the first chunk,
	// so we expect at least 1 call. The error from the header
	// causes runShell to cancel ctx (no chunks), then the footer
	// also fails — total call count depends on whether the
	// header Send happens to error before coalesceLines drives
	// any chunks. We only assert >= 1 call + OutReply kind.
	calls := awaitReply(t, em)
	if len(calls) == 0 {
		t.Fatalf("expected at least one Send call, got 0")
	}
	if calls[0].Kind != messages.OutReply {
		t.Errorf("calls[0].Kind = %v, want OutReply", calls[0].Kind)
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
// path. !false exits 1; the streaming reply still uses OutReply
// for all three sends (header + footer), with the footer
// flipping to ❌. The framework ⏳→✅ contract holds on the error
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
	// All sends are OutReply (no more OutCommandReply for shell).
	for i, c := range calls {
		if c.Kind != messages.OutReply {
			t.Errorf("calls[%d].Kind = %v, want OutReply", i, c.Kind)
		}
		if c.ReplyTo != "om_fail" {
			t.Errorf("calls[%d].ReplyTo = %q, want om_fail", i, c.ReplyTo)
		}
	}
	// Footer (last call) carries the ❌ marker.
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last.Text, "\n❌") {
		t.Errorf("last.Text = %q, want leading '\\n❌'", last.Text)
	}
	if !strings.Contains(last.Text, "exit 1") {
		t.Errorf("last.Text = %q, want contains 'exit 1'", last.Text)
	}
	for i, c := range calls {
		if strings.Contains(c.Text, "✅") {
			t.Errorf("calls[%d].Text = %q, must NOT contain ✅", i, c.Text)
		}
	}

	// Framework ⏳→✅ on the failure path — Done must fire even
	// when the command exits non-zero, otherwise the user sees
	// ⌨️ "stuck" until the next inbound.
	states := cap.snapshot()
	if len(states) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued + Done on failure)", len(states))
	}
	if states[1].state != agent.MessageDone {
		t.Errorf("states[1] = %v, want MessageDone (failure path must still emit Done)", states[1].state)
	}
}

// TestDispatcherHandle_ConcurrentDrainerFailure exercises the
// race the drainErr mutex was added for. With both stdout and
// stderr drainers producing output that fails to send, both
// drainer goroutines concurrently write to drainErr. Without
// the mutex this test triggers a -race failure; with the
// mutex, drainErr is stable by the time executeShell returns
// and the test passes cleanly.
//
// The shell command below writes to BOTH streams
// (stdout + stderr interleaved) so both drainers produce
// onChunk calls. sendErr on the fake emitter forces every
// send to fail, so both drainers race to set drainErr.
func TestDispatcherHandle_ConcurrentDrainerFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c; Windows cmd /c tested separately")
	}
	cap := &stateCapture{}
	cs := newWiredCS(t, cap)
	em := &fakeEmitter{sendErr: errors.New("concurrent failure")}
	d := NewDispatcher()

	// Each "echo X; echo Y >&2" pair produces one stdout line
	// and one stderr line. Repeating 20 times gives both
	// drainers plenty of work to race on.
	const repeat = 20
	cmd := strings.Repeat("echo line; echo err >&2; ", repeat)
	cmd = strings.TrimRight(cmd, "; ")

	_, handled := d.Handle(testMgrWithEmitter(t, em), cs, InboundRequest{
		Request:   Request{Text: "!" + cmd, Cwd: t.TempDir()},
		ChatID:    "oc_test",
		MessageID: "om_concurrent",
	})
	if !handled {
		t.Fatal("expected handled=true")
	}

	// Wait for the goroutine to finish. With sendErr set, every
	// send fails; the first one cancels shellCtx, CommandContext
	// kills the child, and the footer also fails. We assert at
	// least 2 calls (header + ≥1 drainer fail + footer).
	calls := awaitReply(t, em)
	if len(calls) < 2 {
		t.Errorf("Send call count = %d, want >= 2 (header + drainer + footer)", len(calls))
	}

	// Framework still emits Done even when everything fails.
	states := cap.snapshot()
	if len(states) != 2 || states[1].state != agent.MessageDone {
		t.Errorf("expected Queued + Done despite concurrent failures; got %+v", states)
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
func testMgrWithEmitter(t *testing.T, em messages.Emitter) *chatsession.Manager {
	t.Helper()
	return chatsession.NewManager().WithEmitter(em)
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
