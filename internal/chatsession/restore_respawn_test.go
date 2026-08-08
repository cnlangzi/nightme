package chatsession

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestRestoreFromRegistry_DemotesRunningToDetached is the commit
// fix-6 regression test: after a daemon restart, the persisted
// status="running" AS is actually dead (handle is in-memory only).
// We must demote to Detached so the next LookupActiveAgentSession
// re-spawns a fresh process.
func TestRestoreFromRegistry_DemotesRunningToDetached(t *testing.T) {
	csFile, asFile := newTestStores(t)

	// Seed a persisted AgentSession whose status=running (the
	// persisted-entry scenario).
	const chatID = "oc_xxx"
	const csID = "cs_xxx"
	asID := "as_running"
	entries := &registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		PID:           99999, // stale PID
		Status:        registry.StatusRunning,
		Args:          nil,
	}
	if err := asFile.Upsert(entries); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}
	csEntry := &registry.ChatSessionEntry{
		ID:                   csID,
		ChatID:               chatID,
		ActiveCwd:            "/code/bailing",
		ActiveAgent:          "claude",
		PrimaryAgent:         "claude",
		AgentSessionIDs:      []string{asID},
		ActiveAgentSessionID: &asID,
		CreatedAt:            time.Now(),
		LastInteractionAt:    time.Now(),
	}
	if err := csFile.Upsert(csEntry); err != nil {
		t.Fatalf("Upsert CS: %v", err)
	}

	// Restore on a new manager.
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithSpawner(newFakeSpawner())
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	if cs == nil {
		t.Fatalf("ChatSession not restored")
	}

	// ActiveAS must be cleared (the in-memory handle is not
	// recoverable from disk).
	if cs.ActiveAgentSession() != nil {
		t.Fatalf("ActiveAS should be nil after restore; got %v", cs.ActiveAgentSession())
	}

	// The pool entry's status must be Demoted (not Running) so the
	// next Spawn will re-fork.
	var as *AgentSession
	for _, candidate := range cs.Pool() {
		if candidate.Agent == "claude" && candidate.Cwd == "/code/bailing" {
			as = candidate
			break
		}
	}
	if as == nil {
		t.Fatalf("Pool entry not found")
	}
	if as.Status() != StatusDetached {
		t.Fatalf("status: got %q, want Detached (Demoted from Running on restart)", as.Status())
	}
	if as.PID() != 0 {
		t.Fatalf("PID should be cleared on restart, got %d", as.PID())
	}
}

// TestRestoreFromRegistry_ThenLookupTriggersSpawn verifies the
// end-to-end fix: after a daemon restart, the next "hi" message
// successfully spawns a fresh agent (because the demoted status
// allows LookupActiveAgentSession to call Spawn).
func TestRestoreFromRegistry_ThenLookupTriggersSpawn(t *testing.T) {
	csFile, asFile := newTestStores(t)

	const chatID = "oc_xxx"
	const csID = "cs_xxx"
	asID := "as_stale"
	entries := &registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		PID:           99999,
		Status:        registry.StatusRunning,
	}
	if err := asFile.Upsert(entries); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}
	csEntry := &registry.ChatSessionEntry{
		ID:                   csID,
		ChatID:               chatID,
		ActiveCwd:            "/code/bailing",
		ActiveAgent:          "claude",
		PrimaryAgent:         "claude",
		AgentSessionIDs:      []string{asID},
		ActiveAgentSessionID: &asID,
		CreatedAt:            time.Now(),
		LastInteractionAt:    time.Now(),
	}
	if err := csFile.Upsert(csEntry); err != nil {
		t.Fatalf("Upsert CS: %v", err)
	}

	spawner := newFakeSpawner()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession post-restore: %v", err)
	}
	if as.Status() != StatusRunning {
		t.Fatalf("after re-spawn: got %q, want Running", as.Status())
	}
	if as.PID() == 0 {
		t.Fatalf("after re-spawn: PID should be non-zero")
	}
	if as.Handle() == nil {
		t.Fatalf("after re-spawn: Handle should be non-nil")
	}

	// SendBlocks should now work (handle is live).
	if err := as.SendBlocks(context.Background(), nil); err != nil {
		// nil blocks is a no-op; expect nil error.
		t.Fatalf("SendBlocks post-respawn: %v", err)
	}
}

// TestRestoreFromRegistry_PreservesResumeIDOnRespawn is the
// regression test for the resume-id-loss bug: after a daemon
// restart, a Detached AgentSession restored from disk must keep
// its captured ResumeID so the next Spawn replays `--resume <id>`
// to the bridge. The pre-fix LookupActiveAgentSession created a
// fresh AgentSession on the spawn path, which discarded the
// restored entry (and its ResumeID), forcing every restart to
// start a brand-new agent session.
func TestRestoreFromRegistry_PreservesResumeIDOnRespawn(t *testing.T) {
	csFile, asFile := newTestStores(t)

	const chatID = "oc_xxx"
	const csID = "cs_xxx"
	asID := "as_with_resume"
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            asID,
		ChatSessionID: csID,
		Agent:         "claude",
		Cwd:           "/code/bailing",
		Status:        registry.StatusDetached,
		ResumeID:      "sess-from-prior-run",
	}); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}
	csIDCopy := csID
	if err := csFile.Upsert(&registry.ChatSessionEntry{
		ID:                     csID,
		ChatID:                 chatID,
		ActiveCwd:              "/code/bailing",
		ActiveAgent:            "claude",
		PrimaryAgent:           "claude",
		AgentSessionIDs:        []string{asID},
		ActiveAgentSessionID:   &csIDCopy,
	}); err != nil {
		t.Fatalf("Upsert CS: %v", err)
	}

	spawner := newFakeSpawner()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	cs := mgr.Get(chatID)
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if as.ID != asID {
		t.Errorf("respawn replaced the pool entry: got ID %q, want %q (ResumeID round-trip depends on identity continuity)",
			as.ID, asID)
	}
	if got := as.ResumeID(); got != "sess-from-prior-run" {
		t.Errorf("in-memory ResumeID lost on respawn: got %q, want %q", got, "sess-from-prior-run")
	}
	if got := spawner.lastResumeID; got != "sess-from-prior-run" {
		t.Errorf("Spawner did not receive the resume id: got %q, want %q", got, "sess-from-prior-run")
	}
}

// TestFromAgentSessionEntry_InitializesEventQueue is the T-alive
// regression for the production test20 hang (2026-08-07):
//
// After daemon restart, AgentSessions are rebuilt via
// FromAgentSessionEntry. The CS-AS Phase 1 eventQueue field was
// only allocated in NewAgentSession, so restored ASes carried a
// nil channel. Spawn → startReadPump then blocked forever on the
// first `as.eventQueue <- EnrichedEvent{}` (send on nil channel),
// s.events filled, claude's stdout pipe backed up, and the user
// saw SendBlocks ok with no OutReply.
//
// This test asserts the restored AS has a usable eventQueue AND
// that a bridge event pushed after Spawn actually lands on
// ActiveEvents (i.e. the full restore → spawn → readpump → pump
// pipeline works).
func TestFromAgentSessionEntry_InitializesEventQueue(t *testing.T) {
	entry := &registry.AgentSessionEntry{
		ID:            "as_restored",
		ChatSessionID: "cs_restored",
		Agent:         "claude",
		Cwd:           "/tmp/ws",
		Status:        registry.StatusRunning, // demoted to Detached by FromAgentSessionEntry
		ResumeID:      "resume-xyz",
	}
	as := FromAgentSessionEntry(entry)
	if as == nil {
		t.Fatal("FromAgentSessionEntry returned nil")
	}
	if as.Events() == nil {
		t.Fatal("restored AS has nil eventQueue — readpump will deadlock on first event")
	}
	if !as.IsReady() {
		t.Fatal("restored AS IsReady=false — TryFlush will SKIP until Spawn re-arms")
	}

	// End-to-end: wire into a ChatSession, Spawn, push a bridge
	// event, assert PumpEvents delivers it via the AgentEventBus subscriber.
	csFile, asFile := newTestStores(t)
	if err := asFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert AS: %v", err)
	}
	asID := entry.ID
	if err := csFile.Upsert(&registry.ChatSessionEntry{
		ID:                   entry.ChatSessionID,
		ChatID:               "oc_restored",
		ActiveCwd:            entry.Cwd,
		ActiveAgent:          entry.Agent,
		PrimaryAgent:         entry.Agent,
		AgentSessionIDs:      []string{asID},
		ActiveAgentSessionID: &asID,
	}); err != nil {
		t.Fatalf("Upsert CS: %v", err)
	}

	spawner := newFakeSpawner()
	mgr := NewManager().
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}
	cs := mgr.Get("oc_restored")
	if cs == nil {
		t.Fatal("ChatSession not restored")
	}

	received := make(chan struct{}, 1)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		ev := env.Event
		if ev.Kind == agent.EventText && ev.Text == "hello-after-restore" {
			select {
			case received <- struct{}{}:
			default:
			}
		}
		return false
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	live, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if live.Events() == nil {
		t.Fatal("post-Spawn restored AS still has nil eventQueue")
	}

	// Drive the fake bridge: PushEvent → AS readpump → eventQueue
	// → PumpEvents → eventHandler.
	fake := spawner.Get(entry.Agent, entry.Cwd)
	if fake == nil {
		t.Fatal("spawner did not capture a handle")
	}
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hello-after-restore"})

	select {
	case <-received:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: bridge event never reached eventHandler after restore — eventQueue likely nil/broken")
	}
}