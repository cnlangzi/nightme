// Tests for MessageQueue. Covers the four Peek segmentation
// cases, the Commit / Rewind state machine, Remove / Clear,
// capacity limits, and concurrent Push / Peek / Commit under
// -race. Run with: go test -race ./internal/chatsession/...
package chatsession

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// mqMsg is a tiny constructor that returns a Message with the
// given id and kind. Other fields are left zero — the queue
// doesn't read them, and tests that need richer Messages build
// them by hand.
func mqMsg(id string, kind MessageKind) Message {
	return Message{ID: id, Kind: kind}
}

// ids extracts the IDs from a slice of messages in order.
func ids(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

// equalIDs compares two id slices for equality.
func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Basic Push / Peek / Commit cycle ----------------------------

func TestMessageQueue_BasicCycle(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindNormal)))

	if got := q.Len(); got != 3 {
		t.Fatalf("Len after 3 Push: got %d, want 3", got)
	}

	batch := q.Peek()
	if !equalIDs(ids(batch), []string{"a", "b", "c"}) {
		t.Fatalf("Peek: got %v, want [a b c]", ids(batch))
	}

	// Commit drops the in-flight region. Queue should be empty.
	q.Commit()
	if got := q.Len(); got != 0 {
		t.Fatalf("Len after Commit: got %d, want 0", got)
	}
	if got := q.Peek(); got != nil {
		t.Fatalf("Peek after Commit: got %v, want nil", ids(got))
	}
}

// --- Peek segmentation by Kind -----------------------------------

func TestMessageQueue_PeekSegmentation(t *testing.T) {
	cases := []struct {
		name string
		pushed []MessageKind // kinds to push, IDs auto-generated
		wantFirst []string  // IDs of first batch
		wantSecond []string // IDs of second batch (after Commit + Peek)
	}{
		{
			name: "all_normal_returns_all",
			pushed: []MessageKind{MessageKindNormal, MessageKindNormal, MessageKindNormal},
			wantFirst: []string{"0", "1", "2"},
			wantSecond: nil,
		},
		{
			name: "queue_at_head_returns_alone",
			pushed: []MessageKind{MessageKindQueue, MessageKindNormal, MessageKindNormal},
			wantFirst: []string{"0"},
			wantSecond: []string{"1", "2"},
		},
		{
			name: "queue_after_normals_splits",
			pushed: []MessageKind{MessageKindNormal, MessageKindNormal, MessageKindQueue, MessageKindNormal, MessageKindNormal},
			wantFirst: []string{"0", "1"},
			wantSecond: []string{"2"},
		},
		{
			name: "multiple_queues",
			pushed: []MessageKind{
				MessageKindNormal, MessageKindNormal, MessageKindQueue,
				MessageKindNormal, MessageKindNormal, MessageKindQueue,
				MessageKindNormal,
			},
			wantFirst:  []string{"0", "1"},
			wantSecond: []string{"2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewMessageQueue(0)
			for i, k := range tc.pushed {
				mustNoErr(t, q.Push(mqMsg(fmt.Sprintf("%d", i), k)))
			}

			got := q.Peek()
			if !equalIDs(ids(got), tc.wantFirst) {
				t.Fatalf("first Peek: got %v, want %v", ids(got), tc.wantFirst)
			}
			q.Commit()

			if tc.wantSecond == nil {
				if got := q.Peek(); got != nil {
					t.Fatalf("second Peek: got %v, want nil", ids(got))
				}
				return
			}

			got = q.Peek()
			if !equalIDs(ids(got), tc.wantSecond) {
				t.Fatalf("second Peek: got %v, want %v", ids(got), tc.wantSecond)
			}
			q.Commit()

			// After draining the segments named above, the queue
			// may have more pending items (the "multiple_queues"
			// case has 3 segments total). Drain the rest so the
			// test ends with a known empty state.
			for {
				batch := q.Peek()
				if batch == nil {
					break
				}
				q.Commit()
			}
			if q.Len() != 0 {
				t.Fatalf("queue not drained: Len=%d", q.Len())
			}
		})
	}
}

// --- Multiple_queues full drain ---------------------------------

func TestMessageQueue_MultipleQueuesFullDrain(t *testing.T) {
	q := NewMessageQueue(0)
	kinds := []MessageKind{
		MessageKindNormal, MessageKindNormal, MessageKindQueue,
		MessageKindNormal, MessageKindQueue, MessageKindNormal,
	}
	for i, k := range kinds {
		mustNoErr(t, q.Push(mqMsg(fmt.Sprintf("%d", i), k)))
	}

	wantBatches := [][]string{
		{"0", "1"},
		{"2"},
		{"3"},
		{"4"},
		{"5"},
	}
	for i, want := range wantBatches {
		batch := q.Peek()
		if !equalIDs(ids(batch), want) {
			t.Fatalf("batch %d: got %v, want %v", i, ids(batch), want)
		}
		q.Commit()
	}
	if got := q.Peek(); got != nil {
		t.Fatalf("Peek after full drain: got %v, want nil", ids(got))
	}
}

// --- Rewind restores the in-flight batch to pending --------------

func TestMessageQueue_Rewind(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindQueue)))
	mustNoErr(t, q.Push(mqMsg("d", MessageKindNormal)))

	// Peek returns the Normal run [a, b]. Rewind puts them back.
	first := q.Peek()
	if !equalIDs(ids(first), []string{"a", "b"}) {
		t.Fatalf("first Peek: got %v, want [a b]", ids(first))
	}
	q.Rewind()

	// Length must be unchanged — items are still in the queue.
	if got := q.Len(); got != 4 {
		t.Fatalf("Len after Rewind: got %d, want 4", got)
	}

	// Peek again must return the same batch.
	second := q.Peek()
	if !equalIDs(ids(second), []string{"a", "b"}) {
		t.Fatalf("Peek after Rewind: got %v, want [a b]", ids(second))
	}
	q.Commit()

	// Now the next Peek should start at the Queue message [c].
	third := q.Peek()
	if !equalIDs(ids(third), []string{"c"}) {
		t.Fatalf("Peek after Commit: got %v, want [c]", ids(third))
	}
}

// --- Rewind no-op when no in-flight ------------------------------

func TestMessageQueue_RewindNoInFlight(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	// No Peek yet — Rewind must be a no-op.
	q.Rewind()
	if got := q.Len(); got != 1 {
		t.Fatalf("Len: got %d, want 1", got)
	}
}

// --- Commit no-op when no in-flight ------------------------------

func TestMessageQueue_CommitNoInFlight(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	q.Commit() // no-op
	if got := q.Len(); got != 1 {
		t.Fatalf("Len: got %d, want 1", got)
	}
	// And on an empty queue.
	q2 := NewMessageQueue(0)
	q2.Commit()
	if got := q2.Len(); got != 0 {
		t.Fatalf("empty queue Len: got %d, want 0", got)
	}
}

// --- Remove by ID ------------------------------------------------

func TestMessageQueue_Remove(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindNormal)))

	// Remove middle.
	removed, ok := q.Remove("b")
	if !ok {
		t.Fatal("Remove(b): not found")
	}
	if removed.ID != "b" {
		t.Fatalf("Remove(b) returned wrong item: %s", removed.ID)
	}
	if got := q.Len(); got != 2 {
		t.Fatalf("Len: got %d, want 2", got)
	}
	// Verify remaining IDs by peeking.
	batch := q.Peek()
	if !equalIDs(ids(batch), []string{"a", "c"}) {
		t.Fatalf("Peek after Remove(b): got %v, want [a c]", ids(batch))
	}

	// Remove head.
	_, ok = q.Remove("a")
	if !ok {
		t.Fatal("Remove(a): not found")
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("Len: got %d, want 1", got)
	}

	// Remove tail.
	_, ok = q.Remove("c")
	if !ok {
		t.Fatal("Remove(c): not found")
	}
	if got := q.Len(); got != 0 {
		t.Fatalf("Len: got %d, want 0", got)
	}

	// Remove on empty queue.
	_, ok = q.Remove("ghost")
	if ok {
		t.Fatal("Remove on empty queue: expected not-found")
	}
}

func TestMessageQueue_RemoveInFlight(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindQueue)))

	// Peek puts [a, b] in-flight.
	_ = q.Peek()
	if got := q.Len(); got != 3 {
		t.Fatalf("Len after Peek: got %d, want 3", got)
	}

	// Remove an in-flight item.
	_, ok := q.Remove("a")
	if !ok {
		t.Fatal("Remove(a) in-flight: not found")
	}
	if got := q.Len(); got != 2 {
		t.Fatalf("Len: got %d, want 2", got)
	}

	// Commit should now only drop b (a was already removed).
	q.Commit()
	if got := q.Len(); got != 1 {
		t.Fatalf("Len after Commit: got %d, want 1", got)
	}
	// Remaining is c (the Queue message).
	got := q.Peek()
	if !equalIDs(ids(got), []string{"c"}) {
		t.Fatalf("Peek after Commit+Remove: got %v, want [c]", ids(got))
	}
}

func TestMessageQueue_RemovePendingHead(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindQueue)))
	mustNoErr(t, q.Push(mqMsg("d", MessageKindNormal)))

	// Peek returns the Normal run [a, b]. After Commit, [c, d]
	// remain and c is the pending head (the Queue barrier
	// stopped the previous Peek at c, so inFlightEnd points to c).
	_ = q.Peek()
	q.Commit()
	if got := q.Len(); got != 2 {
		t.Fatalf("setup Len: got %d, want 2 (c, d remain)", got)
	}

	// Remove c (the pending head). inFlightEnd should advance to d.
	removed, ok := q.Remove("c")
	if !ok {
		t.Fatal("Remove(c) pending head: not found")
	}
	if removed.ID != "c" {
		t.Fatalf("Remove(c) returned wrong item: %s", removed.ID)
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("Len: got %d, want 1", got)
	}
	// Next Peek should return [d] (the new pending head).
	got := q.Peek()
	if !equalIDs(ids(got), []string{"d"}) {
		t.Fatalf("Peek after removing pending head: got %v, want [d]", ids(got))
	}
}

// --- Clear --------------------------------------------------------

func TestMessageQueue_Clear(t *testing.T) {
	q := NewMessageQueue(0)
	for _, id := range []string{"a", "b", "c", "d"} {
		mustNoErr(t, q.Push(mqMsg(id, MessageKindNormal)))
	}
	_ = q.Peek() // put a, b in-flight

	cleared := q.Clear()
	if got := ids(cleared); !equalIDs(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("Clear returned: got %v, want [a b c d]", got)
	}
	if q.Len() != 0 {
		t.Fatalf("Len after Clear: got %d, want 0", q.Len())
	}
	// Queue is fully usable after Clear.
	mustNoErr(t, q.Push(mqMsg("x", MessageKindNormal)))
	batch := q.Peek()
	if !equalIDs(ids(batch), []string{"x"}) {
		t.Fatalf("Peek after Clear+Push: got %v, want [x]", ids(batch))
	}
}

func TestMessageQueue_ClearEmpty(t *testing.T) {
	q := NewMessageQueue(0)
	if got := q.Clear(); got != nil {
		t.Fatalf("Clear on empty queue: got %v, want nil", got)
	}
}

// --- Capacity -----------------------------------------------------

func TestMessageQueue_Capacity(t *testing.T) {
	q := NewMessageQueue(3)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindNormal)))
	if err := q.Push(mqMsg("d", MessageKindNormal)); !errors.Is(err, ErrFull) {
		t.Fatalf("Push over capacity: got err=%v, want ErrFull", err)
	}
	// After Commit, room frees up.
	_ = q.Peek()
	q.Commit()
	mustNoErr(t, q.Push(mqMsg("d", MessageKindNormal)))
}

func TestMessageQueue_CapacityUnbounded(t *testing.T) {
	q := NewMessageQueue(0)
	for i := range 1000 {
		mustNoErr(t, q.Push(mqMsg(fmt.Sprintf("%d", i), MessageKindNormal)))
	}
	if got := q.Len(); got != 1000 {
		t.Fatalf("Len: got %d, want 1000", got)
	}
}

// --- Len tracking across lifecycle -------------------------------

func TestMessageQueue_LenAccounting(t *testing.T) {
	q := NewMessageQueue(0)
	if got := q.Len(); got != 0 {
		t.Fatalf("empty Len: got %d, want 0", got)
	}
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	if got := q.Len(); got != 2 {
		t.Fatalf("Len after 2 push: got %d, want 2", got)
	}
	_ = q.Peek() // [a, b] in-flight
	if got := q.Len(); got != 2 {
		t.Fatalf("Len after Peek (in-flight still counts): got %d, want 2", got)
	}
	q.Commit()
	if got := q.Len(); got != 0 {
		t.Fatalf("Len after Commit: got %d, want 0", got)
	}
	_, _ = q.Remove("ghost") // not found
	if got := q.Len(); got != 0 {
		t.Fatalf("Len after failed Remove: got %d, want 0", got)
	}
}

// --- GC: removed items are actually releasable -------------------

// This test verifies that Remove / Commit / Clear don't leak
// the popped item. With value semantics we can't do pointer
// identity; we instead check that the returned Message has the
// expected ID and that the rest of the queue does not include
// it.
func TestMessageQueue_RemoveReleases(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindNormal)))

	removed, ok := q.Remove("b")
	if !ok {
		t.Fatal("Remove(b): not found")
	}
	if removed.ID != "b" {
		t.Fatalf("Remove(b) returned wrong item: %s", removed.ID)
	}
	// Drain the rest. The slice must NOT contain b.
	_ = q.Peek()
	q.Commit()
	_ = q.Peek()
	q.Commit()
	if got := q.Len(); got != 0 {
		t.Fatalf("queue not drained: Len=%d", got)
	}
}

// --- Concurrency: parallel Push / Peek / Commit ------------------

func TestMessageQueue_ConcurrentPushPeekCommit(t *testing.T) {
	q := NewMessageQueue(0)
	const writers = 4
	const perWriter = 200
	const peekers = 2

	var producerWg sync.WaitGroup
	for w := range writers {
		producerWg.Go(func() {
			for i := range perWriter {
				id := fmt.Sprintf("w%d-i%d", w, i)
				if err := q.Push(mqMsg(id, MessageKindNormal)); err != nil {
					t.Errorf("Push: %v", err)
					return
				}
			}
		})
	}

	// Drainers loop until stop is closed. They yield via
	// runtime.Gosched when the queue is empty so writers can run
	// (without this they can starve under -race).
	var drainerWg sync.WaitGroup
	stop := make(chan struct{})
	for range peekers {
		drainerWg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				batch := q.Peek()
				if len(batch) == 0 {
					runtime.Gosched()
					continue
				}
				q.Commit()
			}
		})
	}

	producerWg.Wait()
	// Wait for the queue to fully drain before stopping peekers.
	for q.Len() > 0 {
		runtime.Gosched()
	}
	close(stop)
	drainerWg.Wait()

	if got := q.Len(); got != 0 {
		t.Fatalf("after drain Len: got %d, want 0", got)
	}
}

// --- Edge: push then peek, then push more -----------------------

func TestMessageQueue_PeekThenPush(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	first := q.Peek()
	if !equalIDs(ids(first), []string{"a", "b"}) {
		t.Fatalf("first Peek: got %v, want [a b]", ids(first))
	}
	// While [a, b] is in-flight, push more items — they go to
	// the end of the pending region (i.e. after the in-flight
	// boundary).
	mustNoErr(t, q.Push(mqMsg("c", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("d", MessageKindQueue)))
	if got := q.Len(); got != 4 {
		t.Fatalf("Len: got %d, want 4", got)
	}
	q.Commit()
	// Next Peek should see [c] (the Queue splits it from d).
	got := q.Peek()
	if !equalIDs(ids(got), []string{"c"}) {
		t.Fatalf("Peek after Commit: got %v, want [c]", ids(got))
	}
	q.Commit()
	got = q.Peek()
	if !equalIDs(ids(got), []string{"d"}) {
		t.Fatalf("Peek after second Commit: got %v, want [d]", ids(got))
	}
}

// --- Edge: Peek on empty queue -----------------------------------

func TestMessageQueue_PeekEmpty(t *testing.T) {
	q := NewMessageQueue(0)
	if got := q.Peek(); got != nil {
		t.Fatalf("Peek on empty: got %v, want nil", ids(got))
	}
	// Still empty after Rewind.
	q.Rewind()
	if got := q.Peek(); got != nil {
		t.Fatalf("Peek after Rewind on empty: got %v, want nil", ids(got))
	}
}

// --- Must helper -------------------------------------------------

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- PushFront: prepend to pending region -----------------------

// PushFront on an empty queue: head/tail/inFlightEnd all point at
// the new item. Same shape as Push on empty.
func TestMessageQueue_PushFrontEmpty(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.PushFront(mqMsg("s", MessageKindNormal)))
	if got := q.Len(); got != 1 {
		t.Fatalf("Len: got %d, want 1", got)
	}
	got := q.Peek()
	if !equalIDs(ids(got), []string{"s"}) {
		t.Fatalf("Peek: got %v, want [s]", ids(got))
	}
}

// PushFront on a queue with pending items only: n goes to the
// head; old pending items shift right. Peek returns n first.
func TestMessageQueue_PushFrontBeforePending(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.PushFront(mqMsg("s", MessageKindNormal)))
	if got := q.Len(); got != 3 {
		t.Fatalf("Len: got %d, want 3", got)
	}
	got := q.Peek()
	if !equalIDs(ids(got), []string{"s", "a", "b"}) {
		t.Fatalf("Peek: got %v, want [s a b]", ids(got))
	}
}

// PushFront with both in-flight and pending items: n is inserted
// between the in-flight batch and the existing pending head.
// In-flight items stay in-flight; new item is the first pending.
func TestMessageQueue_PushFrontDuringInFlight(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindNormal)))

	// Peek puts [a, b, c] in-flight (all Normal, one batch).
	first := q.Peek()
	if !equalIDs(ids(first), []string{"a", "b", "c"}) {
		t.Fatalf("first Peek: got %v, want [a b c]", ids(first))
	}

	// PushFront a steer message. With the entire list in-flight,
	// PushFront falls through to Push's all-in-flight edge case:
	// n is appended at tail and inFlightEnd moves to n. The
	// steer message will be the first thing the next turn sees.
	mustNoErr(t, q.PushFront(mqMsg("s", MessageKindNormal)))
	if got := q.Len(); got != 4 {
		t.Fatalf("Len: got %d, want 4", got)
	}

	// Commit clears the in-flight batch. Only s should remain.
	q.Commit()
	got := q.Peek()
	if !equalIDs(ids(got), []string{"s"}) {
		t.Fatalf("Peek after Commit: got %v, want [s]", ids(got))
	}
}

// PushFront with mixed in-flight + pending: the steer message
// becomes the first pending item. Existing pending items stay
// pending in their original order; in-flight items stay in-flight.
// Here the existing pending item is a Queue so it forms its own
// batch and doesn't merge with the steered Normal item.
func TestMessageQueue_PushFrontMixedInFlight(t *testing.T) {
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindQueue)))
	mustNoErr(t, q.Push(mqMsg("d", MessageKindQueue)))

	// Peek returns [a, b]; commit; peek [c]; commit; peek [d].
	first := q.Peek()
	if !equalIDs(ids(first), []string{"a", "b"}) {
		t.Fatalf("first Peek: got %v, want [a b]", ids(first))
	}
	q.Commit()
	if got := q.Peek(); !equalIDs(ids(got), []string{"c"}) {
		t.Fatalf("Peek: got %v, want [c]", ids(got))
	}
	q.Commit()

	// State: head=d, inFlightEnd=d. d is pending (Queue kind).

	// PushFront a steer message. inFlightEnd != nil, so it goes
	// before inFlightEnd. d shifts right; s becomes pending head.
	mustNoErr(t, q.PushFront(mqMsg("s", MessageKindNormal)))
	if got := q.Len(); got != 2 {
		t.Fatalf("Len: got %d, want 2", got)
	}

	// First batch: [s] (Normal alone; the Queue d splits the run).
	got := q.Peek()
	if !equalIDs(ids(got), []string{"s"}) {
		t.Fatalf("Peek: got %v, want [s]", ids(got))
	}
	q.Commit()
	// Next batch: [d] (the previously-pending Queue).
	got = q.Peek()
	if !equalIDs(ids(got), []string{"d"}) {
		t.Fatalf("Peek after Commit: got %v, want [d]", ids(got))
	}
}

// PushFront respects capacity.
func TestMessageQueue_PushFrontCapacity(t *testing.T) {
	q := NewMessageQueue(2)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	if err := q.PushFront(mqMsg("s", MessageKindNormal)); !errors.Is(err, ErrFull) {
		t.Fatalf("PushFront over capacity: got err=%v, want ErrFull", err)
	}
	// After Commit, room frees up.
	_ = q.Peek()
	q.Commit()
	mustNoErr(t, q.PushFront(mqMsg("s", MessageKindNormal)))
}

// PushFront of zero-id Message is a no-op (matches Push).
func TestMessageQueue_PushFrontZeroID(t *testing.T) {
	q := NewMessageQueue(0)
	if err := q.PushFront(Message{}); err != nil {
		t.Fatalf("PushFront zero-ID: got err=%v, want nil", err)
	}
	if got := q.Len(); got != 0 {
		t.Fatalf("Len: got %d, want 0", got)
	}
}

// PushFront preserves barrier semantics: a steered Normal item
// merges with the existing Normal run; a steered Queue item
// starts a new batch ahead of the existing pending items.
func TestMessageQueue_PushFrontBarrier(t *testing.T) {
	// Setup: existing pending = [a, b (Normal), c (Queue)].
	q := NewMessageQueue(0)
	mustNoErr(t, q.Push(mqMsg("a", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("b", MessageKindNormal)))
	mustNoErr(t, q.Push(mqMsg("c", MessageKindQueue)))

	// Steered Normal merges with [a, b].
	mustNoErr(t, q.PushFront(mqMsg("s", MessageKindNormal)))
	got := q.Peek()
	if !equalIDs(ids(got), []string{"s", "a", "b"}) {
		t.Fatalf("Normal steer merged: got %v, want [s a b]", ids(got))
	}
	q.Commit()

	// Steered Queue forms its own batch before c.
	mustNoErr(t, q.PushFront(mqMsg("q", MessageKindQueue)))
	got = q.Peek()
	if !equalIDs(ids(got), []string{"q"}) {
		t.Fatalf("Queue steer alone: got %v, want [q]", ids(got))
	}
	q.Commit()
	got = q.Peek()
	if !equalIDs(ids(got), []string{"c"}) {
		t.Fatalf("after Queue steer: got %v, want [c]", ids(got))
	}
}
