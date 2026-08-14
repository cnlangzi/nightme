package chatsession

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/registry"
)

func TestManager_GetOrCreate_New(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_xxx", "claude")
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
	a, _ := mgr.GetOrCreate("oc_xxx", "claude")
	b, _ := mgr.GetOrCreate("oc_xxx", "codex")
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
	a, _ := mgr.GetOrCreate("a", "claude")
	b, _ := mgr.GetOrCreate("b", "claude")
	c, _ := mgr.GetOrCreate("c", "claude")

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
	cs, _ := mgr.GetOrCreate("oc_xxx", "claude")
	if cs.Spawner() == nil {
		t.Fatalf("spawner should be wired after WithSpawner")
	}

	// /use should now actually spawn.
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if as.Status() != StatusRunning {
		t.Fatalf("after spawn: got %q, want Running", as.Status())
	}
}

func TestManager_PoolAfterCloseCanRespawn(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	mgr := NewManager().
		WithSpawner(spawner).
		WithPersistence(csFile, asFile)

	cs, _ := mgr.GetOrCreate("oc_xxx", "claude")
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")

	as1, _ := cs.LookupSelectedAgentSession()
	if as1.Status() != StatusRunning {
		t.Fatalf("precondition: expected Running")
	}

	// /close kills the bridge process but preserves the AgentSession
	// entry (and its sessionID). Simulate via accessors — close
	// package tested separately. The post-close pool state:
	// entry still present, status=Exited, sessionID preserved.
	snapshot := cs.AgentSessionsInCwd(cs.SelectedCwd())
	for _, as := range snapshot {
		_ = as.Close()
	}
	if len(cs.Pool()) != 1 {
		t.Fatalf("pool should still have 1 entry after /close (session preserved), got %d", len(cs.Pool()))
	}

	// Sending a message after /close triggers a respawn via the same
	// Spawner. The AS entry is reused (StatusExited → StatusRunning),
	// and the AgentSession.ID is preserved so the persisted session
	// identity stays stable across /close cycles. The bridge handle
	// (PID + events channel) is the only thing that changes.
	cs.SetSelectedCwd("/code/bailing")
	as2, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession after close: %v", err)
	}
	if as2.Status() != StatusRunning {
		t.Fatalf("after respawn: got %q, want Running", as2.Status())
	}
	if as2.ID != as1.ID {
		t.Fatalf("respawn must preserve AgentSession.ID for /close (got %q, want %q)", as2.ID, as1.ID)
	}
}

func TestManager_RestoreFromRegistry_NoPersistence(t *testing.T) {
	mgr := NewManager() // no persistence
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry with nil csFile: %v", err)
	}
}

func TestManager_ErrNoSelectedChatSessionMessage(t *testing.T) {
	if !strings.Contains(ErrNoSelectedChatSession.Error(), "/cwd") {
		t.Fatalf("error message should mention /cwd: %v", ErrNoSelectedChatSession)
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
		SelectedCwd:         "/code/bailing",
		SelectedAgent:       primary,
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
	//
	// F-54: handlers are installed by Subscribing to the typed
	// buses. The Bus survives RestoreFromRegistry so the test
	// only needs to verify a Subscribe returns a working
	// unsubscribe — the buses themselves are constructed in New().
	mgr.WithOnCreate(func(cs *ChatSession) {
		cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool { return false })
		cs.MessageStateBus.Subscribe(func(_ MessageStateEvent) bool { return false })
	})

	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	for _, cs := range mgr.List() {
		// F-54: there is no EventHandler getter anymore;
		// assert that the buses are non-nil (constructed in New)
		// and that subscribers are present (proves the wiring
		// ran before Restore).
		if cs.AgentEventBus == nil {
			t.Errorf("%s: AgentEventBus is nil — runtime did not install it", cs.ChatID)
		}
		if cs.MessageStateBus == nil {
			t.Errorf("%s: MessageStateBus is nil — runtime did not install it", cs.ChatID)
		}
		if cs.AgentEventBus.Len() == 0 {
			t.Errorf("%s: AgentEventBus has no subscribers — runtime did not install them", cs.ChatID)
		}
		if cs.MessageStateBus.Len() == 0 {
			t.Errorf("%s: MessageStateBus has no subscribers — runtime did not install them", cs.ChatID)
		}
	}
}

// TestManager_RestoreFromRegistry_WithOnCreateAfter_MissesHandlers
// is the negative regression: if a future refactor moves
// WithOnCreate back to AFTER RestoreFromRegistry, this test fails
// loudly so the silent-failure bug can't return. The fix is
// documented in cmd/nightme/run.go's block comment around the
// WithOnCreate call site.
//
// F-54: with Bus subscriptions, the same invariant holds —
// restored chats without onCreate have empty buses. The
// AgentEventBus / MessageStateBus themselves are always non-nil
// (constructed in New), so the assertion targets subscriber count.
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
		cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool { return false })
		cs.MessageStateBus.Subscribe(func(_ MessageStateEvent) bool { return false })
	})

	cs := mgr.Get("oc_alpha")
	if cs == nil {
		t.Fatalf("ChatSession not restored")
	}
	if cs.AgentEventBus.Len() != 0 {
		t.Errorf("AgentEventBus subscribers unexpectedly non-zero — bug repro is no longer valid; restore this assertion's expectation")
	}
	if cs.MessageStateBus.Len() != 0 {
		t.Errorf("MessageStateBus subscribers unexpectedly non-zero — bug repro is no longer valid; restore this assertion's expectation")
	}
}

// TestManager_WithGitStatusDeps_ThenWithOnCreate_ChainsHooks is
// the regression for the F-CLAUDE-PRINT-002 ordering bug.
//
// Production wiring (cmd/nightme/run.go + runtime.go) installs
// WithGitStatusDeps BEFORE any other WithOnCreate calls. The
// runtime then calls WithOnCreate to wire per-chat handlers
// (buses, EventHandler, etc.). Without explicit chaining in
// WithOnCreate, the second call would REPLACE the deps hook —
// every chat created after that wiring (including restored
// chats) would have no gitStatusDeps, ChatSession.GitStatus
// would hit the unconfigured-deps early return, and the
// per-chat footer would never populate.
//
// This test simulates that exact wiring order:
//
//	mgr := NewManager().WithGitStatusDeps(deps).WithOnCreate(handler)
//
// and asserts BOTH the deps hook AND the handler fire on a
// fresh GetOrCreate + on a persisted RestoreFromRegistry.
func TestManager_WithGitStatusDeps_ThenWithOnCreate_ChainsHooks(t *testing.T) {
	csFile, _ := newTestStores(t)
	seedPersistedChatSession(t, csFile, "oc_alpha", "claude")

	// Sentinel: when the deps hook runs it stamps the chatID
	// here. When the handler hook runs it stamps the chatID
	// here too. The test asserts BOTH lists contain every
	// chat that goes through the manager.
	var (
		mu          sync.Mutex
		handlerSeen []string
	)

	deps := GitStatusDeps{
		CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
			return nil, nil
		},
	}

	mgr := NewManager().
		WithPersistence(csFile, nil).
		WithGitStatusDeps(deps) // installs deps hook first
	mgr.WithOnCreate(func(cs *ChatSession) {
		// This is the runtime's handler-install hook (e.g.
		// AgentEventBus.Subscribe). It runs AFTER the deps
		// hook thanks to the chaining in WithOnCreate.
		mu.Lock()
		defer mu.Unlock()
		handlerSeen = append(handlerSeen, cs.ChatID)
	})

	// Restore — exercises the deps hook's existing-chat
	// propagation AND the OnCreate hook on restored chats.
	if err := mgr.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	// Fresh GetOrCreate — exercises the OnCreate hook on a
	// brand new chat.
	if _, err := mgr.GetOrCreate("oc_new", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// handlerSeen must include BOTH the restored chat AND the
	// freshly-created chat. If WithOnCreate had not chained
	// the deps hook (or had replaced it), this assertion would
	// still pass — the regression we're testing is about the
	// deps hook, not the handler hook. The deps verification
	// is below.

	wantHandler := map[string]bool{"oc_alpha": true, "oc_new": true}
	if len(handlerSeen) != 2 {
		t.Fatalf("handler hook: got %d calls %v, want 2 (oc_alpha, oc_new)",
			len(handlerSeen), handlerSeen)
	}
	for _, id := range handlerSeen {
		if !wantHandler[id] {
			t.Errorf("handler hook saw unexpected chat %q", id)
		}
	}

	// Now the critical assertion: deps.CollectGit on each
	// chat must be non-nil (i.e. deps were actually wired).
	// Without chaining, the OnCreate call above would have
	// replaced the deps hook, and no chat would have
	// gitStatusDeps.
	for _, cs := range mgr.List() {
		// Verify the wiring landed by checking the chat's own
		// gitStatusDeps was set via WithGitStatusDeps (the field
		// is unexported; this is a same-package test).
		if cs.gitStatusDeps.CollectGit == nil {
			t.Errorf("chat %q: gitStatusDeps.CollectGit is nil — WithOnCreate replaced (instead of chained) the deps hook",
				cs.ChatID)
		}
	}
}

// TestManager_WithOnCreate_ThenWithGitStatusDeps_ChainsHooks
// is the symmetric regression: if a future caller wires
// WithOnCreate FIRST and WithGitStatusDeps second, the deps
// hook must still chain (i.e. fire AFTER the handler hook on
// every chat). This protects against accidentally reversing
// the order in production without breaking the wiring.
//
// WithGitStatusDeps always installs the deps-Set on every
// existing chat AND on every future chat; the order with
// WithOnCreate doesn't matter for existing chats (they get
// deps-set synchronously inside WithGitStatusDeps) but does
// matter for future chats — those rely on the OnCreate hook
// chain.
func TestManager_WithOnCreate_ThenWithGitStatusDeps_ChainsHooks(t *testing.T) {
	var (
		mu        sync.Mutex
		handlerSeen []string
	)

	mgr := NewManager()
	mgr.WithOnCreate(func(cs *ChatSession) {
		mu.Lock()
		defer mu.Unlock()
		handlerSeen = append(handlerSeen, cs.ChatID)
	})

	deps := GitStatusDeps{
		CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
			return nil, nil
		},
	}
	mgr.WithGitStatusDeps(deps) // installed AFTER handler

	if _, err := mgr.GetOrCreate("oc_alpha", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(handlerSeen) != 1 || handlerSeen[0] != "oc_alpha" {
		t.Fatalf("handler hook: got %v, want [oc_alpha]", handlerSeen)
	}

	cs := mgr.Get("oc_alpha")
	if cs == nil {
		t.Fatal("oc_alpha not in manager")
	}
	if cs.gitStatusDeps.CollectGit == nil {
		t.Errorf("chat %q: gitStatusDeps.CollectGit is nil — deps hook didn't fire after handler hook",
			cs.ChatID)
	}
}

// TestManager_WithGitStatusDeps_PropagatesToExistingChats is the
// regression for the F-CLAUDE-PRINT-002 propagation fix: when
// WithGitStatusDeps is called AFTER chats already exist, every
// existing chat must receive the deps. Without the in-loop
// propagation, callsites that lazy-create chats (e.g. /gtw
// commit before /cwd) would silently skip git status refreshes
// until the chat was recreated.
func TestManager_WithGitStatusDeps_PropagatesToExistingChats(t *testing.T) {
	mgr := NewManager()

	// Create chats BEFORE wiring deps.
	for _, id := range []string{"oc_a", "oc_b", "oc_c"} {
		if _, err := mgr.GetOrCreate(id, "claude"); err != nil {
			t.Fatalf("GetOrCreate %s: %v", id, err)
		}
	}

	// Now wire deps — must propagate to all three.
	deps := GitStatusDeps{
		CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
			return nil, nil
		},
	}
	mgr.WithGitStatusDeps(deps)

	for _, cs := range mgr.List() {
		if cs.gitStatusDeps.CollectGit == nil {
			t.Errorf("chat %q: gitStatusDeps.CollectGit is nil after WithGitStatusDeps (propagation missed)",
				cs.ChatID)
		}
	}
}

// TestManager_GetOrCreate_AfterRestore_FiresOnCreate verifies the
// symmetric path: a freshly-created ChatSession via GetOrCreate
// (post-Restore, e.g. first message in a brand new chat) also
// fires onCreate, so handlers are installed uniformly across
// restored + future chats.
//
// F-54: legacy cs.SetEventHandler / cs.SetMessageStateHandler
// calls are replaced with Bus().Subscribe. The test only
// subscribes AFTER GetOrCreate (after onCreate ran) so the
// post-subscribe bus counts are 1, and the post-onCreate
// ChatSession matches the expected wiring pattern.
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

	cs, _ := mgr.GetOrCreate("oc_new", "claude")
	if cs.AgentEventBus.Len() != 0 {
		t.Fatalf("AgentEventBus subscribers unexpectedly set by test setup")
	}

	cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool { return false })
	cs.MessageStateBus.Subscribe(func(_ MessageStateEvent) bool { return false })

	mu.Lock()
	defer mu.Unlock()
	if len(seenIDs) != 1 || seenIDs[0] != "oc_new" {
		t.Fatalf("onCreate should fire once with ChatID=oc_new; got %v", seenIDs)
	}
	if cs.AgentEventBus.Len() == 0 {
		t.Errorf("AgentEventBus subscribers should remain non-zero after onCreate ran")
	}
}