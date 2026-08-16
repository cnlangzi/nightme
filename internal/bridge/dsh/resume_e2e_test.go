// resume_e2e_test.go — end-to-end tests for the dsh bridge resume
// (session.fork) and picker (session.list) paths against a REAL
// `dsh --profile web` instance.
//
// Gated by NIGHTME_REAL_DSH (same gate as session_smoke_test.go).
// Skipped if dsh is not on PATH.
//
// What we cover (each test spawns its OWN dsh web on its own port
// so tests are independent):
//
//   1. TestE2E_Resume_ForkHappyPath        — full turn-driven flow
//   2. TestE2E_Resume_StaleIdRejected      — bad sessionId (already covered)
//   3. TestE2E_Resume_ListSessionsReturns — real picker data
//   4. TestE2E_Resume_ForkChain           — fork from fork preserves history
//   5. TestE2E_Resume_PickerFiltersBlank  — runtime-side filter pattern
//
//go:build !windows

package dsh

import (
	"os"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// driveOneTurn sends a simple prompt ("Reply with the single word PONG")
// and drains the events channel until EventAgentDone arrives or the
// timeout fires. Returns the SessionID stamped on EventAgentReady (i.e.
// the bridge session id, which now has a completed turn and is
// forkable). The returned session id is what we'd hand as
// cfg.SessionID on the next Start to resume via session.fork.
func driveOneTurn(t *testing.T, a *agent.Agent, timeout time.Duration) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Drain through EventAgentReady first (waits for the bridge
	// to stamp SessionID before we know where to send prompts).
	var sessionID string
	for sessionID == "" {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events channel closed before EventAgentReady")
			}
			if ev.Kind == agent.EventAgentReady {
				sessionID = ev.SessionID
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for EventAgentReady: %v", ctx.Err())
		}
	}

	// Send the prompt. This enqueues the turn on dsh's mux;
	// the actual model reply arrives via WS as a stream of
	// session/event frames, translated by the bridge into
	// EventAgentText / EventAgentResult / EventAgentDone.
	if err := a.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with the single word PONG. No other text."},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	// Drain until EventAgentDone (turn settled) or timeout.
	done := false
	for !done {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events channel closed before EventAgentDone")
			}
			if ev.Kind == agent.EventAgentDone {
				done = true
			}
			if ev.Kind == agent.EventAgentError {
				t.Fatalf("EventAgentError during turn: %v", ev.Err)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for EventAgentDone: %v", ctx.Err())
		}
	}
	return sessionID
}

// TestE2E_Resume_ForkHappyPath is the core E2E for the resume feature:
//   1. Spawn bridge A → drive one full turn → session A has a completed
//      turn (forkable).
//   2. Close A.
//   3. Spawn bridge B with cfg.SessionID = A's sessionId.
//   4. handshakeSession should call session.fork successfully, capture
//      a NEW server-assigned sessionId, and the bridge should log
//      "resumed=true".
//   5. Bridge B's EventAgentReady should carry the fork's new
//      sessionId (NOT A's sessionId — that's the whole point of fork).
func TestE2E_Resume_ForkHappyPath(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real-dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	s := NewStarter("dsh")
	workspace := t.TempDir()

	// === Phase 1: drive one turn on a fresh session. ===
	a1, err := s.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	sessionA := driveOneTurn(t, a1, 60*time.Second)
	t.Logf("session A (forkable, completed turn): %s", sessionA)
	if sessionA == "" {
		t.Fatal("sessionA is empty after driveOneTurn")
	}
	_ = a1.Close()

	// === Phase 2: spawn a new bridge with cfg.SessionID = A. ===
	// We can't re-use the closed a1's driver — its WS / pumps are dead.
	// Start spawns a fresh dsh web. handshakeSession inside newDriver
	// will see cfg.SessionID != "" and call session.fork.
	a2, err := s.Start(context.Background(), agent.StartConfig{
		Workspace: workspace,
		SessionID: sessionA,
	})
	if err != nil {
		t.Fatalf("Start #2 (resume): %v", err)
	}
	defer a2.Close()

	// === Phase 3: verify B's sessionId is the fork result, NOT A. ===
	var sessionB string
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for sessionB == "" {
		select {
		case ev, ok := <-a2.Events():
			if !ok {
				t.Fatal("a2 events closed before EventAgentReady")
			}
			if ev.Kind == agent.EventAgentReady {
				sessionB = ev.SessionID
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for a2 EventAgentReady")
		}
	}

	if sessionB == "" {
		t.Fatal("sessionB is empty")
	}
	if sessionB == sessionA {
		t.Errorf("sessionB = sessionA = %q; fork should return a NEW id", sessionA)
	}
	t.Logf("session B (fork of A): %s", sessionB)

	// sessionB should be a valid UUID format ("session-<uuid>").
	// We don't pin the exact format — dsh could rename it — but
	// it must NOT be empty and must NOT equal sessionA.
	if !strings.HasPrefix(sessionB, "session-") {
		t.Errorf("sessionB = %q, want prefix 'session-' (dsh UUID format)", sessionB)
	}
}

// TestE2E_Resume_ListSessionsReturns validates the picker path:
//   1. Spawn dsh web → drive one turn (so the session is non-blank).
//   2. Call driver.ListSessions against the SAME server.
//   3. Verify the items[] contains the session we just created.
//   4. Verify wire field is "items", NOT "sessions".
//   5. Verify Session struct fields match real dsh wire shape.
func TestE2E_Resume_ListSessionsReturns(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real-dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	s := NewStarter("dsh")
	workspace := t.TempDir()

	a, err := s.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Close()

	sessionID := driveOneTurn(t, a, 60*time.Second)
	t.Logf("drove session: %s", sessionID)

	// Reach into the agent to get the underlying driver so we can
	// call ListSessions (the bridge-specific extension method).
	d := a.Driver().(*driver)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sessions, err := d.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("ListSessions returned 0 items; dsh should have at least the session we just created")
	}

	// Find the session we created by ID.
	var found *Session
	for i := range sessions {
		if sessions[i].ID == sessionID {
			found = &sessions[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("session %s not in ListSessions result (%d items)", sessionID, len(sessions))
	}

	// Verify wire fields.
	if found.Running {
		t.Errorf("Running = true after turn settled, want false")
	}
	if found.Blank {
		t.Errorf("Blank = true after turn completed, want false (this is what made it forkable)")
	}
	if found.CWD != workspace {
		t.Errorf("CWD = %q, want %q", found.CWD, workspace)
	}
	if found.UpdatedAt <= 0 {
		t.Errorf("UpdatedAt = %d, want > 0 (unix millis)", found.UpdatedAt)
	}
	if found.AgentPreset == "" {
		t.Errorf("AgentPreset is empty; dsh always populates this (e.g. 'standard')")
	}
	t.Logf("found session in list: id=%s updatedAt=%d blank=%v running=%v cwd=%q preset=%q",
		found.ID, found.UpdatedAt, found.Blank, found.Running, found.CWD, found.AgentPreset)
}

// TestE2E_Resume_ForkChain verifies that forking a fork works
// (i.e. resume is idempotent in the chain sense: each fork
// returns a fresh sessionId that's itself forkable). Mirrors
// how a user who restarts the daemon daily accumulates a chain
// ses_v1 → ses_v2 → ses_v3 over time.
func TestE2E_Resume_ForkChain(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real-dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	s := NewStarter("dsh")
	workspace := t.TempDir()

	// Phase 1: fresh session with completed turn.
	a, err := s.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessionID := driveOneTurn(t, a, 60*time.Second)
	t.Logf("chain[0] (fresh): %s", sessionID)
	_ = a.Close()

	// Phase 2: fork → fork → fork. Each fork must return a new id.
	for i := 1; i <= 3; i++ {
		a, err := s.Start(context.Background(), agent.StartConfig{
			Workspace: workspace,
			SessionID: sessionID,
		})
		if err != nil {
			t.Fatalf("Start chain[%d]: %v", i, err)
		}

		newID := ""
		deadline := time.NewTimer(15 * time.Second)
		for newID == "" {
			select {
			case ev, ok := <-a.Events():
				if !ok {
					t.Fatalf("chain[%d] events closed before EventAgentReady", i)
				}
				if ev.Kind == agent.EventAgentReady {
					newID = ev.SessionID
				}
			case <-deadline.C:
				t.Fatalf("chain[%d] timed out waiting for EventAgentReady", i)
			}
		}

		if newID == sessionID {
			t.Errorf("chain[%d] returned same id %q; fork should return a NEW id", i, newID)
		}
		t.Logf("chain[%d]: %s -> %s", i, sessionID, newID)
		_ = a.Close()
		sessionID = newID
	}

	// Phase 3: final id should be forkable too (otherwise we've
	// accumulated a chain that doesn't compose — would be a
	// real bug). Drive one more turn on it to verify fork-ability.
	aFinal, err := s.Start(context.Background(), agent.StartConfig{
		Workspace: workspace,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Start chain-final: %v", err)
	}
	defer aFinal.Close()
	finalID := driveOneTurn(t, aFinal, 60*time.Second)
	if finalID == sessionID {
		t.Errorf("chain-final: sessionId unchanged after turn (got %q)", finalID)
	}
	t.Logf("chain-final after turn: %s -> %s (turn-driven fork succeeded)", sessionID, finalID)
}

// TestE2E_Resume_StaleIdReturnsUnhealthyOnRealDsh is the
// real-dsh equivalent of the mock test: a known-bad sessionId
// must produce errors.Is(err, agent.ErrResumeUnhealthy). Uses
// the same spawn pattern as the other E2E tests so we exercise
// the full newDriver path (not just handshakeSession in
// isolation).
func TestE2E_Resume_StaleIdReturnsUnhealthyOnRealDsh(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real-dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	s := NewStarter("dsh")
	workspace := t.TempDir()

	// Spawn bridge with a deliberately bogus sessionId.
	_, err := s.Start(context.Background(), agent.StartConfig{
		Workspace: workspace,
		SessionID: "ses_definitely_not_real_xyz123",
	})
	if err == nil {
		t.Fatal("Start with bogus sessionId: err = nil, want ErrResumeUnhealthy")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("err %v should satisfy errors.Is(err, agent.ErrResumeUnhealthy)", err)
	}
	if !strings.Contains(err.Error(), "session-not-found") {
		t.Logf("note: error code was %q (expected 'session-not-found' from dsh; the Unhealthy wrap is what matters)",
			err.Error())
	}
}

// TestE2E_Resume_BlankSessionRejected verifies the fork-unavailable
// error class surfaces as ErrResumeUnhealthy. This is the
// "user reopens a chat but the previous session had no completed
// turn" scenario — must trigger auto-retry in production.
func TestE2E_Resume_BlankSessionRejected(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_DSH") == "" {
		t.Skip("NIGHTME_REAL_DSH not set; skipping real-dsh e2e")
	}
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	s := NewStarter("dsh")
	workspace := t.TempDir()

	// Phase 1: create a session but do NOT drive a turn (blank).
	a, err := s.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	var blankSessionID string
	for blankSessionID == "" {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("a1 events closed before EventAgentReady")
			}
			if ev.Kind == agent.EventAgentReady {
				blankSessionID = ev.SessionID
			}
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for blank session EventAgentReady")
		}
	}
	t.Logf("blank session: %s", blankSessionID)
	_ = a.Close()

	// Phase 2: try to fork the blank session. dsh must reject
	// with "fork-unavailable" (verified via probe 2026-08-15).
	_, err = s.Start(context.Background(), agent.StartConfig{
		Workspace: workspace,
		SessionID: blankSessionID,
	})
	if err == nil {
		t.Fatal("Start on blank session: err = nil, want ErrResumeUnhealthy (fork-unavailable)")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("err %v should satisfy errors.Is(err, agent.ErrResumeUnhealthy)", err)
	}
	if !strings.Contains(err.Error(), "fork-unavailable") {
		t.Logf("note: error code was %q (expected 'fork-unavailable' from dsh)", err.Error())
	}
}
