// Package chatsession — MessageState emission tests + shared
// helpers (F-54).
//
// F-54 replaced SetMessageStateHandler with cs.MessageStateBus.
// Subscribe(...). Tests below mirror the new pattern: a
// captureHandler struct subscribes to the bus, the test asserts on
// the captured calls.
//
// This file also hosts the legacy `spySpawner` / `newActiveAgentNoop`
// helpers previously used by message_state_test.go tests; they were
// never moved out when the file was renamed. They're still used by
// chatsession_test.go / readpump_test.go / restore_respawn_test.go
// (see those files' `WithSpawner(&spySpawner{})` calls) so they
// remain here as shared helpers for the package's test files.
package chatsession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// captureHandler records every callback invocation for assertions.
// Subscribes to cs.MessageStateBus with the typed
// `func(MessageStateEvent) bool` signature.
type captureHandler struct {
	mu    sync.Mutex
	calls []messageStateCall
}

type messageStateCall struct {
	chatID, userMsgID string
	state             agent.MessageState
	at                time.Time
}

func (c *captureHandler) handler(e MessageStateEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, messageStateCall{
		chatID:    e.ChatID,
		userMsgID: e.UserMsgID,
		state:     e.State,
		at:        e.At,
	})
	return false
}

func (c *captureHandler) snapshot() []messageStateCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]messageStateCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestEmitMessageState_HandlerInvoked verifies that EmitMessageState
// fires the registered subscriber with the correct (chatID,
// userMsgID, state) triple. No-op when no subscriber installed.
//
// F-53: enum is now MessageQueued / MessageSubmitted /
// MessageDropped (was Received/Forwarded/Done/Failed).
func TestEmitMessageState_HandlerInvoked(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.MessageStateBus.Subscribe(cap.handler)

	before := time.Now()
	cs.EmitMessageState("om_msg_1", agent.MessageQueued)
	cs.EmitMessageState("om_msg_2", agent.MessageSubmitted)
	cs.EmitMessageState("om_msg_3", agent.MessageDropped)
	after := time.Now()

	got := cap.snapshot()
	if len(got) != 3 {
		t.Fatalf("captured %d calls; want 3", len(got))
	}
	want := []messageStateCall{
		{"oc_chat", "om_msg_1", agent.MessageQueued, time.Time{}},
		{"oc_chat", "om_msg_2", agent.MessageSubmitted, time.Time{}},
		{"oc_chat", "om_msg_3", agent.MessageDropped, time.Time{}},
	}
	for i, w := range want {
		if got[i].chatID != w.chatID || got[i].userMsgID != w.userMsgID || got[i].state != w.state {
			t.Errorf("call[%d] = %+v; want {%s %s %s}", i, got[i], w.chatID, w.userMsgID, w.state)
		}
		// Verify the At field is populated by Publish — should fall
		// inside the window we bracketed above. Compare as UnixNano
		// to keep the test robust to monotonic clock differences.
		if got[i].at.UnixNano() < before.UnixNano() || got[i].at.UnixNano() > after.UnixNano() {
			t.Errorf("call[%d].at = %v; want in [%v, %v]", i, got[i].at, before, after)
		}
	}
}

// TestEmitMessageState_NoHandlerIsNoop confirms EmitMessageState is
// safe to call without a registered subscriber — must not panic.
func TestEmitMessageState_NoHandlerIsNoop(t *testing.T) {
	cs := New("oc_chat", "claude")
	// No Subscribe call.
	cs.EmitMessageState("om_msg", agent.MessageQueued)
	// If we got here without panic, success.
}

// TestMessageStateBus_UnsubscribeStopsDelivery confirms a returned
// unsubscribe func, once invoked, stops future events from reaching
// the handler. Mirrors the v1.3 "nil clears handler" semantics
// via Bus.Clear().
func TestMessageStateBus_UnsubscribeStopsDelivery(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	unbind := cs.MessageStateBus.Subscribe(cap.handler)

	cs.EmitMessageState("om_1", agent.MessageQueued)
	unbind()
	cs.EmitMessageState("om_2", agent.MessageDropped)

	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("captured %d calls after unsubscribe; want 1 (only the first emit)", got)
	}
}

// TestMessageStateBus_ClearDropsAllSubscribers is the multi-subscriber
// equivalent: clearing the bus stops all subscribers at once.
// Migration callers that previously relied on
// `cs.SetMessageStateHandler(nil)` to wipe the prior handler can
// now use `cs.MessageStateBus.Clear()`.
func TestMessageStateBus_ClearDropsAllSubscribers(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap1 := &captureHandler{}
	cap2 := &captureHandler{}
	cs.MessageStateBus.Subscribe(cap1.handler)
	cs.MessageStateBus.Subscribe(cap2.handler)

	cs.EmitMessageState("om_pre", agent.MessageQueued)
	if got := len(cap1.snapshot()) + len(cap2.snapshot()); got != 2 {
		t.Fatalf("pre-Clear captured %d calls total; want 2", got)
	}

	cs.MessageStateBus.Clear()

	cs.EmitMessageState("om_post", agent.MessageDropped)
	if got := len(cap1.snapshot()) + len(cap2.snapshot()); got != 2 {
		t.Fatalf("post-Clear captured %d calls total; want still 2 (Clear must drop all subscribers)", got)
	}
}

// TestEmitMessageDropped_FlipsStageAndEmits verifies that the
// post-refactor drop path (cs.emitMessageDropped) flips a
// Message to MessageDropped AND fires the MessageState event.
// F-54: firing goes through cs.MessageStateBus.Publish.
func TestEmitMessageDropped_FlipsStageAndEmits(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.MessageStateBus.Subscribe(cap.handler)

	msg := Message{ID: "om_drop", ChatID: "oc_chat"}
	cs.emitMessageDropped(msg)

	got := cap.snapshot()
	if len(got) != 1 {
		t.Fatalf("captured %d MessageState events; want 1", len(got))
	}
	if got[0].state != agent.MessageDropped || got[0].userMsgID != "om_drop" {
		t.Errorf("event = %+v; want {MessageDropped, om_drop}", got[0])
	}
}

// --- Shared helpers (used by multiple _test.go files) --------------
//
// spySpawner is a no-op Spawner for tests that don't actually need
// to fork a child process. Returns an error so any code that
// unexpectedly calls Spawn fails loudly instead of silently
// proceeding.
type spySpawner struct{}

func (s *spySpawner) Spawn(_ context.Context, _ string, _ string, _ []string, _ string) (agent.Agent, error) {
	return nil, errors.New("spySpawner: not implemented")
}

// newActiveAgentNoop creates a minimal AgentSession whose
// operations are no-ops. Used by tests that need an active AS for
// state-machine checks without spawning a real bridge.
func newActiveAgentNoop() *AgentSession {
	return newAgentSessionRuntime("as_noop", "cs_noop", "noop", "/tmp")
}