// lazy_hydrate_test.go — v1.3+ multi-channel: per-chat restore
// is lazy (Manager.GetOrCreate checks csFile on in-memory miss
// and hydrates from the persisted entry). This file covers
// the hydrate path explicitly so the v0.x eager-restore
// regression can't sneak back in.
package chatsession

import (
	"path/filepath"
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

// TestGetOrCreate_LazyHydrateFromCSFile verifies that a
// chatID with a persisted ChatSessionEntry (but no in-memory
// state) is rehydrated on first GetOrCreate. This is the
// single most important v1.3+ contract: the runtime does NOT
// call Manager.RestoreFromRegistry at startup; restore is
// deferred to the first inbound from that chat.
func TestGetOrCreate_LazyHydrateFromCSFile(t *testing.T) {
	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}

	// Pre-seed a persisted entry for chatID "oc_xxx" (feishu
	// namespace). The in-memory Manager has nothing.
	entry := &registry.ChatSessionEntry{
		ID:           "cs_1",
		ChatID:       "oc_xxx",
		SelectedCwd:  "/code/bailing",
		SelectedAgent: "claude",
		PrimaryAgent:  "claude",
		WatchMode:     0, // default = Mention
		ThinkMode:     0, // default = Show
		ToolsMode:     0, // default = Hide
	}
	if err := csFile.Upsert(entry); err != nil {
		t.Fatalf("csFile.Upsert: %v", err)
	}

	// Build a real Manager. WithPersistence wires it to csFile.
	mgr := NewManager().WithPersistence(csFile, nil)
	// mgr.sessions is empty — no eager restore happened.

	if mgr.Get("oc_xxx") != nil {
		t.Fatal("expected empty mgr.sessions for oc_xxx (no eager restore)")
	}

	// First GetOrCreate: must hydrate from csFile.
	cs, err := mgr.GetOrCreate("oc_xxx", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if cs == nil {
		t.Fatal("GetOrCreate returned nil cs")
	}
	if cs.ChatID != "oc_xxx" {
		t.Errorf("cs.ChatID = %q, want oc_xxx", cs.ChatID)
	}
	if cs.SelectedCwd() != "/code/bailing" {
		t.Errorf("cs.SelectedCwd = %q, want /code/bailing", cs.SelectedCwd())
	}
	if cs.SelectedAgent() != "claude" {
		t.Errorf("cs.SelectedAgent = %q, want claude", cs.SelectedAgent())
	}

	// Second call returns the same (in-memory) cs.
	cs2, _ := mgr.GetOrCreate("oc_xxx", "claude")
	if cs2 != cs {
		t.Error("second GetOrCreate returned a different *ChatSession (expected in-memory cache hit)")
	}
}

// TestGetOrCreate_NoEagerRestore verifies the Manager does
// NOT scan csFile at construction time. v0.x called
// RestoreFromRegistry from runDaemon; v1.3+ leaves the
// in-memory state empty until first inbound. This test
// guards that contract.
func TestGetOrCreate_NoEagerRestore(t *testing.T) {
	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}

	// Pre-seed 5 entries.
	for i := 0; i < 5; i++ {
		if err := csFile.Upsert(&registry.ChatSessionEntry{
			ID:          "cs_" + string(rune('a'+i)),
			ChatID:      "oc_" + string(rune('a'+i)),
			PrimaryAgent: "claude",
		}); err != nil {
			t.Fatalf("csFile.Upsert: %v", err)
		}
	}

	// Construct Manager. mgr.sessions must be empty — no scan.
	mgr := NewManager().WithPersistence(csFile, nil)
	if got := len(mgr.List()); got != 0 {
		t.Errorf("mgr has %d pre-hydrated sessions, want 0 (no eager restore)", got)
	}

	// First GetOrCreate for "oc_a" only hydrates that one.
	if _, err := mgr.GetOrCreate("oc_a", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if got := len(mgr.List()); got != 1 {
		t.Errorf("after 1 GetOrCreate, mgr has %d sessions, want 1", got)
	}
}

// TestHydrateFromEntry_AgentSessionPool verifies that the
// AgentSession pool is restored on hydrate (Detached state;
// LookupSelectedAgentSession will re-spawn). The pool entries
// must be returned by cs.AgentSessionsInCwd after hydrate.
func TestHydrateFromEntry_AgentSessionPool(t *testing.T) {
	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(filepath.Join(dir, "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}

	// Pre-seed cs + as entries.
	csEntry := &registry.ChatSessionEntry{
		ID:          "cs_pool",
		ChatID:      "oc_pool",
		SelectedCwd: "/code/x",
		PrimaryAgent: "claude",
	}
	if err := csFile.Upsert(csEntry); err != nil {
		t.Fatalf("csFile.Upsert: %v", err)
	}
	asEntry := &registry.AgentSessionEntry{
		ID:            "as_pool",
		ChatSessionID: "cs_pool",
		Agent:         "claude",
		Cwd:           "/code/x",
		Status:        "detached",
	}
	if err := asFile.Upsert(asEntry); err != nil {
		t.Fatalf("asFile.Upsert: %v", err)
	}

	mgr := NewManager().WithPersistence(csFile, asFile)
	cs, err := mgr.GetOrCreate("oc_pool", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	agents := cs.AgentSessionsInCwd("/code/x")
	if len(agents) != 1 {
		t.Fatalf("AgentSessionsInCwd: got %d, want 1 (entry should be hydrated)", len(agents))
	}
	// AgentSession's Agent field is the type alias (string)
	// representing the agent name. Hydration must preserve it.
	if agents[0].Agent != "claude" {
		t.Errorf("hydrated agent = %q, want claude", agents[0].Agent)
	}
}
