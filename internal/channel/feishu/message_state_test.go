package feishu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/receipt"
)

// TestMapStateToFeishuEmoji verifies the canonical F-31 §8.3
// mapping table. Pure function, no IO required.
func TestMapStateToFeishuEmoji(t *testing.T) {
	cases := []struct {
		state receipt.MessageState
		want  string
	}{
		{receipt.StateReceived, "OneSecond"},   // ⏳
		{receipt.StateForwarded, "OnIt"},        // 🔄
		{receipt.StateDone, "DONE"},            // ✅
		{receipt.StateError, "THUMBSUP"},       // ❌ closest predefined
		{receipt.MessageState(99), ""},         // unknown → silent drop
	}
	for _, tc := range cases {
		got := mapStateToFeishuEmoji(tc.state)
		if got != tc.want {
			t.Errorf("mapStateToFeishuEmoji(%v) = %q; want %q", tc.state, got, tc.want)
		}
	}
}

// TestSend_OutMessageState_MissingMeta verifies Send returns a
// descriptive error when the event payload lacks the required
// Meta["message_id"] / Meta["state"] fields.
func TestSend_OutMessageState_MissingMeta(t *testing.T) {
	a := testAdapter(t)

	// Missing message_id.
	err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		Meta: map[string]any{
			"state": receipt.StateReceived,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "message_id") {
		t.Errorf("missing message_id: got %v; want error mentioning message_id", err)
	}

	// Missing state.
	err = a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		Meta: map[string]any{
			"message_id": "om_user_msg",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Errorf("missing state: got %v; want error mentioning state", err)
	}

	// Wrong state type (string instead of receipt.MessageState).
	err = a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		Meta: map[string]any{
			"message_id": "om_user_msg",
			"state":      "received", // wrong type
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Errorf("wrong state type: got %v; want error mentioning type", err)
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
		Meta: map[string]any{
			"message_id": "om_user_msg",
			"state":      receipt.MessageState(42), // unknown
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
		Meta: map[string]any{
			"message_id": "om_msg_1",
			"state":      receipt.StateReceived,
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
	a.messageStates["om_msg_1"] = receipt.StateReceived
	a.mu.Unlock()

	// Second emit with same state: should be short-circuited
	// before AddReaction is even attempted. With larkClient nil,
	// AddReaction would fail anyway; but the skip happens first,
	// so we assert no error from the Send dispatcher when
	// messageStates already has the state.
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		Meta: map[string]any{
			"message_id": "om_msg_1",
			"state":      receipt.StateReceived,
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
		Meta: map[string]any{
			"message_id": "om_msg_first",
			"state":      receipt.StateReceived,
		},
	})
	// larkClient is nil → AddReaction returns "feishu: REST client
	// not initialized". If we got nil err, the buggy skip path
	// would have hidden the attempt.
	if err == nil {
		t.Fatalf("Send should attempt AddReaction and fail (nil larkClient); got nil err — likely the skip bug")
	}
}

// errIsUnused keeps the errors import live for future failure tests.
var errIsUnused = errors.New("placeholder")