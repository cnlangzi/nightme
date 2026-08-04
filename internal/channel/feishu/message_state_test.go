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
func TestSend_OutMessageState_FirstReceivedNotSkipped(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	// Inject a stub larkClient that always succeeds so the
	// idempotency decision is observable via messageStates.
	// (We can't easily mock the real larkClient because of deep
	// struct nesting; track via messageStates side-effect which
	// is set BEFORE AddReaction is called.)
	a.mu.Lock()
	// Pre-condition: messageStates has NO entry for this msgID.
	delete(a.messageStates, "om_msg_first")
	a.mu.Unlock()

	// Send StateReceived. Even though larkClient is nil and
	// AddReaction will fail, the messageStates update happens
	// BEFORE AddReaction is called (and reverts on failure). So
	// after the call, messageStates should NOT have an entry
	// (revert due to nil larkClient). The point is: the Send
	// dispatcher MUST have attempted AddReaction, not skipped it.
	//
	// We assert by checking that Send returned an error (i.e.
	// AddReaction was attempted, and failed because larkClient is
	// nil). If the bug were still present, Send would return nil
	// (skip path), falsely indicating success.
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_first",
			State:     agent.StateReceived,
		},
	})
	// larkClient is nil → AddReaction returns "feishu: REST client
	// not initialized". If we got nil err, the buggy skip path
	// would have hidden the attempt.
	if err == nil {
		t.Fatalf("Send should attempt AddReaction and fail (nil larkClient); got nil err — likely the skip bug")
	}
}