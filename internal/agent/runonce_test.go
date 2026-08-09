package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLiveAgent is a minimal Agent implementation that emits a
// canned sequence of AgentEvents on Events() and records SendBlocks
// calls. Used by RunOnceDrain tests.
type fakeLiveAgent struct {
	name    string
	events  chan AgentEvent
	sent    [][]ContentBlock
	closed  bool
	sendErr error
}

func newFakeLiveAgent(events []AgentEvent) *fakeLiveAgent {
	ch := make(chan AgentEvent, len(events)+1)
	for _, e := range events {
		ch <- e
	}
	return &fakeLiveAgent{events: ch}
}

func (f *fakeLiveAgent) Name() string                              { return f.name }
func (f *fakeLiveAgent) Mode() Mode                                { return ModePTY }
func (f *fakeLiveAgent) Command() string                           { return "fake" }
func (f *fakeLiveAgent) Args() []string                            { return nil }
func (f *fakeLiveAgent) Env() []string                             { return nil }
func (f *fakeLiveAgent) Detect() error                             { return nil }
func (f *fakeLiveAgent) Start(context.Context, StartConfig) (Agent, error) {
	return f, nil
}
func (f *fakeLiveAgent) Close() error {
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}
func (f *fakeLiveAgent) Events() <-chan AgentEvent { return f.events }
func (f *fakeLiveAgent) PID() int                 { return 99999 }
func (f *fakeLiveAgent) SendText(string) error    { return nil }
func (f *fakeLiveAgent) SendBlocks(_ context.Context, blocks []ContentBlock) error {
	cp := make([]ContentBlock, len(blocks))
	copy(cp, blocks)
	f.sent = append(f.sent, cp)
	return f.sendErr
}
func (f *fakeLiveAgent) SendPermission(string) error { return nil }
func (f *fakeLiveAgent) New(context.Context) error  { return nil }
func (f *fakeLiveAgent) RunOnce(context.Context, StartConfig, []ContentBlock) (string, error) {
	return "", errors.New("fakeLiveAgent: RunOnce not implemented")
}

func TestRunOnceDrain_HappyPath(t *testing.T) {
	live := newFakeLiveAgent([]AgentEvent{
		{Kind: EventAgentText, Text: "thinking..."},
		{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}},
		{Kind: EventAgentDone, Done: &AgentDoneEvent{ExitCode: 0}},
	})
	defer live.Close()

	got, err := RunOnceDrain(context.Background(), live, []ContentBlock{{Type: ContentText, Text: "hi"}}, "fake")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
	if len(live.sent) != 1 {
		t.Fatalf("SendBlocks calls = %d, want 1", len(live.sent))
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
	defer live.Close()

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
	defer live.Close()

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
	defer live.Close()

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
	defer live.Close()

	_, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err == nil {
		t.Fatalf("expected error on EventAgentError")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wraps %v", err, injected)
	}
}

func TestRunOnceDrain_StreamClosedNoResult(t *testing.T) {
	live := newFakeLiveAgent(nil)
	live.Close() // close without emitting any events

	_, err := RunOnceDrain(context.Background(), live, nil, "fake")
	if err == nil {
		t.Fatalf("expected error on closed stream")
	}
	if !contains(err.Error(), "event stream closed without result") {
		t.Fatalf("err = %v, want 'event stream closed without result'", err)
	}
}

func TestRunOnceDrain_ContextCancel(t *testing.T) {
	// Block forever — no events emitted.
	ch := make(chan AgentEvent)
	live := &fakeLiveAgent{name: "fake", events: ch}
	defer live.Close()

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
	live.sendErr = injected
	defer live.Close()

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
