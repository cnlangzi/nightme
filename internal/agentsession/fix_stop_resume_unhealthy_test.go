package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestRespawn_ResumeUnhealthy_ClearsSessionID fixes regression
// from the T-alive (2026-08-07) loud-failure path: when the
// bridge rejects the saved sessionID, respawn used to leave the
// stale ID on the AS. Every subsequent Spawn would re-pass it
// and re-fail identically, leaving the chat layer unable to
// recover without manual agent_sessions.json edits.
//
// Fix: on errors.Is(err, agent.ErrResumeUnhealthy), clear
// as.sessionID in memory AND persist so the next Spawn lands
// on a clean fresh session.
func TestRespawn_ResumeUnhealthy_ClearsSessionID(t *testing.T) {
	as := newAgentSessionRuntime("as_ru", "cs_x", "cc", "/code", nil)
	as.SetSessionID("sess_stale")

	// Spawner that fails with ErrResumeUnhealthy on the first
	// call (resume rejected) and succeeds on the second (no
	// resume).
	s := &flakyResumeSpawner{
		firstErr: resumeRejectErr{},
		handle:   newFakeAgentSession(4242).buildLive(),
	}

	// First call: should return the wrapped error AND clear the
	// sessionID.
	err := as.respawn(context.Background(), s, nil, "sess_stale")
	if err == nil {
		t.Fatal("first respawn should have returned the resume-rejection error")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Fatalf("first respawn: err = %v, want errors.Is(_, agent.ErrResumeUnhealthy)", err)
	}
	if got := as.SessionID(); got != "" {
		t.Errorf("after rejection, sessionID = %q, want \"\" (must be cleared for retry)", got)
	}

	// Second call (the chat-layer retry): spawner no longer fakes
	// a rejection. Should succeed without resume id.
	if err := as.respawn(context.Background(), s, nil, as.SessionID()); err != nil {
		t.Fatalf("second respawn (fresh): err = %v", err)
	}
	if got := as.Status(); got != StatusRunning {
		t.Errorf("after fresh respawn, status = %s, want StatusRunning", got)
	}
}

// flakyResumeSpawner fails its first Spawn call with the configured
// error and succeeds on every subsequent call. Mirrors the
// claudecode "first --resume fails, second fresh call works"
// recovery path.
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
// only ErrResumeUnhealthy triggers the clear. A generic spawn
// failure (e.g. binary missing) must NOT clear the sessionID —
// the user can `/new` to intentionally drop it, or the next
// spawn fix the underlying cause.
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
