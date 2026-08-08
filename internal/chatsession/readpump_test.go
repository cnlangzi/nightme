// Package chatsession — readpump_test.go (CS-AS 边界重构 Phase 1).
//
// The old tests in this file asserted behavior of the per-CS
// readpump (cs.StartReadPump / cs.HasPump / cs.runReadPump). That
// machinery was deleted in T13 of the Phase 1 plan; the new model
// moves the readpump to live inside each AgentSession (started by
// Spawn), and the runtime consumes events via cs.PumpEvents(ctx).
//
// These tests cover the new pipeline:
//
//   - AS internal readpump starts on Spawn
//   - CS.PumpEvents routes KindAgentEvent / KindPromptEnded /
//     KindLifecycle
//   - TryFlush at-least-once submission semantics
//   - writebackMessageState on KindPromptEnded
//   - AS.Shutdown cleanly drains the readpump
//
// The fakeSpawner test helper is reused (now in spawn_test.go
// conventionally); the tests below reach for it via the standard
// test helpers from this package.
package chatsession

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestAgentSession_ReadPump_DeliversEvents verifies that an
// AgentSession's readpump (started by Spawn) consumes bridge
// events and re-emits them as EnrichedEvent on the eventQueue.
func TestAgentSession_ReadPump_DeliversEvents(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	// Push a synthetic event into the fake bridge.
	fake.PushEvent(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "hello",
	})

	// Read from AS eventQueue; expect KindAgentEvent wrapping the
	// bridge event.
	select {
	case ev := <-as.Events():
		if ev.Kind != KindAgentEvent {
			t.Fatalf("kind = %v, want KindAgentEvent", ev.Kind)
		}
		if ev.AgentEvent == nil || ev.AgentEvent.Text != "hello" {
			t.Fatalf("AgentEvent = %+v, want Text=hello", ev.AgentEvent)
		}
		if ev.AgentSessionID != as.ID {
			t.Fatalf("AgentSessionID = %q, want %q", ev.AgentSessionID, as.ID)
		}
		if ev.ChatID != cs.ID {
			t.Fatalf("ChatID = %q, want %q", ev.ChatID, cs.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for enriched event")
	}
}

// TestAgentSession_ReadPump_Propagates_NotReadyBeforeSubmit covers
// the IsReady() semantics: a freshly spawned AS is ready (no
// currentPrompt installed); after Submit, it flips to not-ready
// until endPrompt restores it.
func TestAgentSession_ReadPump_Propagates_NotReadyBeforeSubmit(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, _ := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	if !as.IsReady() {
		t.Fatal("AS should be ready before any Submit")
	}

	p := &Prompt{Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}}}
	if err := as.Submit(p); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if as.IsReady() {
		t.Fatal("AS should be NOT ready after Submit")
	}

	// End the prompt to restore readiness.
	as.endPrompt(PromptEndClean)
	if !as.IsReady() {
		t.Fatal("AS should be ready after endPrompt")
	}
}

// TestChatSession_PumpEvents_RoutesKindAgentEvent verifies that
// cs.PumpEvents delivers KindAgentEvent to the installed
// EventHandler.
func TestChatSession_PumpEvents_RoutesKindAgentEvent(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	// Promote to active.
	cs.mu.Lock()
	cs.activeAS = as
	cs.mu.Unlock()

	// Install event handler that captures the first event.
	var captured agent.AgentEvent
	received := make(chan struct{})
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		captured = *env.Event
		close(received)
		return false
	})

	// Start pump.
	go cs.PumpEvents(ctx)

	// Push a synthetic event.
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "pumped"})

	select {
	case <-received:
		if captured.Text != "pumped" {
			t.Errorf("captured.Text = %q, want %q", captured.Text, "pumped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PumpEvents did not deliver event within 2s")
	}

	cancel()
	// Drain readpump to avoid leaking goroutines.
	as.Shutdown()
}

// TestChatSession_PumpEvents_RoutesKindPromptEnded verifies that
// KindPromptEnded triggers writebackMessageState on the CS side
// and fires the PromptEndBus event with the right userMsgID and
// reason. Post-refactor (Message is immutable) the "the prompt
// ended" signal is the bus event, not a field mutation.
func TestChatSession_PumpEvents_RoutesKindPromptEnded(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	cs.mu.Lock()
	cs.activeAS = as
	cs.mu.Unlock()

	// Observe the terminal prompt-end event. writebackMessageState
	// fires this on KindPromptEnded.
	var (
		mu             sync.Mutex
		gotEvent       bool
		gotUserMsgID   string
		gotReason      PromptEndReason
	)
	cs.PromptEndBus.Subscribe(func(e PromptEndedEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		gotEvent = true
		gotUserMsgID = e.UserMsgID
		gotReason = e.Reason
		return false
	})

	// Build a Message value and a Prompt carrying it inline.
	// Submit directly via the AS so we observe writeback without
	// going through TryFlush (the test's intent is to exercise
	// the KindPromptEnded → writebackMessageState path).
	msg := Message{
		ID:     "msg-1",
		ChatID: cs.ID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	}
	p := &Prompt{
		ID:            "p-1",
		Messages:      []Message{msg},
		Blocks:        msg.Blocks,
		LastMessageID: msg.ID,
	}
	if err := as.Submit(p); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Start CS pump so KindPromptEnded → routeEvent → writebackMessageState runs.
	// Note: do NOT put the message in cs.queue — TryFlush from the
	// routeEvent would re-Submit on the now-Ready AS, undoing the
	// IsReady flip we want to observe.
	go cs.PumpEvents(ctx)

	// Push terminal event.
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}})

	// Wait for AS to end the prompt (IsReady flips back).
	if !waitForReadiness(t, as, true, 2*time.Second) {
		t.Fatal("AS never became ready after EventAgentDone")
	}

	if !waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotEvent
	}, 2*time.Second) {
		t.Fatal("PromptEndBus saw no event for KindPromptEnded")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotUserMsgID != msg.ID {
		t.Errorf("event UserMsgID = %q, want %q", gotUserMsgID, msg.ID)
	}
	if gotReason != PromptEndClean {
		t.Errorf("event Reason = %v, want %v", gotReason, PromptEndClean)
	}

	// Stage stays at MessageQueued: this test bypasses TryFlush
	// (calls Submit directly) so Stage transitions are not exercised.
	// Stage=Submitted is verified separately by TestChatSession_TryFlush_AtLeastOnce.

	cancel()
	as.Shutdown()
}

// TestChatSession_TryFlush_AtLeastOnce covers the queue semantics:
// a successful Submit commits the prompt and clears the queue.
// (The "failed Submit keeps the message in queue" path is enforced
// by AS.Submit returning an error and CS.TryFlush not modifying
// the queue — covered by the implementation contract; the
// test for retry-after-failure requires a programmable SendBlocks
// which the existing fakeSession doesn't expose.)
func TestChatSession_TryFlush_AtLeastOnce(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, _ := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	cs.mu.Lock()
	cs.activeAS = as
	cs.mu.Unlock()

	// Subscribe to MessageStateBus to observe the post-Submit
	// state transition. Post-refactor there is no per-chat
	// message index to read Stage from directly; the wire
	// event is the canonical signal.
	var capturedState agent.MessageState
	var capturedID string
	var mu sync.Mutex
	cs.MessageStateBus.Subscribe(func(e MessageStateEvent) bool {
		mu.Lock()
		capturedState = e.State
		capturedID = e.UserMsgID
		mu.Unlock()
		return false
	})

	msg := Message{
		ID:     "msg-1",
		ChatID: cs.ID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	}
	if err := cs.queue.Push(msg); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Successful TryFlush.
	if err := cs.TryFlush(); err != nil {
		t.Fatalf("TryFlush: %v", err)
	}
	if got := cs.queue.Len(); got != 0 {
		t.Errorf("queue length after successful flush = %d, want 0", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedID != msg.ID {
		t.Errorf("bus event ID = %q, want %q", capturedID, msg.ID)
	}
	if capturedState != agent.MessageSubmitted {
		t.Errorf("bus event state = %v, want MessageSubmitted", capturedState)
	}
}

// TestAgentSession_Shutdown_ClosesReadPump verifies that AS.Shutdown
// cleanly drains the readpump goroutine and closes eventQueue.
func TestAgentSession_Shutdown_ClosesReadPump(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)

	// Push an event first so the readpump is in the event loop.
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "before-shutdown"})

	// Drain that event so the queue isn't blocked at shutdown.
	select {
	case <-as.Events():
	case <-time.After(time.Second):
		t.Fatal("readpump not delivering events")
	}

	// Shutdown should close the queue and exit readpump.
	as.Shutdown()

	// Drain remaining events; expect channel closed.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-as.Events():
			if !ok {
				return // channel closed; readpump drained
			}
		case <-deadline:
			t.Fatal("eventQueue not closed after Shutdown")
		}
	}
}

// TestChatSession_PromoteActive_NoBackgroundOpCtx covers the
// Phase 1 /use semantics: switching active AS does NOT cancel
// the previous AS's opCtx. The previous AS keeps running.
func TestChatSession_PromoteActive_NoBackgroundOpCtx(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldAS, _ := makeSpawnedAS(t, cs, "pi", ctx)
	oldAS.Activate(cs.Context())
	oldDone := oldAS.OpContext().Done()

	// Register as active.
	cs.mu.Lock()
	cs.activeAS = oldAS
	cs.mu.Unlock()

	// Spawn a new AS, activate it, promote.
	newAS, _ := makeSpawnedAS(t, cs, "pi", ctx)
	defer newAS.Shutdown()
	cs.mu.Lock()
	cs.promoteActiveLocked(newAS)
	cs.mu.Unlock()

	// Old AS's opCtx must still be alive.
	select {
	case <-oldDone:
		t.Fatal("old AS's opCtx was cancelled on /use — should still be alive")
	case <-time.After(50 * time.Millisecond):
		// OK
	}
}

// TestAgentSession_EndPrompt_EmitsKindPromptEnded verifies that
// endPrompt emits a KindPromptEnded event with the prompt
// reference populated.
func TestAgentSession_EndPrompt_EmitsKindPromptEnded(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, _ := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	p := &Prompt{ID: "p-1", LastMessageID: "m-1"}
	as.asMu.Lock()
	as.currentPrompt = p
	as.asMu.Unlock()

	as.endPrompt(PromptEndClean)

	select {
	case ev := <-as.Events():
		if ev.Kind != KindPromptEnded {
			t.Fatalf("kind = %v, want KindPromptEnded", ev.Kind)
		}
		if ev.Prompt != p {
			t.Errorf("prompt pointer mismatch")
		}
		if ev.PromptEnded == nil || ev.PromptEnded.EndReason != PromptEndClean {
			t.Errorf("PromptEnded = %+v, want Clean", ev.PromptEnded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no KindPromptEnded event after endPrompt")
	}
}

// --- test helpers ---

// newChatSessionForTest is a thin wrapper that returns a ChatSession
// with the test-friendly defaults (no Spawner, no persistence).
func newChatSessionForTest(chatID string) *ChatSession {
	return New(chatID, "pi")
}

// makeSpawnedAS creates a new AgentSession in the chat pool, spawns
// it via the fakeSpawner, and returns (as, fakeHandle). The fake
// handle's PushEvent method drives the event channel.
func makeSpawnedAS(t *testing.T, cs *ChatSession, agentName string, parent context.Context) (*AgentSession, *fakeAgentSession) {
	t.Helper()
	as := NewAgentSession(newAgentSessionID(), cs.ID, agentName, "/tmp", nil)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: agentName, Cwd: "/tmp"}] = as
	cs.mu.Unlock()

	as.Activate(parent)

	fake := newFakeAgentSession(99000)
	as.asMu.Lock()
	as.handle = fake
	as.pid = fake.PID()
	as.stat = StatusRunning
	as.asMu.Unlock()
	as.startReadPump()

	return as, fake
}

// waitForReadiness polls as.IsReady until it equals target or
// timeout. Returns true if reached.
func waitForReadiness(t *testing.T, as *AgentSession, target bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if as.IsReady() == target {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitFor polls cond until it returns true or timeout.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestAgentSession_ReadPump_StableEventPointers is a regression
// test for the &ev race that readpumpLoop had: the readpump
// pushed `&ev` to eventQueue, and on the next iteration Go's
// range loop shadowed ev with a fresh value. If the runtime
// was slow to process the previous event, dereferencing the
// pointer could see the new event's bytes. The fix copies
// ev to the heap before taking its address.
//
// This test pushes a flood of events, then reads them ALL out
// of eventQueue, and verifies each one has its original payload
// (not the payload of any other event that came through the
// readpump's stack frame).
func TestAgentSession_ReadPump_StableEventPointers(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	as, fake := makeSpawnedAS(t, cs, "pi", ctx)
	defer as.Shutdown()

	const N = 50
	for i := 0; i < N; i++ {
		fake.PushEvent(agent.AgentEvent{
			Kind: agent.EventAgentText,
			Text: fmt.Sprintf("event-%d", i),
		})
	}

	// Drain all events and verify each one.
	for i := 0; i < N; i++ {
		select {
		case ev := <-as.Events():
			if ev.Kind != KindAgentEvent {
				t.Fatalf("event %d: kind = %v, want KindAgentEvent", i, ev.Kind)
			}
			if ev.AgentEvent == nil {
				t.Fatalf("event %d: AgentEvent is nil", i)
			}
			want := fmt.Sprintf("event-%d", i)
			if ev.AgentEvent.Text != want {
				t.Fatalf("event %d: Text = %q, want %q "+
					"(pointer-corruption regression: readpump's "+
					"stack-allocated ev was overwritten by the next "+
					"iteration before the runtime dereferenced it)",
					i, ev.AgentEvent.Text, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out reading event %d", i)
		}
	}
}

// silence unused warning for helpers that may be conditionally used
var _ = strings.Contains
