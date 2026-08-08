// Readpump event → Prompt-lifecycle transitions (CS-AS 边界重构
// Phase 1 port of the old InputBuffer FSM tests).
//
// The original version of this file asserted on the per-CS
// InputBuffer FSM (cs.BufferState / SetBusy / StateIdle /
// StateBusy). That FSM was deleted in Phase 1: "busy" is no longer
// a buffer state driven by inbound events, it is the presence of an
// in-flight Prompt on the AgentSession, observable via
// as.IsReady(). Submit installs the Prompt (not-ready); endPrompt
// clears it (ready).
//
// The F-32 2026-08-06 contract survives the refactor, restated in
// the new vocabulary:
//
//   - EventAgentConnected MUST NOT end the in-flight Prompt — it is session
//     bootstrap metadata (model + session_id), not a turn boundary.
//     In the old model the symptom was "EventAgentConnected flips the buffer
//     Busy before the first user message is queued"; in the new
//     model the mirror-image hazard is "EventAgentConnected ends the Prompt
//     early", which would make the AS accept a second prompt while
//     the first turn is still streaming.
//   - Other non-terminal events (EventText, EventResult, …) MUST
//     also leave the Prompt in flight.
//   - EventDone MUST end the Prompt, restoring readiness so the
//     next queued message flushes.
//
// Synchronisation note: the readpump processes events
// asynchronously, so every assertion polls via waitReady rather
// than reading IsReady() once immediately after PushEvent.
package chatsession

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// waitReady polls as.IsReady() until it equals want or the deadline
// elapses. Returns true on success.
//
// Needed because PushEvent only hands the event to the fake bridge;
// the readpump goroutine consumes it and calls endPrompt some time
// later. Asserting immediately would race the transition.
func waitReady(as *AgentSession, want bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if as.IsReady() == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// submitInFlight puts a Prompt in flight on as and asserts the AS
// is consequently not-ready. Returns the Prompt.
func submitInFlight(t *testing.T, as *AgentSession) *Prompt {
	t.Helper()
	p := &Prompt{Blocks: []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hi"},
	}}
	if err := as.Submit(p); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if as.IsReady() {
		t.Fatal("AS should be NOT ready immediately after Submit")
	}
	return p
}

// TestEventInit_DoesNotEndPrompt is the F-32 regression, ported.
// EventAgentConnected arriving mid-turn must leave the Prompt in flight —
// if it ended the Prompt, TryFlush would submit the next queued
// message on top of a turn that is still streaming.
func TestEventInit_DoesNotEndPrompt(t *testing.T) {
	cs := newChatSessionForTest("cs_eventinit")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	submitInFlight(t, as)

	fake.PushEvent(agent.AgentEvent{
		Kind: agent.EventAgentConnected,
		Connected: &agent.AgentConnectedEvent{SessionID: "sess", Model: "test"},
	})

	// Drain the enriched event so we know the readpump actually
	// processed it — otherwise "still not ready" could just mean
	// "the pump hasn't looked at it yet", and the test would pass
	// for the wrong reason.
	select {
	case ev := <-as.Events():
		if ev.Kind != KindAgentEvent || ev.AgentEvent == nil ||
			ev.AgentEvent.Kind != agent.EventAgentConnected {
			t.Fatalf("expected KindAgentEvent{EventAgentConnected}, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventAgentConnected was not drained by the readpump within 2s")
	}

	if !waitReady(as, false, 100*time.Millisecond) {
		t.Fatal("after EventAgentConnected the AS became ready; EventAgentConnected must not end the in-flight Prompt")
	}
}

// TestEventText_DoesNotEndPrompt covers the streaming case: text
// chunks arrive throughout a turn and must not be mistaken for a
// turn boundary.
func TestEventText_DoesNotEndPrompt(t *testing.T) {
	cs := newChatSessionForTest("cs_eventtext")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	submitInFlight(t, as)

	fake.PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hello"})

	select {
	case <-as.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("EventText was not drained within 2s")
	}

	if !waitReady(as, false, 100*time.Millisecond) {
		t.Fatal("after EventText the AS became ready; only terminal events end a Prompt")
	}
}

// TestEventResult_DoesNotEndPrompt covers the assistant
// final-result event — same family as EventText. EventDone, not
// EventResult, is the terminal event.
func TestEventResult_DoesNotEndPrompt(t *testing.T) {
	cs := newChatSessionForTest("cs_eventresult")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	submitInFlight(t, as)

	fake.PushEvent(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "done"},
	})

	select {
	case <-as.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("EventResult was not drained within 2s")
	}

	if !waitReady(as, false, 100*time.Millisecond) {
		t.Fatal("after EventResult the AS became ready; EventDone is the terminal event")
	}
}

// TestEventDone_EndsPrompt closes the loop: the terminal event ends
// the Prompt, restoring readiness so the next queued message
// flushes. The readpump also pushes KindPromptEnded — we assert on
// readiness rather than the event so the test does not depend on
// the relative ordering of the two queue pushes.
func TestEventDone_EndsPrompt(t *testing.T) {
	cs := newChatSessionForTest("cs_eventdone")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	submitInFlight(t, as)

	// A non-terminal event first, to prove the AS is genuinely
	// mid-turn rather than never having gone busy.
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "x"})
	if !waitReady(as, false, time.Second) {
		t.Fatal("pre-Done: AS should still be mid-turn")
	}

	fake.PushEvent(agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{Reason: "settled"},
	})

	if !waitReady(as, true, 2*time.Second) {
		t.Fatal("post-Done: AS never became ready; EventDone must end the in-flight Prompt")
	}
}
