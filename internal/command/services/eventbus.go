// Package services — EventBus[T] (F-54).
//
// `EventBus[T]` is a generic in-process pub/sub. Zero business coupling —
// the consumer instantiates one Bus per event type (e.g.
// `*Bus[chatsession.MessageStateEvent]`) and the type parameter
// provides the domain vocabulary. Subscribe / Publish / Clear / Close
// are uniform across every event kind.
//
// Why this exists: replaces the three single-observer callback fields
// on ChatSession (eventHandler / onMessageState / onPromptEnd), which
// were last-wins, single-subscriber, and had distinct parameter
// shapes per event kind. See docs/feat/F-54-event-bus.md for the full
// motivation, invariants, and migration steps.
//
// Semantics:
//
//   - Handlers fire in registration order.
//   - First handler returning true stops the chain (consumed = stop
//     later handlers from running).
//   - Per-handler panic is recovered and logged; the chain continues
//     (consumed = false on panic).
//   - Publish on a nil / closed Bus is a no-op.
//   - Subscribe from inside a handler is safe (Publish snapshots
//     under lock, then invokes outside the lock).
//   - Unsubscribe from inside a handler is safe (entry removed after
//     the handler returns, not before).
//   - Clear from inside a handler is NOT safe: it mutates the slice
//     the Publish snapshot was just copied from and would deadlock on
//     the bus mutex.
//
// Use this instead of:
//
//   - A single func field with a Set…Handler() (last-wins, no
//     fan-out, no unsubscribe).
//   - Go channels + select (no panic isolation, no unsubscribe).
//   - External pub/sub libs (overkill for in-process, in-memory).
package services

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// EventBus[T] is a typed in-process pub/sub. See package doc for full
// semantics. Construct with NewEventBus[T]().
type EventBus[T any] struct {
	mu       sync.Mutex
	handlers []busEntry[T]
	closed   atomic.Bool
}

// busEntry pairs a handler with a process-unique id so unsubscribe
// is O(1) and survives in-handler unsubscribes (we look up by id,
// not by pointer, so the entry value can be copied safely).
type busEntry[T any] struct {
	id uint64
	fn func(T) bool
}

// Handler[T] is the typed callback. Return true to consume (later
// handlers do not run); false to pass through.
type Handler[T any] func(T) bool

// NewEventBus returns an empty EventBus. Pair with `defer b.Close()`
// if the owning object has a finite lifetime; otherwise the bus is
// GC'd when the last reference drops.
func NewEventBus[T any]() *EventBus[T] {
	return &EventBus[T]{}
}

// Subscribe registers fn on the bus. The returned func unsubscribes
// fn; calling it more than once is a no-op (sync.Once); calling it
// from inside fn itself is safe (the entry is removed after fn
// returns, not before).
//
// nil fn is silently dropped and the returned func is a no-op.
//
// Subscribe on a closed bus is a no-op (returns a no-op
// unsubscribe). Subscribe on a nil receiver is a no-op.
//
// Race-free vs. Close: the closed check is performed UNDER the
// bus mutex (not just before the Lock). Without this guard, a
// Subscribe racing with Close could append to b.handlers after
// Close flipped the atomic — the handler would sit in the slice
// forever (Publish checks closed, so the handler never fires; the
// func reference leaks until the bus itself is GC'd).
func (b *EventBus[T]) Subscribe(fn Handler[T]) (unsubscribe func()) {
	if b == nil || fn == nil {
		return func() {}
	}
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return func() {}
	}
	id := globalBusID.Add(1)
	b.handlers = append(b.handlers, busEntry[T]{id: id, fn: fn})
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { b.remove(id) })
	}
}

// Publish invokes every registered handler in registration order,
// stopping at the first that returns true.
//
// Returns true if any handler consumed the event. Returns false on
// a nil receiver, a closed bus, an empty handler list, or when all
// handlers returned false.
//
// Handlers are invoked outside the bus mutex; Publish from inside a
// handler is unsafe (would deadlock on b.mu). Use Subscribe /
// Unsubscribe instead if mid-fan-out mutation is needed.
func (b *EventBus[T]) Publish(v T) bool {
	if b == nil || b.closed.Load() {
		return false
	}

	// Snapshot under lock; invoke outside lock so a handler may
	// safely call Subscribe / Unsubscribe without deadlocking.
	b.mu.Lock()
	snap := make([]busEntry[T], len(b.handlers))
	copy(snap, b.handlers)
	b.mu.Unlock()

	for _, h := range snap {
		if invokeBusHandler(b, v, h) {
			return true
		}
	}
	return false
}

// Clear drops every subscriber. Use this when migrating call sites
// that previously relied on SetXxxHandler's last-wins replacement
// semantics:
//
//	bus.Clear()
//	bus.Subscribe(newHandler)
//
// After Clear the bus is NOT closed — Subscribe / Publish keep
// working. Use Close for shutdown.
//
// NOT safe to call from inside a handler: it acquires b.mu while
// Publish still holds a (now-stale) reference to the old handlers
// slice; the mutex would be held twice. Callers must defer Clear
// outside fan-out.
//
// Clear on a nil receiver or closed bus is a no-op.
func (b *EventBus[T]) Clear() {
	if b == nil || b.closed.Load() {
		return
	}
	b.mu.Lock()
	b.handlers = b.handlers[:0]
	b.mu.Unlock()
}

// Close marks the bus permanently closed. Publish / Subscribe /
// Clear are no-ops after Close. Already-registered handlers are not
// invoked (idempotent with Publish's closed-check).
//
// Close is intended for shutdown only — it does NOT free the
// handlers slice, because subscribers may still hold unsubscribe
// funcs they plan to call. The slice becomes unreachable once the
// last reference to the bus drops.
func (b *EventBus[T]) Close() {
	if b == nil {
		return
	}
	b.closed.Store(true)
}

// Len reports the current subscriber count. Debug / tests only;
// callers should not branch on this for correctness.
func (b *EventBus[T]) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.handlers)
}

// remove unsubscribes by id. O(n) scan; acceptable for the
// subscriber counts we expect (≤10 per bus in production).
//
// Caller invariant: only invoked from the closure returned by
// Subscribe; Subscribe pre-checks `b == nil` and returns a no-op
// closure when the bus is nil, so this method never receives a
// nil receiver in practice.
func (b *EventBus[T]) remove(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, h := range b.handlers {
		if h.id == id {
			b.handlers = append(b.handlers[:i], b.handlers[i+1:]...)
			return
		}
	}
}

// invokeBusHandler runs one handler with panic isolation. Returns
// the handler's bool; on panic, logs the bus type + recovered value
// and returns false (chain continues — one buggy subscriber must
// not silence later ones).
func invokeBusHandler[T any](b *EventBus[T], v T, h busEntry[T]) (consumed bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("services.Bus: handler panic recovered",
				"type", b.typeName(),
				"panic", rec)
			consumed = false
		}
	}()
	return h.fn(v)
}

// typeName returns a best-effort label for panic logs. Always
// returns fmt.Sprintf("%T", zero). The earlier nil check was
// removed because Go's typed-nil interface semantics mean
// `any(zero) == nil` is only true when T itself is the empty
// interface — a case the Bus doesn't need to special-case (the
// resulting string would be "interface {}", which is fine).
func (b *EventBus[T]) typeName() string {
	var zero T
	return fmt.Sprintf("%T", zero)
}

// globalBusID is process-unique; never exported. Uses atomic.Uint64
// so the first Subscribe on any bus doesn't race with itself.
var globalBusID atomic.Uint64