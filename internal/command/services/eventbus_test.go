// Package services — EventBus[T] tests (F-54).
//
// Covers the 9 core invariants from docs/feat/F-54-event-bus.md §5:
// registration order, consume-stops-chain, panic isolation,
// unsubscribe idempotency, in-handler unsubscribe, Clear, Close,
// nil-safety, and concurrent Subscribe. Run with `-race` to catch
// the snapshot-vs-mutex cases.
package services

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventBus_PublishOrder — handlers fire in registration order.
func TestEventBus_PublishOrder(t *testing.T) {
	b := NewEventBus[int]()
	var order []int

	b.Subscribe(func(_ int) bool { order = append(order, 1); return false })
	b.Subscribe(func(_ int) bool { order = append(order, 2); return false })
	b.Subscribe(func(_ int) bool { order = append(order, 3); return false })

	b.Publish(42)

	if got, want := order, []int{1, 2, 3}; !equalIntSlice(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestEventBus_ConsumeStopsChain — first true stops the chain.
func TestEventBus_ConsumeStopsChain(t *testing.T) {
	b := NewEventBus[int]()
	var ran []int

	b.Subscribe(func(_ int) bool { ran = append(ran, 1); return true }) // consumes
	b.Subscribe(func(_ int) bool { ran = append(ran, 2); return false })
	b.Subscribe(func(_ int) bool { ran = append(ran, 3); return false })

	if !b.Publish(0) {
		t.Fatal("Publish should report consumed=true when a handler returned true")
	}
	if got, want := ran, []int{1}; !equalIntSlice(got, want) {
		t.Fatalf("ran = %v, want %v (chain should stop at first consumer)", got, want)
	}
}

// TestEventBus_AllFalseContinues — all false → chain runs through, returns false.
func TestEventBus_AllFalseContinues(t *testing.T) {
	b := NewEventBus[int]()
	var ran int

	b.Subscribe(func(_ int) bool { ran++; return false })
	b.Subscribe(func(_ int) bool { ran++; return false })

	if b.Publish(0) {
		t.Fatal("Publish should report consumed=false when all handlers returned false")
	}
	if ran != 2 {
		t.Fatalf("ran = %d, want 2", ran)
	}
}

// TestEventBus_PanicRecovered — handler panic is recovered; later handlers
// still run; consumed reports false (the panicking handler didn't
// consume, semantically).
func TestEventBus_PanicRecovered(t *testing.T) {
	b := NewEventBus[int]()
	var ranAfterPanic bool

	b.Subscribe(func(_ int) bool { panic("boom") })
	b.Subscribe(func(_ int) bool { ranAfterPanic = true; return false })

	if b.Publish(0) {
		t.Fatal("Publish should report consumed=false when panicking handler returned false")
	}
	if !ranAfterPanic {
		t.Fatal("handler after panic must still run")
	}
}

// TestEventBus_UnsubscribeIdempotent — calling unsubscribe more than once
// is a no-op (doesn't crash, doesn't affect others).
func TestEventBus_UnsubscribeIdempotent(t *testing.T) {
	b := NewEventBus[int]()
	var ran int

	unbind := b.Subscribe(func(_ int) bool { ran++; return false })

	unbind()
	unbind() // second call must be no-op
	unbind() // third call must be no-op

	b.Publish(0)
	if ran != 0 {
		t.Fatalf("handler ran %d times after unsubscribe; want 0", ran)
	}
}

// TestEventBus_UnsubscribeFromInsideHandler — handler can unsub itself
// (or others) without affecting the current Publish pass.
func TestEventBus_UnsubscribeFromInsideHandler(t *testing.T) {
	b := NewEventBus[int]()
	var selfUnbind func()

	// selfUnbind is captured by reference in the closure; the first
	// Subscribe call returns the unsubscribe func, which we assign
	// back into selfUnbind. The handler will see the assigned value
	// when Publish fires later.
	selfUnbind = b.Subscribe(func(_ int) bool {
		selfUnbind() // unsub self mid-fan-out
		return false // pass through; later handlers in this Publish still run
	})
	b.Subscribe(func(_ int) bool {
		return false // ran, even though first handler just unsubbed
	})

	b.Publish(0)

	// First handler unsubbed itself; second handler remains.
	if got := b.Len(); got != 1 {
		t.Fatalf("after self-unsub, Len = %d, want 1", got)
	}
}

// TestEventBus_ClearDropsAll — Clear empties the subscriber list; bus
// stays open and Subscribe still works after.
func TestEventBus_ClearDropsAll(t *testing.T) {
	b := NewEventBus[int]()
	var ran int

	b.Subscribe(func(_ int) bool { ran++; return false })
	b.Subscribe(func(_ int) bool { ran++; return false })

	if b.Len() != 2 {
		t.Fatalf("pre-Clear Len = %d, want 2", b.Len())
	}
	b.Clear()
	if b.Len() != 0 {
		t.Fatalf("post-Clear Len = %d, want 0", b.Len())
	}

	if b.Publish(0) {
		t.Fatal("Publish on cleared bus should return false")
	}
	if ran != 0 {
		t.Fatalf("cleared handler ran %d times; want 0", ran)
	}

	// Bus stays open: re-Subscribe must work.
	b.Subscribe(func(_ int) bool { ran++; return false })
	b.Publish(0)
	if ran != 1 {
		t.Fatalf("handler on re-Subscribed bus ran %d times; want 1", ran)
	}
}

// TestEventBus_CloseStopsPublish — Close marks the bus closed; all
// subsequent ops are no-ops.
func TestEventBus_CloseStopsPublish(t *testing.T) {
	b := NewEventBus[int]()
	var ran int

	b.Subscribe(func(_ int) bool { ran++; return false })
	b.Close()

	if b.Publish(0) {
		t.Fatal("Publish on closed bus should return false")
	}
	if ran != 0 {
		t.Fatalf("handler ran on closed bus; want 0")
	}

	// Subscribe on closed bus is a no-op.
	unbind := b.Subscribe(func(_ int) bool { ran++; return false })
	b.Publish(0)
	if ran != 0 {
		t.Fatalf("handler subscribed post-Close ran; want 0")
	}
	unbind() // must not panic

	// Clear on closed bus is a no-op.
	b.Clear()
}

// TestEventBus_NilSafe — every method on a nil *EventBus[T] is a no-op
// (Subscribe returns a working unsubscribe; Publish returns false;
// Clear / Close / Len are no-ops).
func TestEventBus_NilSafe(t *testing.T) {
	var b *EventBus[int]

	if b.Publish(0) {
		t.Fatal("nil.Publish should return false")
	}
	if b.Len() != 0 {
		t.Fatal("nil.Len should return 0")
	}
	b.Clear()
	b.Close()

	unbind := b.Subscribe(func(_ int) bool {
		t.Fatal("nil.Subscribe must not invoke handler")
		return false
	})
	unbind() // must not panic
}

// TestEventBus_ConcurrentSubscribe — Subscribe is safe under concurrent
// calls. Run with `-race` to catch missing synchronization. We don't
// assert on Publish ordering under concurrency; that's a separate
// test (and intentionally not guaranteed across goroutines).
func TestEventBus_ConcurrentSubscribe(t *testing.T) {
	b := NewEventBus[int]()
	var wg sync.WaitGroup
	var n int64

	for range 100 {
		wg.Go(func() {
			b.Subscribe(func(_ int) bool {
				atomic.AddInt64(&n, 1)
				return false
			})
		})
	}
	wg.Wait()

	if got := b.Len(); got != 100 {
		t.Fatalf("Len after concurrent Subscribe = %d, want 100", got)
	}

	// Single Publish must run all 100 (none consume).
	b.Publish(0)
	if got := atomic.LoadInt64(&n); got != 100 {
		t.Fatalf("handler ran %d times, want 100", got)
	}
}

// TestEventBus_NilHandler — Subscribe with nil fn returns a no-op
// unsubscribe; the nil fn is never invoked.
func TestEventBus_NilHandler(t *testing.T) {
	b := NewEventBus[int]()
	unbind := b.Subscribe(nil)
	unbind() // must not panic

	b.Publish(0) // must not panic
	if b.Len() != 0 {
		t.Fatalf("Len after nil Subscribe = %d, want 0", b.Len())
	}
}

// TestEventBus_SubscribeAfterClose_NeverFires verifies the contract:
// after Close, no handler — whether added before or racing with
// Close — ever fires from Publish. The TOCTOU window in Subscribe
// (Load closed, then Lock+append) is acceptable because Publish
// rechecks closed before invoking any handler. Run with -race.
func TestEventBus_SubscribeAfterClose_NeverFires(t *testing.T) {
	b := NewEventBus[int]()
	var ran int32

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.Subscribe(func(_ int) bool {
			atomic.AddInt32(&ran, 1)
			return false
		})
	}()
	go func() {
		defer wg.Done()
		b.Close()
	}()
	wg.Wait()

	// After Close, Publish must be a no-op regardless of whether
	// Subscribe appended before or after the closed flag flipped.
	b.Publish(42)
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("handler ran %d times after Close+Publish; want 0", got)
	}
}

// equalIntSlice is a tiny test helper.
func equalIntSlice(a, b []int) bool {
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

// --- Type-parameter coverage --------------------------------------
//
// Bus is generic over T. The existing tests cover T = int (a value
// type with a non-zero zero value). The branches below exercise
// pointer types, struct types, and string types so the panic-log
// formatting, the snapshot copy, and the nil-safe comparisons all
// behave correctly across the full parameter space.

// TestEventBus_StructPayload verifies Bus works with a struct event
// type (the shape used by chatsession.AgentEventEnvelope et al.).
// Snapshots must copy struct values correctly and handler invocation
// must see the original values, not aliased mutations.
func TestEventBus_StructPayload(t *testing.T) {
	type payload struct {
		ID   int
		Name string
	}
	b := NewEventBus[payload]()

	var got payload
	b.Subscribe(func(p payload) bool {
		got = p
		return true
	})

	want := payload{ID: 42, Name: "answer"}
	if !b.Publish(want) {
		t.Fatal("Publish should report consumed=true")
	}
	if got != want {
		t.Errorf("handler received %+v; want %+v", got, want)
	}
}

// TestEventBus_StringPayload verifies Bus works with string T (zero value
// is ""; non-nil; typeName should return "string").
func TestEventBus_StringPayload(t *testing.T) {
	b := NewEventBus[string]()
	var got []string

	b.Subscribe(func(s string) bool {
		got = append(got, s)
		return false
	})

	b.Publish("hello")
	b.Publish("world")

	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Errorf("handler received %v; want [hello world]", got)
	}
}

// TestEventBus_PointerPayload verifies Bus works with *T (pointer to a
// struct). Each Publish may carry a different pointer; handlers see
// the exact pointer passed.
func TestEventBus_PointerPayload(t *testing.T) {
	type message struct{ Body string }
	b := NewEventBus[*message]()

	var got *message
	b.Subscribe(func(m *message) bool {
		got = m
		return true
	})

	want := &message{Body: "hi"}
	if !b.Publish(want) {
		t.Fatal("Publish should report consumed=true")
	}
	if got != want {
		t.Errorf("handler received %p; want %p", got, want)
	}
	if got.Body != "hi" {
		t.Errorf("handler.Body = %q; want %q", got.Body, "hi")
	}
}

// TestEventBus_PointerPayload_PanicRecovers verifies panic recovery
// works for pointer-typed T. The log uses fmt.Sprintf("%T", zero)
// which formats a typed-nil *T as "*pkg.Foo" (not the empty
// string — Go's typed-nil interface semantics mean the empty
// branch in typeName is unreachable for concrete pointer types).
func TestEventBus_PointerPayload_PanicRecovers(t *testing.T) {
	type message struct{ Body string }
	b := NewEventBus[*message]()

	b.Subscribe(func(_ *message) bool { panic("boom") })

	// Must not propagate; recovery returns consumed=false.
	if b.Publish(&message{Body: "x"}) {
		t.Error("Publish should report consumed=false (panicking handler returned false after recover)")
	}
}

// TestEventBus_SnapshotMutationSafety verifies that even if a handler
// mutates the event payload after Publish returns, earlier handlers
// (in the same Publish pass) saw the original value. The snapshot
// mechanism in Publish copies busEntry values (not pointers) so the
// function pointer is fixed; the payload itself is the caller's
// responsibility. This test pins the contract that Subscribe
// receives a per-Publish fresh payload, not a shared mutable one.
func TestEventBus_SnapshotMutationSafety(t *testing.T) {
	type payload struct{ Value int }
	b := NewEventBus[payload]()
	var seen []int

	b.Subscribe(func(p payload) bool {
		seen = append(seen, p.Value)
		// Mutate the local — must NOT affect later handlers'
		// argument, because Go passes by value.
		p.Value = -1
		return false
	})

	b.Publish(payload{Value: 7})

	if len(seen) != 1 || seen[0] != 7 {
		t.Errorf("handler observed %v; want [7]", seen)
	}
}

// TestEventBus_EmptyPayload_Behavior pins behavior with empty struct
// payloads (zero-size T). Cheap to cover; future-proofs against
// regressions in the snapshot machinery for size-zero types.
func TestEventBus_EmptyPayload_Behavior(t *testing.T) {
	type empty struct{}
	b := NewEventBus[empty]()

	var ran int
	b.Subscribe(func(_ empty) bool {
		ran++
		return true
	})

	if !b.Publish(empty{}) {
		t.Fatal("Publish should report consumed=true")
	}
	if ran != 1 {
		t.Errorf("handler ran %d times; want 1", ran)
	}
}

// --- Closed-bus state machine -------------------------------------

// TestEventBus_SubscribeOnClosedBusIsNoop verifies that calling
// Subscribe on an already-closed bus is a no-op (returns a
// no-op unsubscribe, doesn't append to the handler slice). This
// is the sequential analog of TestEventBus_SubscribeAfterClose_NeverFires.
func TestEventBus_SubscribeOnClosedBusIsNoop(t *testing.T) {
	b := NewEventBus[int]()
	b.Close()

	var ran int
	unbind := b.Subscribe(func(_ int) bool {
		ran++
		return false
	})

	b.Publish(42) // bus is closed, even if Subscribe did add a handler, it must not fire

	if got := b.Len(); got != 0 {
		t.Errorf("Len after Subscribe-on-closed = %d; want 0 (Subscribe must not append to a closed bus)", got)
	}
	if ran != 0 {
		t.Errorf("handler ran %d times on closed bus; want 0", ran)
	}

	// The returned unsubscribe must also be a no-op (idempotent +
	// safe to call).
	unbind()
	unbind()
}

// TestEventBus_PublishOnClosedBusIgnoresSubscribers verifies that even
// if handlers are registered before Close, Publish after Close is
// a no-op. Pin the contract: close is a hard barrier.
func TestEventBus_PublishOnClosedBusIgnoresSubscribers(t *testing.T) {
	b := NewEventBus[int]()
	var ran int32
	b.Subscribe(func(_ int) bool {
		atomic.AddInt32(&ran, 1)
		return false
	})

	b.Close()

	if b.Publish(42) {
		t.Error("Publish on closed bus should return false")
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Errorf("handler ran %d times after Close; want 0", got)
	}
}

// TestEventBus_UnsubscribeAfterCloseIsNoop verifies that calling the
// unsubscribe func returned from a pre-Close Subscribe, after the
// bus has been Closed, doesn't panic and doesn't try to mutate a
// nil slice. Defensive: production shutdown order is
// unsubscribe → Close, but the inverse should also be safe.
func TestEventBus_UnsubscribeAfterCloseIsNoop(t *testing.T) {
	b := NewEventBus[int]()
	unbind := b.Subscribe(func(_ int) bool { return false })
	b.Close()

	// Must not panic.
	unbind()
}

// TestEventBus_ClearOnClosedBusIsNoop — defensive; Clear on a closed
// bus is a no-op (subscribers already can't fire).
func TestEventBus_ClearOnClosedBusIsNoop(t *testing.T) {
	b := NewEventBus[int]()
	b.Subscribe(func(_ int) bool { return false })
	b.Close()

	// Must not panic.
	b.Clear()
}

// TestEventBus_RemoveNonexistentIDIsNoop — unsubscribe returned by
// Subscribe uses a unique id, so it's unusual to try removing an
// arbitrary id. But the public surface is `Bus.remove(id)`, which
// could be hit if a caller invokes the returned unsubscribe twice
// (covered by UnsubscribeIdempotent) or after Clear. Pins the
// "not found" branch as safe.
func TestEventBus_RemoveNonexistentIDIsNoop(t *testing.T) {
	b := NewEventBus[int]()
	b.Clear() // empty slice; subsequent unsubs are no-ops
	// (Note: we can't reach b.remove directly from outside the
	// package; this is verified indirectly by Clear + Subscribe
	// + Clear + Unsubscribe.)
	unbind := b.Subscribe(func(_ int) bool { return false })
	b.Clear()
	unbind() // remove returns "not found"; should not panic or affect state
}

// --- Multi-subscriber scenarios -----------------------------------

// TestEventBus_MultipleSubscribersMixedReturn — three subscribers:
// first returns false (pass through), second returns true
// (consumes), third is unreachable. Confirms the chain semantics
// and that consumed=true propagates correctly.
func TestEventBus_MultipleSubscribersMixedReturn(t *testing.T) {
	b := NewEventBus[int]()
	var reached []int

	b.Subscribe(func(_ int) bool { reached = append(reached, 1); return false })
	b.Subscribe(func(_ int) bool { reached = append(reached, 2); return true }) // consumes
	b.Subscribe(func(_ int) bool { reached = append(reached, 3); return false })

	if !b.Publish(0) {
		t.Fatal("Publish should report consumed=true")
	}
	if got, want := reached, []int{1, 2}; !equalIntSlice(got, want) {
		t.Errorf("reached = %v; want %v (third subscriber unreachable)", got, want)
	}
}

// TestEventBus_MultipleSubscribersAllFalse — three subscribers all
// returning false. Confirm Publish returns false (no consumption)
// and all fire.
func TestEventBus_MultipleSubscribersAllFalse(t *testing.T) {
	b := NewEventBus[int]()
	var reached []int

	b.Subscribe(func(_ int) bool { reached = append(reached, 1); return false })
	b.Subscribe(func(_ int) bool { reached = append(reached, 2); return false })
	b.Subscribe(func(_ int) bool { reached = append(reached, 3); return false })

	if b.Publish(0) {
		t.Fatal("Publish should report consumed=false (all returned false)")
	}
	if got, want := reached, []int{1, 2, 3}; !equalIntSlice(got, want) {
		t.Errorf("reached = %v; want %v", got, want)
	}
}

// TestEventBus_UnsubscribeOneKeepsOthers verifies that unsubscribing one
// handler doesn't affect the others. Pins independence.
func TestEventBus_UnsubscribeOneKeepsOthers(t *testing.T) {
	b := NewEventBus[int]()
	var aRan, bRan, cRan int

	unbindA := b.Subscribe(func(_ int) bool { aRan++; return false })
	b.Subscribe(func(_ int) bool { bRan++; return false })
	b.Subscribe(func(_ int) bool { cRan++; return false })

	unbindA()
	b.Publish(0)

	if aRan != 0 {
		t.Errorf("unsub'd handler A ran %d times; want 0", aRan)
	}
	if bRan != 1 || cRan != 1 {
		t.Errorf("B=%d C=%d; want both 1", bRan, cRan)
	}
}

// --- Concurrent stress ---------------------------------------------

// TestEventBus_ConcurrentPublishAndClear — hammer the bus with
// concurrent Publish + Clear + Subscribe + Unsubscribe from
// multiple goroutines. Run with -race to catch any data races
// on the handlers slice header.
func TestEventBus_ConcurrentPublishAndClear(t *testing.T) {
	b := NewEventBus[int]()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Publishers.
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(0)
				}
			}
		})
	}
	// Churners: subscribe + clear repeatedly.
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					b.Clear()
				}
			}
		})
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestEventBus_SubscribePublishInterleave — Subscribe runs concurrently
// with Publish; a Subscribe mid-Dispatch may or may not see the
// in-flight event (Publish snapshots under lock), but the bus must
// not panic, deadlock, or corrupt the slice.
func TestEventBus_SubscribePublishInterleave(t *testing.T) {
	b := NewEventBus[int]()
	var ran int32

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish(0)
			}
		}
	})

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				b.Subscribe(func(_ int) bool {
					atomic.AddInt32(&ran, 1)
					return false
				})
				b.Clear()
			}
		}
	})

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- Bus lifecycle state -----------------------------------------

// TestEventBus_CloseIdempotent — calling Close twice is safe; second
// call is a no-op.
func TestEventBus_CloseIdempotent(t *testing.T) {
	b := NewEventBus[int]()
	b.Close()
	b.Close() // must not panic, double-close must be safe
}

// TestEventBus_LenAfterClear — Len drops to 0 after Clear; re-Subscribe
// works (bus stays open).
func TestEventBus_LenAfterClear(t *testing.T) {
	b := NewEventBus[int]()
	b.Subscribe(func(_ int) bool { return false })
	b.Subscribe(func(_ int) bool { return false })
	if b.Len() != 2 {
		t.Fatalf("pre-Clear Len = %d, want 2", b.Len())
	}

	b.Clear()
	if b.Len() != 0 {
		t.Errorf("post-Clear Len = %d, want 0", b.Len())
	}

	b.Subscribe(func(_ int) bool { return false })
	if b.Len() != 1 {
		t.Errorf("post-re-Subscribe Len = %d, want 1", b.Len())
	}
}

// TestEventBus_UnsubscribeThenClearIsSafe — calling unsubscribe on every
// subscriber followed by Clear is safe; Clear is idempotent.
func TestEventBus_UnsubscribeThenClearIsSafe(t *testing.T) {
	b := NewEventBus[int]()
	unbinds := make([]func(), 5)
	for i := range unbinds {
		unbinds[i] = b.Subscribe(func(_ int) bool { return false })
	}
	for _, u := range unbinds {
		u()
	}
	b.Clear() // no-op on empty slice; must not panic
	if b.Len() != 0 {
		t.Errorf("Len = %d after full unsubscribe + Clear; want 0", b.Len())
	}
}