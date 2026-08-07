package feishu

import (
	"context"
	"fmt"
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
			State: agent.MessageQueued,
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
			State:     agent.MessageQueued,
		},
	})
	// After failure, messageStates should not be marked (revert).
	a.mu.RLock()
	_, hasPrev := a.messageStates.Get("om_msg_1")
	a.mu.RUnlock()
	if hasPrev {
		t.Errorf("after failed AddReaction, messageStates should be reverted (no entry); got hasPrev=true")
	}

	// Pre-populate messageStates with MessageReceived (simulating
	// a successful prior render).
	a.mu.Lock()
	a.messageStates.Add("om_msg_1", agent.MessageQueued)
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
			State:     agent.MessageQueued,
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
	a.messageStates.Remove("om_msg_first")
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
			State:     agent.MessageQueued,
		},
	})
	if err == nil {
		t.Fatalf("expected AddReaction error against nil larkClient; got nil")
	}
	a.mu.Lock()
	_, hasEntry := a.messageStates.Get("om_msg_first")
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
	a.messageStates.Remove("om_msg_fwd")
	a.mu.Unlock()

	// First MessageForwarded emit must proceed to AddReaction
	// (not silently dropped). AddReaction against nil larkClient
	// returns an error; the dispatcher reverts messageStates.
	err := a.Send(ctx, gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_msg_fwd",
			State:     agent.MessageSubmitted,
		},
	})
	if err == nil {
		t.Fatalf("expected AddReaction error against nil larkClient; got nil")
	}
	a.mu.Lock()
	_, hasEntry := a.messageStates.Get("om_msg_fwd")
	a.mu.Unlock()
	if hasEntry {
		t.Errorf("after failed AddReaction, messageStates should be reverted (no entry); got hasEntry=true")
	}
}

// TestMessageStatesLRU_BoundsMemory pins the bounded-memory
// invariant: the cache must not grow past its configured cap
// regardless of how many distinct userMsgIDs get rendered. A
// long-running daemon with high chat volume must not leak.
//
// Mechanism: hashicorp/golang-lru/v2 evicts the least-recently-
// accessed entry on Add overflow. We fill past the cap and
// verify (a) Len() stays at the cap and (b) the oldest entry is
// gone from the cache.
func TestMessageStatesLRU_BoundsMemory(t *testing.T) {
	a := testAdapter(t)

	// Fill the cache with cap+100 distinct user message ids.
	const overflow = 100
	total := messageStatesLRUSize + overflow
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("om_lru_%d", i)
		// F-53: any non-zero MessageState is fine here; we just
		// need distinct entries to exercise the LRU. Use
		// MessageSubmitted as the canonical "lifecycle state".
		a.messageStates.Add(key, agent.MessageSubmitted)
	}

	// Cache size must be capped at messageStatesLRUSize.
	if got := a.messageStates.Len(); got != messageStatesLRUSize {
		t.Fatalf("after %d adds, Len() = %d; want %d", total, got, messageStatesLRUSize)
	}

	// The oldest entries (om_lru_0..om_lru_overflow-1) must be
	// evicted; the newest cap entries must survive.
	for i := 0; i < overflow; i++ {
		key := fmt.Sprintf("om_lru_%d", i)
		if _, ok := a.messageStates.Peek(key); ok {
			t.Errorf("oldest entry %q should have been evicted; still present", key)
		}
	}
	for i := overflow; i < total; i++ {
		key := fmt.Sprintf("om_lru_%d", i)
		if _, ok := a.messageStates.Peek(key); !ok {
			t.Errorf("recent entry %q should still be present; missing", key)
		}
	}
}

// TestMessageStatesLRU_TerminalGuardSurvivesEviction verifies
// that the terminal-state guard's correctness does not silently
// regress when the LRU evicts a terminal entry. After eviction,
// a same-state MessageDone emit must be treated as a fresh
// first-emit (hasPrev=false) — this is the documented trade-off
// in adapter.go's messageStates field comment. We pin the trade-off
// here so any future change to "preserve terminal entries forever"
// is forced to revisit the LRU design.
func TestMessageStatesLRU_TerminalGuardSurvivesEviction(t *testing.T) {
	a := testAdapter(t)

	// Fill the cache with cap terminal entries to evict any prior
	// terminal entry we'll add below. (Cap+1 adds guarantees
	// om_evict_me is evicted.)
	for i := 0; i <= messageStatesLRUSize; i++ {
		a.messageStates.Add(fmt.Sprintf("om_filler_%d", i), agent.MessageSubmitted)
	}
	a.messageStates.Add("om_evict_me", agent.MessageSubmitted)

	// Fill again to push om_evict_me out of the LRU.
	for i := 0; i < messageStatesLRUSize+1; i++ {
		a.messageStates.Add(fmt.Sprintf("om_evict_%d", i), agent.MessageSubmitted)
	}

	// om_evict_me should be gone (evicted).
	if _, ok := a.messageStates.Peek("om_evict_me"); ok {
		t.Fatalf("om_evict_me should have been evicted by the second fill")
	}

	// A new MessageDone emit for the evicted userMsgID is
	// treated as a fresh first-emit. The dispatcher proceeds to
	// AddReaction (which fails against nil larkClient), then
	// reverts. We verify the cache ends up empty for this key
	// — the post-failure revert is intact under LRU.
	err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:   gateway.OutMessageState,
		ChatID: "oc_chat",
		MessageState: &gateway.MessageStatePayload{
			MessageID: "om_evict_me",
			State:     agent.MessageSubmitted,
		},
	})
	if err == nil {
		t.Fatalf("expected AddReaction error against nil larkClient; got nil")
	}
	a.mu.RLock()
	_, hasEntry := a.messageStates.Get("om_evict_me")
	a.mu.RUnlock()
	if hasEntry {
		t.Errorf("after failed AddReaction, messageStates should be reverted (no entry); got hasEntry=true")
	}
}