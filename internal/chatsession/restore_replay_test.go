package chatsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
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
// entry for lazy mounting preserves its in-flight mirror and SessionID.
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
		InFlightMessages: []registry.InFlightMessageRef{
			{
				ID: "m_original_1",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "first half"},
				},
				ReceivedAt: now,
			},
			{
				ID: "m_original_2",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "second half"},
				},
				ReceivedAt: now.Add(time.Second),
			},
		},
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

	// F-62 §3.3.1: cs.queue stays empty after restore. The old
	// "replay into queue" behavior is what bled in-flight messages
	// across ASes when the user later /cwd'd to a different agent.
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("F-62: cs.queue after restore = %v, want empty (no replay)", got)
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
	// F-62 §3.3.4: in-flight is in AS's in-memory mirror only;
	// FromAgentSessionEntry hydrates it. Verify the slice is present
	// at restore time so the next /cwd / hadPrior can clear it.
	if got := found.Entry().InFlightMessages; len(got) != 2 {
		t.Errorf("AS.inFlightMessages after restore = %d, want 2", len(got))
	}
}

// TestRestoreFromRegistry_LegacyEntryWithoutInFlightMessages
// confirms that an entry written before the InFlightMessages field
// existed restores cleanly: queue stays empty, no panic.
func TestRestoreFromRegistry_LegacyEntryWithoutInFlightMessages(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_legacy"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	// Legacy entry: no InFlightMessages field. (Upsert writes the
	// current schema — so simulate the legacy shape by writing JSON
	// directly.)
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

// TestRestoreFromRegistry_EmptyInFlightSlice ensures that a present-
// but-empty InFlightMessages doesn't trigger any replay.
func TestRestoreFromRegistry_EmptyInFlightSlice(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_empty"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:               "as_empty_1",
		ChatSessionID:    csID,
		Agent:            "claude",
		Cwd:              "/code/bailing",
		Status:           registry.StatusDetached,
		SessionID:        "sess-empty",
		CreatedAt:        now,
		LastRunAt:        now,
		InFlightMessages: []registry.InFlightMessageRef{}, // explicitly empty
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
		t.Errorf("queue = %v, want empty when InFlightMessages is []", got)
	}
}

// TestRestoreFromRegistry_MultipleAgentSessionsEachHydrateOwn
// asserts that multiple persisted ASes remain independent while
// restore leaves the active pool and queue empty. Lazy rebuilding
// preserves each entry's own in-flight mirror.
func TestRestoreFromRegistry_MultipleAgentSessionsEachHydrateOwn(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_multi"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Now()
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_a",
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/A",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_a_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "a1"}}, ReceivedAt: now},
		},
	}); err != nil {
		t.Fatalf("Upsert as_a: %v", err)
	}
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_b",
		ChatSessionID: csID,
		Agent:         "codex",
		Cwd:           "/code/B",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_b_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "b1"}}, ReceivedAt: now},
		},
	}); err != nil {
		t.Fatalf("Upsert as_b: %v", err)
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

	// F-62 §3.3.1: cs.queue must stay empty. The previous behavior
	// of pushing both AS's in-flight into the same chat-level queue
	// is what caused the cross-AS misdelivery bug.
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("F-62: cs.queue after restore = %v, want empty", got)
	}

	if got := len(cs.Pool()); got != 0 {
		t.Fatalf("active pool after restore = %d, want 0 until Lookup", got)
	}
	for _, id := range []string{"as_a", "as_b"} {
		entry, _ := asFile.Get(id)
		globalPool.Put(chatID, FromAgentSessionEntry(entry))
	}
	if a := globalPool.Get(chatID, "/code/A", "claude"); a == nil || len(a.Entry().InFlightMessages) != 1 ||
		a.Entry().InFlightMessages[0].ID != "m_a_1" {
		t.Errorf("as_a in-flight = %+v, want [m_a_1]", a)
	}
	if b := globalPool.Get(chatID, "/code/B", "codex"); b == nil || len(b.Entry().InFlightMessages) != 1 ||
		b.Entry().InFlightMessages[0].ID != "m_b_1" {
		t.Errorf("as_b in-flight = %+v, want [m_b_1]", b)
	}
}

// TestRestoreFromRegistry_ReplayThenSpawnResumesAgent is the
// end-to-end chain that proves restart-replay works: a persisted
// AS with InFlightMessages + SessionID gets restored, replayed
// into the queue, and on next Spawn the spawner receives the
// persisted session id so the bridge can issue `--resume <id>`.
// This is the single test that ties the three PRs (P1 data model,
// P2 hooks, P3 replay) together.
// TestRestoreFromRegistry_ResumeAfterSpawnClearsInFlight asserts
// the end-to-end F-62 contract:
//
//  1. RestoreFromRegistry hydrates the AS in-memory mirror only;
//     cs.queue stays empty.
//  2. The next LookupSelectedAgentSession on the hadPrior branch
//     (F-62 §3.3.3) clears the AS's in-flight mirror and persists
//     the empty state — Spawn then resolves with the captured
//     SessionID preserved (so --resume works), but the abandoned
//     message is gone before any new TryFlush can pick it up.
func TestRestoreFromRegistry_ResumeAfterSpawnClearsInFlight(t *testing.T) {
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
		InFlightMessages: []registry.InFlightMessageRef{
			{
				ID: "m_resume_1",
				Blocks: []agent.ContentBlock{
					{Type: agent.ContentText, Text: "before crash"},
				},
				ReceivedAt: now,
			},
		},
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

	// F-62 §3.3.1: no replay into cs.queue.
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("F-62: cs.queue after restore = %v, want empty", got)
	}

	// Trigger the spawn chain: LookupSelectedAgentSession sees the
	// detached AS (hadPrior branch), calls ClearInFlight first
	// (F-62 §3.3.3), then Spawn which forwards SessionID to the
	// spawner (so the bridge can issue --resume).
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

	// F-62 §3.3.3 (hadPrior branch): the in-flight mirror is gone
	// after this Spawn — the next new TryFlush cannot pick the
	// "before crash" message up. cs.queue still empty.
	if got := as.Entry().InFlightMessages; len(got) != 0 {
		t.Errorf("F-62: AS.inFlightMessages after Spawn = %v, want empty", got)
	}
	if got := cs.queue.Peek(); len(got) != 0 {
		t.Errorf("F-62: cs.queue after Spawn = %v, want empty", got)
	}
}

// TestSetSelectedCwd_ClearsOldASInFlight asserts the F-62 §3.3.2
// contract: when the user /cwd's to a different workspace, the
// previously-selected AS's in-flight mirror is cleared (in-memory
// + on disk) BEFORE the new selectedCwd assignment. The new AS
// (if any) is untouched. This is the chat-session-level "new
// session" boundary that closes the cross-CWD misdelivery bug.
func TestSetSelectedCwd_ClearsOldASInFlight(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_cwd_clear"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)

	// Old AS at /code/A with two stale in-flight messages — the
	// "hung" state we want to drop on /cwd.
	oldASID := "as_cwd_old"
	oldEntry := &registry.AgentSessionEntry{
		ID:            oldASID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/A",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_old_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hung part 1"}}, ReceivedAt: now},
			{ID: "m_old_2", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hung part 2"}}, ReceivedAt: now.Add(time.Second)},
		},
	}
	if err := asFile.Upsert(oldEntry); err != nil {
		t.Fatalf("Upsert old AS: %v", err)
	}

	// New AS at /code/B with a fresh in-flight (this should be
	// untouched by the /cwd that drops the old one).
	newASID := "as_cwd_new"
	newEntry := &registry.AgentSessionEntry{
		ID:            newASID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/B",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_new_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "fresh"}}, ReceivedAt: now},
		},
	}
	if err := asFile.Upsert(newEntry); err != nil {
		t.Fatalf("Upsert new AS: %v", err)
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

	// Set initial cwd to /code/A so the old AS is "selected".
	if err := cs.SetSelectedCwd("/code/A"); err != nil {
		t.Fatalf("initial SetSelectedCwd: %v", err)
	}
	oldAS := mountPersistedASForTest(t, cs, globalPool, asFile, chatID, oldASID)
	newAS := FromAgentSessionEntry(newEntry)
	globalPool.Put(chatID, newAS)

	// Now /cwd to /code/B — the chat-session "new session"
	// boundary. F-62 §3.3.2 must clear the old AS's in-flight.
	if err := cs.SetSelectedCwd("/code/B"); err != nil {
		t.Fatalf("switch SetSelectedCwd: %v", err)
	}

	if got := len(cs.Pool()); got != 0 {
		t.Fatalf("active pool after cwd switch = %d, want 0", got)
	}
	if warm := globalPool.Get(chatID, "/code/A", "claude"); warm != oldAS {
		t.Fatalf("old AS was not retained warm in AgentSessionPool")
	}
	if got := oldAS.Entry().InFlightMessages; len(got) != 0 {
		t.Errorf("F-62 §3.3.2: old AS.inFlightMessages after /cwd = %v, want empty", got)
	}

	// Disk also reflects the empty state (ClearInFlight persists).
	reread, ok := asFile.Get(oldASID)
	if !ok || reread == nil {
		t.Fatalf("re-read old AS: ok=%v entry=%v", ok, reread)
	}
	if len(reread.InFlightMessages) != 0 {
		t.Errorf("F-62 §3.3.2: persisted old AS.InFlightMessages = %v, want empty", reread.InFlightMessages)
	}

	// New AS stays warm and untouched; cwd switching does not pre-mount it.
	if warm := globalPool.Get(chatID, "/code/B", "claude"); warm != newAS {
		t.Fatalf("new AS %s missing from AgentSessionPool", newASID)
	}
	if got := newAS.Entry().InFlightMessages; len(got) != 1 || got[0].ID != "m_new_1" {
		t.Errorf("new AS.inFlightMessages = %v, want [m_new_1] (must be untouched)", got)
	}
}

// TestSetSelectedCwd_SameCwdIsNoop asserts that re-asserting the
// same cwd does NOT clear the old AS's in-flight (no spurious
// drops). The "new session" boundary fires only when the cwd
// actually changes.
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
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_keep_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "kept"}}, ReceivedAt: now},
		},
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
	// Re-assert the same cwd.
	if err := cs.SetSelectedCwd("/code/same"); err != nil {
		t.Fatalf("second SetSelectedCwd: %v", err)
	}

	if active := cs.Pool(); len(active) != 1 || active[0] != as {
		t.Fatalf("same-cwd setter detached AS %s", asID)
	}
	if got := as.Entry().InFlightMessages; len(got) != 1 || got[0].ID != "m_keep_1" {
		t.Errorf("AS.inFlightMessages after same-cwd /cwd = %v, want [m_keep_1] (no spurious drop)", got)
	}
}

// TestSetSelectedAgent_ClearsOldAgentInFlight asserts the F-62
// §3.3.3 contract: when the user /use's to a different agent, the
// previously-selected (oldAgent, currentCwd) AS's in-flight mirror
// is cleared (in-memory + on-disk) BEFORE the new selectedAgent
// assignment. The new AS (if any) is untouched. This is the
// code-review-found gap: SetSelectedAgent was previously only
// mutating cs.selectedAgent, leaving the old agent's AS in the pool
// with its in-flight slice forever (zombie reloaded on every
// daemon restart). The LookupSelectedAgentSession hadPrior branch
// cannot cover this because the pool key changes when selectedAgent
// changes.
func TestSetSelectedAgent_ClearsOldAgentInFlight(t *testing.T) {
	csFile, asFile := newTestStores(t)
	chatID := "oc_use_clear"
	csID := seedPersistedChatSession(t, csFile, chatID, "claude")

	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)

	oldASID := "as_use_old"
	oldEntry := &registry.AgentSessionEntry{
		ID:            oldASID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/A",
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_use_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "stuck on claude"}}, ReceivedAt: now},
		},
	}
	if err := asFile.Upsert(oldEntry); err != nil {
		t.Fatalf("Upsert old AS: %v", err)
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

	// Set initial cwd + agent so the old AS is "selected".
	if err := cs.SetSelectedCwd("/code/A"); err != nil {
		t.Fatalf("initial SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("initial SetSelectedAgent: %v", err)
	}
	oldAS := mountPersistedASForTest(t, cs, globalPool, asFile, chatID, oldASID)

	// Now /use codex — the chat-session "new session" boundary on
	// agent switch. F-62 §3.3.3 must clear the old claude AS's
	// in-flight, even though the cwd did not change.
	if err := cs.SetSelectedAgent("codex"); err != nil {
		t.Fatalf("switch SetSelectedAgent: %v", err)
	}

	if warm := globalPool.Get(chatID, "/code/A", "claude"); warm != oldAS {
		t.Fatalf("old AS %s missing from AgentSessionPool", oldASID)
	}
	if got := oldAS.Entry().InFlightMessages; len(got) != 0 {
		t.Errorf("F-62 §3.3.3: old AS.inFlightMessages after /use = %v, want empty", got)
	}

	// Disk also reflects the empty state (ClearInFlight persists).
	reread, ok := asFile.Get(oldASID)
	if !ok || reread == nil {
		t.Fatalf("re-read old AS: ok=%v entry=%v", ok, reread)
	}
	if len(reread.InFlightMessages) != 0 {
		t.Errorf("F-62 §3.3.3: persisted old AS.InFlightMessages = %v, want empty", reread.InFlightMessages)
	}
}

// TestSetSelectedAgent_SameAgentIsNoop asserts that re-asserting
// the same agent does NOT clear the old AS's in-flight (no spurious
// drops). The "new session" boundary fires only when the agent
// actually changes.
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
		InFlightMessages: []registry.InFlightMessageRef{
			{ID: "m_keep_use_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "kept"}}, ReceivedAt: now},
		},
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
	// Re-assert the same agent.
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("second SetSelectedAgent: %v", err)
	}

	if active := cs.Pool(); len(active) != 1 || active[0] != as {
		t.Fatalf("same-agent setter detached AS %s", asID)
	}
	if got := as.Entry().InFlightMessages; len(got) != 1 || got[0].ID != "m_keep_use_1" {
		t.Errorf("AS.inFlightMessages after same-agent /use = %v, want [m_keep_use_1] (no spurious drop)", got)
	}
}
