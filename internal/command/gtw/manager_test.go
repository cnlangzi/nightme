package gtw

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// fakeChatSession returns a minimal *chatsession.ChatSession for
// Manager tests. Production wiring passes cs into HandleReaction
// directly (the runtime-layer wrapper resolves it before calling
// gtw). Tests just need a stable pointer with a settable cwd.
// We use chatsession.New directly (rather than embedding a
// smaller interface) because HandleReaction is typed against
// *chatsession.ChatSession — no duck typing.
func fakeChatSession(id, cwd string) *chatsession.ChatSession {
	cs, _ := chatsession.New(id, "test-agent")
	if cwd != "" {
		_ = cs.SetSelectedCwd(cwd)
	}
	return cs
}

func newTestManager() *Manager {
	m := NewManager()
	m.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return m
}

func TestManager_StoreTakeDraft(t *testing.T) {
	m := newTestManager()
	d := &Draft{Kind: DraftFixWorktreeFail, Payload: FixDraftPayload{IssueID: 42, ChatID: "c1"}}
	m.StoreDraft("c1", "msg-1", d)

	got := m.GetDraft("c1", "msg-1")
	if got == nil {
		t.Fatalf("expected draft to be stored")
	}
	if got.Payload.IssueID != 42 {
		t.Errorf("got issue %d, want 42", got.Payload.IssueID)
	}

	// Take removes the draft.
	taken := m.TakeDraft("c1", "msg-1")
	if taken == nil {
		t.Fatalf("expected draft to be taken")
	}
	if m.GetDraft("c1", "msg-1") != nil {
		t.Errorf("expected draft to be removed after Take")
	}
}

func TestManager_StoreDraftEmptyIDIgnored(t *testing.T) {
	m := newTestManager()
	d := &Draft{Kind: DraftFixWorktreeFail}
	m.StoreDraft("", "msg-1", d)
	m.StoreDraft("c1", "", d)
	m.StoreDraft("c1", "msg-1", nil)
	if m.DraftCount("c1") != 0 {
		t.Errorf("expected no drafts to be stored with empty inputs")
	}
}

func TestManager_ListDraftsAndCount(t *testing.T) {
	m := newTestManager()
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixWorktreeFail})
	m.StoreDraft("c1", "m2", &Draft{Kind: DraftFixWorktreeFail})
	m.StoreDraft("c2", "m1", &Draft{Kind: DraftFixWorktreeFail})

	if got := m.DraftCount("c1"); got != 2 {
		t.Errorf("c1 count = %d, want 2", got)
	}
	if got := m.DraftCount("c2"); got != 1 {
		t.Errorf("c2 count = %d, want 1", got)
	}
	if got := m.DraftCount("c3"); got != 0 {
		t.Errorf("c3 count = %d, want 0", got)
	}

	c1Drafts := m.ListDrafts("c1")
	if len(c1Drafts) != 2 {
		t.Errorf("c1 ListDrafts len = %d, want 2", len(c1Drafts))
	}
}

func TestManager_ClearDrafts(t *testing.T) {
	m := newTestManager()
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixWorktreeFail})
	m.StoreDraft("c1", "m2", &Draft{Kind: DraftFixWorktreeFail})

	m.ClearDrafts("c1")
	if m.DraftCount("c1") != 0 {
		t.Errorf("expected drafts cleared, got count %d", m.DraftCount("c1"))
	}
}

func TestManager_HandleReaction_NoDraft(t *testing.T) {
	m := newTestManager()
	cs := fakeChatSession("c1", "/code/A")
	consumed := m.HandleReaction(context.Background(), ReactionEvent{
		ChatID:    "c1",
		RequestID: "msg-1",
		Emoji:     "✅",
	}, cs)
	if consumed {
		t.Errorf("no draft → expected consumed=false")
	}
}

func TestManager_HandleReaction_EmptyEvent(t *testing.T) {
	m := newTestManager()
	cs := fakeChatSession("c1", "")
	consumed := m.HandleReaction(context.Background(), ReactionEvent{}, cs)
	if consumed {
		t.Errorf("empty event → expected consumed=false")
	}
}

func TestManager_HandleReaction_NilCS(t *testing.T) {
	m := newTestManager()
	consumed := m.HandleReaction(context.Background(), ReactionEvent{
		ChatID:    "c1",
		RequestID: "msg-1",
		Emoji:     "✅",
	}, nil)
	if consumed {
		t.Errorf("nil cs → expected consumed=false (no panic)")
	}
}

func TestManager_TakeDraftRemovesEmptyMap(t *testing.T) {
	m := newTestManager()
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixWorktreeFail})
	m.TakeDraft("c1", "m1")
	if got := m.ListDrafts("c1"); len(got) != 0 {
		t.Errorf("expected empty drafts after take, got %d", len(got))
	}
}

// --- mode helper tests ---

// TestManager_ModeFromPayload exercises the helper that infers
// Mode from a draft payload (used by reaction handlers when
// reconstructing fix metadata from a clicked card).
func TestManager_ModeFromPayload(t *testing.T) {
	if got := ModeFromDraftPayload(FixDraftPayload{IssueID: -1}); got != ModeLocal {
		t.Errorf("IssueID=-1 → %q, want %q", got, ModeLocal)
	}
	if got := ModeFromDraftPayload(FixDraftPayload{IssueID: 42}); got != ModeRemote {
		t.Errorf("IssueID=42 → %q, want %q", got, ModeRemote)
	}
	if got := ModeFromDraftPayload(FixDraftPayload{IssueID: 0}); got != ModeRemote {
		t.Errorf("IssueID=0 → %q, want %q (legacy zero treated as Remote)", got, ModeRemote)
	}
}

// --- run lock tests ---

// TestManager_RunLockFor_EmptyChatIDReturnsNil pins the contract
// that chatID == "" returns nil so Factory.Handle can no-op
// safely in tests and synthetic inputs that bypass the runtime
// dispatcher. The caller nil-checks before Lock, so a nil return
// must not panic.
func TestManager_RunLockFor_EmptyChatIDReturnsNil(t *testing.T) {
	m := newTestManager()
	if got := m.runLockFor(""); got != nil {
		t.Errorf("runLockFor(\"\") = %v, want nil", got)
	}
}

// TestManager_RunLockFor_PerChatIndependence verifies that
// different chatIDs get independent mutexes: a slow holder in
// chat A must not block chat B from acquiring its lock.
//
// Implementation: chat A holds the lock for holdDuration; we
// record when chat B's Lock returns. If B's wait is shorter
// than A's holdDuration, the locks are independent. A test
// threshold slightly below holdDuration filters out scheduling
// noise while still failing if the locks are accidentally
// shared (in which case B would wait the full holdDuration).
func TestManager_RunLockFor_PerChatIndependence(t *testing.T) {
	m := newTestManager()
	const holdDuration = 200 * time.Millisecond

	muA := m.runLockFor("chat-A")
	muB := m.runLockFor("chat-B")
	if muA == nil || muB == nil {
		t.Fatalf("runLockFor returned nil for non-empty chatID: A=%v B=%v", muA, muB)
	}
	if muA == muB {
		t.Fatalf("runLockFor returned the same mutex for different chatIDs; expected independent locks")
	}

	muA.Lock()
	aReleased := make(chan struct{})
	go func() {
		defer close(aReleased)
		time.Sleep(holdDuration)
		muA.Unlock()
	}()

	startB := time.Now()
	muB.Lock()
	bAcquired := time.Since(startB)
	muB.Unlock()

	if bAcquired >= holdDuration {
		t.Errorf("chat B waited %v (>= %v holdDuration); locks are NOT per-chat", bAcquired, holdDuration)
	}
	<-aReleased
}

// TestManager_RunLockFor_SameChatSerializes verifies that two
// Lock calls on the same chatID's mutex serialise: the second
// goroutine must observe the first's Unlock before it acquires.
//
// Implementation: chatID = "chat-X" gets the same *sync.Mutex
// from both calls. First goroutine holds for holdDuration; we
// start the second goroutine immediately after, then measure
// how long until it acquires. The wait must be at least
// holdDuration (minus a small jitter tolerance for slow CI).
func TestManager_RunLockFor_SameChatSerializes(t *testing.T) {
	m := newTestManager()
	const holdDuration = 150 * time.Millisecond

	mu := m.runLockFor("chat-X")
	if mu == nil {
		t.Fatalf("runLockFor returned nil for non-empty chatID")
	}
	// Second call must return the same instance — LoadOrStore
	// must not allocate a fresh mutex for the same chatID.
	if mu2 := m.runLockFor("chat-X"); mu2 != mu {
		t.Errorf("runLockFor returned a different mutex for the same chatID; LoadOrStore race")
	}

	mu.Lock()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(holdDuration)
		mu.Unlock()
	}()

	// Second acquirer on the same mutex; record wait time.
	acquired := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		mu.Lock()
		wait := time.Since(start)
		mu.Unlock()
		acquired <- wait
	}()

	select {
	case wait := <-acquired:
		if wait < holdDuration-50*time.Millisecond {
			t.Errorf("second acquirer waited %v (< %v); serialisation broken", wait, holdDuration)
		}
	case <-time.After(2 * holdDuration):
		t.Fatalf("second acquirer never acquired the lock within %v", 2*holdDuration)
	}
	<-released
}

// TestManager_RunLockFor_ConcurrentFirstCallRace pins the
// LoadOrStore contract: even under concurrent first-time access
// for the same chatID, both callers must observe the SAME
// *sync.Mutex instance. A naive Load-then-Store pattern would
// race and produce two distinct mutexes, breaking serialisation
// for that chatID. This test fails the implementation if anyone
// replaces sync.Map.LoadOrStore with Load + Store.
func TestManager_RunLockFor_ConcurrentFirstCallRace(t *testing.T) {
	m := newTestManager()
	const N = 32

	var wg sync.WaitGroup
	results := make([]*sync.Mutex, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = m.runLockFor("chat-race")
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, r := range results {
		if r != first {
			t.Errorf("goroutine %d got a different *sync.Mutex; LoadOrStore race", i)
			break
		}
	}
}
