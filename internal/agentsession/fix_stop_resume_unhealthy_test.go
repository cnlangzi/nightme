package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestRespawn_ResumeUnhealthy_PreservesSessionID pins the
// post-2026-09-13 design: the bridge's "resume rejected" error
// is loud, not silently masked. The saved sessionId stays put
// (the user can inspect /Users/.../agent_sessions.json or send
// `/new` to explicitly start a fresh session) — the bridge
// does NOT auto-clear it.
//
// Previous (fix-stop 2026-08-15) behavior was to clear
// sessionId on ErrResumeUnhealthy so the chat-layer's
// retry path could land on a fresh session. That worked
// mechanically but masked the original cause: the user never
// saw that their saved session (e.g. session-26c4ff1a-...)
// was rejected in favor of a fresh id (d5de9b4e-...).
func TestRespawn_ResumeUnhealthy_PreservesSessionID(t *testing.T) {
	as := newAgentSessionRuntime("as_ru", "cs_x", "cc", "/code", nil)
	as.SetSessionID("sess_stale")

	// Spawner that always fails with ErrResumeUnhealthy.
	s := &flakyResumeSpawner{
		firstErr: resumeRejectErr{},
		handle:   newFakeAgentSession(4242).buildLive(),
	}

	err := as.respawn(context.Background(), s, nil, "sess_stale")
	if err == nil {
		t.Fatal("respawn should have returned the resume-rejection error")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Fatalf("err = %v, want errors.Is(_, agent.ErrResumeUnhealthy)", err)
	}
	if got := as.SessionID(); got != "sess_stale" {
		t.Errorf("after rejection, sessionID = %q, want \"sess_stale\" (must NOT be cleared)", got)
	}
}

// flakyResumeSpawner fails its first Spawn call with the configured
// error and succeeds on every subsequent call.
type flakyResumeSpawner struct {
	firstErr error
	handle   *agent.Agent
	calls    int
}

func (s *flakyResumeSpawner) Spawn(_ context.Context, _, _ string, _ []string, sessionID string) (*agent.Agent, error) {
	s.calls++
	if s.calls == 1 && s.firstErr != nil {
		return nil, s.firstErr
	}
	return s.handle, nil
}

// resumeRejectErr wraps both the bridge-level sentinel (for
// callers that already import claudecode) and the agent-level
// sentinel (for chatsession). It mimics claudecode's
// resumeUnhealthyError in tests.
type resumeRejectErr struct{}

func (e resumeRejectErr) Error() string { return "claudecode: --resume session unhealthy" }
func (e resumeRejectErr) Is(target error) bool {
	return target == agent.ErrResumeUnhealthy
}

// TestRespawn_OtherError_LeavesSessionID guards the test above:
// any error that is NOT ErrResumeUnhealthy must not clear the
// sessionId either (consistent with the new design — no auto
// mutation on spawn failure). A generic spawn failure (e.g.
// binary missing) leaves sessionId alone; the user can `/new`
// to intentionally drop it, or the next spawn fix the
// underlying cause.
func TestRespawn_OtherError_LeavesSessionID(t *testing.T) {
	as := newAgentSessionRuntime("as_other", "cs_x", "cc", "/code", nil)
	as.SetSessionID("sess_keep")

	s := &flakyResumeSpawner{
		firstErr: errors.New("binary missing"),
		handle:   newFakeAgentSession(1).buildLive(),
	}

	err := as.respawn(context.Background(), s, nil, "sess_keep")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Fatalf("plain error should NOT be classified as ErrResumeUnhealthy; got %v", err)
	}
	if got := as.SessionID(); got != "sess_keep" {
		t.Errorf("after non-respawn failure, sessionID = %q, want sess_keep (must NOT clear)", got)
	}
}
