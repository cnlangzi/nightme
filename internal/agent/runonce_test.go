package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDriver is a minimal driver implementation for RunOnceDrain
// tests. Only SendBlocks matters for the drain; the other driver
// methods are stubs.
type fakeDriver struct {
	mu       sync.Mutex
	events   []AgentEvent
	sendErr  error
	closeCnt int
}

func (f *fakeDriver) SendText(string) error                       { return nil }
func (f *fakeDriver) SendBlocks(_ context.Context, _ []ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendErr
}
func (f *fakeDriver) SendPermission(string) error                 { return nil }
func (f *fakeDriver) Reset(context.Context) error                 { return nil }
func (f *fakeDriver) Abort(context.Context) error                  { return nil }
func (f *fakeDriver) SetModel(context.Context, string, string) error { return nil }
func (f *fakeDriver) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCnt++
	return nil
}

// newFakeLiveAgent builds a *Agent whose events chan is pre-loaded
// with the given events. The chan is buffered so a synchronous
// RunOnceDrain can read it.
func newFakeLiveAgent(events []AgentEvent) *Agent {
	ch := make(chan AgentEvent, len(events)+1)
	for _, e := range events {
		ch <- e
	}
	d := &fakeDriver{events: events}
	return NewAgent(
		Info{Name: "fake", Mode: ModePTY, Command: "fake"},
		99999,
		ch,
		d,
	)
}

func TestRunOnceDrain_HappyPath(t *testing.T) {
	live := newFakeLiveAgent([]AgentEvent{
		{Kind: EventAgentText, Text: "thinking..."},
		{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}},
		{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 0}},
	})

	got, err := RunOnceDrain(context.Background(), live, []ContentBlock{{Type: ContentText, Text: "hi"}}, "fake")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
}

// TestRunOnceDrain_TrimsResultText verifies RunOnceDrain trims
// surrounding whitespace on the result text. Mirrors pty.RunOnce's
// TrimSpace so the dispatcher's success card has consistent
// whitespace across bridges.
func TestRunOnceDrain_TrimsResultText(t *testing.T) {
	live := newFakeLiveAgent([]AgentEvent{
		{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "  \n  pushed abc1234\n  "}},
	})

	got, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "pushed abc1234" {
		t.Fatalf("got %q, want %q", got, "pushed abc1234")
	}
}

func TestRunOnceDrain_DoneWithoutResult(t *testing.T) {
	live := newFakeLiveAgent([]AgentEvent{
		{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 0}},
	})

	_, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err == nil {
		t.Fatalf("expected error when turn ends without result")
	}
	if !contains(err.Error(), "turn ended without result event") {
		t.Fatalf("err = %v, want 'turn ended without result event'", err)
	}
}

func TestRunOnceDrain_NonZeroExit(t *testing.T) {
	live := newFakeLiveAgent([]AgentEvent{
		{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 1}},
	})

	_, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err == nil {
		t.Fatalf("expected error on non-zero exit")
	}
	if !contains(err.Error(), "exit 1") {
		t.Fatalf("err = %v, want mentions exit 1", err)
	}
}

func TestRunOnceDrain_ErrorEvent(t *testing.T) {
	injected := errors.New("agent exploded")
	live := newFakeLiveAgent([]AgentEvent{
		{Kind: EventAgentError, Err: injected},
	})

	_, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err == nil {
		t.Fatalf("expected error on EventAgentError")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wraps %v", err, injected)
	}
}

func TestRunOnceDrain_StreamClosedNoResult(t *testing.T) {
	ch := make(chan AgentEvent)
	live := NewAgent(Info{Name: "fake"}, 0, ch, &fakeDriver{})
	close(ch) // close without emitting any events

	_, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err == nil {
		t.Fatalf("expected error on closed stream")
	}
	if !contains(err.Error(), "event stream closed without result") {
		t.Fatalf("err = %v, want 'event stream closed without result'", err)
	}
}

func TestRunOnceDrain_ContextCancel(t *testing.T) {
	ch := make(chan AgentEvent)
	live := NewAgent(Info{Name: "fake"}, 0, ch, &fakeDriver{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := RunOnceDrain(ctx, live, nil, "fake")
	if err == nil {
		t.Fatalf("expected ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunOnceDrain_SendBlocksError(t *testing.T) {
	injected := errors.New("send failed")
	live := newFakeLiveAgent(nil)
	// Replace the driver with one that errors on SendBlocks.
	live.driver.(*fakeDriver).sendErr = injected

	_, err := RunOnceDrain(context.Background(), live, []ContentBlock{{Type: ContentText}}, "fake")
	if err == nil {
		t.Fatalf("expected error when SendBlocks fails")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wraps %v", err, injected)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
