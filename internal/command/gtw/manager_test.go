package gtw

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSender implements Sender for Manager tests.
type fakeSender struct {
	activeCwd  string
	activeErr  error
	setErr     error
	sentMsgs   []OutMsg
}

func (f *fakeSender) ActiveCwd() string               { return f.activeCwd }
func (f *fakeSender) SetActiveCwd(cwd string) error    { f.activeCwd = cwd; return f.setErr }
func (f *fakeSender) Send(_ context.Context, m OutMsg) error { f.sentMsgs = append(f.sentMsgs, m); return nil }

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

	want := Context{Issue: 42, Branch: "fix/42-foo", Worktree: "/code/A", State: StateFixing}
	m.SetContext("c1", want)
	got := m.GetContext("c1")
	if got.Issue != want.Issue || got.Branch != want.Branch || got.State != want.State {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("expected UpdatedAt to be set by Manager.now, got zero")
	}
}

func TestManager_ClearContext(t *testing.T) {
	m := newTestManager()
	m.SetContext("c1", Context{Issue: 1, State: StateFixing})
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
	m.SetContext("c1", Context{State: StateFixing})
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
	m.SetContext("c1", Context{State: StateFixing})

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
	m.SetContext("c1", Context{State: StateFixing})
	m.StoreDraft("c1", "m1", &Draft{Kind: DraftFixBranchExists})
	m.SetSender("c1", &fakeSender{})

	m.Reset("c1")
	if m.HasContext("c1") {
		t.Errorf("Reset should clear context")
	}
	if m.DraftCount("c1") != 0 {
		t.Errorf("Reset should clear drafts")
	}
	if m.GetSender("c1") != nil {
		t.Errorf("Reset should clear sender")
	}
}

func TestManager_SetGetSender(t *testing.T) {
	m := newTestManager()
	if m.GetSender("c1") != nil {
		t.Errorf("expected nil sender initially")
	}
	s := &fakeSender{activeCwd: "/code/A"}
	m.SetSender("c1", s)
	if got := m.GetSender("c1"); got != s {
		t.Errorf("expected to retrieve same sender")
	}
	m.UnsetSender("c1")
	if m.GetSender("c1") != nil {
		t.Errorf("expected nil after UnsetSender")
	}
}

func TestManager_SetSender_NilAndEmpty(t *testing.T) {
	m := newTestManager()
	m.SetSender("", &fakeSender{})
	m.SetSender("c1", nil)
	if m.GetSender("c1") != nil {
		t.Errorf("expected empty/nil SetSender calls to be ignored")
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
	// c1's inner map should be gone, so DraftCount is 0
	// (and a subsequent ListDrafts should be empty).
	if got := m.ListDrafts("c1"); len(got) != 0 {
		t.Errorf("expected empty drafts after take, got %d", len(got))
	}
}

// TestManager_SetSenderFactory_LazyCreate covers the F-51 P0
// fix: the runtime installs a senderFactory; GetSender calls
// it on first miss and caches the result. Without this, /gtw
// fix and reaction paths would nil-deref.
func TestManager_SetSenderFactory_LazyCreate(t *testing.T) {
	m := newTestManager()
	calls := 0
	m.SetSenderFactory(func(chatID string) Sender {
		calls++
		return &fakeSender{activeCwd: "/code/A"}
	})

	// First call: factory invoked, Sender cached.
	s1 := m.GetSender("c1")
	if s1 == nil {
		t.Fatalf("expected Sender after factory call, got nil")
	}
	if calls != 1 {
		t.Errorf("expected 1 factory call, got %d", calls)
	}
	if s1.ActiveCwd() != "/code/A" {
		t.Errorf("expected cwd /code/A, got %q", s1.ActiveCwd())
	}

	// Second call: same Sender (cache hit, no new factory call).
	s2 := m.GetSender("c1")
	if s2 != s1 {
		t.Errorf("expected cached Sender, got different instance")
	}
	if calls != 1 {
		t.Errorf("expected factory to be called once, got %d calls", calls)
	}
}

// TestManager_SetSenderFactory_NilFallback covers the case
// where the factory returns nil (e.g. the chat session can't
// be created). GetSender should return nil, not crash.
func TestManager_SetSenderFactory_NilFallback(t *testing.T) {
	m := newTestManager()
	m.SetSenderFactory(func(chatID string) Sender { return nil })
	if s := m.GetSender("c1"); s != nil {
		t.Errorf("expected nil when factory returns nil, got %v", s)
	}
}

// TestManager_SetSenderFactory_NotInstalled covers the legacy
// path (factory never installed). GetSender returns nil;
// callers must handle nil (gtw commands fail loudly rather
// than silently corrupting state).
func TestManager_SetSenderFactory_NotInstalled(t *testing.T) {
	m := newTestManager()
	if s := m.GetSender("c1"); s != nil {
		t.Errorf("expected nil when no factory installed, got %v", s)
	}
}

// TestManager_SetSenderFactory_ConcurrentSafety exercises the
// race-detector. Multiple goroutines call GetSender for the
// same chatID simultaneously; the factory must be called
// exactly once (or at most a small constant number of times
// due to benign races — the exact count is implementation-
// defined; what matters is no panic / no data race).
func TestManager_SetSenderFactory_ConcurrentSafety(t *testing.T) {
	m := newTestManager()
	var calls atomic.Int32
	m.SetSenderFactory(func(chatID string) Sender {
		calls.Add(1)
		return &fakeSender{activeCwd: chatID}
	})
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			_ = m.GetSender("c1")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if calls.Load() == 0 {
		t.Errorf("expected factory to be called at least once")
	}
	// Allow some benign races but verify the cached value
	// is consistent.
	if s := m.GetSender("c1"); s == nil || s.ActiveCwd() != "c1" {
		t.Errorf("expected cached Sender for c1, got %v", s)
	}
}
