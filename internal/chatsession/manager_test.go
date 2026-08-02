package chatsession

import (
	"strings"
	"testing"
)

func TestManager_GetOrCreate_New(t *testing.T) {
	mgr := NewManager()
	cs := mgr.GetOrCreate("oc_xxx", "p2p", "claude")
	if cs == nil {
		t.Fatalf("GetOrCreate returned nil")
	}
	if cs.ChatID != "oc_xxx" {
		t.Fatalf("ChatID: %q", cs.ChatID)
	}
	if cs.DefaultAgent() != "claude" {
		t.Fatalf("DefaultAgent: %q", cs.DefaultAgent())
	}
	if mgr.Get("oc_xxx") != cs {
		t.Fatalf("Get should return the same instance")
	}
}

func TestManager_GetOrCreate_ReturnsSameOnRepeat(t *testing.T) {
	mgr := NewManager()
	a := mgr.GetOrCreate("oc_xxx", "p2p", "claude")
	b := mgr.GetOrCreate("oc_xxx", "p2p", "codex")
	if a != b {
		t.Fatalf("GetOrCreate should return same instance; got different pointers")
	}
	// DefaultAgent snapshot is captured at first creation; subsequent
	// GetOrCreate with different defaultAgent does NOT mutate.
	if a.DefaultAgent() != "claude" {
		t.Fatalf("DefaultAgent mutated: got %q, want claude (first-create wins)", a.DefaultAgent())
	}
}

func TestManager_GetMissing(t *testing.T) {
	mgr := NewManager()
	if cs := mgr.Get("missing"); cs != nil {
		t.Fatalf("expected nil, got %v", cs)
	}
}

func TestManager_List(t *testing.T) {
	mgr := NewManager()
	a := mgr.GetOrCreate("a", "p2p", "claude")
	b := mgr.GetOrCreate("b", "p2p", "claude")
	c := mgr.GetOrCreate("c", "p2p", "claude")

	list := mgr.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(list))
	}
	seen := map[string]*ChatSession{}
	for _, cs := range list {
		seen[cs.ChatID] = cs
	}
	if seen["a"] != a || seen["b"] != b || seen["c"] != c {
		t.Fatalf("List returns wrong instances")
	}
}

func TestManager_WithSpawner(t *testing.T) {
	spawner := newFakeSpawner()
	mgr := NewManager().WithSpawner(spawner)
	cs := mgr.GetOrCreate("oc_xxx", "p2p", "claude")
	if cs.Spawner() == nil {
		t.Fatalf("spawner should be wired after WithSpawner")
	}

	// /use should now actually spawn.
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if as.Status() != StatusRunning {
		t.Fatalf("after spawn: got %q, want Running", as.Status())
	}
}

func TestManager_PoolAfterKillCanRespawn(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	mgr := NewManager().
		WithSpawner(spawner).
		WithPersistence(csFile, asFile)

	cs := mgr.GetOrCreate("oc_xxx", "p2p", "claude")
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")

	as1, _ := cs.LookupActiveAgentSession()
	if as1.Status() != StatusRunning {
		t.Fatalf("precondition: expected Running")
	}

	// /kill clears the pool.
	if err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("pool should be empty after kill")
	}

	// Sending a message after /kill re-spawns via the same Spawner.
	cs.SetActiveCwd("/code/bailing")
	as2, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession after kill: %v", err)
	}
	if as2.Status() != StatusRunning {
		t.Fatalf("after respawn: got %q, want Running", as2.Status())
	}
	if as2.ID == as1.ID {
		t.Fatalf("respawn should produce a new AgentSession ID")
	}
}

func TestManager_RestoreFromRegistry_NoPersistence(t *testing.T) {
	mgr := NewManager() // no persistence
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry with nil csFile: %v", err)
	}
}

func TestManager_ErrNoActiveChatSessionMessage(t *testing.T) {
	if !strings.Contains(ErrNoActiveChatSession.Error(), "/cwd") {
		t.Fatalf("error message should mention /cwd: %v", ErrNoActiveChatSession)
	}
}