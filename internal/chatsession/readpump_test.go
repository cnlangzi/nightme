package chatsession

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// trackingEventHandler records every event it sees; thread-safe.
type trackingEventHandler struct {
	mu     sync.Mutex
	calls  int
	lastChatID string
	lastKind string
}

func (h *trackingEventHandler) handler(chatID string, _ *AgentSession, ev agent.AgentEvent, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.lastChatID = chatID
	h.lastKind = ev.Kind.String()
}

func (h *trackingEventHandler) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestChatSession_ReadPump_StartStopRequiresSpawn(t *testing.T) {
	cs := New("oc_xxx", "claude")
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	// No active AS yet → StartReadPump returns ErrNoActiveAgentSession.
	if err := cs.StartReadPump(); err == nil {
		t.Fatalf("expected error before spawn")
	}
	if cs.HasPump() {
		t.Fatalf("pump should not be running without spawn")
	}
}

func TestChatSession_ReadPump_StartAfterSpawn(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	as, _ := cs.LookupActiveAgentSession()

	// Manually start pump (commit 8c: LookupActiveAgentSession does
	// NOT auto-start; the runtime does). Set handler BEFORE pump
	// start (StartReadPump captures h from cs.eventHandler).
	handler := &trackingEventHandler{}
	cs.SetEventHandler(handler.handler)

	if err := cs.StartReadPump(); err != nil {
		t.Fatalf("StartReadPump after spawn: %v", err)
	}
	if !cs.HasPump() {
		t.Fatalf("pump should be running after start")
	}

	// Pump an event; expect handler to receive it.
	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hi"})

	// Give the pump a moment to drain.
	if !waitFor(func() bool { return handler.Calls() > 0 }, 2*time.Second) {
		t.Fatalf("handler not called within 2s; calls=%d", handler.Calls())
	}

	cs.StopReadPump()
	if cs.HasPump() {
		t.Fatalf("pump should not be running after stop")
	}

	_ = as
}

func TestChatSession_ReadPump_DriveFSM_BusyOnFirstEvent(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()

	if cs.BufferState() != StateIdle {
		t.Fatalf("initial BufferState should be StateIdle, got %s", cs.BufferState())
	}

	// Push a non-terminal event → pump drives SetBusy.
	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hi"})

	if !waitFor(func() bool { return cs.BufferState() == StateBusy }, 2*time.Second) {
		t.Fatalf("BufferState should transition to StateBusy; got %s", cs.BufferState())
	}

	cs.StopReadPump()
}

func TestChatSession_ReadPump_DriveFSM_IdleOnDone(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()

	// Push non-terminal + terminal → FSM should drive back to Idle.
	fake := spawner.Get("claude", "/x")
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hi"})
	if !waitFor(func() bool { return cs.BufferState() == StateBusy }, 2*time.Second) {
		t.Fatalf("should be Busy after non-terminal event")
	}

	// Finish (closes channel after sending EventDone).
	fake.FinishEvent()

	// Wait for pump to exit (channel closed) AND FSM to be Idle.
	if !waitFor(func() bool { return cs.BufferState() == StateIdle && !cs.HasPump() }, 2*time.Second) {
		t.Fatalf("expected StateIdle + no pump after FinishEvent; got state=%s, pump=%v",
			cs.BufferState(), cs.HasPump())
	}

	cs.StopReadPump() // idempotent
}

func TestChatSession_ReadPump_StopIsIdempotent(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()

	// Stop multiple times — should not panic.
	cs.StopReadPump()
	cs.StopReadPump()
	cs.StopReadPump()

	if cs.HasPump() {
		t.Fatalf("pump should not be running")
	}
}

func TestChatSession_KillAll_StopsReadPump(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()

	if !cs.HasPump() {
		t.Fatalf("pump should be running pre-kill")
	}

	if err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	if cs.HasPump() {
		t.Fatalf("pump should be stopped after KillAll")
	}
}

func TestChatSession_KillAll_ClearsBuffer(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()
	cs.SetBusy()
	_ = cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "lost"}}, "m1")

	// Pre-condition: 1 queued, busy.
	if cs.BufferPending() != 1 {
		t.Fatalf("precondition: expected 1 queued, got %d", cs.BufferPending())
	}

	cs.KillAll()

	// After kill: buffer should be cleared (queued messages lost).
	if cs.BufferPending() != 0 {
		t.Fatalf("expected buffer cleared after KillAll; got %d pending", cs.BufferPending())
	}
}

func TestChatSession_SetEventHandler_PersistsAcrossUse(t *testing.T) {
	// commit 8c invariant: the EventHandler is installed ONCE per
	// ChatSession and persists across /use (no per-/use rebind
	// needed). /use only restarts the pump.
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	handler := &trackingEventHandler{}
	cs.SetEventHandler(handler.handler)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()

	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "first"})
	if !waitFor(func() bool { return handler.Calls() == 1 }, 2*time.Second) {
		t.Fatalf("first event not received")
	}

	// /use again (same agent, same cwd — reuse via Q-B fallback).
	// Same handler should still be installed and called.
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()
	_ = cs.StartReadPump()

	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "second"})
	if !waitFor(func() bool { return handler.Calls() == 2 }, 2*time.Second) {
		t.Fatalf("second event not received; calls=%d", handler.Calls())
	}

	handler.mu.Lock()
	lastChat := handler.lastChatID
	handler.mu.Unlock()
	if lastChat != "oc_xxx" {
		t.Errorf("handler should receive correct chatID; got %q", lastChat)
	}

	cs.StopReadPump()
}

func TestChatSession_SetAgentExitObserver(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	_, _ = cs.LookupActiveAgentSession()

	var observed *AgentSession
	var mu sync.Mutex
	cs.SetAgentExitObserver(func(s *AgentSession) {
		mu.Lock()
		observed = s
		mu.Unlock()
	})

	as, _ := cs.LookupActiveAgentSession()
	cs.StartObserveClose(as)

	spawner.Get("claude", "/x").FinishEvent()

	if !waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return observed != nil
	}, 2*time.Second) {
		t.Fatalf("exit observer not fired within 2s")
	}

	if as.Status() != StatusExited {
		t.Errorf("AgentSession should be StatusExited after FinishEvent")
	}
}

// waitFor polls until cond() is true or timeout.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// Pin imports used by tests in case of dead-code elimination.
var (
	_ = filepath.Join
	_ = context.Background
)