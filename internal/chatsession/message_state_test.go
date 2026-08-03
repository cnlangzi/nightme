package chatsession

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/receipt"
)

// captureHandler records every callback invocation for assertions.
// Implements the func(chatID, userMsgID, state) signature.
type captureHandler struct {
	mu    sync.Mutex
	calls []messageStateCall
}

type messageStateCall struct {
	chatID, userMsgID string
	state             receipt.MessageState
}

func (c *captureHandler) handler(chatID, userMsgID string, state receipt.MessageState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, messageStateCall{chatID, userMsgID, state})
}

func (c *captureHandler) snapshot() []messageStateCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]messageStateCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestEmitMessageState_HandlerInvoked verifies that EmitMessageState
// fires the registered handler with the correct (chatID, userMsgID,
// state) triple. No-op when no handler installed.
func TestEmitMessageState_HandlerInvoked(t *testing.T) {
	cs := New("oc_chat", "p2p", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	cs.EmitMessageState("om_msg_1", receipt.StateReceived)
	cs.EmitMessageState("om_msg_2", receipt.StateForwarded)
	cs.EmitMessageState("om_msg_3", receipt.StateDone)

	got := cap.snapshot()
	if len(got) != 3 {
		t.Fatalf("captured %d calls; want 3", len(got))
	}
	want := []messageStateCall{
		{"oc_chat", "om_msg_1", receipt.StateReceived},
		{"oc_chat", "om_msg_2", receipt.StateForwarded},
		{"oc_chat", "om_msg_3", receipt.StateDone},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call[%d] = %+v; want %+v", i, got[i], w)
		}
	}
}

// TestEmitMessageState_NoHandlerIsNoop confirms EmitMessageState is
// safe to call without a registered handler — must not panic.
func TestEmitMessageState_NoHandlerIsNoop(t *testing.T) {
	cs := New("oc_chat", "p2p", "claude")
	// No SetMessageStateHandler call.
	cs.EmitMessageState("om_msg", receipt.StateReceived)
	// If we got here without panic, success.
}

// TestSetMessageStateHandler_NilClears confirms passing nil clears
// the handler (subsequent emits become no-ops).
func TestSetMessageStateHandler_NilClears(t *testing.T) {
	cs := New("oc_chat", "p2p", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)
	cs.EmitMessageState("om_1", receipt.StateReceived)
	cs.SetMessageStateHandler(nil)
	cs.EmitMessageState("om_2", receipt.StateDone)

	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("captured %d calls after nil-clear; want 1 (only the first emit)", got)
	}
}

// TestEmitMessageStateForCurrentTurn_FansOut verifies that
// emitMessageStateForCurrentTurn (called from runReadPump on
// EventDone/Error) emits once per userMsgID in
// currentTurnUserMsgIDs, then clears the slice.
func TestEmitMessageStateForCurrentTurn_FansOut(t *testing.T) {
	cs := New("oc_chat", "p2p", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	// Simulate InputBuffer flush tracking 2 userMsgIDs.
	cs.mu.Lock()
	cs.currentTurnUserMsgIDs = []string{"om_a", "om_b"}
	cs.mu.Unlock()

	cs.emitMessageStateForCurrentTurn(receipt.StateDone)

	got := cap.snapshot()
	if len(got) != 2 {
		t.Fatalf("captured %d calls; want 2 (one per userMsgID)", len(got))
	}
	if got[0] != (messageStateCall{"oc_chat", "om_a", receipt.StateDone}) {
		t.Errorf("call[0] = %+v; want om_a/Done", got[0])
	}
	if got[1] != (messageStateCall{"oc_chat", "om_b", receipt.StateDone}) {
		t.Errorf("call[1] = %+v; want om_b/Done", got[1])
	}

	// currentTurnUserMsgIDs must be cleared so the next flush
	// starts fresh.
	cs.mu.RLock()
	remaining := cs.currentTurnUserMsgIDs
	cs.mu.RUnlock()
	if len(remaining) != 0 {
		t.Errorf("currentTurnUserMsgIDs not cleared: %v", remaining)
	}

	// Subsequent call with empty currentTurnUserMsgIDs → no-op.
	cs.emitMessageStateForCurrentTurn(receipt.StateError)
	if got := len(cap.snapshot()); got != 2 {
		t.Errorf("captured %d calls; want 2 (no-op after clear)", got)
	}
}

// TestDefaultFlushHook_TracksUserMsgIDs verifies that
// defaultFlushHookLocked captures the userMsgIDs into
// currentTurnUserMsgIDs (F-31 design: runReadPump needs this to
// know which messages to mark Done/Error after the turn ends).
func TestDefaultFlushHook_TracksUserMsgIDs(t *testing.T) {
	cs := New("oc_chat", "p2p", "claude")
	cs.WithSpawner(&spySpawner{})

	cs.mu.Lock()
	hook := cs.defaultFlushHookLocked()
	// Pre-populate activeAS with a no-op agent so SendBlocks succeeds.
	cs.activeAS = newActiveAgentNoop()
	cs.mu.Unlock()

	// FlushHook should capture userMsgIDs but SendBlocks will fail
	// (noop returns no error, but that's fine — we only assert on
	// the capture side-effect).
	_ = hook([]agent.ContentBlock{{Text: "hi"}}, []string{"om_x", "om_y"})

	cs.mu.RLock()
	tracked := append([]string(nil), cs.currentTurnUserMsgIDs...)
	cs.mu.RUnlock()

	want := []string{"om_x", "om_y"}
	if len(tracked) != len(want) {
		t.Fatalf("tracked %d userMsgIDs; want %d (%v)", len(tracked), len(want), tracked)
	}
	for i, w := range want {
		if tracked[i] != w {
			t.Errorf("tracked[%d] = %q; want %q", i, tracked[i], w)
		}
	}
}

// spySpawner is a no-op Spawner for tests that don't actually
// fork a process.
type spySpawner struct{}

func (s *spySpawner) Spawn(_ context.Context, _ string, _ string, _ []string) (agent.AgentSession, error) {
	return nil, errors.New("spySpawner: not implemented")
}

// newActiveAgentNoop creates a minimal AgentSession whose
// SendBlocks is a no-op, so FlushHook tests can run without
// spawning a real CLI.
func newActiveAgentNoop() *AgentSession {
	as := NewAgentSession("test-as", "test-cs", "claude", "/tmp", nil)
	// Inject a fake handle so SendBlocks succeeds. We use the
	// recordingAgentSession pattern from flushhook_test.go.
	rec := &recordingAgentSession{pid: 1, events: make(chan agent.AgentEvent, 16)}
	as.handle = rec
	return as
}

// Use the existing recordingAgentSession type from flushhook_test.go
// to satisfy AgentSession.Handle(). This avoids an import cycle
// (test types live in the same package).
var _ = (*recordingAgentSession)(nil)