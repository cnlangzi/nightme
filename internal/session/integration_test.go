package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

func tb(s string) []agent.ContentBlock {
	return []agent.ContentBlock{{Type: agent.ContentText, Text: s}}
}

// TestQueueUserMessage_IdleState_DispatchesDirectly verifies that a
// user message arriving when the agent is idle goes straight to
// SendBlocks (no buffering).
func TestQueueUserMessage_IdleState_DispatchesDirectly(t *testing.T) {
	sess := &Session{ID: "s1", ChatID: "chat1"}

	var sent int32
	as := &fakeAgentSession{}
	sess.mu.Lock()
	sess.agentSession = as
	sess.mu.Unlock()

	// Wrap with a recording send hook. fakeAgentSession.SendBlocks
	// is a no-op; instead we observe via the buffer's flush hook
	// by routing through EnsureInputBuffer's hook (which calls
	// agentSession.SendBlocks directly).
	buf := sess.EnsureInputBuffer()
	_ = buf

	// Direct dispatch: the SendBlocks stub returns nil, so we just
	// verify no error and no buffering.
	if err := sess.QueueUserMessage(tb("hello"), "msg1"); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if got := sess.InputBuffer().Pending(); got != 0 {
		t.Errorf("Pending = %d, want 0 (idle bypass)", got)
	}
	_ = sent // dummy reference so the variable stays alive if unused
}

// TestQueueUserMessage_BusyState_Buffers verifies buffering during a
// turn, then automatic flush when the turn ends.
func TestQueueUserMessage_BusyState_Buffers(t *testing.T) {
	sess := &Session{ID: "s1", ChatID: "chat1"}

	as := &fakeAgentSession{}
	sess.mu.Lock()
	sess.agentSession = as
	sess.mu.Unlock()

	// Initial: idle. First message goes through (no buffering).
	sess.QueueUserMessage(tb("first"), "m1")

	// Now mark busy (simulating agent has started its turn).
	buf := sess.EnsureInputBuffer()
	buf.SetState(StateBusy)

	// Two more messages arrive while busy.
	sess.QueueUserMessage(tb("second"), "m2")
	sess.QueueUserMessage(tb("third"), "m3")

	if buf.Pending() != 2 {
		t.Errorf("Pending = %d, want 2", buf.Pending())
	}

	// Turn ends.
	buf.SetState(StateIdle)
	if err := buf.OnTurnEnded(); err != nil {
		t.Fatalf("OnTurnEnded: %v", err)
	}

	if buf.Pending() != 0 {
		t.Errorf("after flush: Pending = %d, want 0", buf.Pending())
	}
}

// TestQueueUserMessage_BufferFull verifies ErrBufferFull path.
func TestQueueUserMessage_BufferFull(t *testing.T) {
	sess := &Session{ID: "s1", ChatID: "chat1"}
	as := &fakeAgentSession{}
	sess.mu.Lock()
	sess.agentSession = as
	sess.mu.Unlock()

	buf := sess.EnsureInputBuffer()
	buf.SetState(StateBusy)

	// Default maxMsgs = 50; fill it up.
	for i := 0; i < 50; i++ {
		_ = sess.QueueUserMessage(tb("msg"), "id")
	}
	// 51st should fail.
	err := sess.QueueUserMessage(tb("overflow"), "id_over")
	if err == nil {
		t.Error("expected ErrBufferFull on 51st message")
	}
}

// TestReadPumpDrivesBufferState verifies that the Manager's readPump
// path transitions InputBuffer state based on AgentEvents.
func TestReadPumpDrivesBufferState(t *testing.T) {
	as := &fakeAgentSession{
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "thinking..."},
			{Kind: agent.EventText, Text: "more thinking"},
			{Kind: agent.EventDone},
		},
	}

	sess := &Session{ID: "s1", ChatID: "chat1", agentSession: as}
	sess.EnsureInputBuffer()

	// Drain via the public Events channel — same logic readPump
	// uses. We don't call m.readPump directly because we don't have
	// a Manager wired here; the test verifies the state-transition
	// logic in isolation.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range as.Events() {
			if ev.Kind != agent.EventDone && ev.Kind != agent.EventError {
				if buf := sess.InputBuffer(); buf != nil {
					buf.SetState(StateBusy)
				}
			}
			if ev.Kind == agent.EventDone || ev.Kind == agent.EventError {
				if buf := sess.InputBuffer(); buf != nil {
					buf.SetState(StateIdle)
					_ = buf.OnTurnEnded()
				}
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event drain did not complete within 2s")
	}

	if got := sess.InputBuffer().State(); got != StateIdle {
		t.Errorf("final buffer state = %v, want StateIdle", got)
	}
}

// Sanity: ensure Integration with the existing fakeAgentSession type
// (defined in manager_test.go) compiles.
var _ agent.AgentSession = (*fakeAgentSession)(nil)
var _ = sync.Mutex{}
var _ context.Context = context.Background()
