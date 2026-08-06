// FSM state transitions on agent events — specifically the
// F-32 2026-08-06 incident follow-up:
//
//   - EventInit MUST NOT flip the buffer to Busy (it's session
//     metadata, not a turn in flight).
//   - All other non-terminal events (EventText, EventToolStart,
//     EventResult, etc.) MUST still flip Busy so concurrent
//     QueueUserMessage calls don't race a real turn.
//
// Regression target: before the fix, the readPump called
// SetBusy() in its default branch, which covered EventInit too.
// That meant a fast handshake race would mark the buffer Busy
// before the first user message reached QueueUserMessage — the
// message got queued, no prompt reached the agent, and the receipt
// sat at "Working..." forever.
//
// Synchronisation note: runReadPump processes each event in two
// phases — EventHandler call (h(...)) then SetBusy/SetIdle. The
// captureEventHandler `got` chan signals when h() returned, which
// is BEFORE the FSM transition. Tests must therefore poll
// BufferState() rather than rely on `got` to indicate the FSM
// is settled.
package chatsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// waitForBufferState polls BufferState() until it equals want,
// or the deadline elapses. Returns true on success. Used by the
// FSM tests because runReadPump runs h() then SetBusy/SetIdle in
// two separate steps — receiving the event on the handler-side
// `got` channel only proves h() returned, not that the FSM has
// settled. Without this poll, tests like TestEventDone_ResetsToIdle
// see the failure mode "pre-Done: state = idle; want Busy" when
// the SetBusy transition loses the race to the assertion.
func waitForBufferState(t *testing.T, cs *ChatSession, want SessionState, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cs.BufferState() == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// captureEventHandler swaps in a recording EventHandler for the
// duration of the test. It tees events into `got` so the test
// can synchronise against pump drain. The previous handler is
// restored on test cleanup.
func captureEventHandler(t *testing.T, cs *ChatSession, got chan<- agent.AgentEvent) {
	t.Helper()
	prev := cs.EventHandler()
	cs.SetEventHandler(func(chatID string, s *AgentSession, ev agent.AgentEvent, userMsgID string) {
		got <- ev
		if prev != nil {
			prev(chatID, s, ev, userMsgID)
		}
	})
	t.Cleanup(func() { cs.SetEventHandler(prev) })
}

// TestEventInit_DoesNotSetBusy is the F-32 race fix regression:
// the pump reading an EventInit event from the bridge must not
// flip the InputBuffer FSM to Busy, because EventInit is session
// bootstrap metadata (model + session_id), not an in-flight turn.
//
// Before the fix: state goes IDLE → BUSY on EventInit, racing
// the first user message into QueueUserMessage. The race made
// the user message get queued, the prompt never sent, the
// receipt stuck at "Working...".
func TestEventInit_DoesNotSetBusy(t *testing.T) {
	cs := New("test_eventinit", "pi").WithSpawner(nil)
	_, sess := newTestASWithFakeHandle(cs)

	got := make(chan agent.AgentEvent, 32)
	captureEventHandler(t, cs, got)
	if err := cs.StartReadPump(); err != nil {
		t.Fatalf("StartReadPump: %v", err)
	}
	t.Cleanup(func() { cs.StopReadPump() })

	sess.PushEvent(agent.AgentEvent{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{SessionID: "sess", Model: "test"},
	})

	select {
	case ev := <-got:
		if ev.Kind != agent.EventInit {
			t.Fatalf("expected EventInit, got %s", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("EventInit was not drained by runReadPump within 1s")
	}

	// The contract: state must still be IDLE. We poll briefly
	// because h() returning only proves the EventHandler ran —
	// the FSM transition (which we expect NOT to happen) needs a
	// moment to settle, in case some future refactor accidentally
	// moves SetBusy before SetIdle.
	if !waitForBufferState(t, cs, StateIdle, 100*time.Millisecond) {
		t.Fatalf("after EventInit, BufferState = %s; want StateIdle (EventInit must not flip Busy)", cs.BufferState())
	}
}

// TestEventText_SetsBusy is the regression guard for the OTHER
// direction: non-EventInit non-terminal events (EventText,
// EventToolStart, EventResult, …) must still flip the FSM to
// Busy so concurrent QueueUserMessage calls during a real turn
// queue instead of stamping the bridge with overlapping prompts.
//
// We deliberately keep this behaviour — only EventInit was the
// race; turning off SetBusy for other events would re-introduce
// turn-stomping.
func TestEventText_SetsBusy(t *testing.T) {
	cs := New("test_eventtext", "pi").WithSpawner(nil)
	_, sess := newTestASWithFakeHandle(cs)

	got := make(chan agent.AgentEvent, 32)
	captureEventHandler(t, cs, got)
	if err := cs.StartReadPump(); err != nil {
		t.Fatalf("StartReadPump: %v", err)
	}
	t.Cleanup(func() { cs.StopReadPump() })

	sess.PushEvent(agent.AgentEvent{
		Kind: agent.EventText,
		Text: "hello",
	})

	select {
	case ev := <-got:
		if ev.Kind != agent.EventText {
			t.Fatalf("expected EventText, got %s", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("EventText was not drained within 1s")
	}

	// h() returned when `got` received the event; poll for the
	// FSM transition. (CI failure mode without this poll:
	// "after EventText: state = idle; want Busy" — the assertion
	// ran before SetBusy fired.)
	if !waitForBufferState(t, cs, StateBusy, time.Second) {
		t.Fatalf("after EventText, BufferState = %s; want StateBusy (the F-32 fix only special-cases EventInit)", cs.BufferState())
	}
}

// TestEventResult_SetsBusy covers the assistant final-result
// event too — same family as EventText (non-terminal within the
// turn; EventDone is the only terminal).
func TestEventResult_SetsBusy(t *testing.T) {
	cs := New("test_eventresult", "pi").WithSpawner(nil)
	_, sess := newTestASWithFakeHandle(cs)

	got := make(chan agent.AgentEvent, 32)
	captureEventHandler(t, cs, got)
	if err := cs.StartReadPump(); err != nil {
		t.Fatalf("StartReadPump: %v", err)
	}
	t.Cleanup(func() { cs.StopReadPump() })

	sess.PushEvent(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "done"},
	})

	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("EventResult was not drained within 1s")
	}

	if !waitForBufferState(t, cs, StateBusy, time.Second) {
		t.Fatalf("after EventResult, BufferState = %s; want StateBusy", cs.BufferState())
	}
}

// TestEventDone_ResetsToIdle closes the loop: after a turn
// finishes, state must return to IDLE so the next user message
// flushes immediately.
func TestEventDone_ResetsToIdle(t *testing.T) {
	cs := New("test_eventdone", "pi").WithSpawner(nil)
	_, sess := newTestASWithFakeHandle(cs)

	got := make(chan agent.AgentEvent, 32)
	captureEventHandler(t, cs, got)
	if err := cs.StartReadPump(); err != nil {
		t.Fatalf("StartReadPump: %v", err)
	}
	t.Cleanup(func() { cs.StopReadPump() })

	// First, drive to Busy. Poll for the FSM transition (h()
	// returned when got received the event; SetBusy runs after).
	sess.PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "x"})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("EventText not drained")
	}
	if !waitForBufferState(t, cs, StateBusy, time.Second) {
		t.Fatalf("pre-Done: state = %s; want Busy (within 1s of EventText)", cs.BufferState())
	}

	// Now the terminal event — state must flip back to Idle.
	// Same pattern: h() runs first (signalled on `got`), then
	// SetIdle. Poll so we don't race the transition.
	sess.PushEvent(agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{Reason: "settled"},
	})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("EventDone not drained")
	}
	if !waitForBufferState(t, cs, StateIdle, time.Second) {
		t.Fatalf("post-Done: state = %s; want Idle (within 1s of EventDone)", cs.BufferState())
	}
}