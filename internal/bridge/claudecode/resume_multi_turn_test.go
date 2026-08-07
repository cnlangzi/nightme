package claudecode

// T-alive (2026-08-07): end-to-end regression for the test19
// production hang. Validates that:
//
//  1. With --print restored, claude emits init immediately on
//     spawn (no stdin-gated hang like multi-turn interactive
//     mode caused).
//  2. init.SessionID matches the requested ResumeID — resume
//     context survives the respawn boundary.
//  3. Within one bridge session, multiple SendBlocks on the
//     same claude process produce multiple responses (proves
//     --print + bridge-held-stdin = multi-turn per chat).
//
// Skipped if claude isn't on PATH.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestStart_ResumeMultiTurnRespawn reproduces the test19 scenario:
// spawn with --resume, confirm init.SessionID matches and a turn
// completes. Then start a SECOND bridge session (separate go
// process) with the same ResumeID and confirm init.SessionID still
// matches — proves resume context survives the per-chat-respawn
// boundary that main's working mode relies on.
//
// Note on claude lifecycle with --print: claude does NOT auto-exit
// after one message when stdin stays open. The bridge keeps the
// stdin pipe open for the chat's lifetime (multi-turn via repeated
// SendBlocks on the same bridge session), and claude waits for the
// next newline-terminated JSON envelope. This is the desired
// behavior for a chat session — one claude process handles every
// turn of the conversation within a chat, with --resume carrying
// the persisted context only when the daemon restarts or AS
// respawns (not across user turns within one chat).
func TestStart_ResumeMultiTurnRespawn(t *testing.T) {
	requireRealClaude(t)

	ws := t.TempDir()
	a := New("claude", "claude", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Phase 1: fresh spawn → capture session id.
	t.Logf("[multi-turn] phase 1: fresh spawn to capture session id")
	sess1, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("phase 1 Start: %v", err)
	}
	if err := sess1.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "capture my session id, then say only: pong"},
	}); err != nil {
		t.Fatalf("phase 1 SendBlocks: %v", err)
	}
	capturedID := captureInitSessionID(t, sess1, 60*time.Second)
	_ = sess1.Close()
	t.Logf("[multi-turn] phase 1: captured sessionID=%q", capturedID)

	// Phase 2: separate bridge session with --resume <capturedID>.
	// Proves resume context survives a fresh process with same id.
	t.Logf("[multi-turn] phase 2: spawn with --resume")
	sess2, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
		ResumeID:       capturedID,
	})
	if err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	t.Cleanup(func() { _ = sess2.Close() })

	// Turn A on sess2.
	if err := sess2.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "reply with one word: pong"},
	}); err != nil {
		t.Fatalf("phase 2 SendBlocks turn A: %v", err)
	}
	gotInit := false
	gotResultA := false
	for !gotResultA {
		select {
		case ev, ok := <-sess2.Events():
			if !ok {
				t.Fatalf("phase 2 turn A: events closed before result (gotInit=%v)", gotInit)
			}
			t.Logf("[multi-turn] phase 2 turn A EV kind=%v", ev.Kind)
			if ev.Kind == agent.EventInit && ev.Init != nil {
				if ev.Init.SessionID != capturedID {
					t.Fatalf("phase 2: init.SessionID = %q, want %q (resume context lost across respawn)",
						ev.Init.SessionID, capturedID)
				}
				gotInit = true
				t.Logf("[multi-turn] phase 2: init.SessionID matches resumeID — resume preserved across respawn")
			}
			if ev.Kind == agent.EventResult {
				gotResultA = true
				if ev.Result == nil || !strings.Contains(strings.ToLower(ev.Result.Text), "pong") {
					t.Errorf("phase 2 turn A: result = %q, want contains 'pong'", ev.Result.Text)
				}
			}
			if ev.Kind == agent.EventError {
				t.Fatalf("phase 2 turn A: bridge emitted EventError: %v", ev.Error.Err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("phase 2 turn A: no result within 60s (gotInit=%v) — regression of test19 hang", gotInit)
		}
	}
	if !gotInit {
		t.Fatalf("phase 2 turn A: never saw EventInit — regression of test19 hang")
	}

	// Phase 3 (the multi-turn proof): turn B on the SAME bridge
	// session (= same claude process). This is what real chat
	// sessions do — multiple SendBlocks without respawning
	// claude. If this turns, then per-chat = one process and the
	// chat-session doesn't need per-turn respawn logic.
	t.Logf("[multi-turn] phase 3: turn B on same bridge session (proves multi-turn)")
	if err := sess2.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "now reply with one word: ping"},
	}); err != nil {
		t.Fatalf("phase 3 SendBlocks turn B: %v", err)
	}
	gotResultB := false
	for !gotResultB {
		select {
		case ev, ok := <-sess2.Events():
			if !ok {
				t.Fatalf("phase 3 turn B: events closed before result — claude exited unexpectedly")
			}
			t.Logf("[multi-turn] phase 3 turn B EV kind=%v", ev.Kind)
			if ev.Kind == agent.EventResult {
				gotResultB = true
				if ev.Result == nil || !strings.Contains(strings.ToLower(ev.Result.Text), "ping") {
					t.Errorf("phase 3 turn B: result = %q, want contains 'ping'", ev.Result.Text)
				}
			}
			if ev.Kind == agent.EventError {
				t.Fatalf("phase 3 turn B: bridge emitted EventError: %v", ev.Error.Err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("phase 3 turn B: no result within 60s — same-session multi-turn broken")
		}
	}
	t.Logf("[multi-turn] phase 3: turn B succeeded on same bridge session — multi-turn per-chat works")
}

// captureInitSessionID drains the events channel until it sees
// an EventInit with a non-empty SessionID, or until the deadline
// elapses. Returns the captured id.
func captureInitSessionID(t *testing.T, sess agent.AgentSession, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("captureInit: events channel closed before init")
			}
			if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.SessionID != "" {
				return ev.Init.SessionID
			}
		case <-deadline:
			t.Fatalf("captureInit: no EventInit within %s — claude hung (regression of test19 hang)", timeout)
		}
	}
}