// Package feishu — F-63.1 pending heartbeat stash tests.
//
// Covers the OutHeartbeat→receipt-missing scenario: the first
// countable event (OutThinking / OutToolStart) can arrive at
// the adapter BEFORE ensureReceiptForTyping has installed a
// receipt for that userMsgID. Without the pending stash, the
// heartbeat update is dropped and the receipt's per-instance
// r.heartbeat later jumps from zero to the LATEST count.
//
// These tests pin:
//   - OutHeartbeat stashes when receipt is nil
//   - applyPendingHeartbeat drains + applies on receipt creation
//   - Latest-write-wins monotonic merge (no counter regression)
//   - drainPendingHeartbeat removes the entry (idempotent)

package feishu

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestPendingHeartbeat_StashWhenReceiptMissing drives the
// adapter's OutHeartbeat case when no receipt exists for the
// userMsgID. Expects the snapshot to be stashed in
// a.pendingHeartbeats rather than dropped.
func TestPendingHeartbeat_StashWhenReceiptMissing(t *testing.T) {
	adapter := newAdapterWithBot(&mockReceiptBot{})

	// No receipt installed — first heartbeat lands cold.
	msg := messages.OutboundMessage{
		ChatID:    "oc_pending",
		Kind:      messages.OutHeartbeat,
		ReplyTo:   "om_pending_user",
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 1, ToolCount: 0, LastBeatAt: time.Now()},
	}
	if err := adapter.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	adapter.mu.Lock()
	stashed, ok := adapter.pendingHeartbeats["om_pending_user"]
	adapter.mu.Unlock()
	if !ok {
		t.Fatal("expected heartbeat stashed in pendingHeartbeats")
	}
	if stashed.ThinkCount != 1 {
		t.Fatalf("stashed ThinkCount = %d, want 1", stashed.ThinkCount)
	}
}

// TestPendingHeartbeat_DrainOnReceiptInstall covers the
// recovery path: after stashing, when the receipt is
// installed, applyPendingHeartbeat must drain + apply so the
// first heartbeat-driven PATCH already shows the right counts.
func TestPendingHeartbeat_DrainOnReceiptInstall(t *testing.T) {
	adapter := newAdapterWithBot(&mockReceiptBot{})

	// Stash a heartbeat BEFORE receipt exists.
	now := time.Now()
	if err := adapter.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "oc_drain",
		Kind:      messages.OutHeartbeat,
		ReplyTo:   "om_drain_user",
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 2, ToolCount: 3, LastBeatAt: now},
	}); err != nil {
		t.Fatalf("Send (stash): %v", err)
	}

	// Install the receipt.
	receipt := installReceipt(adapter, &mockReceiptBot{}, "oc_drain", "om_drain_user", "om_drain_card_1")
	adapter.applyPendingHeartbeat(context.Background(), receipt)

	// Verify r.heartbeat absorbed the stashed snapshot.
	receipt.mu.Lock()
	hb := receipt.heartbeat
	receipt.mu.Unlock()
	if hb.ThinkCount != 2 {
		t.Fatalf("receipt ThinkCount = %d, want 2", hb.ThinkCount)
	}
	if hb.ToolCount != 3 {
		t.Fatalf("receipt ToolCount = %d, want 3", hb.ToolCount)
	}

	// Pending map is drained.
	adapter.mu.Lock()
	_, stillThere := adapter.pendingHeartbeats["om_drain_user"]
	adapter.mu.Unlock()
	if stillThere {
		t.Fatal("pendingHeartbeats should be drained after install")
	}
}

// TestPendingHeartbeat_MonotonicMerge — multiple stashes
// before receipt creation must NOT regress counters (later
// stash with lower count must not overwrite higher one). This
// matters because the tracker is monotonic but the OutHeartbeat
// emission order is not strictly ordered by count (each
// OutHeartbeat is independent).
func TestPendingHeartbeat_MonotonicMerge(t *testing.T) {
	adapter := newAdapterWithBot(&mockReceiptBot{})

	// First stash: ThinkCount=5
	t0 := time.Now()
	if err := adapter.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "oc_mono",
		Kind:      messages.OutHeartbeat,
		ReplyTo:   "om_mono_user",
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 5, ToolCount: 0, LastBeatAt: t0},
	}); err != nil {
		t.Fatalf("Send 1: %v", err)
	}

	// Second stash: ThinkCount=3 (out-of-order arrival) — must
	// NOT regress to 3; highest wins.
	t1 := t0.Add(time.Millisecond)
	if err := adapter.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "oc_mono",
		Kind:      messages.OutHeartbeat,
		ReplyTo:   "om_mono_user",
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 3, ToolCount: 7, LastBeatAt: t1},
	}); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	adapter.mu.Lock()
	merged := adapter.pendingHeartbeats["om_mono_user"]
	adapter.mu.Unlock()
	if merged.ThinkCount != 5 {
		t.Fatalf("merged ThinkCount = %d, want 5 (monotonic — must not regress from 5→3)",
			merged.ThinkCount)
	}
	if merged.ToolCount != 7 {
		t.Fatalf("merged ToolCount = %d, want 7", merged.ToolCount)
	}
	if !merged.LastBeatAt.Equal(t1) {
		// LastBeatAt semantics: "when did the agent LAST do
		// something". The tracker overwrites LastBeatAt on
		// every Observe (regardless of kind). When two
		// snapshots arrive out of order, the LATER one is
		// strictly newer activity — keep it.
		t.Fatalf("merged LastBeatAt = %v, want %v (later = most recent activity)",
			merged.LastBeatAt, t1)
	}
}

// TestPendingHeartbeat_DrainIsIdempotent — calling drain on
// an empty map is a no-op; calling drain twice on the same
// userMsgID returns false the second time.
func TestPendingHeartbeat_DrainIsIdempotent(t *testing.T) {
	adapter := newAdapterWithBot(&mockReceiptBot{})

	// Empty: drain returns false.
	if _, ok := adapter.drainPendingHeartbeat("om_nothing"); ok {
		t.Fatal("drain on missing entry returned ok=true")
	}

	// Stash + drain = true.
	_ = adapter.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "oc_idem",
		Kind:      messages.OutHeartbeat,
		ReplyTo:   "om_idem_user",
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 1},
	})
	if _, ok := adapter.drainPendingHeartbeat("om_idem_user"); !ok {
		t.Fatal("first drain returned ok=false")
	}
	if _, ok := adapter.drainPendingHeartbeat("om_idem_user"); ok {
		t.Fatal("second drain returned ok=true (not idempotent)")
	}
}

// TestPendingHeartbeat_ApplyNoOpWhenMissing — install a receipt
// for a userMsgID with no pending snapshot. applyPendingHeartbeat
// must silently do nothing.
func TestPendingHeartbeat_ApplyNoOpWhenMissing(t *testing.T) {
	adapter := newAdapterWithBot(&mockReceiptBot{})
	receipt := installReceipt(adapter, &mockReceiptBot{}, "oc_noop", "om_noop_user", "om_noop_card_1")

	// Snapshot before.
	receipt.mu.Lock()
	before := receipt.heartbeat
	receipt.mu.Unlock()

	adapter.applyPendingHeartbeat(context.Background(), receipt)

	// Snapshot after — identical.
	receipt.mu.Lock()
	after := receipt.heartbeat
	receipt.mu.Unlock()
	if before != after {
		t.Fatalf("applyPendingHeartbeat mutated snapshot without pending data:\n  before=%+v\n  after=%+v",
			before, after)
	}
}