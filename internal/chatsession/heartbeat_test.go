// Package chatsession — HeartbeatTracker tests (F-63 §7.1).
//
// These tests cover the per-chat heartbeat accumulator in
// isolation (no ChatSession / runtime / channel needed). The
// full end-to-end flow (handler emits OutHeartbeat, Feishu
// adapter renders) is covered by:
//
//   - internal/runtime/handler_test.go   — observer placement
//                                          / policy interaction
//   - internal/channel/feishu/*_test.go  — receipt rendering

package chatsession

import (
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestNewHeartbeatTracker_Defaults pins the constructor's
// fallback behaviour: zero / negative cap resolves to
// DefaultHeartbeatCap, snapshots map is pre-sized for the cap
// to avoid the first cap map grows.
func TestNewHeartbeatTracker_Defaults(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero", 0, DefaultHeartbeatCap},
		{"negative", -5, DefaultHeartbeatCap},
		{"explicit", 16, 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewHeartbeatTracker(c.in)
			if got := tr.Cap(); got != c.want {
				t.Fatalf("Cap() = %d, want %d", got, c.want)
			}
			if got := tr.Len(); got != 0 {
				t.Fatalf("Len() = %d, want 0 (fresh tracker)", got)
			}
		})
	}
}

// TestObserve_ThinkIncrementsCount pins the basic counting
// contract for the kind that drives the most user-visible
// counter: OutThinking → ThinkCount++, returns true.
func TestObserve_ThinkIncrementsCount(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	if changed := tr.Observe("u1", messages.OutThinking); !changed {
		t.Fatal("first OutThinking should return changed=true")
	}
	snap := tr.Snapshot("u1")
	if snap.ThinkCount != 1 {
		t.Fatalf("ThinkCount = %d, want 1", snap.ThinkCount)
	}
	if snap.ToolCount != 0 {
		t.Fatalf("ToolCount = %d, want 0", snap.ToolCount)
	}
	if snap.LastBeatAt.IsZero() {
		t.Fatal("LastBeatAt not refreshed")
	}
}

// TestObserve_ToolStartIncrementsCount — same as above for
// OutToolStart.
func TestObserve_ToolStartIncrementsCount(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	if changed := tr.Observe("u1", messages.OutToolStart); !changed {
		t.Fatal("first OutToolStart should return changed=true")
	}
	snap := tr.Snapshot("u1")
	if snap.ToolCount != 1 {
		t.Fatalf("ToolCount = %d, want 1", snap.ToolCount)
	}
	if snap.ThinkCount != 0 {
		t.Fatalf("ThinkCount = %d, want 0", snap.ThinkCount)
	}
}

// TestObserve_ToolEndNoCount ensures OutToolEnd does NOT count
// (each tool call has exactly one OutToolStart + one OutToolEnd;
// counting both would inflate the visible counter).
func TestObserve_ToolEndNoCount(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	tr.Observe("u1", messages.OutToolEnd)
	snap := tr.Snapshot("u1")
	if snap.ToolCount != 0 {
		t.Fatalf("OutToolEnd should not count: ToolCount = %d", snap.ToolCount)
	}
	if snap.LastBeatAt.IsZero() {
		t.Fatal("LastBeatAt still expected to refresh on OutToolEnd")
	}
}

// TestObserve_ReplyNoCount ensures OutReply chunks (streaming
// reply deltas) do NOT inflate the visible counter — that
// would be the "others bucket inflates to hundreds" failure mode
// called out in F-63 §2 非目标.
func TestObserve_ReplyNoCount(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	for i := 0; i < 50; i++ {
		tr.Observe("u1", messages.OutReply)
	}
	snap := tr.Snapshot("u1")
	if snap.ThinkCount != 0 || snap.ToolCount != 0 {
		t.Fatalf("OutReply must not count: snap=%+v", snap)
	}
}

// TestObserve_ResultNoCount — OutResult is one-per-turn, not a
// counter.
func TestObserve_ResultNoCount(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	changed := tr.Observe("u1", messages.OutResult)
	if changed {
		t.Fatal("OutResult should return changed=false")
	}
}

// TestObserve_ErrorNoCount — OutError is rare and one-shot;
// counter semantics would be misleading.
func TestObserve_ErrorNoCount(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	if changed := tr.Observe("u1", messages.OutError); changed {
		t.Fatal("OutError should return changed=false")
	}
}

// TestObserve_AllKindsRefreshLastBeat pins the "any activity
// keeps the clock alive" guarantee. Every OutboundKind — even
// the non-counting ones — must refresh LastBeatAt so the user
// sees ⏱ keep moving during streaming reply chunks.
func TestObserve_AllKindsRefreshLastBeat(t *testing.T) {
	kinds := []messages.OutboundKind{
		messages.OutReply,
		messages.OutToolStart,
		messages.OutToolEnd,
		messages.OutThinking,
		messages.OutMessageState,
		messages.OutMessageStateRemoved,
		messages.OutCard,
		messages.OutResult,
		messages.OutInit,
		messages.OutCommandReply,
		messages.OutTaskCreate,
		messages.OutTaskUpdate,
		messages.OutCardPatch,
		messages.OutError,
		messages.OutHeartbeat,
	}
	for _, k := range kinds {
		t.Run(k.String(), func(t *testing.T) {
			tr := NewHeartbeatTracker(0)
			tr.Observe("u1", k)
			snap := tr.Snapshot("u1")
			if snap.LastBeatAt.IsZero() {
				t.Fatalf("kind %s did not refresh LastBeatAt", k.String())
			}
		})
	}
}

// TestObserve_LastBeatAlone covers the case where a kind is
// non-counting but still observed: changed must be false so the
// caller doesn't fire a useless OutHeartbeat.
func TestObserve_LastBeatAlone(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	if changed := tr.Observe("u1", messages.OutReply); changed {
		t.Fatal("OutReply-only observe should return changed=false")
	}
	if changed := tr.Observe("u1", messages.OutReply); changed {
		t.Fatal("repeat OutReply observe should still return changed=false")
	}
}

// TestObserve_OutHeartbeatIgnored covers the defensive path: if
// Observe is somehow called with OutHeartbeat (shouldn't happen
// in production — the handler emits OutHeartbeat via em.Send,
// not via Observe), the call is a no-op. Returns false (no
// double-counting).
func TestObserve_OutHeartbeatIgnored(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	if changed := tr.Observe("u1", messages.OutHeartbeat); changed {
		t.Fatal("OutHeartbeat should never report changed=true")
	}
	snap := tr.Snapshot("u1")
	if snap.ThinkCount != 0 || snap.ToolCount != 0 {
		t.Fatalf("OutHeartbeat must not count: snap=%+v", snap)
	}
	if snap.LastBeatAt.IsZero() {
		t.Fatal("LastBeatAt still expected to refresh")
	}
}

// TestObserve_EmptyUserMsgNoOp covers the orphan-event guard:
// userMsgID == "" is a no-op (returns false). This protects
// against EventAgentReady / EventAgentDone events that don't
// anchor to a receipt.
func TestObserve_EmptyUserMsgNoOp(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	if changed := tr.Observe("", messages.OutThinking); changed {
		t.Fatal("empty userMsgID should return changed=false")
	}
	if tr.Len() != 0 {
		t.Fatalf("empty userMsgID must not allocate a snapshot: Len=%d", tr.Len())
	}
}

// TestObserve_NilTrackerSafe covers the nil-receiver path
// (test fakes that bypass New() may produce a nil heartbeat).
// Both Observe and Snapshot must not panic.
func TestObserve_NilTrackerSafe(t *testing.T) {
	var tr *HeartbeatTracker
	if changed := tr.Observe("u1", messages.OutThinking); changed {
		t.Fatal("nil tracker should return changed=false")
	}
	if snap := tr.Snapshot("u1"); !snap.Empty() {
		t.Fatalf("nil tracker snapshot should be empty, got %+v", snap)
	}
}

// TestObserve_LRUEvicts covers the eviction boundary: writing
// cap+1 distinct userMsgIDs evicts the oldest.
func TestObserve_LRUEvicts(t *testing.T) {
	const cap = 4
	tr := NewHeartbeatTracker(cap)

	// Fill to capacity.
	for i := 0; i < cap; i++ {
		uid := userMsgIDForIndex(i)
		tr.Observe(uid, messages.OutThinking)
	}
	if tr.Len() != cap {
		t.Fatalf("Len after fill = %d, want %d", tr.Len(), cap)
	}

	// Add one more — should evict the first ("u0").
	tr.Observe(userMsgIDForIndex(cap), messages.OutThinking)
	if tr.Len() != cap {
		t.Fatalf("Len after overflow = %d, want %d", tr.Len(), cap)
	}

	// "u0" should be gone.
	if snap := tr.Snapshot(userMsgIDForIndex(0)); !snap.Empty() {
		t.Fatalf("expected u0 to be evicted, got %+v", snap)
	}
	// "u4" should be present.
	if snap := tr.Snapshot(userMsgIDForIndex(cap)); snap.ThinkCount != 1 {
		t.Fatalf("u4 should be present with ThinkCount=1, got %+v", snap)
	}
}

// TestObserve_LRUTouchUpdates covers the "recently used" path:
// touching an existing userMsgID moves it to the head so it
// doesn't get evicted by subsequent writes to other uids.
func TestObserve_LRUTouchUpdates(t *testing.T) {
	const cap = 3
	tr := NewHeartbeatTracker(cap)

	tr.Observe("u0", messages.OutThinking) // head: [u0]
	tr.Observe("u1", messages.OutThinking) // head: [u1, u0]
	tr.Observe("u2", messages.OutThinking) // head: [u2, u1, u0]
	tr.Observe("u0", messages.OutReply)    // head: [u0, u2, u1] — u0 moves up

	// Now write u3 — should evict u1 (the tail), NOT u0.
	tr.Observe("u3", messages.OutThinking) // head: [u3, u0, u2]

	if snap := tr.Snapshot("u1"); !snap.Empty() {
		t.Fatalf("u1 should be evicted (oldest), got %+v", snap)
	}
	if snap := tr.Snapshot("u0"); snap.ThinkCount != 1 {
		t.Fatalf("u0 should survive (recently touched), got %+v", snap)
	}
	if snap := tr.Snapshot("u2"); snap.ThinkCount != 1 {
		t.Fatalf("u2 should survive, got %+v", snap)
	}
	if snap := tr.Snapshot("u3"); snap.ThinkCount != 1 {
		t.Fatalf("u3 should be present, got %+v", snap)
	}
}

// TestObserve_LastBeatReflectsLastEvent — F-63 §7.1 case
// "Observe_MixedTurn_LastBeatReflectsLastEvent". Multiple
// events in sequence; LastBeatAt should be the most recent.
func TestObserve_LastBeatReflectsLastEvent(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	t0 := time.Now()
	tr.Observe("u1", messages.OutThinking)
	first := tr.Snapshot("u1").LastBeatAt

	time.Sleep(2 * time.Millisecond)
	tr.Observe("u1", messages.OutToolStart)
	second := tr.Snapshot("u1").LastBeatAt

	if !second.After(first) {
		t.Fatalf("LastBeatAt not advancing: first=%v second=%v", first, second)
	}
	if second.Before(t0) {
		t.Fatalf("LastBeatAt before observation start: %v < %v", second, t0)
	}
}

// TestObserve_MixedTurn — combined sequence reproduces a real
// turn: 2 thinking + 3 tool starts + 1 tool end + 5 reply chunks
// + 1 result. Counter expectations per F-63 §3.7.
func TestObserve_MixedTurn(t *testing.T) {
	tr := NewHeartbeatTracker(0)

	tr.Observe("u1", messages.OutThinking)
	tr.Observe("u1", messages.OutToolStart)
	tr.Observe("u1", messages.OutThinking)
	tr.Observe("u1", messages.OutToolStart)
	tr.Observe("u1", messages.OutReply)
	tr.Observe("u1", messages.OutToolEnd)
	tr.Observe("u1", messages.OutReply)
	tr.Observe("u1", messages.OutToolStart)
	tr.Observe("u1", messages.OutReply)
	tr.Observe("u1", messages.OutReply)
	tr.Observe("u1", messages.OutReply)
	tr.Observe("u1", messages.OutResult)

	snap := tr.Snapshot("u1")
	if snap.ThinkCount != 2 {
		t.Fatalf("ThinkCount = %d, want 2", snap.ThinkCount)
	}
	if snap.ToolCount != 3 {
		t.Fatalf("ToolCount = %d, want 3", snap.ToolCount)
	}
}

// TestObserve_ConcurrentSafe runs Observe + Snapshot from
// multiple goroutines to validate the mutex. Run with -race.
func TestObserve_ConcurrentSafe(t *testing.T) {
	tr := NewHeartbeatTracker(64)

	const goroutines = 16
	const opsPerGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers.
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				uid := userMsgIDForIndex((g + i) % 32)
				switch i % 3 {
				case 0:
					tr.Observe(uid, messages.OutThinking)
				case 1:
					tr.Observe(uid, messages.OutToolStart)
				case 2:
					tr.Observe(uid, messages.OutReply)
				}
			}
		}(g)
	}

	// Readers.
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				uid := userMsgIDForIndex((g + i) % 32)
				_ = tr.Snapshot(uid)
			}
		}(g)
	}

	wg.Wait()

	// Post-condition: tracker is still bounded by cap (LRU
	// invariant holds under concurrent load).
	if tr.Len() > tr.Cap() {
		t.Fatalf("Len() = %d > Cap() = %d after concurrent load", tr.Len(), tr.Cap())
	}
}

// TestSnapshot_ZeroValueForUnknownUserMsg covers the "no entry
// yet" path: Snapshot returns zero-valued HeartbeatSnapshot
// which the channel adapter's Empty() guard drops.
func TestSnapshot_ZeroValueForUnknownUserMsg(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	snap := tr.Snapshot("never-observed")
	if !snap.Empty() {
		t.Fatalf("snapshot for unknown userMsgID should be Empty, got %+v", snap)
	}
}

// TestSnapshot_DoesNotMutateInternalState — Snapshot must be a
// pure read; mutating the returned copy must not affect future
// Observe results.
func TestSnapshot_DoesNotMutateInternalState(t *testing.T) {
	tr := NewHeartbeatTracker(0)
	tr.Observe("u1", messages.OutThinking)
	snap := tr.Snapshot("u1")
	snap.ThinkCount = 999
	snap.ToolCount = 999
	// Subsequent Snapshot must return the original value.
	again := tr.Snapshot("u1")
	if again.ThinkCount != 1 || again.ToolCount != 0 {
		t.Fatalf("snapshot leaked into tracker: %+v", again)
	}
}

// userMsgIDForIndex returns a deterministic, distinct userMsgID
// string for an integer index. Kept local to the test file so
// the production code doesn't accumulate test-only helpers
// (per F-58-style "no test-only exports in production packages").
func userMsgIDForIndex(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return "u" + string(digits[i:i+1])
	}
	return "u" + string(digits[i/10:i/10+1]) + string(digits[i%10:i%10+1])
}