package chatsession

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLookup_RecursionDoesNotDeadlockOnCwdToggle pins the
// deadlock fix in LookupSelectedAgentSession: when /cwd toggles
// A → B → A while a cold Lookup is in flight, the recursive
// retry must release the per-key resolve lock for the previous
// key before re-entering — otherwise the second recursion would
// try to lockResolve(A) while the outer call still holds it via
// defer, deadlocking on the non-reentrant sync.Mutex.
func TestLookup_RecursionDoesNotDeadlockOnCwdToggle(t *testing.T) {
	csFile, asFile := newTestStores(t)
	pool := NewAgentSessionPool()
	cs, err := New("oc_recurse", "claude")
	if err != nil {
		t.Fatal(err)
	}
	cs.WithPersistence(csFile, asFile).WithAgentSessionPool(pool)
	if err := cs.ensureStoreBootstrapped(); err != nil {
		t.Fatal(err)
	}

	dirA := filepath.Clean(t.TempDir())
	dirB := filepath.Clean(t.TempDir())
	if err := cs.SetSelectedCwd(dirA); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 50 alternating /cwd B → /cwd A toggles, racing with the
		// Lookup's I/O window. Each toggle forces the Lookup's
		// recursion check to fire; without the unlock-before-recurse
		// fix, the second toggle (B → A) would deadlock.
		for i := 0; i < 50; i++ {
			_ = cs.SetSelectedCwd(dirB)
			_ = cs.SetSelectedCwd(dirA)
		}
	}()

	// 50 Lookups, each potentially recursing once or twice. Wrap
	// each with a soft 2s deadline so a regression surfaces as a
	// timeout rather than hanging the test runner forever.
	for i := 0; i < 50; i++ {
		ch := make(chan error, 1)
		go func() {
			_, err := cs.LookupSelectedAgentSession()
			ch <- err
		}()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("Lookup #%d deadlocked under cwd toggle", i)
		}
	}

	<-done
}
