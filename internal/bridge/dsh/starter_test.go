package dsh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── TestStarter_Info_NoArgs ─────────────────────────────────────
// Starter.Info().Args must be nil — Starter no longer spawns a
// subprocess directly, the shared-host web does. nil prevents the
// misleading impression that Starter is the spawner.
func TestStarter_Info_NoArgs(t *testing.T) {
	s := NewStarter("dsh")
	info := s.Info()
	if info.Name != "dsh" {
		t.Errorf("Info.Name = %q, want dsh", info.Name)
	}
	if info.Mode != agent.ModeJSONIO {
		t.Errorf("Info.Mode = %v, want ModeJSONIO", info.Mode)
	}
	if len(info.Args) != 0 {
		t.Errorf("Info.Args = %v, want nil (Starter no longer spawns)", info.Args)
	}
}

// ─── TestDrainForRunResult_EventAgentResult ──────────────────────
// drainForRunResult must exit cleanly with the captured RunResult
// when EventAgentResult is delivered.
func TestDrainForRunResult_EventAgentResult(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// Drain startup EventAgentReady so it doesn't clog the chan.
	if !drainOneReady(t, a, 2*time.Second) {
		t.Fatal("timed out draining startup EventAgentReady")
	}

	d, ok := a.Driver().(*driver)
	if !ok {
		t.Fatalf("a.Driver() = %T, want *driver", a.Driver())
	}

	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "drain ok",
			Subtype: "success",
		},
	})

	res, err := drainForRunResult(ctx, a, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "hi",
	}}, nil, 0) // skipPriming=0: mock doesn't synthesize a priming turn
	if err != nil {
		t.Fatalf("drainForRunResult: %v", err)
	}
	if res.Text != "drain ok" {
		t.Errorf("RunResult.Text = %q, want %q", res.Text, "drain ok")
	}
	if !strings.HasPrefix(res.SessionID, "session-fresh-") {
		t.Errorf("RunResult.SessionID = %q, want session-fresh-*", res.SessionID)
	}
}

// ─── TestDrainForRunResult_DoneWithoutResult ──────────────────────
// EventAgentDone without a preceding EventAgentResult must error
// (the turn ended without producing assistant output).
func TestDrainForRunResult_DoneWithoutResult(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)
	s := NewStarter("dsh")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if !drainOneReady(t, a, 2*time.Second) {
		t.Fatal("timed out draining startup EventAgentReady")
	}
	d, _ := a.Driver().(*driver)

	d.deliver(agent.AgentEvent{Kind: agent.EventAgentDone})

	_, err = drainForRunResult(ctx, a, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "hi",
	}}, nil, 0) // skipPriming=0: mock doesn't synthesize a priming turn
	if err == nil {
		t.Fatal("want error when EventAgentDone fires without Result")
	}
	if !strings.Contains(err.Error(), "turn ended without result event") {
		t.Errorf("err = %v; want 'turn ended without result event'", err)
	}
}

// ─── TestDrainForRunResult_ErrorEvent ─────────────────────────────
// EventAgentError must surface as a wrapped RunOnce error.
func TestDrainForRunResult_ErrorEvent(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)
	s := NewStarter("dsh")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if !drainOneReady(t, a, 2*time.Second) {
		t.Fatal("timed out draining startup EventAgentReady")
	}
	d, _ := a.Driver().(*driver)

	wantErr := errors.New("upstream blew up")
	d.deliver(agent.AgentEvent{Kind: agent.EventAgentError, Err: wantErr})

	_, err = drainForRunResult(ctx, a, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "hi",
	}}, nil, 0) // skipPriming=0: mock doesn't synthesize a priming turn
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrap of %v", err, wantErr)
	}
}

// ─── TestRunOnce_StripsSessionID ───────────────────────────────────
// cfg.SessionID must be ignored on RunOnce (Q1 decision: RunOnce
// is always a fresh session). We verify by checking the
// handshakeMock's createCount and the captured sessionId via
// lastCreate payload — a fork attempt would have populated the
// sessionId field in the payload.
func TestRunOnce_StripsSessionID(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	s := NewStarter("dsh")

	// Override RunOnce to use a very short ctx so the drain exits
	// via deadline (we only care about the side effects, not the
	// drain's terminal event). The archive still happens in
	// defer Close.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, _ = s.RunOnce(ctx, agent.StartConfig{
		Workspace: "/tmp/ws",
		SessionID: "session-stale-from-caller",
	}, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "should ignore",
	}})
	// We expect RunOnce to fail with context-deadline. That's fine
	// — we only care about whether session.fork was attempted.

	// mock.handleSessionCreate's logic: if payload["sessionId"]
	// was non-empty, the mock echoes that id (fork attach); if
	// empty, the mock returns session-fresh-N. We seeded
	// cfg.SessionID = "session-stale-from-caller", so if RunOnce
	// did NOT strip it, the lastCreate payload would carry it
	// and mock.createIDs[0] would be that id. After stripping,
	// the lastCreate payload has no sessionId field, and the mock
	// returns a fresh id.
	if got := mock.createCount.Load(); got == 0 {
		t.Fatal("session.create never fired; RunOnce did not reach Start")
	}
	// Look up the most recent id the mock returned. If
	// RunOnce hadn't stripped cfg.SessionID, this would be
	// "session-stale-from-caller" (fork attach semantics).
	ids := mock.createdIDs()
	if len(ids) == 0 {
		t.Fatal("no created session ids recorded")
	}
	last := ids[len(ids)-1]
	if last == "session-stale-from-caller" {
		t.Fatalf("RunOnce forked cfg.SessionID %q instead of creating fresh", last)
	}
	if !strings.HasPrefix(last, "session-fresh-") {
		t.Fatalf("RunOnce returned id %q; want session-fresh-*", last)
	}
}

// NOTE: TestRunOnce_IsolatedSessions and TestReview_UsesbuiltinPrompt
// were dropped because the mock's session.create handler doesn't
// reset state cleanly between consecutive RunOnce calls (the
// first RunOnce's Close races with the second's session.create).
// Both invariants are exercised end-to-end against the real dsh
// daemon in runonce_real_unix_test.go (TestE2E_RunOnce_RealDSH
// and TestE2E_Review_RealDSH).

// ─── TestRunOnce_DeleteOnClose ─────────────────────────────────────
// The defer a.Close() must drive workspace.delete (R4 — keep dsh
// web's in-memory store clean). Each RunOnce / Review owns its own
// workspace (created in createFreshSession); archiveSession left
// empty workspaces behind. Close now fully tears down the
// driver-owned workspace.
func TestRunOnce_DeleteOnClose(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	s := NewStarter("dsh")
	// 200ms ctx is enough for the handshake + SendBlocks + delete
	// path; drain blocks on events (no terminal delivered in this
	// test) so we exit via deadline. The 5s default is wasted.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Use a goroutine to break out of RunOnce after the delete
	// fires. RunOnce's drain will block on events; we use a
	// short ctx so it exits via deadline, and the delete still
	// happens in defer Close.
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_, _ = s.RunOnce(ctx, agent.StartConfig{Workspace: "/tmp/ws"},
			[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}})
	}()
	<-ctx.Done()
	<-doneCh

	// Poll for delete completion (cheap, max ~500ms).
	deadline := time.Now().Add(500 * time.Millisecond)
	for mock.deleteCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := mock.deleteCount.Load(); got == 0 {
		t.Fatalf("workspace.delete never fired; defer Close did not tear down driver-owned workspace")
	}
}

// TestRunOnce_DedupWorkspace_SkipsDelete locks the critical bug
// fix from /review on this branch: workspace.create is idempotent
// by path (dsh-api.md §2.4.2), so two drivers in the same cwd
// get the SAME workspaceID. createFreshSession must only stamp
// d.workspaceID when the response says created=true; on a dedup
// hit (created=false) the workspace is shared with other drivers
// or dashboard sessions, and Close must NOT tear it down.
//
// Locks /review finding "WorkspaceID is set even when the
// workspace already existed (dedup path) — silent multi-session
// breakage".
func TestRunOnce_DedupWorkspace_SkipsDelete(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.dedupWorkspace.Store(true) // workspace.create returns created:false
	mock.installGlobal(t)

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_, _ = s.RunOnce(ctx, agent.StartConfig{Workspace: "/tmp/shared"},
			[]agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}})
	}()
	<-ctx.Done()
	<-doneCh

	// WorkspaceDelete MUST NOT fire — the workspace was not ours
	// to delete. Wait for the cancel-timeout path to settle so
	// any spurious delete would have a chance to fire.
	time.Sleep(100 * time.Millisecond)
	if got := mock.deleteCount.Load(); got != 0 {
		t.Fatalf("workspace.delete fired %d times on dedup-hit path; want 0 (shared workspace must not be torn down)", got)
	}
	// session.cancel still fires (we own the sessionId).
	if mock.cancelCount.Load() < 1 {
		t.Fatal("session.cancel did not fire on dedup-hit close")
	}
}

// TestClose_AttachedSession_SkipsWorkspaceDelete locks the
// attachSession path: d.workspaceID stays "" when the driver
// attaches to an existing sessionId (no workspace allocation),
// so Close must skip workspace.delete (would damage the
// pre-existing workspace that hosts the session).
func TestClose_AttachedSession_SkipsWorkspaceDelete(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		SessionID: "session-pre-existing",
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver (attach path): %v", err)
	}
	if d.workspaceID != "" {
		t.Fatalf("attached driver has workspaceID=%q; want \"\" (attach path doesn't allocate)", d.workspaceID)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := mock.deleteCount.Load(); got != 0 {
		t.Fatalf("workspace.delete fired %d times on attach-session close; want 0", got)
	}
	if mock.cancelCount.Load() < 1 {
		t.Fatal("session.cancel did not fire on attach-session close")
	}
}

// ─── helpers ────────────────────────────────────────────────────────

// drainOneReady consumes one AgentEventReady from a.Events() within
// the timeout. Used to clear the startup EventAgentReady before
// injecting terminal events.
func drainOneReady(t *testing.T, a *agent.Agent, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				return false
			}
			if ev.Kind == agent.EventAgentReady {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
// ─── TestDrainForRunResult_SinkReceivesEvents ───────────────────
// When a sink is supplied, drainForRunResult must invoke it
// synchronously for every event read off a.Events(). The sink
// receives Ready, Text, ToolStart, ToolEnd, Result, Done — every
// Kind. The caller is responsible for non-blocking semantics
// (see outbound.StreamRunOnceToEmitter for the canonical pattern);
// drain just calls it.
func TestDrainForRunResult_SinkReceivesEvents(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)
	s := NewStarter("dsh")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if !drainOneReady(t, a, 2*time.Second) {
		t.Fatal("timed out draining startup EventAgentReady")
	}
	d, _ := a.Driver().(*driver)

	// Push a mix of event kinds — Ready is already in the chan
	// drain but the sink attached AFTER drainOneReady cleared it,
	// so the sink will see only the events we push below.
	var got []agent.EventKind
	var mu sync.Mutex
	sink := func(ev agent.AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev.Kind)
	}

	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "thinking about it...",
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{
			ID: "tool-1", Name: "Bash", Args: `{"cmd":"ls"}`,
		},
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{
			ID: "tool-1", Name: "Bash", Output: "file1\nfile2\n",
		},
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "all done",
			Subtype: "success",
		},
	})

	res, err := drainForRunResult(ctx, a, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "hi",
	}}, sink, 0) // skipPriming=0: mock doesn't synthesize a priming turn
	if err != nil {
		t.Fatalf("drainForRunResult: %v", err)
	}
	if res.Text != "all done" {
		t.Errorf("RunResult.Text = %q, want %q", res.Text, "all done")
	}

	// Snapshot got under lock; the sink was called from drain's
	// goroutine and we read after drain returned.
	mu.Lock()
	defer mu.Unlock()
	want := []agent.EventKind{
		agent.EventAgentText,
		agent.EventAgentToolStart,
		agent.EventAgentToolEnd,
		agent.EventAgentResult,
	}
	if len(got) != len(want) {
		t.Fatalf("sink got %d events, want %d: %v", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("sink event[%d] = %s, want %s", i, got[i], k)
		}
	}
}

// ─── TestDrainForRunResult_NilSinkSafe ───────────────────────────
// A nil sink must not panic. drain is called by RunOnce / Review
// without WithEventSink — the sink param is just nil.
func TestDrainForRunResult_NilSinkSafe(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)
	s := NewStarter("dsh")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if !drainOneReady(t, a, 2*time.Second) {
		t.Fatal("timed out draining startup EventAgentReady")
	}
	d, _ := a.Driver().(*driver)

	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "no sink, that's fine",
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{Text: "ok"},
	})

	res, err := drainForRunResult(ctx, a, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "hi",
	}}, nil, 0) // skipPriming=0: mock doesn't synthesize a priming turn
	if err != nil {
		t.Fatalf("drainForRunResult with nil sink: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("RunResult.Text = %q, want %q", res.Text, "ok")
	}
}

// TestDrainForRunResult_SkipsPrimingTurn: when skipPriming=2 is
// passed (the RunOnce path), drain swallows the first terminal pair
// (Result + Done) and reads the SECOND Result as the actual prompt
// output. Locks the dsh-r--fix contract in mock-land so a future
// refactor doesn't regress to "priming turn's text leaks into
// RunResult.Text".
func TestDrainForRunResult_SkipsPrimingTurn(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)
	s := NewStarter("dsh")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if !drainOneReady(t, a, 2*time.Second) {
		t.Fatal("timed out draining startup EventAgentReady")
	}
	d, _ := a.Driver().(*driver)

	// Synthesize a priming turn: text + result + done.
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "priming acknowledgment — should be skipped",
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "priming result — should be skipped",
			Subtype: "completed",
		},
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done:   &agent.AgentDoneEvent{Reason: "settled"},
	})

	// Then the real turn: text + result + done.
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "real review text — should appear in RunResult.Text",
	})
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "real result — the actual output",
			Subtype: "completed",
		},
	})

	var seen []string
	var mu sync.Mutex
	sink := func(ev agent.AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Kind == agent.EventAgentResult {
			seen = append(seen, ev.Result.Text)
		}
	}

	res, err := drainForRunResult(ctx, a, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "review prompt",
	}}, sink, 2)
	if err != nil {
		t.Fatalf("drainForRunResult: %v", err)
	}
	if res.Text != "real result — the actual output" {
		t.Errorf("RunResult.Text = %q; want priming-skipped real output", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("sink saw %d Result events, want 2 (priming + real): %v", len(seen), seen)
	}
	if seen[0] != "priming result — should be skipped" {
		t.Errorf("sink Result[0] = %q; want priming text", seen[0])
	}
	if seen[1] != "real result — the actual output" {
		t.Errorf("sink Result[1] = %q; want real text", seen[1])
	}
}
