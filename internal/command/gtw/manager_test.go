package gtw

import (
	"sync"
	"testing"
	"time"
)

// newTestManager returns a Manager with no special configuration
// beyond NewManager's defaults. v1.5 stripped the test override
// surface (Manager.deps / Manager.now / the drafts registry);
// only the run lock remains to test.
func newTestManager() *Manager {
	return NewManager()
}

// --- run lock tests ---

// TestManager_RunLockFor_EmptyChatIDReturnsNil pins the contract
// that chatID == "" returns nil so Factory.Handle can no-op
// safely in tests and synthetic inputs that bypass the runtime
// dispatcher. The caller nil-checks before Lock, so a nil return
// must not panic.
func TestManager_RunLockFor_EmptyChatIDReturnsNil(t *testing.T) {
	m := newTestManager()
	if got := m.runLockFor(""); got != nil {
		t.Errorf("runLockFor(\"\") = %v, want nil", got)
	}
}

// TestManager_RunLockFor_PerChatIndependence verifies that
// different chatIDs get independent mutexes: a slow holder in
// chat A must not block chat B from acquiring its lock.
//
// Implementation: chat A holds the lock for holdDuration; we
// record when chat B's Lock returns. If B's wait is shorter
// than A's holdDuration, the locks are independent. A test
// threshold slightly below holdDuration filters out scheduling
// noise while still failing if the locks are accidentally
// shared (in which case B would wait the full holdDuration).
func TestManager_RunLockFor_PerChatIndependence(t *testing.T) {
	m := newTestManager()
	const holdDuration = 200 * time.Millisecond

	muA := m.runLockFor("chat-A")
	muB := m.runLockFor("chat-B")
	if muA == nil || muB == nil {
		t.Fatalf("runLockFor returned nil for non-empty chatID: A=%v B=%v", muA, muB)
	}
	if muA == muB {
		t.Fatalf("runLockFor returned the same mutex for different chatIDs; expected independent locks")
	}

	muA.Lock()
	aReleased := make(chan struct{})
	go func() {
		defer close(aReleased)
		time.Sleep(holdDuration)
		muA.Unlock()
	}()

	startB := time.Now()
	muB.Lock()
	bAcquired := time.Since(startB)
	muB.Unlock()

	if bAcquired >= holdDuration {
		t.Errorf("chat B waited %v (>= %v holdDuration); locks are NOT per-chat", bAcquired, holdDuration)
	}
	<-aReleased
}

// TestManager_RunLockFor_SameChatSerializes verifies that two
// Lock calls on the same chatID's mutex serialise: the second
// goroutine must observe the first's Unlock before it acquires.
//
// Implementation: chatID = "chat-X" gets the same *sync.Mutex
// from both calls. First goroutine holds for holdDuration; we
// start the second goroutine immediately after, then measure
// how long until it acquires. The wait must be at least
// holdDuration (minus a small jitter tolerance for slow CI).
func TestManager_RunLockFor_SameChatSerializes(t *testing.T) {
	m := newTestManager()
	const holdDuration = 150 * time.Millisecond

	mu := m.runLockFor("chat-X")
	if mu == nil {
		t.Fatalf("runLockFor returned nil for non-empty chatID")
	}
	// Second call must return the same instance — LoadOrStore
	// must not allocate a fresh mutex for the same chatID.
	if mu2 := m.runLockFor("chat-X"); mu2 != mu {
		t.Errorf("runLockFor returned a different mutex for the same chatID; LoadOrStore race")
	}

	mu.Lock()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(holdDuration)
		mu.Unlock()
	}()

	// Second acquirer on the same mutex; record wait time.
	acquired := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		mu.Lock()
		wait := time.Since(start)
		mu.Unlock()
		acquired <- wait
	}()

	select {
	case wait := <-acquired:
		if wait < holdDuration-50*time.Millisecond {
			t.Errorf("second acquirer waited %v (< %v); serialisation broken", wait, holdDuration)
		}
	case <-time.After(2 * holdDuration):
		t.Fatalf("second acquirer never acquired the lock within %v", 2*holdDuration)
	}
	<-released
}

// TestManager_RunLockFor_ConcurrentFirstCallRace pins the
// LoadOrStore contract: even under concurrent first-time access
// for the same chatID, both callers must observe the SAME
// *sync.Mutex instance. A naive Load-then-Store pattern would
// race and produce two distinct mutexes, breaking serialisation
// for that chatID. This test fails the implementation if anyone
// replaces sync.Map.LoadOrStore with Load + Store.
func TestManager_RunLockFor_ConcurrentFirstCallRace(t *testing.T) {
	m := newTestManager()
	const N = 32

	var wg sync.WaitGroup
	results := make([]*sync.Mutex, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = m.runLockFor("chat-race")
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, r := range results {
		if r != first {
			t.Errorf("goroutine %d got a different *sync.Mutex; LoadOrStore race", i)
			break
		}
	}
}
