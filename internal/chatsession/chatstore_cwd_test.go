package chatsession

import (
	"path/filepath"
	"testing"
)

// TestCwdSwitch_ParksWarmAndRemountsSameAS pins docs/CHATSTORE.md:
// /cwd A → Lookup → /cwd B parks A in asPool; remount A reuses same ID.
func TestCwdSwitch_ParksWarmAndRemountsSameAS(t *testing.T) {
	csFile, asFile := newTestStores(t)
	pool := NewAgentSessionPool()
	cs, err := New("oc_warm", "claude")
	if err != nil {
		t.Fatal(err)
	}
	cs.WithPersistence(csFile, asFile).WithAgentSessionPool(pool)
	_ = cs.ensureStoreBootstrapped()

	dirA := filepath.Clean(t.TempDir())
	dirB := filepath.Clean(t.TempDir())

	if err := cs.SetSelectedCwd(dirA); err != nil {
		t.Fatalf("cwd A: %v", err)
	}
	asA, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("lookup A: %v", err)
	}
	idA := asA.ID

	if err := cs.SetSelectedCwd(dirB); err != nil {
		t.Fatalf("cwd B: %v", err)
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("after cwd B, cs.Pool want 0, got %d", len(cs.Pool()))
	}
	if pool.Get("oc_warm", dirA, "claude") == nil {
		t.Fatal("AS A should remain in asPool warm")
	}

	if err := cs.SetSelectedCwd(dirA); err != nil {
		t.Fatalf("cwd A again: %v", err)
	}
	asAgain, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("lookup A again: %v", err)
	}
	if asAgain.ID != idA {
		t.Fatalf("remount ID = %s, want %s", asAgain.ID, idA)
	}
	if asAgain != asA {
		t.Fatal("expected same AgentSession pointer from asPool")
	}
}

// TestEvict_RemovesFromAsPoolEvenIfNotMounted ensures Evict uses
// asPool.ListByChatCwd, not only cs.pool.
func TestEvict_RemovesFromAsPoolEvenIfNotMounted(t *testing.T) {
	csFile, asFile := newTestStores(t)
	pool := NewAgentSessionPool()
	cs, _ := New("oc_evict", "claude")
	cs.WithPersistence(csFile, asFile).WithAgentSessionPool(pool)
	_ = cs.ensureStoreBootstrapped()

	dir := filepath.Clean(t.TempDir())
	_ = cs.SetSelectedCwd(dir)
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatal(err)
	}
	// Park by switching away.
	other := filepath.Clean(t.TempDir())
	_ = cs.SetSelectedCwd(other)
	if pool.Get("oc_evict", dir, "claude") == nil {
		t.Fatal("expected warm AS in asPool")
	}

	n, _ := cs.EvictAgentSessionsInCwd(dir)
	if n < 1 {
		t.Fatalf("Evict count = %d, want >= 1", n)
	}
	if pool.Get("oc_evict", dir, "claude") != nil {
		t.Fatal("Evict should Delete from asPool")
	}
	_ = as // keep for clarity
}
