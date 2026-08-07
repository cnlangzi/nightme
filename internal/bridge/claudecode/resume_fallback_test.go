package claudecode

// T-alive (2026-08-07): regression coverage for --resume
// preservation. The bridge must SURFACE --resume failures as
// ErrResumeUnhealthy rather than silently falling back to a
// fresh session (which previously dropped the user's resume
// context — the runtime saw a "working" session but the
// assistant had no memory of the prior conversation).
//
// Skipped if claude isn't on PATH.

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestStart_ResumeRejectionSurfacesError verifies that an invalid
// --resume session id results in a returned ErrResumeUnhealthy,
// NOT a silent fallback to a fresh session. Before the
// preservation fix (commit T-alive), the bridge would close the
// wedged session and re-spawn with ResumeID="" — the runtime saw
// a "working" session but the assistant had no resume context.
func TestStart_ResumeRejectionSurfacesError(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a := New("claude", "claude", nil)
	// valid-format UUID that doesn't exist in claude's session store.
	// claude emits "No conversation found with session ID: ..." on
	// stderr, which the probe classifies as a resume rejection.
	resumeID := "deadbeef-dead-dead-dead-deadbeefdead"

	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace:      "/tmp",
		PermissionMode: "bypassPermissions",
		ResumeID:       resumeID,
	})
	if sess != nil {
		t.Cleanup(func() { _ = sess.Close() })
	}
	if err == nil {
		t.Fatalf("Start should fail with ErrResumeUnhealthy; got success (silent fallback regressed)")
	}
	if !errors.Is(err, ErrResumeUnhealthy) {
		t.Errorf("Start error = %v, want ErrResumeUnhealthy", err)
	}
	if !strings.Contains(err.Error(), resumeID) {
		t.Errorf("error should mention resume id %q for debuggability; got %q", resumeID, err.Error())
	}
	t.Logf("[test] Start rejected --resume %q with: %v (no silent fallback)", resumeID, err)
}

// TestStart_ResumeID_PreservedAcrossProbe verifies that for a
// VALID --resume id, the session id captured by EventInit equals
// the requested resume id — i.e. the bridge is actually
// resuming, not silently replacing with a fresh session.
//
// The test is self-contained: it spawns claude in a temp dir to
// capture a fresh session id, then closes it and re-spawns with
// --resume <captured_id>. No hardcoded user paths or session
// ids — the test exercises the same code path the runtime uses
// for resume, end-to-end.
func TestStart_ResumeID_PreservedAcrossProbe(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ws := t.TempDir()
	a := New("claude", "claude", nil)

	// Phase 1: fresh spawn → capture session id.
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
	var capturedID string
phase1Loop:
	for {
		select {
		case <-ctx.Done():
			t.Fatal("phase 1: ctx done")
		case ev, ok := <-sess1.Events():
			if !ok {
				t.Fatal("phase 1: events closed")
			}
			if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.SessionID != "" {
				capturedID = ev.Init.SessionID
				t.Logf("[test] phase 1: captured sessionID=%q", capturedID)
				break phase1Loop
			}
		case <-time.After(90 * time.Second):
			t.Fatal("phase 1: no init within 90s")
		}
	}
	_ = sess1.Close()
	if capturedID == "" {
		t.Skip("phase 1 did not produce a session id (claude may be offline)")
	}

	// Phase 2: re-spawn with --resume <capturedID>. The bridge's
	// probe should see no stderr rejection and return the
	// resumed session. init.SessionID must equal capturedID.
	sess2, err := a.Start(ctx, agent.StartConfig{
		Workspace:      ws,
		PermissionMode: "bypassPermissions",
		ResumeID:       capturedID,
	})
	if err != nil {
		t.Fatalf("phase 2 Start: %v (resume was lost — bridge errored instead of preserving)", err)
	}
	t.Cleanup(func() { _ = sess2.Close() })

	if err := sess2.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "reply with one word: pong"},
	}); err != nil {
		t.Fatalf("phase 2 SendBlocks: %v", err)
	}

	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev, ok := <-sess2.Events():
			if !ok {
				t.Fatalf("phase 2: events closed before init")
			}
			if ev.Kind == agent.EventInit && ev.Init != nil {
				if ev.Init.SessionID != capturedID {
					t.Fatalf("phase 2: init.SessionID = %q, want %q (resume context lost — bridge replaced with fresh session)",
						ev.Init.SessionID, capturedID)
				}
				t.Logf("[test] phase 2: init.SessionID = %q matches resumeID — resume preserved", ev.Init.SessionID)
				return
			}
			if ev.Kind == agent.EventError {
				t.Fatalf("phase 2: bridge emitted EventError: %v", ev.Error.Err)
			}
		case <-deadline:
			t.Fatalf("phase 2: no init within 60s")
		}
	}
}

// TestIsResumeErrorMessage covers the substring matcher. Each
// row is (input text, expected match).
func TestIsResumeErrorMessage(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		// Real shapes from claude 2.1.220 (verified 2026-08-07)
		{"No conversation found with session ID: 4E62175B-C86A-4C80-B0F1-0641057A6C2C", true},
		{"--resume requires a valid session ID or session title when used with --print.", true},
		{"Error: --resume requires a valid session ID or session title. Provided value \"...\" is not a UUID and does not match any session title.", true},

		// Negative: unrelated errors must NOT trigger fallback.
		{"EACCES: permission denied", false},
		{"connection refused", false},
		{"rate limit exceeded", false},
		{"", false},

		// Edge: substring-only "session" without "not found"/"resume requires" — also no.
		{"session expired", false},

		// Case-insensitive match.
		{"NO CONVERSATION FOUND WITH SESSION ID: x", true},
		{"Resume Requires a Valid Session", true},
	}
	for _, c := range cases {
		got := isResumeErrorMessage(c.text)
		if got != c.want {
			t.Errorf("isResumeErrorMessage(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestClassifyStderrLineForResume_MCPNotTrigger verifies the
// classifier change: MCP server failure stderr lines must NOT
// trigger an unhealthy probe. Previously these lines caused
// silent fallback to a fresh session, dropping the user's
// resume context whenever an MCP server was misconfigured.
// Verified 2026-08-07: MCP failures do not prevent claude from
// emitting init or processing the user message — they are
// informational only.
func TestClassifyStderrLineForResume_MCPNotTrigger(t *testing.T) {
	badLines := []string{
		"Failed to connect MCP server foo: connection refused",
		"MCP server bar timed out after 5s",
		"MCP server baz unreachable",
		"Failed to load MCP config: file not found",
		"Tool execution error: missing tool foo",
		"tool failed: timeout",
	}
	for _, l := range badLines {
		if reason, isBad := classifyStderrLineForResume(l); isBad {
			t.Errorf("classify(%q) = (%q, true), want (_, false) — MCP/tool failures must not trigger resume rejection",
				l, reason)
		}
	}
}

// TestClassifyStderrLineForResume_RejectionTriggers verifies the
// classifier still flags --resume rejection signals.
func TestClassifyStderrLineForResume_RejectionTriggers(t *testing.T) {
	goodLines := []struct {
		line   string
		reason string
	}{
		{"No conversation found with session ID: 4E62175B-C86A-4C80-B0F1-0641057A6C2C", "stderr_resume_rejection"},
		{"--resume requires a valid session ID or session title.", "stderr_resume_rejection"},
		{"Session not found in store", "stderr_resume_rejection"},
	}
	for _, c := range goodLines {
		reason, isBad := classifyStderrLineForResume(c.line)
		if !isBad {
			t.Errorf("classify(%q) = (_, false), want (%q, true)", c.line, c.reason)
			continue
		}
		if reason != c.reason {
			t.Errorf("classify(%q) = (%q, true), want (%q, true)", c.line, reason, c.reason)
		}
	}
}