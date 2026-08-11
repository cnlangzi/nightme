package gtw

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// fakeChatSession returns a minimal *chatsession.ChatSession for
// Manager tests. Production wiring uses real chatsession.New +
// real mgr.GetOrCreate; tests just need a stable pointer with a
// settable cwd. We use chatsession.New directly (rather than
// embedding a smaller interface) because Manager.chatSessions
// is typed as *chatsession.ChatSession — no duck typing.
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

func TestManager_SetGetContext(t *testing.T) {
	m := newTestManager()
	if got := m.GetContext("c1"); got.State != "" {
		t.Errorf("expected empty context initially, got %+v", got)
	}

	want := Context{Mode: ModeRemote, Issue: 42, Branch: "login-state-expiration", Worktree: "/code/A", State: StateFixing}
	m.SetContext("c1", want)
	got := m.GetContext("c1")
	if got.Mode != want.Mode || got.Issue != want.Issue || got.Branch != want.Branch || got.State != want.State {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("expected UpdatedAt to be set by Manager.now, got zero")
	}
}

func TestManager_ClearContext(t *testing.T) {
	m := newTestManager()
	m.SetContext("c1", Context{Mode: ModeRemote, Issue: 1, State: StateFixing})
	m.ClearContext("c1")
	if m.HasContext("c1") {
		t.Errorf("expected HasContext false after clear")
	}
}

func TestManager_HasContext(t *testing.T) {
	m := newTestManager()
	if m.HasContext("c1") {
		t.Errorf("empty context should be HasContext=false")
	}
	m.SetContext("c1", Context{Mode: ModeRemote, State: StateFixing})
	if !m.HasContext("c1") {
		t.Errorf("set context should be HasContext=true")
	}
}

func TestManager_StoreTakeDraft(t *testing.T) {
	m := newTestManager()
	d := &Draft{Kind: DraftFixBranchExists, Payload: FixDraftPayload{IssueID: 42, ChatID: "c1"}}
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
	d := &Draft{Kind: DraftFixBranchExists}
	m.StoreDraft("", "msg-1", d)
	m.StoreDraft("c1", "", d)
	m.StoreDraft("c1", "msg-1", nil)
	if m.DraftCount("c1") != 0 {
		t.Errorf("expected no drafts to be stored with empty inputs")
	}
}

func TestManager_ListDraftsAndCount(t *testing.T) {
	m := newTestManager()
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixBranchExists})
	m.StoreDraft("c1", "m2", &Draft{Kind: DraftFixWorktreeFail})
	m.StoreDraft("c2", "m1", &Draft{Kind: DraftFixLabelTaken})

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
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixBranchExists})
	m.StoreDraft("c1", "m2", &Draft{Kind: DraftFixWorktreeFail})
	m.SetContext("c1", Context{Mode: ModeRemote, State: StateFixing})

	m.ClearDrafts("c1")
	if m.DraftCount("c1") != 0 {
		t.Errorf("expected drafts cleared, got count %d", m.DraftCount("c1"))
	}
	if !m.HasContext("c1") {
		t.Errorf("ClearDrafts should NOT touch context state")
	}
}

func TestManager_Reset(t *testing.T) {
	m := newTestManager()
	m.SetContext("c1", Context{Mode: ModeRemote, State: StateFixing})
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixBranchExists})
	m.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		return fakeChatSession(chatID, "/code/A")
	})
	// Warm the cache via the lookup.
	if m.GetChatSession("c1") == nil {
		t.Fatalf("expected lookup to populate cache, got nil")
	}

	m.Reset("c1")
	if m.HasContext("c1") {
		t.Errorf("Reset should clear context")
	}
	if m.DraftCount("c1") != 0 {
		t.Errorf("Reset should clear drafts")
	}
	if m.GetChatSession("c1") == nil {
		t.Errorf("Reset should NOT clear the ChatSession lookup closure (subsequent GetChatSession should still resolve via the closure)")
	}
}

func TestManager_SetGetChatSession(t *testing.T) {
	m := newTestManager()
	if m.GetChatSession("c1") != nil {
		t.Errorf("expected nil ChatSession initially")
	}
	cs := fakeChatSession("c1", "/code/A")
	m.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		return cs
	})
	if got := m.GetChatSession("c1"); got != cs {
		t.Errorf("expected lookup to return the cached fake ChatSession")
	}
}

func TestManager_HandleReaction_NoDraft(t *testing.T) {
	m := newTestManager()
	consumed := m.HandleReaction(context.Background(), ReactionEvent{
		ChatID:      "c1",
		TargetMsgID: "msg-1",
		Emoji:       "✅",
	})
	if consumed {
		t.Errorf("no draft → expected consumed=false")
	}
}

func TestManager_HandleReaction_EmptyEvent(t *testing.T) {
	m := newTestManager()
	consumed := m.HandleReaction(context.Background(), ReactionEvent{})
	if consumed {
		t.Errorf("empty event → expected consumed=false")
	}
}

func TestManager_TakeDraftRemovesEmptyMap(t *testing.T) {
	m := newTestManager()
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixBranchExists})
	m.TakeDraft("c1", "m1")
	if got := m.ListDrafts("c1"); len(got) != 0 {
		t.Errorf("expected empty drafts after take, got %d", len(got))
	}
}

// TestManager_SetGetChatSession_LazyCreate covers the F-XX
// wiring path: the runtime installs a chatSessionLookup;
// GetChatSession calls it on first miss and caches the result.
// Without this, /gtw fix and reaction paths would nil-deref.
func TestManager_SetGetChatSession_LazyCreate(t *testing.T) {
	m := newTestManager()
	calls := 0
	m.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		calls++
		return fakeChatSession(chatID, "/code/A")
	})

	// First call: factory invoked, ChatSession cached.
	cs1 := m.GetChatSession("c1")
	if cs1 == nil {
		t.Fatalf("expected ChatSession after factory call, got nil")
	}
	if calls != 1 {
		t.Errorf("expected 1 factory call, got %d", calls)
	}
	if cs1.SelectedCwd() != "/code/A" {
		t.Errorf("expected cwd /code/A, got %q", cs1.SelectedCwd())
	}

	// Second call: same ChatSession (cache hit, no new factory call).
	cs2 := m.GetChatSession("c1")
	if cs2 != cs1 {
		t.Errorf("expected cached ChatSession, got different instance")
	}
	if calls != 1 {
		t.Errorf("expected factory to be called once, got %d calls", calls)
	}
}

// TestManager_SetGetChatSession_NilFallback covers the case
// where the lookup returns nil (e.g. the chat session can't be
// created). GetChatSession should return nil, not crash.
func TestManager_SetGetChatSession_NilFallback(t *testing.T) {
	m := newTestManager()
	m.SetGetChatSession(func(chatID string) *chatsession.ChatSession { return nil })
	if cs := m.GetChatSession("c1"); cs != nil {
		t.Errorf("expected nil when lookup returns nil, got %v", cs)
	}
}

// TestManager_SetGetChatSession_NotInstalled covers the legacy
// path (lookup never installed). GetChatSession returns nil;
// callers must handle nil (gtw commands fail loudly rather than
// silently corrupting state).
func TestManager_SetGetChatSession_NotInstalled(t *testing.T) {
	m := newTestManager()
	if cs := m.GetChatSession("c1"); cs != nil {
		t.Errorf("expected nil when no lookup installed, got %v", cs)
	}
}

// TestManager_SetGetChatSession_ConcurrentSafety exercises the
// race-detector. Multiple goroutines call GetChatSession for
// the same chatID simultaneously; the lookup must be called
// exactly once (or at most a small constant number of times
// due to benign races — the exact count is implementation-
// defined; what matters is no panic / no data race).
func TestManager_SetGetChatSession_ConcurrentSafety(t *testing.T) {
	m := newTestManager()
	var calls atomic.Int32
	m.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		calls.Add(1)
		return fakeChatSession(chatID, "/code/"+chatID)
	})
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			_ = m.GetChatSession("c1")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if calls.Load() == 0 {
		t.Errorf("expected lookup to be called at least once")
	}
	// Allow some benign races but verify the cached value
	// is consistent.
	if cs := m.GetChatSession("c1"); cs == nil || cs.SelectedCwd() != "/code/c1" {
		t.Errorf("expected cached ChatSession for c1, got %v", cs)
	}
}

// --- new tests for F-XX mode split ---

// TestManager_ModeRoundtrip pins the Mode field on Context:
// GetContext returns the mode that was set; legacy zero-mode
// contexts (no Mode field set) are still readable.
func TestManager_ModeRoundtrip(t *testing.T) {
	m := newTestManager()
	m.SetContext("c1", Context{Mode: ModeLocal, Issue: -1, Branch: "b", State: StateFixing})
	got := m.GetContext("c1")
	if got.Mode != ModeLocal {
		t.Errorf("got Mode=%q, want %q", got.Mode, ModeLocal)
	}
	if got.Issue != -1 {
		t.Errorf("got Issue=%d, want -1", got.Issue)
	}
}

// TestManager_LegacyZeroModeDefaultsRemote covers back-compat:
// persisted contexts from before F-XX have Mode == "". Read
// path returns the zero-mode context unchanged. Callers that
// care about mode should treat empty as ModeRemote (the
// pre-F-XX default) — RunFix.runFixRemote handles this.
func TestManager_LegacyZeroModeDefaultsRemote(t *testing.T) {
	m := newTestManager()
	// Simulate a legacy persisted entry by setting zero-value Mode.
	m.SetContext("c1", Context{Issue: 42, Branch: "b", State: StateFixing})
	got := m.GetContext("c1")
	if got.Mode != "" {
		t.Errorf("expected empty Mode for legacy entry, got %q", got.Mode)
	}
	// ModeFromDraftPayload should classify as Remote for any
	// non-(-1) issue.
	if got := ModeFromDraftPayload(FixDraftPayload{IssueID: 42}); got != ModeRemote {
		t.Errorf("legacy payload classified as %q, want %q", got, ModeRemote)
	}
}

// TestManager_ModeFromPayload exercises the helper that infers
// Mode from a draft payload (action handler writes a new
// Context after the user clicks 🆕 / 🔗 / 🔄 / ❌).
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

