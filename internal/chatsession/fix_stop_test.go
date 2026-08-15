package chatsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ruSpawner is a Spawner whose first Spawn call for a given
// (cwd,name) returns ErrResumeUnhealthy; subsequent calls
// succeed. Mirrors the chat-layer retry path:
//   1. First call: bridge rejects --resume (saved id is stale).
//   2. respawn clears as.sessionID.
//   3. LookupSelectedAgentSession catches the error, calls Spawn
//      a second time without a resume id → bridge starts fresh.
type ruSpawner struct {
	mu       sync.Mutex
	calls    int32
	lastIDs  []string
	rejected map[ruKey]bool // (name,cwd) we've already rejected for
}

type ruKey struct{ name, cwd string }

func (s *ruSpawner) Spawn(_ context.Context, name, cwd string, _ []string, sessionID string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := atomic.AddInt32(&s.calls, 1)
	s.lastIDs = append(s.lastIDs, sessionID)
	if s.rejected == nil {
		s.rejected = make(map[ruKey]bool)
	}
	key := ruKey{name, cwd}
	if !s.rejected[key] && sessionID != "" {
		s.rejected[key] = true
		return nil, agent.ErrResumeUnhealthy
	}
	f := newFakeAgentSession(20000 + int(n))
	return f.buildLive(), nil
}

// TestFix2_ResumeUnhealthyRetry — /close lands on a stale
// sessionID; the next user message triggers an auto-recovery
// Spawn (without resume). Without the fix, the user would see
// "Failed to spawn agent" on every message until they hand-edit
// agent_sessions.json.
func TestFix2_ResumeUnhealthyRetry(t *testing.T) {
	csFile, asFile := newTestStores(t)
	sp := &ruSpawner{}
	cs, _ := New("c1", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(sp)
	cs.SetSelectedCwd("/code/A")
	cs.SetSelectedAgent("claude")

	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	as := cs.SelectedAgentSession()
	if as == nil {
		t.Fatal("no AS after first lookup")
	}
	as.SetSessionID("sess_stale")

	// Simulate /close: as.Close() + proactive as.SetExited(0)
	// (close.go fix #1 short-circuits the wait for the readpump's
	// eventual lifecycle event).
	as.Close()
	as.SetExited(0)

	// Real next-message dispatch path: LookupSelectedAgentSession.
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("post-close lookup: %v", err)
	}

	sp.mu.Lock()
	calls := sp.calls
	lastIDs := append([]string(nil), sp.lastIDs...)
	sp.mu.Unlock()

	t.Logf("after /close+message: spawner.calls=%d lastIDs=%v", calls, lastIDs)

	// Expect: cold spawn (""), rejected ("sess_stale"), retry ("").
	if calls < 3 {
		t.Errorf("auto-retry missing: calls=%d, want >= 3", calls)
	}
	if len(lastIDs) >= 3 && lastIDs[2] != "" {
		t.Errorf("retry spawn[2] sessionID = %q, want empty (auto-recovery)", lastIDs[2])
	}
	if got := cs.SelectedAgentSession().Status(); got != StatusRunning {
		t.Errorf("AS.status = %s, want StatusRunning after auto-recovery", got)
	}
}

// TestFix1_ProactiveSetExitedRace — fix-stop (2026-08-15):
// /close in flight, an inbound message arrives during the
// closeGraceTotal window. The proactive as.SetExited(0) in
// close.go makes the cache-hit check in
// LookupSelectedAgentSession skip the dying AS and fall through
// to hadPrior → respawn.
func TestFix1_ProactiveSetExitedRace(t *testing.T) {
	csFile, asFile := newTestStores(t)
	sp := newFakeSpawner()
	cs, _ := New("c1", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(sp)
	cs.SetSelectedCwd("/code/A")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	as.SetSessionID("sess_keep")

	if as.Status() != StatusRunning {
		t.Fatalf("baseline: AS status = %s, want Running", as.Status())
	}

	// Emulate close.go's full flow: as.Close() + proactive SetExited.
	if err := as.Close(); err != nil {
		t.Fatalf("as.Close: %v", err)
	}
	as.SetExited(0) // <- the fix: race-window AS-state update

	// Concurrent inbound message during /close.
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("post-close lookup: %v", err)
	}

	sp.mu.Lock()
	got := sp.calls
	sp.mu.Unlock()
	if got < 2 {
		t.Errorf("race-window respawn missing: spawner.calls=%d, want >= 2", got)
	}
	if as2 := cs.SelectedAgentSession(); as2 != as {
		t.Errorf("selectedAS changed: got %p, want %p (pool entry preserved)", as2, as)
	}
}

// TestFix2_OtherError_NoRetry — non-resume failures must NOT
// trigger the retry path. A binary-missing error from Spawn
// should surface immediately.
func TestFix2_OtherError_NoRetry(t *testing.T) {
	csFile, asFile := newTestStores(t)
	sp := &alwaysFailSpawner{err: errors.New("binary missing")}
	cs, _ := New("c1", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(sp)
	cs.SetSelectedCwd("/code/A")
	cs.SetSelectedAgent("claude")

	as := cs.SelectedAgentSession()
	if as == nil {
		as = NewAgentSession(newAgentSessionID(), cs.ID, "claude", "/code/A", nil)
		cs.attachAgentSessionLocked(as)
		cs.selectAgentSessionLocked(as)
	}
	as.SetSessionID("sess_keep")

	_, err := cs.LookupSelectedAgentSession()
	if err == nil {
		t.Fatal("expected spawn error to surface")
	}
	if errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Fatalf("plain error should not be classified as ErrResumeUnhealthy")
	}
	sp.mu.Lock()
	got := sp.calls
	sp.mu.Unlock()
	if got != 1 {
		t.Errorf("non-resume failure should not retry: calls=%d, want 1", got)
	}
	time.Sleep(50 * time.Millisecond)
	sp.mu.Lock()
	got = sp.calls
	sp.mu.Unlock()
	if got != 1 {
		t.Errorf("non-resume failure must not retry (later check): calls=%d, want 1", got)
	}
}

// alwaysFailSpawner returns a fixed error on every Spawn call.
type alwaysFailSpawner struct {
	mu    sync.Mutex
	calls int32
	err   error
}

func (s *alwaysFailSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	atomic.AddInt32(&s.calls, 1)
	return nil, s.err
}
