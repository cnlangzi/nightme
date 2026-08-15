package acp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeDriver is a minimal driver implementation for collectResult
// tests. Only SendBlocks matters for the drain; the other driver
// methods are stubs that satisfy the unexported agent.driver
// interface so NewAgent's type assertion succeeds.
type fakeDriver struct {
	mu       sync.Mutex
	events   []agent.AgentEvent
	sendErr  error
	closeCnt int
}

func (f *fakeDriver) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendErr
}
func (f *fakeDriver) SendPermission(string) error { return nil }
func (f *fakeDriver) Reset(context.Context) error { return nil }
func (f *fakeDriver) Stop(context.Context) error  { return nil }
func (f *fakeDriver) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCnt++
	return nil
}

// newFakeLiveAgent builds a *agent.Agent whose events chan is
// pre-loaded with the given events. The chan is buffered so a
// synchronous collectResult can read it.
func newFakeLiveAgent(events []agent.AgentEvent) *agent.Agent {
	ch := make(chan agent.AgentEvent, len(events)+1)
	for _, e := range events {
		ch <- e
	}
	d := &fakeDriver{events: events}
	return agent.NewAgent(
		agent.Info{Name: "fake", Mode: agent.ModePTY, Command: "fake"},
		99999,
		ch,
		d,
	)
}

// newFakeStarter builds a Starter with name "fake" so collectResult's
// live.Info().Name matches the error-message prefix the tests assert on.
func newFakeStarter() *Starter {
	return NewStarter("fake", "fake", nil, nil, 0, 0)
}

func TestCollectResult_HappyPath(t *testing.T) {
	s := newFakeStarter()
	live := newFakeLiveAgent([]agent.AgentEvent{
		{Kind: agent.EventAgentText, Text: "thinking..."},
		{Kind: agent.EventAgentResult, Result: &agent.AgentResultEvent{Text: "ok"}},
		{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}},
	})

	got, err := s.collectResult(context.Background(), live, []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Text != "ok" {
		t.Fatalf("got %q, want ok", got.Text)
	}
}

// TestCollectResult_TrimsResultText verifies collectResult trims
// surrounding whitespace on the result text. Mirrors pty.RunOnce's
// TrimSpace so the dispatcher's success card has consistent
// whitespace across bridges.
func TestCollectResult_TrimsResultText(t *testing.T) {
	s := newFakeStarter()
	live := newFakeLiveAgent([]agent.AgentEvent{
		{Kind: agent.EventAgentResult, Result: &agent.AgentResultEvent{Text: "  \n  pushed abc1234\n  "}},
	})

	got, err := s.collectResult(context.Background(), live, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Text != "pushed abc1234" {
		t.Fatalf("got %q, want %q", got.Text, "pushed abc1234")
	}
}

func TestCollectResult_DoneWithoutResult(t *testing.T) {
	s := newFakeStarter()
	live := newFakeLiveAgent([]agent.AgentEvent{
		{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}},
	})

	_, err := s.collectResult(context.Background(), live, nil)
	if err == nil {
		t.Fatalf("expected error when turn ends without result")
	}
	if !contains(err.Error(), "turn ended without result event") {
		t.Fatalf("err = %v, want 'turn ended without result event'", err)
	}
}

// TestCollectResult_DoneWithPendingAuditFields pins the
// F-CLAUDE-PRINT-001 followup audit-field contract: when
// EventAgentDone fires without a preceding terminal Result
// emission, the per-session identity (session_id + model
// captured from EventAgentReady) survives in the wrapped error.
//
// Mirrors the claudecode / pi print-mode auditFields format —
// same bracket-and-key=value shape — so operators can grep
// daemon logs across all three bridges.
func TestCollectResult_DoneWithPendingAuditFields(t *testing.T) {
	s := newFakeStarter()
	live := newFakeLiveAgent([]agent.AgentEvent{
		{Kind: agent.EventAgentReady, SessionID: "sess-abc", Model: "claude-opus"},
		{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}},
	})

	_, err := s.collectResult(context.Background(), live, nil)
	if err == nil {
		t.Fatalf("expected error when turn ends without result")
	}
	if !contains(err.Error(), "turn ended without result event") {
		t.Fatalf("err = %v, want 'turn ended without result event'", err)
	}
	if !contains(err.Error(), "session_id=sess-abc") {
		t.Errorf("audit field session_id dropped: %v", err)
	}
	if !contains(err.Error(), "model=claude-opus") {
		t.Errorf("audit field model dropped: %v", err)
	}
}

// TestCollectResult_NonZeroExitWithAuditFields exercises the
// non-zero-exit path: a Ready event captures identity, then a
// non-zero EventAgentDone arrives. The wrapped error must
// include the audit fields.
func TestCollectResult_NonZeroExitWithAuditFields(t *testing.T) {
	s := newFakeStarter()
	ch := make(chan agent.AgentEvent, 4)
	ch <- agent.AgentEvent{Kind: agent.EventAgentReady, SessionID: "sess-1", Model: "claude-opus"}
	ch <- agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 2}}
	close(ch)
	live := agent.NewAgent(agent.Info{Name: "fake"}, 0, ch, &fakeDriver{})

	_, err := s.collectResult(context.Background(), live, nil)
	if err == nil {
		t.Fatalf("expected error on non-zero exit")
	}
	if !contains(err.Error(), "exit 2") {
		t.Errorf("err = %v, want mentions exit 2", err)
	}
	if !contains(err.Error(), "session_id=sess-1") {
		t.Errorf("audit field session_id dropped: %v", err)
	}
	if !contains(err.Error(), "model=claude-opus") {
		t.Errorf("audit field model dropped: %v", err)
	}
}

func TestCollectResult_NonZeroExit(t *testing.T) {
	s := newFakeStarter()
	live := newFakeLiveAgent([]agent.AgentEvent{
		{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 1}},
	})

	_, err := s.collectResult(context.Background(), live, nil)
	if err == nil {
		t.Fatalf("expected error on non-zero exit")
	}
	if !contains(err.Error(), "exit 1") {
		t.Fatalf("err = %v, want mentions exit 1", err)
	}
}

func TestCollectResult_ErrorEvent(t *testing.T) {
	s := newFakeStarter()
	injected := errors.New("agent exploded")
	live := newFakeLiveAgent([]agent.AgentEvent{
		{Kind: agent.EventAgentError, Err: injected},
	})

	_, err := s.collectResult(context.Background(), live, nil)
	if err == nil {
		t.Fatalf("expected error on EventAgentError")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wraps %v", err, injected)
	}
}

func TestCollectResult_StreamClosedNoResult(t *testing.T) {
	s := newFakeStarter()
	ch := make(chan agent.AgentEvent)
	live := agent.NewAgent(agent.Info{Name: "fake"}, 0, ch, &fakeDriver{})
	close(ch) // close without emitting any events

	_, err := s.collectResult(context.Background(), live, nil)
	if err == nil {
		t.Fatalf("expected error on closed stream")
	}
	if !contains(err.Error(), "event stream closed without result") {
		t.Fatalf("err = %v, want 'event stream closed without result'", err)
	}
}

func TestCollectResult_ContextCancel(t *testing.T) {
	s := newFakeStarter()
	ch := make(chan agent.AgentEvent)
	live := agent.NewAgent(agent.Info{Name: "fake"}, 0, ch, &fakeDriver{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := s.collectResult(ctx, live, nil)
	if err == nil {
		t.Fatalf("expected ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCollectResult_SendBlocksError(t *testing.T) {
	s := newFakeStarter()
	injected := errors.New("send failed")
	live := newFakeLiveAgent(nil)
	// Replace the driver with one that errors on SendBlocks.
	live.Driver().(*fakeDriver).sendErr = injected

	_, err := s.collectResult(context.Background(), live, []agent.ContentBlock{{Type: agent.ContentText}})
	if err == nil {
		t.Fatalf("expected error when SendBlocks fails")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want wraps %v", err, injected)
	}
}

// TestAppendRunOnceAudit_Helper pins the audit-suffix format
// directly. Locked in isolation so a regression in the bracket
// shape (e.g. dropping the leading space or the `=` separator)
// surfaces as a test failure rather than silently changing log
// grep patterns across bridges.
func TestAppendRunOnceAudit_Helper(t *testing.T) {
	cases := []struct {
		name    string
		session string
		model   string
		want    string
	}{
		{"empty inputs", "", "", ""},
		{"session only", "sess-abc", "", " [session_id=sess-abc]"},
		{"model only", "", "claude-opus-4", " [model=claude-opus-4]"},
		{"both", "sess-1", "claude-opus", " [session_id=sess-1] [model=claude-opus]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appendAuditFields(c.session, c.model); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
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
