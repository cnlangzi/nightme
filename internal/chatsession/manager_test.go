package chatsession

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

func TestManager_GetOrCreate_New(t *testing.T) {
	mgr := NewManager()
	cs := mgr.GetOrCreate("oc_xxx", "claude")
	if cs == nil {
		t.Fatalf("GetOrCreate returned nil")
	}
	if cs.ChatID != "oc_xxx" {
		t.Fatalf("ChatID: %q", cs.ChatID)
	}
	if cs.PrimaryAgent() != "claude" {
		t.Fatalf("PrimaryAgent: %q", cs.PrimaryAgent())
	}
	if mgr.Get("oc_xxx") != cs {
		t.Fatalf("Get should return the same instance")
	}
}

func TestManager_GetOrCreate_ReturnsSameOnRepeat(t *testing.T) {
	mgr := NewManager()
	a := mgr.GetOrCreate("oc_xxx", "claude")
	b := mgr.GetOrCreate("oc_xxx", "codex")
	if a != b {
		t.Fatalf("GetOrCreate should return same instance; got different pointers")
	}
	// PrimaryAgent snapshot is captured at first creation; subsequent
	// GetOrCreate with different primaryAgent does NOT mutate.
	if a.PrimaryAgent() != "claude" {
		t.Fatalf("PrimaryAgent mutated: got %q, want claude (first-create wins)", a.PrimaryAgent())
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
	a := mgr.GetOrCreate("a", "claude")
	b := mgr.GetOrCreate("b", "claude")
	c := mgr.GetOrCreate("c", "claude")

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
	cs := mgr.GetOrCreate("oc_xxx", "claude")
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

	cs := mgr.GetOrCreate("oc_xxx", "claude")
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

// seedPersistedChatSession writes a minimal ChatSessionEntry to
// the given store so RestoreFromRegistry has something to rebuild.
// Used by the WithOnCreate regression tests below.
func seedPersistedChatSession(t *testing.T, csFile *registry.ChatSessionFile, chatID, primary string) string {
	t.Helper()
	csID := "cs_" + chatID
	entry := &registry.ChatSessionEntry{
		ID:                csID,
		ChatID:            chatID,
		ActiveCwd:         "/code/bailing",
		ActiveAgent:       primary,
		PrimaryAgent:      primary,
		AgentSessionIDs:   nil,
		CreatedAt:         time.Now(),
		LastInteractionAt: time.Now(),
	}
	if err := csFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert CS: %v", err)
	}
	return csID
}

// TestManager_RestoreFromRegistry_FiresOnCreate verifies that
// RestoreFromRegistry invokes the WithOnCreate callback once per
// restored ChatSession. Without this, runtime wiring (EventHandler,
// MessageStateHandler) would silently miss every restored chat —
// the exact failure mode that F-38 surfaced in production.
//
// Locking note: onCreate is fired while the Manager lock is held
// (manager.go:108). Callers must NOT call mgr.Get / mgr.List from
// inside the callback without care; this test uses the callback
// only to record chatIDs, which is lock-safe.
func TestManager_RestoreFromRegistry_FiresOnCreate(t *testing.T) {
	csFile, _ := newTestStores(t)
	seedPersistedChatSession(t, csFile, "oc_alpha", "claude")
	seedPersistedChatSession(t, csFile, "oc_beta", "claude")

	mgr := NewManager().WithPersistence(csFile, nil)

	var (
		mu       sync.Mutex
		seenChat []string
	)
	mgr.WithOnCreate(func(cs *ChatSession) {
		mu.Lock()
		defer mu.Unlock()
		seenChat = append(seenChat, cs.ChatID)
	})

	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenChat) != 2 {
		t.Fatalf("onCreate should fire for both restored chats; got %d calls: %v", len(seenChat), seenChat)
	}
	want := map[string]bool{"oc_alpha": true, "oc_beta": true}
	for _, id := range seenChat {
		if !want[id] {
			t.Fatalf("unexpected chatID in onCreate: %q", id)
		}
	}
}

// TestManager_RestoreFromRegistry_WithOnCreateBefore_InstallsHandlers
// is the happy-path regression for the F-38 bug: when
// WithOnCreate is set BEFORE RestoreFromRegistry (the order
// cmd/nightme/run.go must use), every restored ChatSession has
// its EventHandler AND MessageStateHandler installed. Without
// these, the readPump's `if h != nil` guard short-circuits and
// all outgoing events vanish — no logs, no channel.Send, no
// reactions.
func TestManager_RestoreFromRegistry_WithOnCreateBefore_InstallsHandlers(t *testing.T) {
	csFile, _ := newTestStores(t)
	seedPersistedChatSession(t, csFile, "oc_alpha", "claude")
	seedPersistedChatSession(t, csFile, "oc_beta", "claude")

	mgr := NewManager().WithPersistence(csFile, nil)

	// Mirror the production wiring in cmd/nightme/run.go:
	// WithOnCreate goes BEFORE RestoreFromRegistry.
	mgr.WithOnCreate(func(cs *ChatSession) {
		cs.SetEventHandler(func(chatID string, _ *AgentSession, _ agent.AgentEvent, _ string) {
			_ = chatID
		})
		cs.SetMessageStateHandler(func(_, _ string, _ agent.MessageState) {})
	})

	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	for _, cs := range mgr.List() {
		if cs.EventHandler() == nil {
			t.Errorf("%s: EventHandler is nil — runtime did not install it", cs.ChatID)
		}
		if cs.MessageStateHandler() == nil {
			t.Errorf("%s: MessageStateHandler is nil — runtime did not install it", cs.ChatID)
		}
	}
}

// TestManager_RestoreFromRegistry_WithOnCreateAfter_MissesHandlers
// is the negative regression: if a future refactor moves
// WithOnCreate back to AFTER RestoreFromRegistry, this test fails
// loudly so the silent-failure bug can't return. The fix is
// documented in cmd/nightme/run.go's block comment around the
// WithOnCreate call site.
func TestManager_RestoreFromRegistry_WithOnCreateAfter_MissesHandlers(t *testing.T) {
	csFile, _ := newTestStores(t)
	seedPersistedChatSession(t, csFile, "oc_alpha", "claude")

	mgr := NewManager().WithPersistence(csFile, nil)

	// Restore FIRST (no handlers yet).
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	// Then register onCreate — too late for already-restored chats.
	mgr.WithOnCreate(func(cs *ChatSession) {
		cs.SetEventHandler(func(chatID string, _ *AgentSession, _ agent.AgentEvent, _ string) {
			_ = chatID
		})
		cs.SetMessageStateHandler(func(_, _ string, _ agent.MessageState) {})
	})

	cs := mgr.Get("oc_alpha")
	if cs == nil {
		t.Fatalf("ChatSession not restored")
	}
	if cs.EventHandler() != nil {
		t.Errorf("EventHandler unexpectedly non-nil — bug repro is no longer valid; restore this assertion's expectation")
	}
	if cs.MessageStateHandler() != nil {
		t.Errorf("MessageStateHandler unexpectedly non-nil — bug repro is no longer valid; restore this assertion's expectation")
	}
}

// TestManager_GetOrCreate_AfterRestore_FiresOnCreate verifies the
// symmetric path: a freshly-created ChatSession via GetOrCreate
// (post-Restore, e.g. first message in a brand new chat) also
// fires onCreate, so handlers are installed uniformly across
// restored + future chats.
func TestManager_GetOrCreate_AfterRestore_FiresOnCreate(t *testing.T) {
	mgr := NewManager()
	var (
		mu      sync.Mutex
		seenIDs []string
	)
	mgr.WithOnCreate(func(cs *ChatSession) {
		mu.Lock()
		defer mu.Unlock()
		seenIDs = append(seenIDs, cs.ChatID)
	})

	cs := mgr.GetOrCreate("oc_new", "claude")
	if cs.EventHandler() != nil {
		t.Fatalf("EventHandler unexpectedly set by test setup")
	}

	cs.SetEventHandler(func(string, *AgentSession, agent.AgentEvent, string) {})
	cs.SetMessageStateHandler(func(string, string, agent.MessageState) {})

	mu.Lock()
	defer mu.Unlock()
	if len(seenIDs) != 1 || seenIDs[0] != "oc_new" {
		t.Fatalf("onCreate should fire once with ChatID=oc_new; got %v", seenIDs)
	}
	if cs.EventHandler() == nil {
		t.Errorf("EventHandler should remain non-nil after onCreate ran")
	}
}