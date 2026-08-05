package chatsession

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// captureHandler records every callback invocation for assertions.
// Implements the func(chatID, userMsgID, state) signature.
type captureHandler struct {
	mu    sync.Mutex
	calls []messageStateCall
}

type messageStateCall struct {
	chatID, userMsgID string
	state             agent.MessageState
}

func (c *captureHandler) handler(chatID, userMsgID string, state agent.MessageState) {
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
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	cs.EmitMessageState("om_msg_1", agent.MessageReceived)
	cs.EmitMessageState("om_msg_2", agent.MessageForwarded)
	cs.EmitMessageState("om_msg_3", agent.MessageDone)

	got := cap.snapshot()
	if len(got) != 3 {
		t.Fatalf("captured %d calls; want 3", len(got))
	}
	want := []messageStateCall{
		{"oc_chat", "om_msg_1", agent.MessageReceived},
		{"oc_chat", "om_msg_2", agent.MessageForwarded},
		{"oc_chat", "om_msg_3", agent.MessageDone},
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
	cs := New("oc_chat", "claude")
	// No SetMessageStateHandler call.
	cs.EmitMessageState("om_msg", agent.MessageReceived)
	// If we got here without panic, success.
}

// TestSetMessageStateHandler_NilClears confirms passing nil clears
// the handler (subsequent emits become no-ops).
func TestSetMessageStateHandler_NilClears(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)
	cs.EmitMessageState("om_1", agent.MessageReceived)
	cs.SetMessageStateHandler(nil)
	cs.EmitMessageState("om_2", agent.MessageDone)

	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("captured %d calls after nil-clear; want 1 (only the first emit)", got)
	}
}

// TestEmitMessageStateForCurrentTurn_AnchorOnly verifies that
// emitMessageStateForCurrentTurn (called from runReadPump on
// EventDone/Error) emits exactly once for the anchor
// currentTurnUserMsgID, then clears the string.
//
// v1.3 (SPEC §2.5): terminal MessageState fires for the anchor
// only. Earlier userMsgIDs in a buffered batch keep their own
// MessageState at StateForwarded until they themselves anchor a
// future turn — see chatsession.go docstring on
// emitMessageStateForCurrentTurn.
func TestEmitMessageStateForCurrentTurn_AnchorOnly(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	// Simulate InputBuffer flush tracking the anchor userMsgID
	// (in v1.3, currentTurnUserMsgID is a single string; the
	// FlushHook already captured the last userMsgID of the batch).
	cs.mu.Lock()
	cs.currentTurnUserMsgID = "om_b"
	cs.mu.Unlock()

	cs.emitMessageStateForCurrentTurn(agent.MessageDone)

	got := cap.snapshot()
	if len(got) != 1 {
		t.Fatalf("captured %d calls; want 1 (anchor only)", len(got))
	}
	if got[0] != (messageStateCall{"oc_chat", "om_b", agent.MessageDone}) {
		t.Errorf("call[0] = %+v; want om_b/Done", got[0])
	}

	// currentTurnUserMsgID must be cleared so the next flush
	// starts fresh.
	cs.mu.RLock()
	remaining := cs.currentTurnUserMsgID
	cs.mu.RUnlock()
	if len(remaining) != 0 {
		t.Errorf("currentTurnUserMsgID not cleared: %q", remaining)
	}

	// Subsequent call with empty currentTurnUserMsgID → no-op.
	cs.emitMessageStateForCurrentTurn(agent.MessageFailed)
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("captured %d calls; want 1 (no-op after clear)", got)
	}
}

// TestDefaultFlushHook_TracksUserMsgIDs verifies that
// defaultFlushHookLocked captures the userMsgIDs into
// currentTurnUserMsgID (F-31 design: runReadPump needs this to
// know which messages to mark Done/Error after the turn ends).
func TestDefaultFlushHook_TracksUserMsgIDs(t *testing.T) {
	cs := New("oc_chat", "claude")
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
	tracked := cs.currentTurnUserMsgID
	cs.mu.RUnlock()

	// v1.3: currentTurnUserMsgID is a single string (the last
	// userMsgID in the flush batch), not a slice.
	want := "om_y"
	if tracked != want {
		t.Errorf("tracked = %q; want %q", tracked, want)
	}
}

// spySpawner is a no-op Spawner for tests that don't actually
// fork a process.
type spySpawner struct{}

func (s *spySpawner) Spawn(_ context.Context, _ string, _ string, _ []string, _ string) (agent.AgentSession, error) {
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