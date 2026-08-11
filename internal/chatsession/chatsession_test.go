package chatsession

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// newTestStores lives in test_helpers_store_test.go — it was
// extracted there while this file was shelved, and other test
// files now depend on it.

func TestNewAndBasics(t *testing.T) {
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
	_, err := cs.LookupSelectedAgentSession()
	if err != ErrNoSelectedCwd {
		t.Fatalf("expected ErrNoSelectedCwd, got %v", err)
	}
}

func TestLookupActiveAgentSession_SpawnWhenMissing(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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

// closeAllForTest simulates the /close (no args) workflow using the
// lifecycle accessors that close.CloseAllAgents would compose:
// AgentSessionsInCwd → per-entry Close. /close does NOT drop the
// AgentSession — the entry stays in the pool with its sessionID
// preserved so the next respawn can continue the conversation.
// Tests use this in place of the close package directly to avoid an
// import cycle (close imports chatsession; chatsession tests cannot
// transitively import close).
func closeAllForTest(t *testing.T, cs *ChatSession) {
	t.Helper()
	snapshot := cs.AgentSessionsInCwd(cs.SelectedCwd())
	for _, as := range snapshot {
		_ = as.Close()
	}
}

func TestCloseAllPreservesPool(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude", newTestChannel())
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")
	cs.LookupSelectedAgentSession() // spawns claude

	if len(cs.Pool()) != 1 {
		t.Fatalf("precondition: pool size=%d", len(cs.Pool()))
	}

	closeAllForTest(t, cs)
	// /close kills the bridge process but preserves the AgentSession
	// entry. The entry stays in the pool with sessionID intact so
	// the next respawn can continue the conversation via
	// --resume <sessionID>.
	if len(cs.Pool()) != 1 {
		t.Fatalf("CloseAllAgents should preserve pool; size=%d", len(cs.Pool()))
	}
	if cs.SelectedAgentSession() == nil {
		t.Fatalf("selectedAS should be preserved after CloseAllAgents (session kept)")
	}
	// Persistence also preserved (no agent_sessions.json deletion).
	all := asFile.GetByChatPool(cs.ID)
	if len(all) != 1 {
		t.Fatalf("persisted AgentSessions should be preserved; got %d", len(all))
	}
	// activeCwd and activeAgent survive (they were never touched).
	if cs.SelectedCwd() != "/code/bailing" {
		t.Fatalf("SelectedCwd should survive /close; got %q", cs.SelectedCwd())
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	csPath := filepath.Join(dir, "chat_sessions.json")
	asPath := filepath.Join(dir, "agent_sessions.json")

	// Write phase.
	csFile, asFile, err := func() (*registry.ChatSessionFile, *registry.AgentSessionFile, error) {
		csFile, err := registry.OpenChatSessionFile(csPath)
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

	cs, _ := New("oc_xxx", "claude", newTestChannel())
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")
	cs.LookupSelectedAgentSession()

	// Read phase: reload from disk.
	csFile2, err := registry.OpenChatSessionFile(csPath)
	if err != nil {
		t.Fatalf("reopen chat: %v", err)
	}
	asFile2, err := registry.OpenAgentSessionFile(asPath)
	if err != nil {
		t.Fatalf("reopen agent: %v", err)
	}

	entry, ok := csFile2.GetByChat("oc_xxx")
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
	a, _ := New("oc_abc", "claude", newTestChannel())
	b, _ := New("oc_abc", "codex", newTestChannel())
	if a.ID != b.ID {
		t.Fatalf("ID should be deterministic by chatID: %s vs %s", a.ID, b.ID)
	}

	c, _ := New("oc_xyz", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
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
	cs, _ := New("oc_xxx", "claude", newTestChannel())
	after := time.Now()

	if cs.CreatedAt().Before(before) || cs.CreatedAt().After(after) {
		t.Fatalf("CreatedAt outside expected range: %v (before=%v after=%v)",
			cs.CreatedAt(), before, after)
	}
}

// TestSetActiveAgent_ClearsStaleAnchor verifies the v1.3 fix:
// F-53: the old TestSetActiveAgent_ClearsStaleAnchor and
// TestCloseAll_ClearsStaleAnchor tests were deleted — their
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
	cs, _ := New("oc_chat", "claude", newTestChannel())
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
	cs, _ := New("oc_chat", "claude", newTestChannel())
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

// TestCloseAllSequence_QueueSurvivesAndReflushes verifies the
// post-/close contract.
//
// CS-AS 边界重构 Phase 1 port, with a deliberate semantic change:
// the old test asserted the /close handler ran ClearBuffer + SetIdle
// and that queued messages came out MessageDropped. /close no longer
// discards the queue — it only tears down the agent processes.
// Queued messages are still owed a reply, so they must survive the
// close and flush against the respawned AgentSession.
//
// The pre-fix bug this replaces (next message stranded in a "Busy
// but no AS will ever flush it" state) cannot recur: readiness now
// lives on the AgentSession, so a fresh AS is ready by construction.
func TestCloseAllSequence_QueueSurvivesAndReflushes(t *testing.T) {
	cs, _ := New("oc_chat", "claude", newTestChannel())
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

	// AS must live in activeCwd so CloseAllAgents (cwd-scoped)
	// reaches it. Capture activeCwd before allocating the AS so
	// the (Agent, Cwd) key matches the pool filter.
	activeCwd := cs.SelectedCwd()
	as := NewAgentSession("as_close_preserved", "oc_chat", "claude", activeCwd, nil)
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
		t.Fatalf("pre-close: QueueLen = %d, want 1 (AS is mid-turn)", got)
	}

	// Run /close's sequence — KillAllAgents (simulated via accessors
	// to avoid import cycle) and nothing else.
	closeAllForTest(t, cs)

	// The queued message survives, still in the queue (/close
	// must not discard queued work). Stage cannot be observed
	// from the test's local copy (value semantics); we trust
	// the queue to still own it.
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("post-close: QueueLen = %d, want 1 (/close must not discard queued work)", got)
	}

	// Reset the captured state — /close may not have fired any.
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
		t.Fatalf("post-close TryFlush: %v", err)
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
