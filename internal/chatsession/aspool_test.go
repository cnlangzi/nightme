package chatsession

import (
	"path/filepath"
	"testing"
)

func TestAgentSessionPool_GetPutDelete(t *testing.T) {
	p := NewAgentSessionPool()
	as := NewAgentSession("as_1", "cs_oc_x", "claude", "/code/A", nil)
	p.Put("oc_x", as)

	got := p.Get("oc_x", "/code/A", "claude")
	if got != as {
		t.Fatalf("Get = %v, want same pointer", got)
	}
	// Clean normalizes trailing slash.
	if p.Get("oc_x", "/code/A/", "claude") != as {
		t.Fatal("Get after Clean mismatch")
	}
	p.Delete("oc_x", "/code/A", "claude")
	if p.Get("oc_x", "/code/A", "claude") != nil {
		t.Fatal("Get after Delete want nil")
	}
}

func TestAgentSessionPool_GetOrPutAndFindByID(t *testing.T) {
	p := NewAgentSessionPool()
	a1 := NewAgentSession("as_1", "cs_oc_x", "claude", "/code/A", nil)
	a2 := NewAgentSession("as_dup", "cs_oc_x", "claude", "/code/A", nil)

	got := p.GetOrPut("oc_x", a1)
	if got != a1 {
		t.Fatal("first GetOrPut should insert a1")
	}
	got2 := p.GetOrPut("oc_x", a2)
	if got2 != a1 {
		t.Fatal("second GetOrPut must keep winner a1")
	}
	if p.FindByID("as_1") != a1 {
		t.Fatal("FindByID miss")
	}
	if p.FindByID("as_dup") != nil {
		t.Fatal("loser must not be findable")
	}
}

func TestAgentSessionPool_ListByChatCwd(t *testing.T) {
	p := NewAgentSessionPool()
	a1 := NewAgentSession("as_1", "cs_oc_x", "claude", "/code/A", nil)
	a2 := NewAgentSession("as_2", "cs_oc_x", "codex", "/code/A", nil)
	b1 := NewAgentSession("as_3", "cs_oc_x", "claude", "/code/B", nil)
	p.Put("oc_x", a1)
	p.Put("oc_x", a2)
	p.Put("oc_x", b1)

	list := p.ListByChatCwd("oc_x", filepath.Clean("/code/A"))
	if len(list) != 2 {
		t.Fatalf("ListByChatCwd A = %d, want 2", len(list))
	}
	listB := p.ListByChatCwd("oc_x", "/code/B")
	if len(listB) != 1 || listB[0] != b1 {
		t.Fatalf("ListByChatCwd B = %v", listB)
	}
}
