package chatsession

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// newTestStores returns a ChatSessionFile + AgentSessionFile pair
// backed by a temp directory.
func newTestStores(t *testing.T) (*registry.ChatSessionFile, *registry.AgentSessionFile) {
	t.Helper()
	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(filepath.Join(dir, "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	return csFile, asFile
}

func TestNewAndBasics(t *testing.T) {
	cs := New("oc_xxx", "claude")
	if cs.ChatID != "oc_xxx" {
		t.Fatalf("ChatID: got %q", cs.ChatID)
	}
	if cs.ID == "" {
		t.Fatalf("ID should be derived from chatID")
	}
	if cs.ActiveCwd() != "" {
		t.Fatalf("initial ActiveCwd should be empty")
	}
	if cs.ActiveAgent() != "claude" {
		t.Fatalf("initial ActiveAgent should be seeded from PrimaryAgent; got %q", cs.ActiveAgent())
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
	if cs.ActiveAgentSession() != nil {
		t.Fatalf("activeAS should be nil initially")
	}
}

func TestSetActiveCwdDoesNotSpawn(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)

	if err := cs.SetActiveCwd("/code/bailing"); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	if cs.ActiveCwd() != "/code/bailing" {
		t.Fatalf("ActiveCwd: got %q", cs.ActiveCwd())
	}
	// Critical: SetActiveCwd must NOT add anything to the pool.
	if len(cs.Pool()) != 0 {
		t.Fatalf("SetActiveCwd should not spawn; pool size=%d", len(cs.Pool()))
	}
}

func TestSetActiveAgentDoesNotSpawn(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)

	cs.SetActiveCwd("/code/bailing")
	if err := cs.SetActiveAgent("claude"); err != nil {
		t.Fatalf("SetActiveAgent: %v", err)
	}
	if cs.ActiveAgent() != "claude" {
		t.Fatalf("ActiveAgent: got %q", cs.ActiveAgent())
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("SetActiveAgent should not spawn; pool size=%d", len(cs.Pool()))
	}
}

func TestLookupActiveAgentSession_RequiresCwd(t *testing.T) {
	cs := New("oc_xxx", "claude")
	_, err := cs.LookupActiveAgentSession()
	if err != ErrNoActiveCwd {
		t.Fatalf("expected ErrNoActiveCwd, got %v", err)
	}
}

func TestLookupActiveAgentSession_SpawnWhenMissing(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)

	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")

	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
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
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(newFakeSpawner())
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")

	as1, _ := cs.LookupActiveAgentSession()
	as2, _ := cs.LookupActiveAgentSession()

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
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)

	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	claudeAS, _ := cs.LookupActiveAgentSession() // spawns (claude, cwd)
	if claudeAS.Agent != "claude" {
		t.Fatalf("first spawn: got %q, want claude", claudeAS.Agent)
	}

	// /use codex. (claude, cwd) is in the pool; (codex, cwd) is not.
	// Lookup must spawn codex (not reuse the seeded claude entry).
	cs.SetActiveAgent("codex")
	codexAS, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
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
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("codex") // active != default

	// Neither (codex, /code/bailing) nor (claude, /code/bailing) is in pool.
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	// Step 3 spawns (codex, /code/bailing).
	if as.Agent != "codex" {
		t.Fatalf("expected spawn codex, got %q", as.Agent)
	}
	if len(cs.Pool()) != 1 {
		t.Fatalf("pool size: got %d, want 1", len(cs.Pool()))
	}
}

func TestKillAllClearsPool(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession() // spawns claude

	if len(cs.Pool()) != 1 {
		t.Fatalf("precondition: pool size=%d", len(cs.Pool()))
	}

	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if len(cs.Pool()) != 0 {
		t.Fatalf("KillAll should clear pool; size=%d", len(cs.Pool()))
	}
	if cs.ActiveAgentSession() != nil {
		t.Fatalf("activeAS should be nil after KillAll")
	}
	// Persistence also cleared.
	all := asFile.GetByChatPool(cs.ID)
	if len(all) != 0 {
		t.Fatalf("persisted AgentSessions should be cleared; got %d", len(all))
	}
	// activeCwd and activeAgent survive (only the pool is cleared).
	if cs.ActiveCwd() != "/code/bailing" {
		t.Fatalf("ActiveCwd should survive /kill; got %q", cs.ActiveCwd())
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

	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()

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
	if entry.ActiveCwd != "/code/bailing" {
		t.Fatalf("persisted ActiveCwd: %q", entry.ActiveCwd)
	}
	if entry.ActiveAgent != "claude" {
		t.Fatalf("persisted ActiveAgent: %q", entry.ActiveAgent)
	}
	if len(entry.AgentSessionIDs) != 1 {
		t.Fatalf("persisted AgentSessionIDs: got %d, want 1", len(entry.AgentSessionIDs))
	}
	if entry.ActiveAgentSessionID == nil {
		t.Fatalf("ActiveAgentSessionID should be set")
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
	a := New("oc_abc", "claude")
	b := New("oc_abc", "codex")
	if a.ID != b.ID {
		t.Fatalf("ID should be deterministic by chatID: %s vs %s", a.ID, b.ID)
	}

	c := New("oc_xyz", "claude")
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
	cs := New("oc_xxx", "claude").WithPersistence(csFile, asFile)
	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")

	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = cs.LookupActiveAgentSession()
		}()
		go func() {
			defer wg.Done()
			_ = cs.SetActiveAgent("claude")
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
	cs := New("oc_xxx", "claude")
	if err := cs.SetActiveCwd(""); err == nil {
		t.Fatalf("expected error for empty cwd")
	}
	if err := cs.SetActiveAgent(""); err == nil {
		t.Fatalf("expected error for empty agent")
	}
}

// Sanity: ChatSession.ID is derived; ChatSession.CreatedAt is set.
func TestCreatedAtAndID(t *testing.T) {
	before := time.Now()
	cs := New("oc_xxx", "claude")
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
// TestDefaultPromptHook_AnchorWriteIsRaceFree exercises the F-53
// concurrent-write fix: the default PromptHook closure runs
// WITHOUT cs.mu held (InputBuffer releases its own lock before
// invoking the hook), so the closure must acquire cs.mu when
// writing as.currentPrompt. The runReadPump goroutine reads it
// under RLock — racing between them is what the race detector
// catches.
//
// We launch N concurrent flushes and N concurrent reads. If the
// fix is in place (Lock inside the closure), no data race. If
// not, -race trips immediately.
func TestDefaultPromptHook_AnchorWriteIsRaceFree(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	cs.mu.Lock()
	cs.activeAS = newActiveAgentNoop()
	hook := cs.defaultPromptHookLocked()
	cs.mu.Unlock()

	const N = 64
	var wg sync.WaitGroup

	// Writers: each invokes the hook with its own Prompt.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &Prompt{
				MessageIDs:    []string{fmt.Sprintf("om_writer_%d", i)},
				LastMessageID: fmt.Sprintf("om_writer_%d", i),
				Blocks:        []agent.ContentBlock{{Text: "x"}},
				ChatSessionID: cs.ChatID,
			}
			_ = hook(p)
		}(i)
	}

	// Readers: drain as.currentPrompt.LastMessageID under RLock.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 16; j++ {
				cs.mu.RLock()
				if p := cs.activeAS.CurrentPrompt(); p != nil {
					_ = p.LastMessageID
				}
				cs.mu.RUnlock()
			}
		}()
	}

	wg.Wait()
}

// TestQueueUserMessage_RemovesGhostOnBufferFull verifies the F-53
// invariant: messagesByID contains only messages that are either
// in the buffer queue, Submitted, or Dropped. A failed Add (e.g.
// ErrBufferFull) must NOT leave a "ghost" entry behind —
// otherwise the chat would accumulate stale Queued entries
// forever (ClearBuffer only clears the queue, not messagesByID).
func TestQueueUserMessage_RemovesGhostOnBufferFull(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	// Force BufferFull by setting maxMsgs to 0 on the inputBuffer.
	// We can't reach the constructor from outside, so simulate by
	// filling the buffer to its cap.
	cs.ensureBuffer().maxMsgs = 0
	cs.ensureBuffer().maxBytes = 0
	cs.SetBusy()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Type: agent.ContentText, Text: "will fail"}}, "om_ghost")

	err := cs.QueueUserMessage(msg)
	if err == nil {
		t.Fatalf("expected buffer-full error from QueueUserMessage with 0 capacity, got nil")
	}

	if cs.GetMessage("om_ghost") != nil {
		t.Errorf("messagesByID still has om_ghost after Add failure; want removed (no ghost leak)")
	}
}

// TestKillAllSequence_ResetsBufferFSM verifies the post-/kill FSM
// state — the InputBuffer must be Idle after /kill so the next
// user message doesn't get stranded in a "Busy but no active AS
// will ever flush" state.
//
// Sequence (mimics cmd/command/kill/cmd.go Handle):
//  1. Spawn an AS, mark Busy, queue messages
//  2. KillAll → pool cleared, activeAS=nil
//  3. ClearBuffer → queued messages flipped to Dropped
//  4. SetIdle → FSM reset (the /kill handler does this; Phase 0
//     documents it explicitly because KillAll alone doesn't touch
//     the FSM)
//
// Pre-fix bug: step 4 was missing. The next user message would see
// state=Busy, get queued by InputBuffer.Add, and never flush
// (the dead AS can't drive a flushPending). User sees the
// message silently dropped.
func TestKillAllSequence_ResetsBufferFSM(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.SetActiveCwd(t.TempDir())

	// Spawn an AS via the recordingSpawner pattern would be
	// heavier than needed — for this test we just need any
	// activeAS that KillAll can iterate over.
	as := NewAgentSession("as_kill", "oc_chat", "claude", t.TempDir(), nil)
	as.handle = newRecordingAgentSession(1)
	as.stat = StatusRunning

	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
	cs.activeAS = as
	cs.mu.Unlock()

	// Mark Busy and queue a message — simulate "agent is mid-turn,
	// user sends another message".
	cs.SetBusy()
	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "x"}}, "om_queued")
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if cs.BufferState() != StateBusy {
		t.Fatalf("pre-kill: BufferState = %s, want Busy", cs.BufferState())
	}

	// Run the /kill handler's sequence.
	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	cs.ClearBuffer()
	cs.SetIdle()

	if got := cs.BufferState(); got != StateIdle {
		t.Errorf("post-kill: BufferState = %s, want Idle (next user message would be stranded)", got)
	}

	// The queued message should be Dropped (Stage=Dropped), not
	// silently stranded in Queued.
	if msg.Stage != agent.MessageDropped {
		t.Errorf("queued message after /kill: Stage = %v, want MessageDropped", msg.Stage)
	}

	// Verify the next user message can dispatch immediately
	// (Idle path: hook fires synchronously). Use the new
	// recordingAgentSession so the hook succeeds.
	newAS := NewAgentSession("as_respawn", "oc_chat", "claude", t.TempDir(), nil)
	newAS.handle = newRecordingAgentSession(2)
	newAS.stat = StatusRunning
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: newAS.Agent, Cwd: newAS.Cwd}] = newAS
	cs.activeAS = newAS
	cs.mu.Unlock()

	nextMsg := makeTestMessage(cs, []agent.ContentBlock{{Text: "after-kill"}}, "om_after_kill")
	if err := cs.QueueUserMessage(nextMsg); err != nil {
		t.Fatalf("post-kill QueueUserMessage: %v", err)
	}
	if nextMsg.Stage != agent.MessageSubmitted {
		t.Errorf("post-kill message: Stage = %v, want MessageSubmitted (Idle path)", nextMsg.Stage)
	}
	if cs.BufferPending() != 0 {
		t.Errorf("post-kill buffer should be empty, got %d queued", cs.BufferPending())
	}
}
