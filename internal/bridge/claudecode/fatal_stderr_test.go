package claudecode

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fatalLine is the shape Claude Code writes when it refuses to work:
// a structured marker, then the details it knows about.
const fatalLine = `[claude-code:unrecognized_model] {"model":"MiniMax-M3[1m]","query_source":"sdk"}`

// newFatalTestDriver builds a driver with just enough runtime state
// for the fatal-stderr path: an events channel, a stderr tail, and
// the close signals the report path touches.
func newFatalTestDriver(t *testing.T) *driver {
	t.Helper()
	d := &driver{
		stdin:      bufio.NewWriter(io.Discard),
		events:     make(chan agent.AgentEvent, 8),
		stderrTail: agent.NewStderrRingBuffer(agent.StderrTailBytes),
		agentName:  "claudecode",
		workspace:  "/tmp",
		closed:     make(chan struct{}),
		exitDone:   make(chan struct{}),
	}
	// The report path closes the bridge in the background; the close
	// waits for the exit signal, so the test raises it on the way
	// out instead of leaking the goroutine.
	t.Cleanup(func() { close(d.exitDone) })
	return d
}

// TestIsFatalStderr pins the declared format: the CLI's own marker
// prefix. A line that merely mentions a failure is not the marker.
func TestIsFatalStderr(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"claude code marker", fatalLine, true},
		{"bare marker", "[claude-code:api_error]", true},
		{"leading whitespace", "  " + fatalLine, true},
		{"empty", "", false},
		{"ordinary warning", "npm warn deprecated foo@1.0.0", false},
		{"contains the word error", "Error: request failed (error 42)", false},
		{"contains the word failed", "MCP server failed to start", false},
		{"marker not at the start", "recovered from [claude-code:api_error]", false},
		{"different marker", "[codex:unrecognized_model] {}", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFatalStderr(tc.line); got != tc.want {
				t.Errorf("isFatalStderr(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestDriver_FatalStderrLine_EndsTheTurnWithTheCLILine is the core
// of the contract: a fatal stderr line read while a turn is in
// flight ends that turn with an EventAgentError whose text is the
// CLI's own line.
func TestDriver_FatalStderrLine_EndsTheTurnWithTheCLILine(t *testing.T) {
	d := newFatalTestDriver(t)
	d.turnActive.Store(true)

	_ = drainStderr(strings.NewReader(fatalLine+"\n"), nil, nil, d.stderrTail,
		isFatalStderr, d.reportFatalStderr)

	ev := waitForAgentEvent(t, d.events)
	if ev.Kind != agent.EventAgentError {
		t.Fatalf("kind = %v, want EventAgentError", ev.Kind)
	}
	if ev.Err == nil {
		t.Fatal("Err = nil, want the CLI's line")
	}
	if ev.Err.Error() != fatalLine {
		t.Errorf("Err.Error() = %q, want %q", ev.Err.Error(), fatalLine)
	}
	if ev.Diagnostic == nil {
		t.Fatal("Diagnostic = nil; the chat renders the stderr tail from it")
	}
	if !strings.Contains(ev.Diagnostic.StderrTail, "unrecognized_model") {
		t.Errorf("Diagnostic.StderrTail = %q, want the CLI's line in it", ev.Diagnostic.StderrTail)
	}
	// The CLI has not exited when the failure is reported, so there
	// is no exit kind to claim — a wrong one would reach the card
	// title and /diagnose.
	if ev.Diagnostic.ExitKind != agent.BridgeExitUnknown {
		t.Errorf("Diagnostic.ExitKind = %v, want %v", ev.Diagnostic.ExitKind, agent.BridgeExitUnknown)
	}
}

// TestDriver_OrdinaryStderrLine_KeepsTheTurn pins the negative
// case: a line that talks about an error but is not the CLI's own
// marker ends nothing.
func TestDriver_OrdinaryStderrLine_KeepsTheTurn(t *testing.T) {
	d := newFatalTestDriver(t)
	d.turnActive.Store(true)

	const line = "Error: request failed (error 42)"
	_ = drainStderr(strings.NewReader(line+"\n"), nil, nil, d.stderrTail,
		isFatalStderr, d.reportFatalStderr)

	assertNoAgentEvent(t, d.events)
	if tail := d.stderrTail.String(); !strings.Contains(tail, "request failed") {
		t.Errorf("stderr tail = %q, want the line in it", tail)
	}
}

// TestDriver_FatalStderrLine_WithoutATurn_KeepsTheTail pins the
// case where the CLI prints a fatal line outside a turn — the
// startup handshake, or after the turn closed. Nothing ends, but the
// line stays where an operator can find it.
func TestDriver_FatalStderrLine_WithoutATurn_KeepsTheTail(t *testing.T) {
	d := newFatalTestDriver(t)

	_ = drainStderr(strings.NewReader(fatalLine+"\n"), nil, nil, d.stderrTail,
		isFatalStderr, d.reportFatalStderr)

	assertNoAgentEvent(t, d.events)
	if tail := d.stderrTail.String(); !strings.Contains(tail, "unrecognized_model") {
		t.Errorf("stderr tail = %q, want the line in it", tail)
	}
}

// TestDriver_FatalStderrLine_OnAClosingBridge_ReportsNothing pins
// the shutdown precedence: a bridge the daemon is tearing down does
// not report a failure of its own.
func TestDriver_FatalStderrLine_OnAClosingBridge_ReportsNothing(t *testing.T) {
	d := newFatalTestDriver(t)
	d.turnActive.Store(true)
	close(d.closed)

	_ = drainStderr(strings.NewReader(fatalLine+"\n"), nil, nil, d.stderrTail,
		isFatalStderr, d.reportFatalStderr)

	assertNoAgentEvent(t, d.events)
}

// TestDriver_FatalStderrLines_EndsTheTurnOnce pins "one error per
// failure": a CLI that prints the failure several times still
// reaches the chat as a single error.
func TestDriver_FatalStderrLines_EndsTheTurnOnce(t *testing.T) {
	d := newFatalTestDriver(t)
	d.turnActive.Store(true)

	_ = drainStderr(strings.NewReader(fatalLine+"\n"+fatalLine+"\n"), nil, nil,
		d.stderrTail, isFatalStderr, d.reportFatalStderr)

	ev := waitForAgentEvent(t, d.events)
	if ev.Kind != agent.EventAgentError {
		t.Fatalf("kind = %v, want EventAgentError", ev.Kind)
	}
	assertNoAgentEvent(t, d.events)
}

// TestDriver_ClaimError_AdmitsOneReport pins the shared slot the
// fatal-stderr path and the exit path both claim: the CLI can report
// a failure and exit right after, and that is one failure.
func TestDriver_ClaimError_AdmitsOneReport(t *testing.T) {
	d := newFatalTestDriver(t)
	if !d.claimError() {
		t.Fatal("first claimError() = false, want true")
	}
	if d.claimError() {
		t.Error("second claimError() = true, want false")
	}
}

// TestPumpStream_ClearsTheTurnFlagOnResult pins the turn bookkeeping
// the fatal-stderr path depends on: the flag drops when the CLI
// delivers the turn's result.
func TestPumpStream_ClearsTheTurnFlagOnResult(t *testing.T) {
	turnActive := &atomic.Bool{}
	turnActive.Store(true)
	events := make(chan agent.AgentEvent, 8)

	pumpStream(strings.NewReader(`{"type":"result","result":"done"}`+"\n"),
		events, nil, nil, turnActive, "claude", "/tmp", "main", nil)

	if turnActive.Load() {
		t.Error("turn flag still set after the turn's result")
	}
}

// TestPumpStream_KeepsTheTurnFlagOnCompaction pins the mid-turn
// case: a compaction is not the end of the turn, so the flag stays.
func TestPumpStream_KeepsTheTurnFlagOnCompaction(t *testing.T) {
	turnActive := &atomic.Bool{}
	turnActive.Store(true)
	events := make(chan agent.AgentEvent, 8)

	pumpStream(strings.NewReader(`{"type":"result","subtype":"compact"}`+"\n"),
		events, nil, nil, turnActive, "claude", "/tmp", "main", nil)

	if !turnActive.Load() {
		t.Error("turn flag cleared by a mid-turn compaction")
	}
}

// TestPumpStream_ClearsTheTurnFlagOnInit pins the conversation
// boundary: a fresh conversation means the previous turn is over,
// whatever the CLI did or did not report about it.
func TestPumpStream_ClearsTheTurnFlagOnInit(t *testing.T) {
	turnActive := &atomic.Bool{}
	turnActive.Store(true)
	events := make(chan agent.AgentEvent, 8)

	pumpStream(strings.NewReader(`{"type":"system","subtype":"init","session_id":"s1"}`+"\n"),
		events, nil, nil, turnActive, "claude", "/tmp", "main", nil)

	if turnActive.Load() {
		t.Error("turn flag survived a conversation boundary")
	}
}

// TestDriver_SendBlocks_FailedWriteLeavesNoTurn pins the turn
// bookkeeping on a failed write: the CLI never got the message, so
// no turn is in flight — and a fatal line that arrives afterwards
// must not be read as a turn failure.
func TestDriver_SendBlocks_FailedWriteLeavesNoTurn(t *testing.T) {
	d := &driver{
		stdin:    bufio.NewWriter(failingWriter{}),
		events:   make(chan agent.AgentEvent, 4),
		closed:   make(chan struct{}),
		exitDone: make(chan struct{}),
	}
	err := d.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
	})
	if err == nil {
		t.Fatal("SendBlocks should fail when the write fails")
	}
	if d.turnActive.Load() {
		t.Error("a failed write left the turn flag set")
	}
}

// failingWriter rejects every write.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("write failed")
}

// waitForAgentEvent returns the next event on ch, failing the test
// when none arrives.
func waitForAgentEvent(t *testing.T, ch <-chan agent.AgentEvent) agent.AgentEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no agent event arrived")
		return agent.AgentEvent{}
	}
}

// assertNoAgentEvent fails the test when ch produces an event.
func assertNoAgentEvent(t *testing.T, ch <-chan agent.AgentEvent) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected agent event: kind=%v err=%v", ev.Kind, ev.Err)
	case <-time.After(200 * time.Millisecond):
	}
}
