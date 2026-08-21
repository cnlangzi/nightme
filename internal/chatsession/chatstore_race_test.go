package chatsession

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestLookup_ConcurrentColdMissSharesOneAS pins the singleflight
// resolve path: many concurrent Lookups for the same key must
// share one AgentSession ID (no divergent Put/Spawn winners).
func TestLookup_ConcurrentColdMissSharesOneAS(t *testing.T) {
	csFile, asFile := newTestStores(t)
	pool := NewAgentSessionPool()
	cs, err := New("oc_race", "claude")
	if err != nil {
		t.Fatal(err)
	}
	cs.WithPersistence(csFile, asFile).WithAgentSessionPool(pool)
	dir := filepath.Clean(t.TempDir())
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	ids := make([]string, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			as, err := cs.LookupSelectedAgentSession()
			if err != nil {
				t.Errorf("lookup: %v", err)
				return
			}
			ids[i] = as.ID
		}(i)
	}
	wg.Wait()

	first := ids[0]
	if first == "" {
		t.Fatal("empty id")
	}
	for i, id := range ids {
		if id != first {
			t.Fatalf("ids[%d]=%s want %s (divergent AS)", i, id, first)
		}
	}
	if pool.Get("oc_race", dir, "claude") == nil {
		t.Fatal("asPool missing winner")
	}
}

// TestSetSelectedCwd_SerializedCacheMatchesStore ensures concurrent
// cwd flips leave cache and chatstore agreeing on the last write.
func TestSetSelectedCwd_SerializedCacheMatchesStore(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_setrace", "claude")
	cs.WithPersistence(csFile, asFile).WithAgentSessionPool(NewAgentSessionPool())
	_ = cs.ensureStoreBootstrapped()

	dirA := filepath.Clean(t.TempDir())
	dirB := filepath.Clean(t.TempDir())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = cs.SetSelectedCwd(dirA)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = cs.SetSelectedCwd(dirB)
		}
	}()
	wg.Wait()

	got := cs.SelectedCwd()
	entry, ok := csFile.Get("oc_setrace")
	if !ok {
		t.Fatal("missing store entry")
	}
	if filepath.Clean(entry.SelectedCwd) != got {
		t.Fatalf("cache=%q store=%q", got, entry.SelectedCwd)
	}
	if got != dirA && got != dirB {
		t.Fatalf("unexpected cwd %q", got)
	}
}
