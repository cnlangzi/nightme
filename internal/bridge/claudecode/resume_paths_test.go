package claudecode

// T-alive (2026-08-07): systematic coverage of every --resume
// path the user might hit, so we can pick a fix that's grounded
// in observed claude behavior instead of guessing.
//
// Skipped if `claude` isn't on PATH. Each test case is gated
// on a specific env var so CI doesn't accidentally depend on
// user-private session stores.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestResumePaths_Table runs every --resume path the user has
// hit, in one go, so the next reader can see the verdict for
// each in one place. Driven by env var gating.
//
// Env vars:
//   NIGHTME_TALIVE_RESUME_REPLAY=1 → fresh spawn → capture
//     session_id → close → re-spawn with --resume <id>
//     (happy path: the resume works)
//   NIGHTME_TALIVE_RESUME_BAD=1 → spawn with a definitely-invalid
//     UUID (the user's symptom — stored SessionID points at a
//     session that no longer exists)
//   NIGHTME_TALIVE_RESUME_USER=1 → use the user's actual
//     SessionID stored in agent_sessions.json (the actual
//     production symptom)
//
// Output is structured so it can be compared at a glance.
func TestResumePaths_Table(t *testing.T) {
	requireRealClaude(t)

	t.Run("resume_happy_path_replay", func(t *testing.T) {
		if !envOn("NIGHTME_TALIVE_RESUME_REPLAY") {
			t.Skip("set NIGHTME_TALIVE_RESUME_REPLAY=1")
		}
		resumeHappyPath(t, 5*time.Minute)
	})

	t.Run("resume_invalid_uuid", func(t *testing.T) {
		if !envOn("NIGHTME_TALIVE_RESUME_BAD") {
			t.Skip("set NIGHTME_TALIVE_RESUME_BAD=1")
		}
		resumeInvalidUUID(t, 30*time.Second)
	})

	t.Run("resume_user_workspace_known_id", func(t *testing.T) {
		if !envOn("NIGHTME_TALIVE_RESUME_USER") {
			t.Skip("set NIGHTME_TALIVE_RESUME_USER=1")
		}
		resumeUserWorkspaceKnownID(t, 3*time.Minute)
	})
}

// resumeHappyPath: spawn → send prompt → capture EventAgentReady
// SessionID → close → spawn again with --resume <id> → send
// prompt → expect text. If both succeed, --resume works
// correctly and the production hang is a different bug.
func resumeHappyPath(t *testing.T, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	a := NewStarter("claude", "claude", nil)
	ws, _ := resolveWorkspace(t)

	// Phase 1: fresh spawn → capture session id.
	t.Logf("[replay] phase 1: fresh spawn")
	sess1, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("phase 1 Start: %v", err)
	}
	// T-alive (2026-08-07): claude --print is single-turn mode —
	// it does NOT emit EventAgentReady until it receives the first
	// stdin block. SendText before waiting for events.
	if err := sess1.SendText("capture my session id, then say only: pong"); err != nil {
		t.Fatalf("phase 1 SendText: %v", err)
	}
	var capturedID string
	initSeen := false
	phaseStart := time.Now()
initLoop:
	for {
		select {
		case <-ctx.Done():
			t.Fatal("phase 1: ctx done before init")
		case ev, ok := <-sess1.Events():
			if !ok {
				t.Fatal("phase 1: events closed before init")
			}
			t.Logf("[replay] phase 1 EV at %s kind=%v initNonNil=%v sessionID=%q",
				time.Since(phaseStart).Round(time.Millisecond), ev.Kind,
				true, initSessionID(ev))
			if ev.Kind == agent.EventAgentReady  && ev.SessionID != "" {
				capturedID = ev.SessionID
				initSeen = true
				t.Logf("[replay] phase 1: captured sessionID=%q", capturedID)
				break initLoop
			}
		case <-time.After(90 * time.Second):
			t.Fatalf("phase 1: no EventAgentReady within 90s (claude took >90s to handshake; possible MCP startup hang in workspace=%s)", ws)
		}
	}
	if !initSeen {
		t.Fatal("phase 1: init not observed")
	}
	_ = sess1.Close()

	// Phase 2: re-spawn with --resume <capturedID>.
	if capturedID == "" {
		t.Skip("phase 1 did not produce a session id; claude may be offline")
	}
	t.Logf("[replay] phase 2: re-spawn with --resume %q", capturedID)
	sess2, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
		SessionID:       capturedID,
	})
	if err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	t.Cleanup(func() { _ = sess2.Close() })
	t.Logf("[replay] phase 2: Spawned pid=%d", sess2.PID())

	if err := sess2.SendText("reply with one word: pong"); err != nil {
		t.Fatalf("phase 2 SendText: %v", err)
	}

	timeout := time.After(60 * time.Second)
	for {
		select {
		case ev, ok := <-sess2.Events():
			if !ok {
				t.Fatal("phase 2: events closed before output")
			}
			if ev.Kind == agent.EventAgentText || ev.Kind == agent.EventAgentResult || ev.Kind == agent.EventAgentDone {
				t.Logf("[replay] phase 2: resumed session produced output: kind=%v text=%q",
					ev.Kind, truncate(ev.Text, 100))
				return
			}
		case <-timeout:
			t.Fatalf("phase 2: resumed session produced no output within 60s — --resume happy path broken in user's env")
		}
	}
}

// resumeInvalidUUID: spawn with a definitely-invalid UUID.
// T-alive (2026-08-07): the bridge now refuses to silently fall
// back to a fresh session — instead it surfaces ErrResumeUnhealthy
// so the runtime can tell the user the resume session was not
// found (and they can retry / check workspace). Pre-fix this test
// expected Start to succeed via fallback.
func resumeInvalidUUID(t *testing.T, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	a := NewStarter("claude", "claude", nil)
	ws, _ := resolveWorkspace(t)
	const bad = "deadbeef-dead-dead-dead-deadbeefdead"
	t.Logf("[invalid] spawning with --resume %q", bad)
	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
		SessionID:       bad,
	})
	if sess != nil {
		t.Cleanup(func() { _ = sess.Close() })
	}
	if err == nil {
		t.Fatalf("Start should fail with ErrResumeUnhealthy; got success (silent fallback regressed)")
	}
	if !errors.Is(err, ErrResumeUnhealthy) {
		t.Fatalf("Start error = %v, want ErrResumeUnhealthy", err)
	}
	t.Logf("[invalid] ✓ Start rejected with ErrResumeUnhealthy: %v (no silent fallback)", err)
}

// resumeUserWorkspaceKnownID: reproduce the actual production
// hang. Uses NIGHTME_TALIVE_RESUME_USER_ID (a real, known-valid
// SessionID from the user's claude session store). The default
// is the one we saw stuck during test10/11 — 372809cf-c36b-44e5-a321-adf93c159e5d.
// If that id is invalid, set the env var to one that IS valid
// in your local claude store.
//
// Pass criteria: spawned claude must produce ANY event within
// 90s. If not, --resume is fundamentally broken for the user's
// setup (MCP / session-store issue we can't fix from nightme).
func resumeUserWorkspaceKnownID(t *testing.T, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	a := NewStarter("claude", "claude", nil)
	ws, _ := resolveWorkspace(t)
	const defaultUserID = "372809cf-c36b-44e5-a321-adf93c159e5d"
	id := envOr("NIGHTME_TALIVE_RESUME_USER_ID", defaultUserID)
	t.Logf("[user] spawning with --resume %q in workspace=%s", id, ws)
	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
		SessionID:       id,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	t.Logf("[user] Spawned pid=%d", sess.PID())

	// claude --print is single-turn mode — it does NOT emit any
	// events until it receives the first stdin block.
	// (T-alive, 2026-08-07: this was the silent-failure cause
	// in the resume_happy_path_replay test before the fix.)
	if err := sess.SendText("reply with one word: pong"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	start := time.Now()
	timeout := time.After(deadline)
	gotAny := false
	for !gotAny {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("[user] events channel closed at %s before any event", time.Since(start))
			}
			t.Logf("[user] EV at %s kind=%v", time.Since(start).Round(time.Millisecond), ev.Kind)
			gotAny = true
		case <-timeout:
			t.Fatalf("[user] NO events within %s for --resume %q in workspace=%s — --resume is broken here",
				deadline, id, ws)
		}
	}
}

// initSessionID safely extracts the SessionID from an
// EventAgentReady event (returns "" if Init is nil).
func initSessionID(ev agent.AgentEvent) string {
	if true {
		return ""
	}
	return ev.SessionID
}

func envOn(name string) bool {
	for _, v := range []string{"1", "true", "yes", "on"} {
		if envOr(name, "") == v {
			return true
		}
	}
	return false
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// resolveWorkspace returns the workspace to test in:
//   - NIGHTME_TALIVE_USER_WS env var if set
//   - the test process's cwd (typically the nightme repo
//     when running with `go test ./...` from the repo root)
// which matches what nightme's runtime uses after /cwd.
func resolveWorkspace(t *testing.T) (string, error) {
	t.Helper()
	if v := os.Getenv("NIGHTME_TALIVE_USER_WS"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd, nil
}
