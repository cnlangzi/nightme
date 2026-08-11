// Package chatsession — MessageQueue.
//
// `MessageQueue` is a FIFO queue of `Message` (by value) with
// built-in barrier segmentation based on `Message.Kind`. It
// replaces the ad-hoc `cs.queue []*Message` slice that lived
// directly on `ChatSession` before the refactor; the slice's
// prefix-reference memory leak and the "always commit-with-n"
// bookkeeping noise were the main motivations for extracting
// this type.
//
// # Model
//
// The queue maintains a singly-linked list plus a single
// `inFlightEnd` cursor that marks the end of the in-flight
// region (exclusive):
//
//	head ─────────── inFlightEnd ──────────── tail
//	      in-flight           pending
//
//   - in-flight: items returned by the most recent `Peek`,
//     awaiting either `Commit` (consumed successfully) or
//     `Rewind` (rolled back). Items stay physically in the list —
//     only the cursor moves.
//   - pending: items still available to the next `Peek`.
//
// When `inFlightEnd == head`, the in-flight region is empty (the
// normal steady state). When `inFlightEnd == nil` and `head !=
// nil`, the entire list is in-flight (no pending items left).
// When both are nil, the queue is empty.
//
// # Push-during-in-flight
//
// If items are pushed after a Peek (when `inFlightEnd` may have
// moved into the middle of the list, or be nil), they MUST be
// pending — not silently merged into the in-flight batch.
// `Push` therefore sets `inFlightEnd` to the new item whenever
// it was nil at entry, ensuring the new item starts in the
// pending region.
//
// # Barrier semantics
//
// `Peek` walks the pending region from `inFlightEnd` forward and
// returns one of:
//
//   - All consecutive `MessageKindNormal` items, up to (but not
//     including) the first `MessageKindQueue`; OR
//   - A single `MessageKindQueue` item, if that's the head of the
//     pending region.
//
// A `MessageKindQueue` at the head of pending always returns as
// a 1-element batch. A `MessageKindQueue` in the middle terminates
// the preceding Normal run but is itself NOT included in that
// batch — the next `Peek` returns it alone. `inFlightEnd` is
// positioned to the Queue message (not past it) so that the
// Queue remains pending for the next batch.
//
// # Value semantics
//
// Messages are stored by value, not by pointer. Callers receive
// a `[]Message` slice from `Peek` / `Clear` that they own and
// may freely mutate (typically to flip `Stage` after Commit, or
// to stamp writeback fields on the prompt's slice). Each
// consumer holds its own copy — there is no shared state across
// the queue, the prompt, and the writeback site, eliminating the
// need for a per-chat `messagesByID` index entirely.
//
// # Concurrency
//
// All operations are safe for concurrent use. Each takes a single
// mutex; the queue is data-only and never calls back into
// `ChatSession` code, so callers may hold `ChatSession.mu` while
// invoking (no nested lock ordering concerns as long as the queue
// is treated as data).
//
// # Capacity
//
// `capacity` is the hard upper bound on items in the queue
// (pending + in-flight combined). `Push` returns `ErrFull` when
// at capacity. `capacity <= 0` means unbounded.
package chatsession

import (
	"errors"
	"sync"
)

// ErrFull is returned by MessageQueue.Push when the queue is at
// its configured capacity. ErrQueueFull is the canonical name
// kept for compatibility with external callers (cmd/nightme/run.go
// and tests) that historically depended on the v1.3 symbol —
// they are the same value, so `err == ErrFull`, `err ==
// ErrQueueFull`, and `errors.Is` all work interchangeably.
var (
	ErrFull      = errors.New("chatsession: message queue full")
	ErrQueueFull = ErrFull
)

// node is a singly-linked list element holding a Message by
// value. The queue never needs prev pointers — every traversal
// is forward from head.
type node struct {
	value Message
	next  *node
}

// MessageQueue is a FIFO of Message with two-phase dequeue and
// Kind-driven barrier segmentation.
//
// Zero value is NOT usable; construct via NewMessageQueue.
type MessageQueue struct {
	mu sync.Mutex

	// Linked list. tail is nil iff head is nil.
	head *node
	tail *node

	// inFlightEnd points to the first PENDING node. Items
	// from head to inFlightEnd (exclusive) are in-flight;
	// items from inFlightEnd to tail (inclusive) are pending.
	// inFlightEnd is nil iff there is no pending region
	// (either the queue is empty, or the entire list is
	// in-flight).
	inFlightEnd *node

	// length is the total count of items in the list
	// (pending + in-flight).
	length int

	// capacity is the hard upper bound on length. 0 = unbounded.
	capacity int
}

// NewMessageQueue returns an empty queue. capacity <= 0 means
// unbounded (no ErrFull ever returned by Push).
func NewMessageQueue(capacity int) *MessageQueue {
	if capacity < 0 {
		capacity = 0
	}
	return &MessageQueue{capacity: capacity}
}

// Peek returns the next batch from the pending region by value
// and advances inFlightEnd past it. The returned slice is a
// fresh copy — the caller owns it and may mutate freely (typical
// use: flip Stage after Commit) even if the queue is mutated
// later.
//
// Returns nil when the pending region is empty (queue empty,
// or entire queue already in-flight awaiting Commit/Rewind).
//
// The batch is:
//   - All consecutive MessageKindNormal items from the pending
//     head, up to (not including) the first MessageKindQueue;
//     OR
//   - A single MessageKindQueue item, if that's the pending head.
//
// The returned items are now in-flight: subsequent Peek calls
// will not return them again until Commit (removes them) or
// Rewind (returns them to pending) is called.
func (q *MessageQueue) Peek() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	start := q.inFlightEnd
	if start == nil {
		return nil
	}

	// Walk the pending region, collecting Normal messages until
	// we hit a Queue message or the tail.
	var batch []Message
	var lastNormal *node
	for n := start; n != nil; n = n.next {
		if n.value.Kind == MessageKindQueue {
			break
		}
		batch = append(batch, n.value)
		lastNormal = n
	}

	if len(batch) > 0 {
		// Normal run. inFlightEnd moves to the node AFTER the
		// last Normal — which may be a Queue (next Peek will
		// return it alone) or nil (no more pending).
		q.inFlightEnd = lastNormal.next
	} else {
		// start itself is a Queue message. Return [start] as
		// a 1-element batch; inFlightEnd moves to its
		// successor.
		batch = []Message{start.value}
		q.inFlightEnd = start.next
	}
	return batch
}

// Push appends msg to the tail of the pending region by value.
// Returns ErrFull if the queue is at capacity. A zero-value
// msg (empty ID) is a no-op (returns nil).
//
// If the queue is fully in-flight (inFlightEnd == nil and the
// list is non-empty), the new item becomes the new pending head
// — i.e. inFlightEnd is set to the new item so subsequent Peek
// calls return it.
func (q *MessageQueue) Push(msg Message) error {
	if msg.ID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.capacity > 0 && q.length >= q.capacity {
		return ErrFull
	}
	n := &node{value: msg}
	if q.tail == nil {
		// First item in the queue. head and inFlightEnd both
		// start at n.
		q.head = n
		q.inFlightEnd = n
	} else {
		// Append after tail. If the queue was fully in-flight
		// (inFlightEnd == nil), the new item becomes the new
		// pending head — set inFlightEnd to it so the next
		// Peek returns it.
		q.tail.next = n
		if q.inFlightEnd == nil {
			q.inFlightEnd = n
		}
	}
	q.tail = n
	q.length++
	return nil
}

// PushFront inserts msg at the head of the pending region by value.
// Returns ErrFull if the queue is at capacity. A zero-value
// msg (empty ID) is a no-op (returns nil).
//
// Used by /steer: when the user wants a new message to take
// priority over anything else already queued, it lands at the
// head so the next Peek returns it first.
//
// Edge cases:
//   - Empty queue: head/tail/inFlightEnd all point at n. Same
//     shape as Push on an empty queue.
//   - Queue fully in-flight (inFlightEnd == nil, list non-empty):
//     in-flight items stay in-flight; n is appended at tail and
//     inFlightEnd moves to n. This is functionally equivalent to
//     Push's same edge case — the steer message will be the
//     first (and only) thing the agent sees on the next turn.
//     Prepending at head would break the invariant that
//     inFlightEnd points at the first pending item, since the
//     entire pre-existing list is in-flight (no pending items
//     to prepend before). Appending is the correct shape.
//   - Mixed (inFlightEnd != nil): n is inserted before
//     inFlightEnd; inFlightEnd is updated to n so it remains
//     the first pending item.
func (q *MessageQueue) PushFront(msg Message) error {
	if msg.ID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.capacity > 0 && q.length >= q.capacity {
		return ErrFull
	}
	n := &node{value: msg}
	if q.head == nil {
		// Empty list. head/tail/inFlightEnd all start at n.
		q.head = n
		q.tail = n
		q.inFlightEnd = n
	} else if q.inFlightEnd == nil {
		// Entire list is in-flight. Append at tail and set
		// inFlightEnd to n (the new item is the only pending
		// one). Same shape as Push's all-in-flight edge case.
		q.tail.next = n
		q.tail = n
		q.inFlightEnd = n
	} else {
		// Insert n before inFlightEnd; move inFlightEnd to n
		// so the invariant ("items from inFlightEnd inclusive
		// are pending") is preserved.
		if q.inFlightEnd == q.head {
			// inFlightEnd IS the head. Insert at head (no
			// predecessor to update in a singly-linked list).
			n.next = q.head
			q.head = n
		} else {
			// Find inFlightEnd's predecessor. Length-bounded
			// walk: if the invariant (inFlightEnd is reachable
			// from head via .next) ever breaks, this loop would
			// spin forever. Cap at q.length so a corrupted
			// invariant fails loud (panic) instead of hanging
			// the caller.
			prev := q.head
			steps := 0
			for prev.next != q.inFlightEnd {
				prev = prev.next
				steps++
				if steps > q.length {
					panic("MessageQueue.PushFront: inFlightEnd not reachable from head (broken invariant)")
				}
			}
			n.next = q.inFlightEnd
			prev.next = n
		}
		q.inFlightEnd = n
	}
	q.length++
	return nil
}

// Commit removes the in-flight region — everything from head up
// to inFlightEnd (exclusive). The freed nodes are unlinked and
// their .next pointer cleared to allow prompt GC. No-op if
// there is no in-flight region.
func (q *MessageQueue) Commit() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.head == q.inFlightEnd {
		// Either empty, or no in-flight region.
		return
	}
	removed := 0
	cur := q.head
	// Walk from head toward inFlightEnd. Guard against nil so
	// we don't dereference a nil .next when the entire list is
	// in-flight (inFlightEnd == nil).
	for cur != nil && cur != q.inFlightEnd {
		next := cur.next
		cur.next = nil // help GC
		cur = next
		removed++
	}
	q.head = q.inFlightEnd
	if q.head == nil {
		q.tail = nil
	}
	q.length -= removed
	// Note: we do NOT reset q.inFlightEnd. The pending region
	// (if any) keeps its position; the in-flight region is
	// just gone.
}

// Rewind rolls back the most recent Peek. The in-flight region
// is returned to pending by moving inFlightEnd back to head.
// Items are NOT physically moved. No-op if there is no
// in-flight region.
//
// Use after a failed submission: the items are still in the
// queue and will be returned by the next Peek.
func (q *MessageQueue) Rewind() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.head == q.inFlightEnd {
		return
	}
	q.inFlightEnd = q.head
}

// Remove physically removes the item with the given ID, whether
// it lives in the pending or in-flight region. Returns the
// removed value and true on success; the zero Message and false
// if no item with that ID exists.
//
// Adjusts head / tail / inFlightEnd as needed for the removed
// node's position.
func (q *MessageQueue) Remove(id string) (Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var prev *node
	for n := q.head; n != nil; prev, n = n, n.next {
		if n.value.ID != id {
			continue
		}
		// Unlink n.
		if prev == nil {
			q.head = n.next
		} else {
			prev.next = n.next
		}
		if n == q.tail {
			q.tail = prev
		}
		if n == q.inFlightEnd {
			// Removed the pending head. Advance inFlightEnd to
			// its successor so the next Peek sees the right
			// region. (Could be nil if we removed the tail of
			// pending.)
			q.inFlightEnd = n.next
		}
		n.next = nil // help GC
		q.length--
		return n.value, true
	}
	return Message{}, false
}

// Clear removes every item — pending and in-flight — and returns
// them in their original order. The returned slice is freshly
// allocated; the queue is empty after the call. Used by
// ChatSession.DropQueue (for /close, /new) to clear the queue
// while still obtaining the items for per-message MarkDropped
// wire events.
func (q *MessageQueue) Clear() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.head == nil {
		return nil
	}
	out := make([]Message, 0, q.length)
	for n := q.head; n != nil; {
		next := n.next
		n.next = nil // help GC
		out = append(out, n.value)
		n = next
	}
	q.head = nil
	q.tail = nil
	q.inFlightEnd = nil
	q.length = 0
	return out
}

// Len returns the total number of items in the queue
// (pending + in-flight).
func (q *MessageQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.length
}
