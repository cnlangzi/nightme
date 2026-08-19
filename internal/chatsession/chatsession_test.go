package chatsession

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/registry"
)

// newTestStores lives in test_helpers_store_test.go — it was
// extracted there while this file was shelved, and other test
// files now depend on it.

func TestNewAndBasics(t *testing.T) {
	cs, _ := New("oc_xxx", "claude")
	if cs.ChatID != "oc_xxx" {
		t.Fatalf("ChatID: got %q", cs.ChatID)
	}
	if cs.ID == "" {
		t.Fatalf("ID should be derived from chatID")
	}
	if cs.SelectedCwd() != "" {
		t.Fatalf("initial SelectedCwd should be empty")
	}
	if cs.SelectedAgent() != "claude" {
		t.Fatalf("initial SelectedAgent should be seeded from PrimaryAgent; got %q", cs.SelectedAgent())
	}
	if cs.PrimaryAgent() != "claude" {
		t.Fatalf("PrimaryAgent: got %q", cs.PrimaryAgent())
	}
	if cs.Pool() == nil {
		t.Fatalf("Pool should be non-nil empty slice")
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("Pool should be empty initially")
	}
	if cs.SelectedAgentSession() != nil {
		t.Fatalf("selectedAS should be nil initially")
	}
}

func TestSetActiveCwdDoesNotSpawn(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	if err := cs.SetSelectedCwd("/code/bailing"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if cs.SelectedCwd() != "/code/bailing" {
		t.Fatalf("SelectedCwd: got %q", cs.SelectedCwd())
	}
	// Critical: SetSelectedCwd must NOT add anything to the pool.
	if len(cs.Pool()) != 0 {
		t.Fatalf("SetSelectedCwd should not spawn; pool size=%d", len(cs.Pool()))
	}
}

func TestSetActiveAgentDoesNotSpawn(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	if cs.SelectedAgent() != "claude" {
		t.Fatalf("SelectedAgent: got %q", cs.SelectedAgent())
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("SetSelectedAgent should not spawn; pool size=%d", len(cs.Pool()))
	}
}

func TestLookupActiveAgentSession_RequiresCwd(t *testing.T) {
	cs, _ := New("oc_xxx", "claude")
	_, err := cs.LookupSelectedAgentSession()
	if err != ErrNoSelectedCwd {
		t.Fatalf("expected ErrNoSelectedCwd, got %v", err)
	}
}

func TestLookupActiveAgentSession_SpawnWhenMissing(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if as == nil {
		t.Fatalf("expected AgentSession, got nil")
	}
	if as.Agent != "claude" || as.Cwd != "/code/bailing" {
		t.Fatalf("AS mismatch: agent=%q cwd=%q", as.Agent, as.Cwd)
	}
	if as.Status() != StatusDetached {
		// commit 6 spawn is data-only; status=Detached (no real fork yet).
		t.Fatalf("expected StatusDetached pre-spawn, got %q", as.Status())
	}

	// Pool should have exactly one entry.
	if len(cs.Pool()) != 1 {
		t.Fatalf("pool size: got %d, want 1", len(cs.Pool()))
	}

	// Persistence should have one AgentSessionEntry.
	all := asFile.List()
	if len(all) != 1 {
		t.Fatalf("persisted AgentSessions: got %d, want 1", len(all))
	}
	if all[0].ID != as.ID {
		t.Fatalf("persisted ID mismatch: %s vs %s", all[0].ID, as.ID)
	}
}

func TestLookupActiveAgentSession_ReusesPoolEntry(t *testing.T) {
	csFile, asFile := newTestStores(t)
	// commit fix-6 followup: pool-hit only reuses if the entry is
	// still effectively running (StatusRunning + non-nil Handle).
	// Without a Spawner, the AgentSession stays Detached between
	// lookups, so we wire a Spawner to make the test deterministic.
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(newFakeSpawner())
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")

	as1, _ := cs.LookupSelectedAgentSession()
	as2, _ := cs.LookupSelectedAgentSession()

	if as1.ID != as2.ID {
		t.Fatalf("expected same AgentSession, got %s vs %s", as1.ID, as2.ID)
	}
	if len(cs.Pool()) != 1 {
		t.Fatalf("pool should still have 1 entry, got %d", len(cs.Pool()))
	}
}

// TestLookupActiveAgentSession_UseOverrides covers the v1.2-final
// single-path semantics: after /use codex, the lookup resolves
// (codex, cwd), not whatever cfg.Primary seeded as initial
// activeAgent. The pool entry for the seeded agent (claude, cwd)
// stays; the new entry is spawned alongside it.
func TestLookupActiveAgentSession_UseOverrides(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")
	claudeAS, _ := cs.LookupSelectedAgentSession() // spawns (claude, cwd)
	if claudeAS.Agent != "claude" {
		t.Fatalf("first spawn: got %q, want claude", claudeAS.Agent)
	}

	// /use codex. (claude, cwd) is in the pool; (codex, cwd) is not.
	// Lookup must spawn codex (not reuse the seeded claude entry).
	cs.SetSelectedAgent("codex")
	codexAS, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if codexAS.Agent != "codex" {
		t.Fatalf("after /use codex: got %q, want codex", codexAS.Agent)
	}
	if len(cs.Pool()) != 2 {
		t.Fatalf("expected pool size 2 (claude + codex), got %d", len(cs.Pool()))
	}
}

func TestLookupActiveAgentSession_SpawnWhenDefaultAlsoMiss(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("codex") // active != default

	// Neither (codex, /code/bailing) nor (claude, /code/bailing) is in pool.
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	// Step 3 spawns (codex, /code/bailing).
	if as.Agent != "codex" {
		t.Fatalf("expected spawn codex, got %q", as.Agent)
	}
	if len(cs.Pool()) != 1 {
		t.Fatalf("pool size: got %d, want 1", len(cs.Pool()))
	}
}

// killAllForTest simulates the /kill (no args) workflow using the
// lifecycle accessors that kill.KillAllAgents would compose:
// AgentSessionsInCwd → per-entry Close → per-entry DropAgentSession.
// Tests use this in place of the kill package directly to avoid an
// import cycle (kill imports chatsession; chatsession tests cannot
// transitively import kill).
func killAllForTest(t *testing.T, cs *ChatSession) {
	t.Helper()
	snapshot := cs.AgentSessionsInCwd(cs.SelectedCwd())
	for _, as := range snapshot {
		_ = as.Close()
		cs.DropAgentSession(as)
	}
}

func TestKillAllClearsPool(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")
	cs.LookupSelectedAgentSession() // spawns claude

	if len(cs.Pool()) != 1 {
		t.Fatalf("precondition: pool size=%d", len(cs.Pool()))
	}

	killAllForTest(t, cs)
	if len(cs.Pool()) != 0 {
		t.Fatalf("KillAllAgents should clear pool; size=%d", len(cs.Pool()))
	}
	if cs.SelectedAgentSession() != nil {
		t.Fatalf("selectedAS should be nil after KillAllAgents")
	}
	// Persistence also cleared.
	all := asFile.GetByChatPool(cs.ID)
	if len(all) != 0 {
		t.Fatalf("persisted AgentSessions should be cleared; got %d", len(all))
	}
	// activeCwd and activeAgent survive (only the pool is cleared).
	if cs.SelectedCwd() != "/code/bailing" {
		t.Fatalf("SelectedCwd should survive /kill; got %q", cs.SelectedCwd())
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	csPath := filepath.Join(dir, "chat_sessions.json")
	asPath := filepath.Join(dir, "agent_sessions.json")

	// Write phase.
	csFile, asFile, err := func() (*chatstore.Store, *registry.AgentSessionFile, error) {
		csFile, err := chatstore.New(csPath)
		if err != nil {
			return nil, nil, err
		}
		asFile, err := registry.OpenAgentSessionFile(asPath)
		if err != nil {
			return nil, nil, err
		}
		return csFile, asFile, nil
	}()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")
	cs.LookupSelectedAgentSession()

	// Read phase: reload from disk.
	csFile2, err := chatstore.New(csPath)
	if err != nil {
		t.Fatalf("reopen chat: %v", err)
	}
	asFile2, err := registry.OpenAgentSessionFile(asPath)
	if err != nil {
		t.Fatalf("reopen agent: %v", err)
	}

	entry, ok := csFile2.Get("oc_xxx")
	if !ok {
		t.Fatalf("ChatSessionEntry should be persisted by chatID")
	}
	if entry.SelectedCwd != "/code/bailing" {
		t.Fatalf("persisted SelectedCwd: %q", entry.SelectedCwd)
	}
	if entry.SelectedAgent != "claude" {
		t.Fatalf("persisted SelectedAgent: %q", entry.SelectedAgent)
	}
	if len(entry.AgentSessionIDs) != 1 {
		t.Fatalf("persisted AgentSessionIDs: got %d, want 1", len(entry.AgentSessionIDs))
	}
	if entry.SelectedAgentSessionID == nil {
		t.Fatalf("SelectedAgentSessionID should be set")
	}

	agentEntries := asFile2.GetByChatPool(entry.ID)
	if len(agentEntries) != 1 {
		t.Fatalf("AgentSessions persisted: got %d, want 1", len(agentEntries))
	}
	if agentEntries[0].Agent != "claude" {
		t.Fatalf("Agent: %q", agentEntries[0].Agent)
	}
	if agentEntries[0].Cwd != "/code/bailing" {
		t.Fatalf("Cwd: %q", agentEntries[0].Cwd)
	}
}

func TestChatIDDerivationDeterministic(t *testing.T) {
	a, _ := New("oc_abc", "claude")
	b, _ := New("oc_abc", "codex")
	if a.ID != b.ID {
		t.Fatalf("ID should be deterministic by chatID: %s vs %s", a.ID, b.ID)
	}

	c, _ := New("oc_xyz", "claude")
	if c.ID == a.ID {
		t.Fatalf("different chatID should give different ID")
	}
}

func TestAgentSessionStatusTransitions(t *testing.T) {
	as := NewAgentSession("as_1", "cs_xxx", "claude", "/x", nil)
	if as.Status() != StatusDetached {
		t.Fatalf("initial status: %q", as.Status())
	}

	as.SetRunning(12345)
	if as.Status() != StatusRunning {
		t.Fatalf("after SetRunning: %q", as.Status())
	}
	if as.PID() != 12345 {
		t.Fatalf("PID: %d", as.PID())
	}

	as.SetExited(0)
	if as.Status() != StatusExited {
		t.Fatalf("after SetExited: %q", as.Status())
	}
	if as.PID() != 0 {
		t.Fatalf("PID should be 0 after exit, got %d", as.PID())
	}
	if as.ExitCode() == nil || *as.ExitCode() != 0 {
		t.Fatalf("ExitCode: %v", as.ExitCode())
	}
}

func TestChatSessionConcurrentSetAndLookup(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")

	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = cs.LookupSelectedAgentSession()
		}()
		go func() {
			defer wg.Done()
			_ = cs.SetSelectedAgent("claude")
		}()
	}
	wg.Wait()

	// All lookups should resolve to the SAME AgentSession (no
	// double-spawn under concurrent access).
	if got := len(cs.Pool()); got != 1 {
		t.Fatalf("concurrent lookup should produce 1 pool entry, got %d", got)
	}
}

func TestEmptyCwdRejected(t *testing.T) {
	cs, _ := New("oc_xxx", "claude")
	if err := cs.SetSelectedCwd(""); err == nil {
		t.Fatalf("expected error for empty cwd")
	}
	if err := cs.SetSelectedAgent(""); err == nil {
		t.Fatalf("expected error for empty agent")
	}
}

// Sanity: ChatSession.ID is derived; ChatSession.CreatedAt is set.
func TestCreatedAtAndID(t *testing.T) {
	before := time.Now()
	cs, _ := New("oc_xxx", "claude")
	after := time.Now()

	if cs.CreatedAt().Before(before) || cs.CreatedAt().After(after) {
		t.Fatalf("CreatedAt outside expected range: %v (before=%v after=%v)",
			cs.CreatedAt(), before, after)
	}
}

// TestSetActiveAgent_ClearsStaleAnchor verifies the v1.3 fix:
// F-53: the old TestSetActiveAgent_ClearsStaleAnchor and
// TestKillAll_ClearsStaleAnchor tests were deleted — their
// subject (`currentTurnUserMsgID` scalar) is gone. The new
// anchor lives on `AgentSession.currentPrompt.LastMessageID` and
// is cleared automatically by `endPrompt` / agent-switch paths.
// See docs/feat/message_lifecycle.md §4.2 + §5.1.
//
// TestSubmit_AnchorWriteIsRaceFree exercises the concurrent-write
// fix on the Prompt anchor.
//
// CS-AS 边界重构 Phase 1 port: the subject used to be the default
// PromptHook closure, which ran without cs.mu held and had to take
// the lock itself when writing as.currentPrompt. That hook is gone;
// the write now happens inside `AgentSession.Submit` under
// as.asMu, and readers (the per-AS readpump, CurrentPrompt()) take
// as.asMu.RLock. This test keeps the guarantee under the race
// detector: N concurrent Submits against N concurrent readers.
//
// Run with -race for this to mean anything.
func TestSubmit_AnchorWriteIsRaceFree(t *testing.T) {
	cs, _ := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	as := newActiveAgentNoop()
	cs.mu.Lock()
	cs.selectedAS = as
	cs.mu.Unlock()

	const N = 64
	var wg sync.WaitGroup

	// Writers: each submits its own Prompt.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &Prompt{
				LastMessageID: fmt.Sprintf("om_writer_%d", i),
				Blocks:        []agent.ContentBlock{{Text: "x"}},
				ChatSessionID: cs.ChatID,
			}
			_ = as.Submit(p)
		}(i)
	}

	// Readers: drain currentPrompt.LastMessageID via the accessor.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 16; j++ {
				if p := as.CurrentPrompt(); p != nil {
					_ = p.LastMessageID
				}
			}
		}()
	}

	wg.Wait()
}

// TestQueueUserMessage_RemovesGhostOnQueueFull verifies the F-53
// invariant: messagesByID contains only messages that are either
// in the queue, Submitted, or Dropped. A rejected enqueue
// (ErrQueueFull) must NOT leave a "ghost" entry behind — otherwise
// the chat would accumulate stale Queued entries forever
// (DropQueue only clears the queue, not messagesByID).
//
// CS-AS 边界重构 Phase 1 port: the old version reached into
// inputBuffer.maxMsgs to force the failure. The queue cap is now
// the QueueMaxMsgs constant, so we fill the queue instead. No
// selectedAS is installed, so the TryFlush inside QueueUserMessage
// is a no-op and nothing drains.
func TestQueueUserMessage_RemovesGhostOnQueueFull(t *testing.T) {
	cs, _ := New("oc_chat", "claude")
	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")

	for i := 0; i < QueueMaxMsgs; i++ {
		msg := makeTestMessage(cs,
			[]agent.ContentBlock{{Type: agent.ContentText, Text: "filler"}},
			fmt.Sprintf("om_fill_%d", i))
		if err := cs.QueueUserMessage(msg); err != nil {
			t.Fatalf("QueueUserMessage(filler %d): %v", i, err)
		}
	}
	if got := cs.QueueLen(); got != QueueMaxMsgs {
		t.Fatalf("QueueLen = %d after filling; want %d", got, QueueMaxMsgs)
	}

	msg := makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "will fail"}}, "om_ghost")
	err := cs.QueueUserMessage(msg)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("QueueUserMessage on a full queue = %v; want ErrQueueFull", err)
	}

	// Post-refactor: no per-chat messagesByID index exists.
	// The rejected message was never added to the queue (Push
	// failed at the cap check), so there's no ghost to look
	// for. Verify by trying to Peek — should not surface
	// om_ghost.
	for _, m := range cs.queue.Peek() {
		if m.ID == "om_ghost" {
			t.Errorf("queue surfaced om_ghost after a rejected enqueue; want no ghost leak")
		}
	}
}

// TestKillAllSequence_QueueSurvivesAndReflushes verifies the
// post-/kill contract.
//
// CS-AS 边界重构 Phase 1 port, with a deliberate semantic change:
// the old test asserted the /kill handler ran ClearBuffer + SetIdle
// and that queued messages came out MessageDropped. /kill no longer
// discards the queue — it only tears down the agent processes.
// Queued messages are still owed a reply, so they must survive the
// kill and flush against the respawned AgentSession.
//
// The pre-fix bug this replaces (next message stranded in a "Busy
// but no AS will ever flush it" state) cannot recur: readiness now
// lives on the AgentSession, so a fresh AS is ready by construction.
func TestKillAllSequence_QueueSurvivesAndReflushes(t *testing.T) {
	cs, _ := New("oc_chat", "claude")
	cs.SetSelectedCwd(t.TempDir())

	// Subscribe to MessageStateBus so we can observe the
	// post-TryFlush state transition. With value semantics the
	// test's local `msg` is a copy of what the queue holds;
	// Stage mutations happen on the queue's copy, so the wire
	// event is the canonical signal.
	var (
		mu             sync.Mutex
		capturedState  agent.MessageState
		capturedID     string
		haveStateEvent bool
	)
	cs.MessageStateBus.Subscribe(func(e MessageStateEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		capturedState = e.State
		capturedID = e.UserMsgID
		haveStateEvent = true
		return false
	})

	// AS must live in activeCwd so KillAllAgents (cwd-scoped)
	// reaches it. Capture activeCwd before allocating the AS so
	// the (Agent, Cwd) key matches the pool filter.
	activeCwd := cs.SelectedCwd()
	as := NewAgentSession("as_kill", "oc_chat", "claude", activeCwd, nil)
	as.SetHandleForTest(newRecordingAgentSession(1).buildLive())
	as.SetStatusForTest(StatusRunning)
	// Put a Prompt in flight so the AS is mid-turn and the message
	// queued below is not flushed immediately.
	if err := as.Submit(&Prompt{Blocks: []agent.ContentBlock{{Text: "in-flight"}}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
	cs.selectedAS = as
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "x"}}, "om_queued")
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("pre-kill: QueueLen = %d, want 1 (AS is mid-turn)", got)
	}

	// Run /kill's sequence — KillAllAgents (simulated via accessors
	// to avoid import cycle) and nothing else.
	killAllForTest(t, cs)

	// The queued message survives, still in the queue (/kill
	// must not discard queued work). Stage cannot be observed
	// from the test's local copy (value semantics); we trust
	// the queue to still own it.
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("post-kill: QueueLen = %d, want 1 (/kill must not discard queued work)", got)
	}

	// Reset the captured state — /kill may not have fired any.
	mu.Lock()
	haveStateEvent = false
	mu.Unlock()

	// Respawn: the next AS is ready by construction, so the queued
	// message flushes against it.
	newAS := NewAgentSession("as_respawn", "oc_chat", "claude", t.TempDir(), nil)
	newAS.SetHandleForTest(newRecordingAgentSession(2).buildLive())
	newAS.SetStatusForTest(StatusRunning)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: newAS.Agent, Cwd: newAS.Cwd}] = newAS
	cs.selectedAS = newAS
	cs.mu.Unlock()

	if err := cs.TryFlush(); err != nil {
		t.Fatalf("post-kill TryFlush: %v", err)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("post-respawn: QueueLen = %d, want 0 (queue drained)", got)
	}

	// Verify the bus saw the MessageSubmitted transition for our
	// message id. (matters even more post-refactor: there is no
	// messagesByID to read Stage from directly.)
	mu.Lock()
	defer mu.Unlock()
	if !haveStateEvent {
		t.Fatal("MessageStateBus saw no event for post-respawn flush")
	}
	if capturedID != msg.ID {
		t.Errorf("bus event ID = %q, want %q", capturedID, msg.ID)
	}
	if capturedState != agent.MessageSubmitted {
		t.Errorf("bus event state = %v, want MessageSubmitted", capturedState)
	}
}

// TestChatSession_GitStatus_NoCacheBehavior pins down the new
// "no per-chat cache" contract. For each scenario we assert
// the function returns what its doc-comment promises and that
// repeated calls hit the deps on every invocation (i.e. we did
// NOT silently re-introduce a cache layer).
func TestChatSession_GitStatus_NoCacheBehavior(t *testing.T) {
	t.Run("unwired deps returns nil", func(t *testing.T) {
		cs, _ := New("t_unwired", "claude")
		cs.persistChatEntry() // no-op without stores; safe
		if got := cs.GitStatus(context.Background()); got != nil {
			t.Fatalf("GitStatus with zero deps = %+v, want nil", got)
		}
	})

	t.Run("cwd empty returns nil", func(t *testing.T) {
		cs, _ := New("t_empty_cwd", "claude")
		cs.WithGitStatusDeps(GitStatusDeps{
			CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
				return &messages.GitStatusSnapshot{Branch: "should-not-be-called"}, nil
			},
		})
		if got := cs.GitStatus(context.Background()); got != nil {
			t.Fatalf("GitStatus with empty cwd = %+v, want nil", got)
		}
	})

	t.Run("CollectGit hit on every call (no cache layer)", func(t *testing.T) {
		cs, _ := New("t_rebuild", "claude")
		// Apply workspace via in-process mutex; we do not want to
		// route through SetSelectedCwd's registry persist path here
		// (it needs a registry store). SelectedCwd reads cs.selectedCwd
		// under cs.mu; assigning directly via a helper avoids persistence.
		cs.mu.Lock()
		cs.selectedCwd = "/tmp/fake-workspace"
		cs.mu.Unlock()
		var calls int
		cs.WithGitStatusDeps(GitStatusDeps{
			CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
				calls++
				return &messages.GitStatusSnapshot{Branch: "main"}, nil
			},
		})
		for i := 0; i < 3; i++ {
			gs := cs.GitStatus(context.Background())
			if gs == nil {
				t.Fatalf("call %d: got nil snapshot", i)
			}
			if gs.Workspace != "/tmp/fake-workspace" {
				t.Fatalf("call %d: Workspace=%q", i, gs.Workspace)
			}
			if gs.Snapshot == nil || gs.Snapshot.Branch != "main" {
				t.Fatalf("call %d: Snapshot=%+v", i, gs.Snapshot)
			}
		}
		if calls != 3 {
			t.Fatalf("CollectGit was called %d times across 3 GitStatus() invocations, want 3 (no cache layer)", calls)
		}
	})

	t.Run("CollectGit nil does not panic (partial wiring)", func(t *testing.T) {
		cs, _ := New("t_partial", "claude")
		cs.mu.Lock()
		cs.selectedCwd = "/tmp/fake-workspace"
		cs.mu.Unlock()
		// Only LookupPR wired. CollectGit is nil. GitStatus must
		// not panic and must return a *messages.GitStatus with
		// Snapshot=nil (no fake default). LookupPR is gated on
		// SelectedAgentSession() returning non-nil — in this test
		// fixture we have no AgentSession, so PR is nil too;
		// the assertion that matters here is "no panic, snap=nil".
		cs.WithGitStatusDeps(GitStatusDeps{
			LookupPR: func(asID, cwd string) *messages.PR {
				return &messages.PR{Number: 42, URL: "https://example/pr/42"}
			},
		})
		gs := cs.GitStatus(context.Background())
		if gs == nil {
			t.Fatalf("GitStatus = nil")
		}
		if gs.Snapshot != nil {
			t.Fatalf("Snapshot should be nil when CollectGit is nil; got %+v", gs.Snapshot)
		}
	})

	t.Run("LookupPR hit on every call (per-read update attempt)", func(t *testing.T) {
		// Per the F-CLAUDE-PRINT-002 / fix-gitstatus design: every
		// read of gitstatus triggers an update attempt on the PR
		// cache. The closure is invoked once per cs.GitStatus()
		// call when an AgentSession is active. The runtime-side
		// implementation pairs this with MaybeRefresh + PR(), but
		// those live in the closure body (runtime.go's LookupPR),
		// NOT here — the test fixtures wire a stub LookupPR that
		// just counts calls. End-to-end coverage of the runtime
		// closure wiring lives in internal/runtime's tests.
		cs, _ := New("t_lookuppr", "claude")
		cs.mu.Lock()
		cs.selectedCwd = "/tmp/fake-workspace"
		cs.selectedAS = agentsession.NewAgentSession("as-1", "t_lookuppr", "claude", "/tmp/fake-workspace", nil)
		cs.mu.Unlock()
		var calls int
		cs.WithGitStatusDeps(GitStatusDeps{
			LookupPR: func(asID, cwd string) *messages.PR {
				calls++
				return nil
			},
		})
		for i := 0; i < 5; i++ {
			_ = cs.GitStatus(context.Background())
		}
		if calls != 5 {
			t.Fatalf("LookupPR was called %d times across 5 GitStatus() invocations, want 5 (per-read trigger required)", calls)
		}
	})

	t.Run("3s timeout caps a hung CollectGit", func(t *testing.T) {
		cs, _ := New("t_timeout", "claude")
		cs.mu.Lock()
		cs.selectedCwd = "/tmp/fake-workspace"
		cs.mu.Unlock()
		cs.WithGitStatusDeps(GitStatusDeps{
			CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
				// Block until ctx is cancelled, returning ctx.Err
				// like gtw.CollectReadiness does on timeout. The
				// outer GitStatus caller should observe the cancel
				// well before our manual 5s sleep would fire.
				<-ctx.Done()
				return nil, ctx.Err()
			},
		})
		start := time.Now()
		gs := cs.GitStatus(context.Background())
		elapsed := time.Since(start)
		if gs == nil {
			t.Fatalf("GitStatus returned nil; want non-nil with Snapshot=nil")
		}
		if gs.Snapshot != nil {
			t.Fatalf("Snapshot expected nil on timeout; got %+v", gs.Snapshot)
		}
		// Allow 2.7s-4s range: the 3s cap kills the inner git
		// closure promptly, but scheduler slop can push it to
		// ~3.05s. We deliberately do NOT assert <1s because the
		// timeout is the *cap*, not a target.
		if elapsed < 2700*time.Millisecond || elapsed > 4*time.Second {
			t.Fatalf("GitStatus took %v; expected ~3s timeout cap", elapsed)
		}
	})
}
