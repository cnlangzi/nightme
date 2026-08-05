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
			State: agent.MessageReceived,
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
			State:     agent.MessageReceived,
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
	a.messageStates["om_msg_1"] = agent.MessageReceived
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
			State:     agent.MessageReceived,
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
// v1.3.x: this regression test stays valid — StateReceived is
// rendered (the F-42 silent-drop was reverted so the user
// message gets a ⏳ reaction during the FastAck window). The
// zero-value idempotency bug it guards could re-emerge if a
// future change replaces the comma-ok check; the test below
// confirms the first StateReceived emit does NOT short-circuit
// (it gets passed through to AddReaction, which then fails
// against the nil larkClient — we assert messageStates reverts
// per F-31 failure semantics).
func TestSend_OutMessageState_FirstReceivedNotSkipped(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	// Pre-condition: messageStates has NO entry for this msgID.
	a.mu.Lock()
	delete(a.messageStates, "om_msg_first")
	a.mu.Unlock()

	// First MessageReceived emit must NOT be silently skipped by
	// the idempotency check. It proceeds to AddReaction, which
	// fails with nil larkClient. The dispatcher reverts the
	// messageStates entry so a later retry can re-attempt.
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_first",
			State:     agent.MessageReceived,
		},
	})
	if err == nil {
		t.Fatalf("expected AddReaction error against nil larkClient; got nil")
	}
	a.mu.Lock()
	_, hasEntry := a.messageStates["om_msg_first"]
	a.mu.Unlock()
	if hasEntry {
		t.Errorf("after failed AddReaction, messageStates should be reverted (no entry); got hasEntry=true")
	}
}

// TestSend_OutMessageState_StateForwardedRenders verifies that
// MessageForwarded (🔄) is rendered as a user-message reaction.
// The F-42 silent-drop was reverted so intermediate states
// provide FastAck UX during the gap between user message
// dispatch and first OutReply / OutTask*.
func TestSend_OutMessageState_StateForwardedRenders(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	a.mu.Lock()
	delete(a.messageStates, "om_msg_fwd")
	a.mu.Unlock()

	// First MessageForwarded emit must proceed to AddReaction
	// (not silently dropped). AddReaction against nil larkClient
	// returns an error; the dispatcher reverts messageStates.
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_fwd",
			State:     agent.MessageForwarded,
		},
	})
	if err == nil {
		t.Fatalf("expected AddReaction error against nil larkClient; got nil")
	}
	a.mu.Lock()
	_, hasEntry := a.messageStates["om_msg_fwd"]
	a.mu.Unlock()
	if hasEntry {
		t.Errorf("after failed AddReaction, messageStates should be reverted (no entry); got hasEntry=true")
	}
}