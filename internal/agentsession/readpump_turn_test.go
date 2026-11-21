// Package agentsession — the readpump's turn-ownership decision.
//
// An event that arrives while no prompt is in flight belongs to no
// turn: its anchor is empty, and every consumer of the anchor reads
// an empty one as "not mine" — the receipt's ticker freezes, the
// turn's text lands in a card of its own, and the readpump records
// an orphan prompt end.
//
// So the readpump drops the turn-scoped events and ends no prompt.
// The session identity, the bridge failure and a pending permission
// question keep flowing: they carry information no turn owns.
package agentsession

import (
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// spawnFakeAS spawns an AgentSession on a fake bridge and returns
// the session and the bridge.
func spawnFakeAS(t *testing.T) (*AgentSession, *fakeAgentSession) {
	t.Helper()
	spawner := newFakeSpawner()
	as := makeBareAgentSession(t, "claudecode", "/tmp")
	if err := as.Spawn(t.Context(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { as.Shutdown() })
	return as, spawner.Get("claudecode", "/tmp")
}

// recordedBus collects the events the readpump publishes, so a test
// can assert both what arrived and what did not.
type recordedBus struct {
	mu     sync.Mutex
	events []EnrichedEvent
	notify chan struct{}
}

func recordBus(as *AgentSession) *recordedBus {
	rb := &recordedBus{notify: make(chan struct{}, 64)}
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		rb.mu.Lock()
		rb.events = append(rb.events, ev)
		rb.mu.Unlock()
		select {
		case rb.notify <- struct{}{}:
		default:
		}
		return false
	})
	return rb
}

// agentEvents returns the agent events published so far.
func (rb *recordedBus) agentEvents() []EnrichedEvent {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	var out []EnrichedEvent
	for _, ev := range rb.events {
		if ev.Kind == KindAgentEvent && ev.AgentEvent != nil {
			out = append(out, ev)
		}
	}
	return out
}

// promptEnds returns the prompt-end events published so far.
func (rb *recordedBus) promptEnds() []EnrichedEvent {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	var out []EnrichedEvent
	for _, ev := range rb.events {
		if ev.Kind == KindPromptEnded {
			out = append(out, ev)
		}
	}
	return out
}

// lifecycle returns the lifecycle events published so far.
func (rb *recordedBus) lifecycle() []EnrichedEvent {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	var out []EnrichedEvent
	for _, ev := range rb.events {
		if ev.Kind == KindLifecycle {
			out = append(out, ev)
		}
	}
	return out
}

// waitForAgentKind blocks until an agent event of the given kind was
// published.
func (rb *recordedBus) waitForAgentKind(t *testing.T, kind agent.EventKind, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, ev := range rb.agentEvents() {
			if ev.AgentEvent.Kind == kind {
				return
			}
		}
		select {
		case <-rb.notify:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("the %v event never reached the bus", kind)
}

// waitForPromptEnd blocks until a prompt-end was published.
func (rb *recordedBus) waitForPromptEnd(t *testing.T, timeout time.Duration) EnrichedEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ends := rb.promptEnds(); len(ends) > 0 {
			return ends[0]
		}
		select {
		case <-rb.notify:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("no prompt-end reached the bus")
	return EnrichedEvent{}
}

// TestReadPump_DropsTurnScopedEventsWithNoPromptInFlight pins the
// drop: a turn-scoped event that arrives with no prompt in flight is
// not published.
//
// The marker behind each dropped event proves the readpump went
// through it: the readpump drains the bridge's channel in order, so
// a marker that arrives means the event before it was decided.
func TestReadPump_DropsTurnScopedEventsWithNoPromptInFlight(t *testing.T) {
	kinds := []agent.EventKind{
		agent.EventAgentText,
		agent.EventAgentThinking,
		agent.EventAgentToolStart,
		agent.EventAgentToolEnd,
		agent.EventAgentResult,
		agent.EventAgentTaskCreate,
		agent.EventAgentTaskUpdate,
		agent.EventAgentPermissionSettled,
		agent.EventAgentDone,
	}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) {
			as, bridge := spawnFakeAS(t)
			rb := recordBus(as)

			bridge.PushEvent(agent.AgentEvent{Kind: kind})
			// The marker keeps flowing with no prompt in flight.
			bridge.PushEvent(agent.AgentEvent{Kind: agent.EventAgentReady, SessionID: "sess-1"})

			rb.waitForAgentKind(t, agent.EventAgentReady, 3*time.Second)
			for _, ev := range rb.agentEvents() {
				if ev.AgentEvent.Kind == kind {
					t.Fatalf("the %v event was published with no prompt in flight", kind)
				}
			}
		})
	}
}

// TestReadPump_KeepsTheEventsThatNoTurnOwns pins the survivors: the
// session identity, the bridge failure and a pending permission
// question carry information no turn owns, so they keep flowing with
// no prompt in flight.
func TestReadPump_KeepsTheEventsThatNoTurnOwns(t *testing.T) {
	kinds := []agent.EventKind{
		agent.EventAgentReady,
		agent.EventAgentError,
		agent.EventAgentPermission,
	}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) {
			as, bridge := spawnFakeAS(t)
			rb := recordBus(as)

			bridge.PushEvent(agent.AgentEvent{Kind: kind})

			rb.waitForAgentKind(t, kind, 3*time.Second)
			for _, ev := range rb.agentEvents() {
				if ev.AgentEvent.Kind != kind {
					continue
				}
				if ev.UserMsgID != "" {
					t.Errorf("UserMsgID = %q, want empty (no turn owns the event)", ev.UserMsgID)
				}
			}
		})
	}
}

// TestReadPump_TerminalEventWithNoPromptInFlight_EndsNoPrompt pins
// the other half of the drop: a terminal event that arrives with no
// prompt in flight ends no prompt. There is nothing to end, and
// endPrompt would fabricate a `<as>-end` prompt record and publish a
// prompt-end that no message is waiting for.
func TestReadPump_TerminalEventWithNoPromptInFlight_EndsNoPrompt(t *testing.T) {
	kinds := []agent.EventKind{agent.EventAgentDone, agent.EventAgentError}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) {
			as, bridge := spawnFakeAS(t)
			rb := recordBus(as)

			bridge.PushEvent(agent.AgentEvent{Kind: kind})
			// The marker proves the readpump went through the
			// terminal event before the check.
			bridge.PushEvent(agent.AgentEvent{Kind: agent.EventAgentReady, SessionID: "sess-1"})

			rb.waitForAgentKind(t, agent.EventAgentReady, 3*time.Second)
			for _, ev := range rb.promptEnds() {
				t.Fatalf("the %v event ended a prompt that was not in flight: %+v", kind, ev)
			}
		})
	}
}

// TestReadPump_EventWithPromptInFlight_KeepsItsAnchor pins the
// unchanged half: an event that arrives while a prompt is in flight
// is published with that prompt's anchor.
func TestReadPump_EventWithPromptInFlight_KeepsItsAnchor(t *testing.T) {
	as, bridge := spawnFakeAS(t)
	rb := recordBus(as)

	as.SetCurrentPrompt(&Prompt{ID: "p-1", LastMessageID: "m-1"})
	bridge.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"})

	rb.waitForAgentKind(t, agent.EventAgentText, 3*time.Second)
	ev := rb.agentEvents()[0]
	if ev.UserMsgID != "m-1" {
		t.Errorf("UserMsgID = %q, want %q", ev.UserMsgID, "m-1")
	}
	if ev.PromptID != "p-1" {
		t.Errorf("PromptID = %q, want %q", ev.PromptID, "p-1")
	}
}

// TestReadPump_ReplyToOverride_AnchorsTheEventWithoutAPrompt pins
// the /review injection path: the override supplies the anchor, so
// the events belong to a turn even with no prompt installed.
func TestReadPump_ReplyToOverride_AnchorsTheEventWithoutAPrompt(t *testing.T) {
	as, bridge := spawnFakeAS(t)
	rb := recordBus(as)

	as.asMu.Lock()
	as.currentPromptOverrideUserMsgID = "om_review"
	as.asMu.Unlock()

	bridge.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "review findings"})

	rb.waitForAgentKind(t, agent.EventAgentText, 3*time.Second)
	ev := rb.agentEvents()[0]
	if ev.UserMsgID != "om_review" {
		t.Errorf("UserMsgID = %q, want %q", ev.UserMsgID, "om_review")
	}
}

// TestReadPump_ChannelClose_EndsThePromptAndMarksExited pins the
// lifecycle handling that must not change: when the bridge's event
// channel closes, the in-flight prompt ends with ProcessDied and the
// session is marked exited.
func TestReadPump_ChannelClose_EndsThePromptAndMarksExited(t *testing.T) {
	as, bridge := spawnFakeAS(t)
	rb := recordBus(as)

	as.SetCurrentPrompt(&Prompt{ID: "p-1", LastMessageID: "m-1"})
	bridge.Close()

	end := rb.waitForPromptEnd(t, 3*time.Second)
	if end.PromptEnded == nil || end.PromptEnded.EndReason != agent.PromptEndProcessDied {
		t.Fatalf("prompt-end = %+v, want the process-died reason", end.PromptEnded)
	}
	if end.UserMsgID != "m-1" {
		t.Errorf("UserMsgID = %q, want %q", end.UserMsgID, "m-1")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range rb.lifecycle() {
			if ev.Lifecycle != nil && ev.Lifecycle.Status == StatusExited {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the session was never marked exited")
}
