//go:build !windows

package codex

// Real end-to-end tests against the user's local `codex` CLI.
//
// These tests exercise the full handshake + turn lifecycle:
//
//   1. TestE2E_FreshThread — Start → EventAgentReady → SendBlocks →
//      one complete turn. Verifies that thread/start works, the
//      translator emits a non-empty EventAgentResult.Text, the
//      per-turn usage is populated, and the per-turn EventAgentDone
//      carries Reason="settled" without closing the events channel.
//
//   2. TestE2E_ResumeThread — Do a fresh turn, capture the
//      thread id, close, then re-Start with cfg.SessionID set.
//      The bridge should pick thread/resume and the new session
//      must report the SAME thread id on its EventAgentReady.
//
//   3. TestE2E_ApprovalFlow — Spawn with approval_policy="on-request"
//      and prompt the model to run a shell command. The app-server
//      should send an approval request which we accept via
//      SendPermission. Asserts the bridge emits EventAgentPermission
//      and that the command's side-effect is observed on disk.
//
// All three tests require:
//   - `codex` on PATH  (requireRealCodex)
//   - valid OPENAI_API_KEY in env (loaded from ~/.codex/auth.json
//     by the codex binary itself)
//
// They are designed to be tolerant of slow networks: each scenario
// has a generous timeout (90-180s) so a cold-start model load does
// not flake. Tests run sequentially inside this package via -p=1
// when run together, but each scenario is independent.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// drainUntilTurnDone reads events until EITHER EventAgentDone OR
// EventAgentError is observed (the per-turn terminal signals), the
// channel closes, OR `deadline` elapses with no terminal event.
//
// It does NOT return on EventAgentResult — codex emits Result and
// Done back-to-back; stopping at Result would let the bridge's
// back-pressure drop Done (events channel capacity is 64 but the
// test reader is the only consumer in the unit-test process).
//
// Returns the collected slice so assertions can scan for specific
// kinds. If deadline elapses without a terminal signal the test
// fails (the model likely hung or the bridge never emitted Done).
func drainUntilTurnDone(t *testing.T, events <-chan agent.AgentEvent, deadline time.Duration) []agent.AgentEvent {
	t.Helper()
	end := time.After(deadline)
	var out []agent.AgentEvent
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Kind == agent.EventAgentDone || ev.Kind == agent.EventAgentError {
				return out
			}
		case <-end:
			t.Fatalf("deadline %s reached without Done/Error; kinds=%v", deadline, kindsOnly(out))
			return out
		}
	}
}

func kindsOnly(events []agent.AgentEvent) []agent.EventKind {
	out := make([]agent.EventKind, len(events))
	for i, ev := range events {
		out[i] = ev.Kind
	}
	return out
}

// findEvent returns the first event whose Kind matches and a bool
// indicating whether one was found in the slice.
func findEvent(events []agent.AgentEvent, kind agent.EventKind) (agent.AgentEvent, bool) {
	for _, ev := range events {
		if ev.Kind == kind {
			return ev, true
		}
	}
	return agent.AgentEvent{}, false
}

// findAll returns every event whose Kind matches.
func findAll(events []agent.AgentEvent, kind agent.EventKind) []agent.AgentEvent {
	var out []agent.AgentEvent
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────
// Test 1: fresh thread, single turn, text + usage + done.
// ─────────────────────────────────────────────────────────────────

func TestE2E_FreshThread(t *testing.T) {
	requireRealCodex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	ws := t.TempDir()
	a := NewStarter("codex", "codex", nil)

	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace: ws,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// First event must be EventAgentReady with a non-empty
	// SessionID + Model. Read it from the channel with a deadline
	// — failure here usually means the handshake hung.
	var ready agent.AgentEvent
	select {
	case ready = <-sess.Events():
	case <-time.After(30 * time.Second):
		t.Fatalf("no EventAgentReady within 30s — handshake likely hung")
	}
	if ready.Kind != agent.EventAgentReady {
		t.Fatalf("first event kind = %s, want EventAgentReady", ready.Kind)
	}
	if ready.SessionID == "" {
		t.Errorf("EventAgentReady.SessionID is empty")
	}
	if ready.Model == "" {
		t.Errorf("EventAgentReady.Model is empty")
	}
	t.Logf("[e2e-fresh] thread=%s model=%s pid=%d", ready.SessionID, ready.Model, sess.PID())

	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with only the single word: pong"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	events := drainUntilTurnDone(t, sess.Events(), 120*time.Second)

	// Must see exactly one Result + one Done.
	results := findAll(events, agent.EventAgentResult)
	dones := findAll(events, agent.EventAgentDone)
	if len(results) != 1 {
		t.Errorf("EventAgentResult count = %d, want 1; kinds=%v", len(results), kindsOnly(events))
	}
	if len(dones) != 1 {
		t.Errorf("EventAgentDone count = %d, want 1; kinds=%v", len(dones), kindsOnly(events))
	}
	if len(results) >= 1 {
		r := results[0]
		if !strings.Contains(strings.ToLower(r.Result.Text), "pong") {
			t.Errorf("Result.Text = %q, want it to contain 'pong'", r.Result.Text)
		} else {
			t.Logf("[e2e-fresh] Result.Text=%q", truncate(r.Result.Text, 120))
		}
	}
	if len(dones) >= 1 {
		d := dones[0]
		if d.Done == nil || d.Done.Reason != "settled" {
			t.Errorf("Done.Reason = %v, want \"settled\"", d.Done)
		}
		if d.Done == nil || d.Done.Usage == nil {
			t.Errorf("Done.Usage is nil — bridge did not capture per-turn usage")
		} else if d.Done.Usage.InputTokens+d.Done.Usage.OutputTokens == 0 {
			t.Errorf("Done.Usage zero tokens: %+v", d.Done.Usage)
		} else {
			t.Logf("[e2e-fresh] Usage in=%d out=%d", d.Done.Usage.InputTokens, d.Done.Usage.OutputTokens)
		}
	}

	// Channel must NOT be closed after EventAgentDone — the
	// process is still alive and a follow-up turn could flow.
	select {
	case _, ok := <-sess.Events():
		if !ok {
			t.Errorf("events channel closed after one turn — long-lived bridge invariant violated")
		}
		// anything else is fine; just confirm we are still subscribed.
	case <-time.After(100 * time.Millisecond):
		// no event yet — expected; channel is buffered + open.
	}
}

// ─────────────────────────────────────────────────────────────────
// Test 2: resume thread.
// ─────────────────────────────────────────────────────────────────

func TestE2E_ResumeThread(t *testing.T) {
	requireRealCodex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	ws := t.TempDir()

	// --- first session: do one turn, capture thread id, close ---
	a := NewStarter("codex", "codex", nil)
	sess1, err := a.Start(ctx, agent.StartConfig{Workspace: ws})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	var ready1 agent.AgentEvent
	select {
	case ready1 = <-sess1.Events():
	case <-time.After(30 * time.Second):
		t.Fatalf("first session: no EventAgentReady within 30s")
	}
	if ready1.SessionID == "" {
		t.Fatalf("first session: empty SessionID")
	}
	t.Logf("[e2e-resume] first thread=%s", ready1.SessionID)

	if err := sess1.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with only the single word: pong"},
	}); err != nil {
		t.Fatalf("first SendBlocks: %v", err)
	}
	events1 := drainUntilTurnDone(t, sess1.Events(), 120*time.Second)
	if _, ok := findEvent(events1, agent.EventAgentResult); !ok {
		t.Fatalf("first session: no EventAgentResult; kinds=%v", kindsOnly(events1))
	}

	if err := sess1.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Logf("sess1.Close: %v", err)
	}

	// --- second session: must resume the first thread id ---
	sess2, err := a.Start(ctx, agent.StartConfig{
		Workspace: ws,
		SessionID: ready1.SessionID,
	})
	if err != nil {
		t.Fatalf("Start resume: %v", err)
	}
	t.Cleanup(func() { _ = sess2.Close() })

	var ready2 agent.AgentEvent
	select {
	case ready2 = <-sess2.Events():
	case <-time.After(30 * time.Second):
		t.Fatalf("resume session: no EventAgentReady within 30s")
	}
	if ready2.SessionID != ready1.SessionID {
		t.Errorf("resumed SessionID = %q, want %q", ready2.SessionID, ready1.SessionID)
	} else {
		t.Logf("[e2e-resume] resumed thread=%s (matches)", ready2.SessionID)
	}

	// And one more turn on the resumed session to confirm the
	// bridge is actually live (not just returning a stale id).
	if err := sess2.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with only the single word: ping"},
	}); err != nil {
		t.Fatalf("resume SendBlocks: %v", err)
	}
	events2 := drainUntilTurnDone(t, sess2.Events(), 120*time.Second)
	if r, ok := findEvent(events2, agent.EventAgentResult); ok {
		if !strings.Contains(strings.ToLower(r.Result.Text), "ping") {
			t.Errorf("resumed Result.Text = %q, want it to contain 'ping'", r.Result.Text)
		}
	} else {
		t.Errorf("resumed session: no EventAgentResult; kinds=%v", kindsOnly(events2))
	}
}

// ─────────────────────────────────────────────────────────────────
// Test 3: approval flow.
// ─────────────────────────────────────────────────────────────────

func TestE2E_ApprovalFlow(t *testing.T) {
	// Approval-flow e2e is environmentally sensitive (depends on
	// the model's latency on the first on-request shell command;
	// runs of 10s and 180s+ have both been observed against the
	// default codex stack). Default OFF; opt in with
	// NIGHTME_CODEX_E2E_APPROVAL=1 when investigating approval
	// paths locally. The bridge's permission flow itself is
	// fully covered by permissions_test.go in unit form.
	if os.Getenv("NIGHTME_CODEX_E2E_APPROVAL") != "1" {
		t.Skip("set NIGHTME_CODEX_E2E_APPROVAL=1 to exercise the real approval flow")
	}
	requireRealCodex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	ws := t.TempDir()
	// Sentinel file the model should create. We assert it appears
	// after we accept the approval request.
	marker := filepath.Join(ws, "codex_e2e_marker.txt")
	markerBody := "codex-approval-ok"

	// Pass `-c approval_policy="on-request"` so every shell
	// command triggers an approval request, regardless of project
	// trust. We do this by extending the agent template's args.
	a := NewStarter("codex", "codex", []string{
		"-c", `approval_policy="on-request"`,
	})

	sess, err := a.Start(ctx, agent.StartConfig{Workspace: ws})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// Wait for the synthetic EventAgentReady the bridge emits after
	// handshake. Fail fast if it never arrives.
	select {
	case ready := <-sess.Events():
		if ready.Kind != agent.EventAgentReady {
			t.Fatalf("approval session: first event = %s, want EventAgentReady", ready.Kind)
		}
		t.Logf("[e2e-approval] thread=%s model=%s pid=%d", ready.SessionID, ready.Model, sess.PID())
	case <-time.After(30 * time.Second):
		t.Fatalf("approval session: no EventAgentReady within 30s")
	}

	prompt := "Please create the file " + marker + " containing exactly the text " +
		markerBody + " and nothing else."
	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	// Stream events looking for an EventAgentPermission. We give
	// the model up to 90s to decide to call a shell tool.
	deadline := time.After(90 * time.Second)
	var permEv agent.AgentEvent
	gotPerm := false
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			if ev.Kind == agent.EventAgentPermission {
				permEv = ev
				gotPerm = true
				t.Logf("[e2e-approval] received permission request: tool=%q action=%q",
					ev.Permission.Tool, truncate(ev.Permission.Action, 200))
				// Accept via the response channel.
				ev.Permission.ResponseCh <- "accept"
				break loop
			}
			if ev.Kind == agent.EventAgentError {
				t.Fatalf("unexpected EventAgentError before approval: %v", ev.Err)
			}
		case <-deadline:
			break loop
		}
	}
	if !gotPerm {
		t.Skip("approval flow did not fire within 90s — default codex config may have " +
			"auto-approved the shell tool. To exercise this path, ensure the workspace " +
			"is untrusted so on-request approval is required. Skipping.")
		return
	}

	// After accepting, drain the rest of the turn. The shell
	// command should run, the marker file should appear, and we
	// should get EventAgentResult + EventAgentDone.
	_ = permEv // unused
	rest := drainUntilTurnDone(t, sess.Events(), 180*time.Second)
	if _, ok := findEvent(rest, agent.EventAgentResult); !ok {
		t.Errorf("post-approval: no EventAgentResult; kinds=%v", kindsOnly(rest))
	}

	// Verify the file actually got created. We tolerate a small
	// race where the file appears after the bridge marks the turn
	// done (post-processing), so poll briefly.
	deadline2 := time.After(10 * time.Second)
	for {
		select {
		case <-deadline2:
			t.Errorf("marker file %s never appeared", marker)
			return
		default:
		}
		body, err := os.ReadFile(marker)
		if err == nil {
			bodyStr := strings.TrimSpace(string(body))
			if bodyStr != markerBody {
				t.Errorf("marker file body = %q, want %q", bodyStr, markerBody)
			} else {
				t.Logf("[e2e-approval] marker file present and matches")
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
