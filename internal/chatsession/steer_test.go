package chatsession

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestSteerUserMessage_NoSelectedAS — when there's no selected
// AS, SteerUserMessage skips the Stop call entirely and just
// PushFronts the message. The message lands at the head of the
// pending region.
func TestSteerUserMessage_NoSelectedAS(t *testing.T) {
	cs := newChatSessionForTest("cs_steer_no_as")
	if cs.SelectedAgentSession() != nil {
		t.Fatalf("precondition: want nil selectedAS")
	}

	msg := makeTestMessage(cs, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "new direction"},
	}, "m_steer_1")
	if err := cs.SteerUserMessage(msg); err != nil {
		t.Fatalf("SteerUserMessage: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("QueueLen: got %d, want 1", got)
	}
	// Head of queue should be the steered message.
	batch := cs.queue.Peek()
	if len(batch) != 1 || batch[0].ID != "m_steer_1" {
		t.Fatalf("queue head: got %v, want [m_steer_1]", batch)
	}
}

// TestSteerUserMessage_PrependsAheadOfPending — when there's
// already a pending message, the steered message lands at the
// head (before the pending one).
func TestSteerUserMessage_PrependsAheadOfPending(t *testing.T) {
	cs := newChatSessionForTest("cs_steer_prepend")
	cs.SetSelectedCwd("/tmp")

	// Queue an initial follow-up (no live AS needed for
	// QueueUserMessage; FlushHook is a no-op until AS is ready).
	if err := cs.QueueUserMessage(makeTestMessage(cs, nil, "m_follow")); err != nil {
		t.Fatalf("QueueUserMessage setup: %v", err)
	}

	if err := cs.SteerUserMessage(makeTestMessage(cs, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "steered"},
	}, "m_steer")); err != nil {
		t.Fatalf("SteerUserMessage: %v", err)
	}

	batch := cs.queue.Peek()
	if len(batch) != 2 {
		t.Fatalf("queue batch: got %d items, want 2", len(batch))
	}
	if batch[0].ID != "m_steer" {
		t.Errorf("queue head: got id=%q, want m_steer (steer must land first)", batch[0].ID)
	}
	if batch[1].ID != "m_follow" {
		t.Errorf("queue tail: got id=%q, want m_follow (follow-up must be second)", batch[1].ID)
	}
}

// TestSteerUserMessage_ExitedAS_StillPushesFront — when the
// selected AS is in StatusExited (e.g. after /close killed the
// bridge process), SteerUserMessage skips the Stop call (the
// bridge is gone — calling Stop would be a wasted RPC against
// a defunct handle) but still PushFronts. Verify the queue
// grows and SteerUserMessage returns no error.
func TestSteerUserMessage_ExitedAS_StillPushesFront(t *testing.T) {
	cs := newChatSessionForTest("cs_steer_exited")

	// Inject an AS in StatusExited (mimics post-/close state).
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: "/tmp"}] = NewAgentSession("as_exited", cs.ID, "cc", "/tmp", nil)
	as := cs.pool[agentCwdKey{Agent: "cc", Cwd: "/tmp"}]
	as.SetHandleForTest(newRecordingAgentSession(1).buildLive())
	as.SetStatusForTest(StatusRunning)
	as.SetExited(0) // mark process gone
	cs.selectedAS = as
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "after close"},
	}, "m_post_close")
	if err := cs.SteerUserMessage(msg); err != nil {
		t.Fatalf("SteerUserMessage: %v", err)
	}

	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen: got %d, want 1 (PushFront must run even with StatusExited AS)", got)
	}
}

// TestSteerUserMessage_RunningAS_NoPanic — when the AS is
// running, SteerUserMessage invokes Stop on it. The recording
// driver's Stop returns agent.ErrNotSupported (no-op); the call
// is fire-and-forget so the error doesn't propagate. The test
// exists to lock in the "no panic, returns nil" contract for
// the happy path.
func TestSteerUserMessage_RunningAS_NoPanic(t *testing.T) {
	cs := newChatSessionForTest("cs_steer_running")

	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: "/tmp"}] = NewAgentSession("as_run", cs.ID, "cc", "/tmp", nil)
	as := cs.pool[agentCwdKey{Agent: "cc", Cwd: "/tmp"}]
	as.SetHandleForTest(newRecordingAgentSession(1).buildLive())
	as.SetStatusForTest(StatusRunning)
	cs.selectedAS = as
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "redirect"},
	}, "m_redirect")
	if err := cs.SteerUserMessage(msg); err != nil {
		t.Fatalf("SteerUserMessage on running AS: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen: got %d, want 1", got)
	}
}

// TestSteerUserMessage_EmptyID — zero-id messages are no-ops
// (consistent with QueueUserMessage and Push).
func TestSteerUserMessage_EmptyID(t *testing.T) {
	cs := newChatSessionForTest("cs_steer_empty")
	if err := cs.SteerUserMessage(Message{}); err != nil {
		t.Fatalf("SteerUserMessage zero-ID: got err=%v, want nil", err)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("QueueLen: got %d, want 0", got)
	}
}