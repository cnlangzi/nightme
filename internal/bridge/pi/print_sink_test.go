//go:build !windows

// print_sink_test.go — regression lock for the WithEventSink
// forwarding fix in pi's RunOnce / runPrintMode. The original
// bug: pi's Starter.RunOnce accepted `opts ...RunOnceOption`
// but silently dropped them when calling runPrintMode.
// /review's aggregator (and any other per-call observer) was
// therefore invisible to the print-mode path, leaving the
// chat sink permanently open ("review running…" forever)
// and the agent goroutine waiting for a synthetic outer
// Result that never came.
//
// The fix wires opts into runPrintMode (which now emits an
// up-front Ready + per-event forwarding through parsePrintStream
// + a final Error on the failure path). These tests drive the
// full RunOnce via the existing mock shell
// (internal/testdata/pi_print_mock.sh) so we exercise the
// real runPrintMode pipeline end-to-end, including the
// Ready/Text/Result sink emission.
package pi

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// sinkRecorder is a thread-safe AgentEvent collector used as
// the test sink.
type sinkRecorder struct {
	mu     sync.Mutex
	events []agent.AgentEvent
}

func (r *sinkRecorder) sink() func(agent.AgentEvent) {
	return func(ev agent.AgentEvent) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	}
}

func (r *sinkRecorder) snapshot() []agent.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agent.AgentEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *sinkRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// piPrintSinkMockPath mirrors piPrintMockPath from
// print_mock_unix_test.go but is defined in this file because
// the mock itself produces the events the sink test consumes.
// Tests in this file require the same mock script.
var piPrintSinkMockPath string

func init() {
	abs, err := filepath.Abs("../../testdata/pi_print_mock.sh")
	if err != nil {
		panic("pi_print_sink_test: filepath.Abs: " + err.Error())
	}
	piPrintSinkMockPath = abs
}

// TestRunOnce_ForwardsWithEventSink is the core regression
// lock for the opts-drop bug. Pre-fix, the sink received ZERO
// events because pi's RunOnce threw opts away. Post-fix, the
// sink must see: an up-front Ready (runPrintMode emits it
// before spawn), a terminal Result (with the mock's text +
// usage), and a paired Done marker — closing the lifecycle.
//
// pi's translator does NOT emit EventAgentText for individual
// text_delta events — it buffers them in pendingText and
// surfaces the joined text only on agent_settled as part of
// the EventAgentResult payload. So the sink observation here
// is minimal: Ready → Result → Done.
func TestRunOnce_ForwardsWithEventSink(t *testing.T) {
	a := NewStarter("pi-mock", piPrintSinkMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := &sinkRecorder{}
	t.Setenv("PI_PRINT_TEXT", "sink-test-payload")

	result, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	}, agent.WithEventSink(rec.sink()))
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if result.Text != "sink-test-payload" {
		t.Errorf("result.Text = %q, want %q", result.Text, "sink-test-payload")
	}

	events := rec.snapshot()

	// Required lifecycle events (must appear at minimum):
	//   - EventAgentReady   — runPrintMode's up-front emit
	//   - EventAgentResult  — turn-end result with text
	//   - EventAgentDone    — turn-end marker (paired with Result)
	gotReady := 0
	gotResult := 0
	gotDone := 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentReady:
			gotReady++
		case agent.EventAgentResult:
			gotResult++
		case agent.EventAgentDone:
			gotDone++
		}
	}

	if gotReady != 1 {
		t.Errorf("EventAgentReady count = %d, want 1 (runPrintMode emits one up-front)", gotReady)
	}
	if gotResult != 1 {
		t.Errorf("EventAgentResult count = %d, want 1 (translator emits one per turn on agent_settled)", gotResult)
	}
	if gotDone != 1 {
		t.Errorf("EventAgentDone count = %d, want 1 (translator emits one per turn)", gotDone)
	}

	// Verify order: Ready must come before Result, Result
	// before Done. Without ordering, the chat channel's
	// receipt header could flip in surprising ways.
	if len(events) >= 2 && events[0].Kind != agent.EventAgentReady {
		t.Errorf("event[0].Kind = %v, want EventAgentReady (lifecycle must open with Ready)", events[0].Kind)
	}
	if len(events) >= 2 && events[len(events)-2].Kind != agent.EventAgentResult {
		t.Errorf("event[len-2].Kind = %v, want EventAgentResult (Result must precede Done)", events[len(events)-2].Kind)
	}
	if len(events) >= 1 && events[len(events)-1].Kind != agent.EventAgentDone {
		t.Errorf("event[len-1].Kind = %v, want EventAgentDone (lifecycle must close with Done)", events[len(events)-1].Kind)
	}
}

// TestRunOnce_NoSink_StillSucceeds: passing NO WithEventSink
// must not break the call. Pre-fix this was trivially true
// (no sink ever saw anything anyway); post-fix this asserts
// the nil-sink path is non-blocking and the call returns the
// same result text as with a sink installed.
func TestRunOnce_NoSink_StillSucceeds(t *testing.T) {
	a := NewStarter("pi-mock", piPrintSinkMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PI_PRINT_TEXT", "no-sink-payload")

	result, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	}) // no opts
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if result.Text != "no-sink-payload" {
		t.Errorf("result.Text = %q, want %q", result.Text, "no-sink-payload")
	}
}

// TestRunOnce_FailureEmitsErrorToSink: when pi fails
// (non-zero exit), the sink must receive EventAgentError so
// the chat channel can flip the receipt to an error state.
// Without this, /review with a failing pi would leave the
// chat stuck on "review running…" forever — same hang as the
// opts-drop bug, different cause.
func TestRunOnce_FailureEmitsErrorToSink(t *testing.T) {
	a := NewStarter("pi-mock", piPrintSinkMockPath, []string{"--mode", "rpc"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := &sinkRecorder{}
	t.Setenv("PI_PRINT_FAIL", "1")
	t.Setenv("PI_PRINT_STDERR", "auth error: invalid API key")

	_, err := a.RunOnce(ctx, agent.StartConfig{Workspace: t.TempDir()}, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "do thing"},
	}, agent.WithEventSink(rec.sink()))
	if err == nil {
		t.Fatal("expected error when PI_PRINT_FAIL=1")
	}

	events := rec.snapshot()

	// Required: at least one EventAgentReady (pre-spawn) and
	// one EventAgentError (post-failure) so the lifecycle is
	// balanced. Pre-fix, the sink saw nothing at all — no
	// Ready, no Error — and the chat stuck on "running…".
	gotReady := 0
	gotError := 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentReady:
			gotReady++
		case agent.EventAgentError:
			gotError++
		}
	}
	if gotReady < 1 {
		t.Errorf("EventAgentReady count = %d, want >= 1 (up-front emit)", gotReady)
	}
	if gotError < 1 {
		t.Errorf("EventAgentError count = %d, want >= 1 (failure-path emit)", gotError)
	}
}