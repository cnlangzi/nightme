package feishu

import (
	"context"
	"fmt"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestReceiptsLRU_BoundsMemory pins the bounded-memory invariant of
// the receipt index: it must not grow past receiptsLRUSize no matter
// how many distinct user message ids opened a card. A long-running
// daemon must not leak one receipt per turn.
//
// Mechanism: hashicorp/golang-lru/v2 evicts the least-recently-
// accessed entry on overflow. We fill past the cap and verify
// (a) Len() stays at the cap, (b) the oldest entries are gone and
// (c) the newest ones survive.
func TestReceiptsLRU_BoundsMemory(t *testing.T) {
	a := testAdapter(t)

	const overflow = 100
	total := receiptsLRUSize + overflow
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("om_lru_%d", i)
		a.receiptsByUserMsgID.Add(key, NewMessageReceiptForReply("oc_test", key, a))
	}

	if got := a.receiptsByUserMsgID.Len(); got != receiptsLRUSize {
		t.Fatalf("after %d adds, Len() = %d; want %d", total, got, receiptsLRUSize)
	}

	for i := 0; i < overflow; i++ {
		key := fmt.Sprintf("om_lru_%d", i)
		if _, ok := a.receiptsByUserMsgID.Peek(key); ok {
			t.Errorf("oldest entry %q should have been evicted; still present", key)
		}
	}
	for i := overflow; i < total; i++ {
		key := fmt.Sprintf("om_lru_%d", i)
		if _, ok := a.receiptsByUserMsgID.Peek(key); !ok {
			t.Errorf("recent entry %q should still be present; missing", key)
		}
	}
}

// TestReceiptsLRU_HotPathKeepsReceiptAlive pins the recency refresh
// of the hot lookup: receiptFor runs on every outbound event, so the
// receipt of a running turn stays at the front of the LRU and is not
// evicted while the turn is still producing events.
func TestReceiptsLRU_HotPathKeepsReceiptAlive(t *testing.T) {
	a := testAdapter(t)

	a.receiptsByUserMsgID.Add("om_live", NewMessageReceiptForReply("oc_test", "om_live", a))

	// Fill the index past the cap so exactly one eviction happens.
	// The live receipt is looked up along the way, so the entry it
	// must outlive is the oldest filler — not the live receipt.
	for i := 0; i < receiptsLRUSize; i++ {
		key := fmt.Sprintf("om_fill_%d", i)
		a.receiptsByUserMsgID.Add(key, NewMessageReceiptForReply("oc_test", key, a))
		if i%100 == 0 {
			a.receiptFor(context.Background(), "oc_test", "om_live")
		}
	}

	if _, ok := a.receiptsByUserMsgID.Peek("om_live"); !ok {
		t.Errorf("receipt of a running turn was evicted; the hot lookup must refresh recency")
	}
	if _, ok := a.receiptsByUserMsgID.Peek("om_fill_0"); ok {
		t.Errorf("oldest filler should have been evicted; still present")
	}
}

// TestReceiptsLRU_EvictedReceiptOpensNewCard pins the documented
// consequence of eviction: the card stays in the chat but its receipt
// is gone, so a later event for that user message id cold-starts a
// fresh card instead of PATCHing the old one.
func TestReceiptsLRU_EvictedReceiptOpensNewCard(t *testing.T) {
	a := testAdapter(t)

	var sends int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sends++
		return fmt.Sprintf("om_card_%d", sends), nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if _, err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutReply,
		ChatID:  "oc_test",
		ReplyTo: "om_user",
		Text:    "first chunk",
	}); err != nil {
		t.Fatalf("cold-start Send(OutReply): %v", err)
	}
	if sends != 1 {
		t.Fatalf("cold-start send count = %d, want 1", sends)
	}
	a.mu.RLock()
	first, ok := a.receiptsByUserMsgID.Get("om_user")
	a.mu.RUnlock()
	if !ok || first == nil {
		t.Fatalf("receipt not registered after cold-start")
	}

	// Overflow the index so om_user is evicted.
	for i := 0; i < receiptsLRUSize; i++ {
		key := fmt.Sprintf("om_fill_%d", i)
		a.receiptsByUserMsgID.Add(key, NewMessageReceiptForReply("oc_test", key, a))
	}
	if _, ok := a.receiptsByUserMsgID.Peek("om_user"); ok {
		t.Fatalf("om_user should have been evicted")
	}

	// The next event for the evicted id opens a new card.
	if _, err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutReply,
		ChatID:  "oc_test",
		ReplyTo: "om_user",
		Text:    "second chunk",
	}); err != nil {
		t.Fatalf("post-eviction Send(OutReply): %v", err)
	}
	if sends != 2 {
		t.Errorf("send count = %d, want 2 (an evicted receipt cold-starts a new card)", sends)
	}
	a.mu.RLock()
	second, ok := a.receiptsByUserMsgID.Get("om_user")
	a.mu.RUnlock()
	if !ok || second == nil {
		t.Fatalf("no receipt registered for the reopened turn")
	}
	if second == first {
		t.Errorf("reopened turn reuses the evicted receipt")
	}
}
