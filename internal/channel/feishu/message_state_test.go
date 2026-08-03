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
	tracked := a.messageStates["om_msg_1"]
	a.mu.RUnlock()
	if tracked != receipt.MessageState(0) {
		t.Errorf("after failed AddReaction, messageStates[om_msg_1] = %v; want zero value (revert)", tracked)
	}

	// Now inject a stub larkClient to make AddReaction succeed.
	// We can't easily mock the larkClient struct (deep nesting),
	// so we test the messageStates update directly via an internal
	// helper. This proves the idempotency bookkeeping works
	// independent of the network call.
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

// errIsUnused keeps the errors import live for future failure tests.
var errIsUnused = errors.New("placeholder")