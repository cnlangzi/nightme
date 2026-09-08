package chatsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

func mountPersistedASForTest(t *testing.T, cs *ChatSession, pool *AgentSessionPool, asFile *registry.AgentSessionFile, chatID, asID string) *AgentSession {
	t.Helper()
	entry, ok := asFile.Get(asID)
	if !ok {
		t.Fatalf("persisted AS %s missing", asID)
	}
	as := FromAgentSessionEntry(entry)
	pool.Put(chatID, as)
	cs.mu.Lock()
	cs.attachAgentSessionLocked(as)
	cs.selectAgentSessionLocked(as)
	cs.mu.Unlock()
	return as
}

// TestRestoreFromRegistry_HydratesASInMemoryNotQueue asserts that
// restore leaves cs.pool and cs.queue empty. Rebuilding the persisted
// entry for lazy mounting preserves SessionID so the next Spawn
// can issue `--resume <id>`.
func TestRestoreFromRegistry_HydratesASInMemoryNotQueue(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_replay"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	asID := "as_replay_1"
	asEntry := &registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		Status:        registry.StatusDetached,
		SessionID:     "sess-resume-xyz",
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(asEntry); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	globalPool := NewAgentSessionPool()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithAgentSessionPool(globalPool)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}

	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("cs.queue after restore = %v, want empty (no replay)", got)
	}

	if got := len(cs.Pool()); got != 0 {
		t.Fatalf("active pool after restore = %d, want 0 until Lookup", got)
	}
	entry, _ := asFile.Get(asID)
	found := FromAgentSessionEntry(entry)
	globalPool.Put(chatID, found)
	if found.SessionID() != "sess-resume-xyz" {
		t.Errorf("restored SessionID = %q, want sess-resume-xyz", found.SessionID())
	}
}

// TestRestoreFromRegistry_LegacyEntryPersistsAcrossRestart
// confirms that an entry written before any newer schema changes
// restores cleanly: queue stays empty, no panic.
func TestRestoreFromRegistry_LegacyEntryPersistsAcrossRestart(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_legacy"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	asID := "as_legacy_1"
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		Status:        registry.StatusDetached,
		SessionID:     "sess-legacy",
		CreatedAt:     now,
		LastRunAt:     now,
	}); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	globalPool := NewAgentSessionPool()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithAgentSessionPool(globalPool)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("queue = %v, want empty for legacy entry", got)
	}
}

// TestRestoreFromRegistry_ResumeAfterSpawnForwardsSessionID is
// the end-to-end chain that proves --resume flows through restore
// and the next Spawn: a persisted AS with a captured SessionID
// gets restored (queue stays empty), and the subsequent
// LookupSelectedAgentSession reattaches the AS and Spawn hands the
// session id to the spawner so the bridge can issue --resume.
func TestRestoreFromRegistry_ResumeAfterSpawnForwardsSessionID(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_resume"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	asID := "as_resume_1"
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		Status:        registry.StatusDetached,
		SessionID:     "sess-end-to-end",
		CreatedAt:     now,
		LastRunAt:     now,
	}); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	spawner := newFakeSpawner()

	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("restored chat missing for %q", chatID)
	}

	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("cs.queue after restore = %v, want empty", got)
	}

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if as == nil {
		t.Fatal("LookupSelectedAgentSession returned nil")
	}

	if spawner.calls == 0 {
		t.Error("spawner.Spawn was not called; expected at least one invocation")
	}
	if got := spawner.lastResumeID; got != "sess-end-to-end" {
		t.Errorf("spawner.lastResumeID = %q, want sess-end-to-end", got)
	}
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("cs.queue after Spawn = %v, want empty", got)
	}
}

// TestSetSelectedCwd_SameCwdIsNoop asserts that re-asserting the
// same cwd does not detach the currently mounted AS.
func TestSetSelectedCwd_SameCwdIsNoop(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_cwd_same"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	asID := "as_cwd_same"
	asEntry := &registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/same",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(asEntry); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	globalPool := NewAgentSessionPool()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithAgentSessionPool(globalPool)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if err := cs.SetSelectedCwd("/code/same"); err != nil {
		t.Fatalf("first SetSelectedCwd: %v", err)
	}
	as := mountPersistedASForTest(t, cs, globalPool, asFile, chatID, asID)
	if err := cs.SetSelectedCwd("/code/same"); err != nil {
		t.Fatalf("second SetSelectedCwd: %v", err)
	}

	if active := cs.Pool(); len(active) != 1 || active[0] != as {
		t.Fatalf("same-cwd setter detached AS %s", asID)
	}
}

// TestSetSelectedAgent_SameAgentIsNoop asserts that re-asserting
// the same agent does not detach the currently mounted AS.
func TestSetSelectedAgent_SameAgentIsNoop(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_use_same"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	asID := "as_use_same"
	asEntry := &registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/A",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(asEntry); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}

	globalPool := NewAgentSessionPool()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithAgentSessionPool(globalPool)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if err := cs.SetSelectedCwd("/code/A"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("first SetSelectedAgent: %v", err)
	}
	as := mountPersistedASForTest(t, cs, globalPool, asFile, chatID, asID)
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("second SetSelectedAgent: %v", err)
	}

	if active := cs.Pool(); len(active) != 1 || active[0] != as {
		t.Fatalf("same-agent setter detached AS %s", asID)
	}
}
