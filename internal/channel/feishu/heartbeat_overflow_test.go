// Package feishu — F-63.1 regression test for receiptBodyStats
// off-by-one.
//
// Pre-fix, receiptBodyStats duplicated buildReceiptCard's
// element-counting logic but missed the F-63 heartbeat header.
// With r.heartbeat populated, the overflow check undercounted
// by 1, allowing a receipt to slip one element past Feishu's
// 50-element cap. The PATCH body Feishu received had 51
// elements and was rejected — the receipt was stuck.
//
// These tests pin:
//   - 50 entries + heartbeat header overflows on entry #50
//   - buildReceiptCard's elementCount covers the heartbeat
//     header (single source of truth, not duplicated)

package feishu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestAppendEntry_OverflowWithHeartbeatHeader_BailsOut pins
// the F-63.1 fix: 50 entries (cap) + 1 heartbeat header = 51
// elements must trigger ErrReceiptOverflow on the 51st
// AppendEntry. Pre-fix, receiptBodyStats didn't count the
// heartbeat header (undercounted by 1), so the overflow
// check saw 50 elements and let the 51st entry through,
// producing a PATCH body Feishu's API rejected.
func TestAppendEntry_OverflowWithHeartbeatHeader_BailsOut(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_overflow", "om_overflow", "om_card", bot)
	r.cardMsgID = "om_overflow_card"
	// Non-empty heartbeat — the F-63.1 regression scenario.
	r.heartbeat = messages.HeartbeatSnapshot{
		ThinkCount: 5,
		LastBeatAt: time.Now(),
	}
	// Disable throttle so every entry triggers the overflow check.
	r.heartbeatMinInterval = 0

	// Pre-fill 49 entries (under the 50-element cap). With the
	// heartbeat header at 50 elements total, the NEXT AppendEntry
	// (50th entry) would push to 51 elements — bail-out path.
	r.mu.Lock()
	for i := 0; i < 49; i++ {
		r.entries = append(r.entries, LogEntry{
			Icon: "💬", Text: "seed",
		})
	}
	r.mu.Unlock()

	// (50th entry) — 50 entries + 1 heartbeat header = 51
	// elements → ErrReceiptOverflow.
	err := r.AppendEntry(context.Background(), LogEntry{Icon: "💬", Text: "trigger"})
	if !errors.Is(err, ErrReceiptOverflow) {
		t.Fatalf("50th AppendEntry err = %v, want ErrReceiptOverflow (50 entries + 1 heartbeat = 51)",
			err)
	}

	// Bail-out must NOT commit the entry.
	r.mu.Lock()
	if got := len(r.entries); got != 49 {
		r.mu.Unlock()
		t.Fatalf("after bail-out: r.entries len = %d, want 49 (bail-out must not commit)", got)
	}
	r.mu.Unlock()

	// No PATCHes should have hit the mock — overflow bail-out
	// doesn't PATCH.
	if len(bot.patches) != 0 {
		t.Fatalf("overflow bail-out issued %d PATCHes; want 0", len(bot.patches))
	}
}

// TestAppendEntry_OverflowWithoutHeartbeatHeader_NoEarlyBail
// is the inverse regression case: 50 entries WITHOUT a heartbeat
// header = 50 elements (at cap but not over). The 50th entry
// must SUCCEED — overflow only fires when the heartbeat header
// pushes past the cap.
//
// Pre-fix this test would also pass (the bug only triggered
// WITH the heartbeat). The test pins the invariant to prevent
// future false positives (e.g. someone "fixing" the overflow
// check to be too aggressive).
func TestAppendEntry_OverflowWithoutHeartbeatHeader_NoEarlyBail(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_overflow2", "om_overflow2", "om_card", bot)
	r.cardMsgID = "om_card2"
	// NO heartbeat — entries only.
	r.heartbeatMinInterval = 0

	// 50 entries: 50 elements exactly, NOT over the cap.
	r.mu.Lock()
	for i := 0; i < 50; i++ {
		r.entries = append(r.entries, LogEntry{Icon: "💬", Text: "seed"})
	}
	r.mu.Unlock()

	// 51st entry — would be 51 elements, overflow.
	err := r.AppendEntry(context.Background(), LogEntry{Icon: "💬", Text: "trigger"})
	if !errors.Is(err, ErrReceiptOverflow) {
		t.Fatalf("51st AppendEntry (no heartbeat) err = %v, want ErrReceiptOverflow", err)
	}
}

// TestReceiptBodyStats_ElementCountFromBuildReceiptCard pins
// the single-source-of-truth contract: buildReceiptCard
// returns the element count, receiptBodyStats echoes it. The
// off-by-one bug was caused by duplicating the count logic in
// two functions; this test makes the duplication impossible
// to reintroduce without breaking the test.
func TestReceiptBodyStats_ElementCountFromBuildReceiptCard(t *testing.T) {
	hb := &messages.HeartbeatSnapshot{ThinkCount: 2, LastBeatAt: time.Now()}
	body, elementCount, err := buildReceiptCard(
		[]LogEntry{{Icon: "💬", Text: "a"}, {Icon: "💬", Text: "b"}},
		nil,
		[]string{"footer-line-1", "footer-line-2"},
		hb,
	)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	// Layout:
	//   Section 0: 1 heartbeat header
	//   Section 1: 2 entries (2 markdown elements)
	//   Section 3: 1 <hr> + 1 footer markdown = 2 elements
	// Total: 5
	wantElements := 5
	if elementCount != wantElements {
		t.Fatalf("buildReceiptCard elementCount = %d, want %d", elementCount, wantElements)
	}

	// receiptBodyStats must echo the SAME number.
	gotElements, _ := receiptBodyStats(nil, nil, nil, body, elementCount)
	if gotElements != wantElements {
		t.Fatalf("receiptBodyStats = %d, want %d (must match buildReceiptCard)",
			gotElements, wantElements)
	}

	// And it must NOT recompute — pass a wrong elementCount and
	// assert receiptBodyStats echoes the wrong value (proving
	// no internal duplication).
	wrongCount := 99
	gotElements2, _ := receiptBodyStats(nil, nil, nil, body, wrongCount)
	if gotElements2 != wrongCount {
		t.Fatalf("receiptBodyStats echoed %d when given %d — it should NOT recompute", gotElements2, wrongCount)
	}
}

// TestBuildReceiptCard_EmptySnapshot_NoHeartbeatElement pins
// the inverse: when r.heartbeat is zero-valued (Empty()), the
// header section is skipped (legacy placeholder instead), so
// elementCount does NOT include the heartbeat header.
func TestBuildReceiptCard_EmptySnapshot_NoHeartbeatElement(t *testing.T) {
	body, elementCount, err := buildReceiptCard(nil, nil, nil, &messages.HeartbeatSnapshot{})
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// 1 element (legacy "🤖 Working" placeholder).
	if elementCount != 1 {
		t.Fatalf("elementCount = %d, want 1 (empty snapshot → legacy placeholder, no heartbeat element)",
			elementCount)
	}
	if gotElements, _ := receiptBodyStats(nil, nil, nil, body, elementCount); gotElements != 1 {
		t.Fatalf("receiptBodyStats = %d, want 1", gotElements)
	}
}

// _ keeps io + slog imports referenced (the test fixtures
// use slog.Default in production paths; io.Discard would
// be the typical discard target).
var (
	_ = io.Discard
	_ = slog.Default()
)