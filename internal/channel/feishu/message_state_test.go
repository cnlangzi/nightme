package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// TestSend_OutMessageState_MissingPayload verifies that the Send
// dispatcher returns a descriptive error when the event payload
// lacks the required typed fields (§1.4 cleanup: was Meta, now
// MessageState typed field).
func TestSend_OutMessageState_MissingPayload(t *testing.T) {
	a := testAdapter(t)

	// Missing MessageState payload entirely.
	err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
	})
	if err == nil || !strings.Contains(err.Error(), "MessageState") {
		t.Errorf("missing payload: got %v; want error mentioning MessageState", err)
	}

	// Missing MessageID.
	err = a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			State: agent.StateReceived,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "MessageID") {
		t.Errorf("missing MessageID: got %v; want error mentioning MessageID", err)
	}
}

// TestSend_OutMessageState_UnknownStateDrops verifies that an
// unknown state value is a silent drop (forward-compatible: new
// states added in future versions degrade gracefully on old
// channels).
func TestSend_OutMessageState_UnknownStateDrops(t *testing.T) {
	a := testAdapter(t)
	err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_user_msg",
			State:     agent.MessageState(42), // unknown
		},
	})
	if err != nil {
		t.Errorf("unknown state should drop silently; got err = %v", err)
	}
}

// TestSend_OutMessageState_TracksStateIdempotency verifies the
// messageStates map is populated for successful renders and can
// short-circuit duplicate emits. AddReaction is a no-op because
// larkClient is nil (testAdapter default) — we assert on the
// side-effect (messageStates map) rather than the API call.
func TestSend_OutMessageState_TracksStateIdempotency(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	// First emit: larkClient is nil → AddReaction returns error,
	// messageStates reverts (per F-31 failure semantics).
	_ = a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_1",
			State:     agent.StateReceived,
		},
	})
	// After failure, messageStates should not be marked (revert).
	a.mu.RLock()
	_, hasPrev := a.messageStates["om_msg_1"]
	a.mu.RUnlock()
	if hasPrev {
		t.Errorf("after failed AddReaction, messageStates should be reverted (no entry); got hasPrev=true")
	}

	// Pre-populate messageStates with StateReceived (simulating a
	// successful prior render).
	a.mu.Lock()
	a.messageStates["om_msg_1"] = agent.StateReceived
	a.mu.Unlock()

	// Second emit with same state: should be short-circuited
	// before AddReaction is even attempted. With larkClient nil,
	// AddReaction would fail anyway; but the skip happens first,
	// so we assert no error from the Send dispatcher when
	// messageStates already has the state.
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_1",
			State:     agent.StateReceived,
		},
	})
	if err != nil {
		t.Errorf("idempotent re-emit should be no-op; got err = %v", err)
	}
}

// TestSend_OutMessageState_FirstReceivedNotSkipped is a
// regression test for v1.3.1: previously, the idempotency check
// used `prev := messageStates[messageID]` which returned the zero
// value (StateReceived) for an unseen messageID, causing every
// first StateReceived emit to be silently skipped. The fix uses
// the comma-ok form to distinguish "no entry" from "prev ==
// StateReceived".
//
// F-42: this test is now superseded — StateReceived is intentionally
// silent-dropped by the Feishu adapter (see OutMessageState case in
// adapter.go). The idempotency bug it guards no longer matters
// because we never reach AddReaction for StateReceived. The test
// remains as a regression sentinel for the OLD behavior (if a
// future change re-enables StateReceived rendering, the same
// zero-value idempotency bug could return). The assertion now
// verifies the silent-drop path: Send returns nil without touching
// messageStates or AddReaction.
func TestSend_OutMessageState_FirstReceivedNotSkipped(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	// Pre-condition: messageStates has NO entry for this msgID.
	a.mu.Lock()
	delete(a.messageStates, "om_msg_first")
	a.mu.Unlock()

	// F-42: StateReceived is silent-dropped before the
	// idempotency / AddReaction logic. Send must return nil
	// without touching messageStates. (Pre-F-42 this same call
	// would have attempted AddReaction, which fails with nil
	// larkClient — the test originally asserted that error to
	// detect the zero-value idempotency bug. After F-42 the
	// reaction is never attempted, so we assert the opposite:
	// nil err AND messageStates stays untouched.)
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_first",
			State:     agent.StateReceived,
		},
	})
	if err != nil {
		t.Fatalf("StateReceived should be silent-dropped; got err=%v", err)
	}
	a.mu.Lock()
	_, hasEntry := a.messageStates["om_msg_first"]
	a.mu.Unlock()
	if hasEntry {
		t.Errorf("StateReceived silent-drop must not populate messageStates; entry found")
	}
}

// TestSend_OutMessageState_StateForwardedIsSilentDrop mirrors the
// StateReceived test for StateForwarded (🔄). F-42 drops both
// intermediate states; only StateDone / StateError remain visible.
func TestSend_OutMessageState_StateForwardedIsSilentDrop(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	a.mu.Lock()
	delete(a.messageStates, "om_msg_fwd")
	a.mu.Unlock()

	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_fwd",
			State:     agent.StateForwarded,
		},
	})
	if err != nil {
		t.Fatalf("StateForwarded should be silent-dropped; got err=%v", err)
	}
	a.mu.Lock()
	_, hasEntry := a.messageStates["om_msg_fwd"]
	a.mu.Unlock()
	if hasEntry {
		t.Errorf("StateForwarded silent-drop must not populate messageStates; entry found")
	}
}