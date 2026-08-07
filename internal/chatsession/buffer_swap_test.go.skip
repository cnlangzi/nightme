// /use swap-path invariants — the cross-AS FSM pollution class:
//
//   1. ClearBuffer drops every queued message and returns the
//      dropped count. Used by handleUse to ensure the new AS
//      starts with a clean queue (no orphan messages from the
//      previous AS bleeding across).
//   2. SetIdle resets the FSM from Busy → Idle so the next user
//      message after /use flushes immediately to the new AS
//      instead of being queued.
//   3. Together: after /use, the new AS receives the next user
//      message synchronously — no FSM state can be carried over
//      from the previous (possibly hung) AS.
//
// Background: in the F-32 2026-08-06 incident, pi hung while
// its pump had already flipped state to Busy on an early
// non-terminal event. /use to a different agent did NOT reset
// the FSM — claude inherited Busy → "hi" got queued behind a
// turn that would never produce EventDone. The receipt stayed at
// "Working..." indefinitely. These tests pin the fix.
package chatsession

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestClearBuffer_DropsQueuedMessages verifies that messages
// sitting in the InputBuffer waiting for an in-flight turn are
// dropped on demand, with the dropped count returned. The new
// AS that /use promotes must not inherit the previous AS's
// abandoned queue.
func TestClearBuffer_DropsQueuedMessages(t *testing.T) {
	cs := New("test_clearbuf", "pi").WithSpawner(nil)
	_, _ = newTestASWithFakeHandle(cs)

	// Drop a fake message into the buffer by setting state to
	// Busy and calling Add — Add's Busy branch queues.
	cs.SetBusy()
	if err := cs.QueueUserMessage(makeTestMessage(cs, []agent.ContentBlock{{Type: agent.ContentText, Text: "abandoned hi"}}, "um_abandoned")); err != nil {
		t.Fatalf("QueueUserMessage (Busy): %v", err)
	}
	if n := cs.BufferPending(); n != 1 {
		t.Fatalf("pre-Clear: BufferPending = %d; want 1", n)
	}

	// /use calls ClearBuffer.
	n := cs.ClearBuffer()
	if n != 1 {
		t.Errorf("ClearBuffer returned %d; want 1", n)
	}
	if n := cs.BufferPending(); n != 0 {
		t.Errorf("post-Clear: BufferPending = %d; want 0", n)
	}
}

// TestSetIdle_ResetsBusyFromHang pins the cross-AS pollution
// fix: when the previous AS leaves the FSM in Busy (because it
// never sent EventDone), /use must explicitly reset to Idle so
// the new AS receives the next user message synchronously.
//
// We exercise the transition via the InputBuffer FSM state
// machine directly (not via /use wiring) — handleUse already
// covers the wiring in handlers_chatsession_test.go.
func TestSetIdle_ResetsBusyFromHang(t *testing.T) {
	cs := New("test_setidle", "pi").WithSpawner(nil)
	_, _ = newTestASWithFakeHandle(cs)

	// Simulate the F-32 incident: a hung AS left the FSM Busy.
	cs.SetBusy()
	if cs.BufferState() != StateBusy {
		t.Fatalf("pre-SetIdle: state = %s; want Busy", cs.BufferState())
	}

	// /use calls SetIdle on the new AS to clean carry-over.
	cs.SetIdle()
	if cs.BufferState() != StateIdle {
		t.Fatalf("post-SetIdle: state = %s; want Idle", cs.BufferState())
	}
}

// TestUseSwap_BothInvariantsRunTogether verifies the full
// /use pipeline on the InputBuffer FSM:
//
//   - the previous AS leaves the FSM Busy (or messages queued)
//   - /use clears the queue AND resets to Idle
//   - the next user message flushes immediately (sync path),
//     even though the previous AS never produced an EventDone.
//
// This is the integration-level regression test for the F-32
// 2026-08-06 incident fix; both halves of the fix must hold
// simultaneously or claude still inherits the polluted state.
func TestUseSwap_BothInvariantsRunTogether(t *testing.T) {
	cs := New("test_useswap", "pi").WithSpawner(nil)
	_, _ = newTestASWithFakeHandle(cs)

	// Step 1: simulate pi hang. A user message arrives, FSM is
	// flipped Busy by some leaked pi event, and the message is
	// queued (instead of being sent to the hung pi bridge).
	cs.SetBusy()
	if err := cs.QueueUserMessage(makeTestMessage(cs, []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}}, "um_pi")); err != nil {
		t.Fatalf("QueueUserMessage (Busy): %v", err)
	}
	if cs.BufferState() != StateBusy {
		t.Fatalf("setup: state should be Busy, got %s", cs.BufferState())
	}
	if cs.BufferPending() != 1 {
		t.Fatalf("setup: queue should have 1, got %d", cs.BufferPending())
	}

	// Step 2: /use pipeline runs ClearBuffer + SetIdle.
	if n := cs.ClearBuffer(); n != 1 {
		t.Errorf("ClearBuffer = %d; want 1", n)
	}
	cs.SetIdle()

	// Step 3: the next user message must flush immediately
	// (FSM is IDLE) — the bug would queue it silently.
	if err := cs.QueueUserMessage(makeTestMessage(cs, []agent.ContentBlock{{Type: agent.ContentText, Text: "hi to claude"}}, "um_claude")); err != nil {
		t.Fatalf("QueueUserMessage (post-/use): %v", err)
	}
	// FlushHook was called synchronously (state==Idle); if the
	// bug were still present, Add would have queued silently.
	if cs.BufferPending() != 0 {
		t.Fatalf("post-/use flush: queue should be empty, got %d — the bug would leave the message queued here", cs.BufferPending())
	}
}

// TestClearBuffer_NoBufferYetIsZero confirms ClearBuffer is a
// safe no-op on a brand-new ChatSession that hasn't seen a
// message yet — the lazy ensureBuffer() path must not panic.
func TestClearBuffer_NoBufferYetIsZero(t *testing.T) {
	cs := New("test_clearbuf_empty", "pi").WithSpawner(nil)
	// Don't trigger any QueueUserMessage — inputBuffer is nil.

	if n := cs.ClearBuffer(); n != 0 {
		t.Errorf("ClearBuffer (no buffer) = %d; want 0", n)
	}
}

// TestBufferState_NoBufferYetIsIdle mirrors ClearBuffer for the
// FSM state accessor — also lazy-safe.
func TestBufferState_NoBufferYetIsIdle(t *testing.T) {
	cs := New("test_bufstate_empty", "pi").WithSpawner(nil)

	if s := cs.BufferState(); s != StateIdle {
		t.Errorf("BufferState (no buffer) = %s; want StateIdle", s)
	}
}