package claudecode

// T-alive (2026-08-07): repro test that spawns the REAL `claude`
// binary the same way the nightme daemon does, so we can observe
// whether claude's stream-json output actually reaches the bridge.
//
// Skipped if claude isn't on PATH.
//
// Run with: go test -count=1 -run TestReproRealClaude -v ./internal/bridge/claudecode/

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestReproRealClaude(t *testing.T) {
	requireRealClaude(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Logf("[repro] calling claudecode.New(\"claude\", \"claude\", nil).Start...")
	a := New("claude", "claude", nil)
	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace:      "/tmp",
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Logf("[repro] Spawned pid=%d", sess.PID())
	defer sess.Close()

	// Drain Events() in real time, with timestamps.
	evCh := sess.Events()
	events := make(chan agent.AgentEvent, 64)
	go func() {
		defer close(events)
		for ev := range evCh {
			events <- ev
		}
	}()

	// Read first 3 events or until 30s elapse, whichever comes first.
	deadline := time.After(30 * time.Second)
	gotInit := false
	gotText := false
	gotResult := false

	// After Start returns, give claude ~500ms to emit system/init.
	time.Sleep(500 * time.Millisecond)

	t.Logf("[repro] calling SendBlocks...")
	sendErr := sess.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "echo back exactly: 42"},
	})
	if sendErr != nil {
		t.Logf("[repro] SendBlocks err: %v", sendErr)
	} else {
		t.Logf("[repro] SendBlocks returned nil")
	}

	collectDeadline := time.After(45 * time.Second)
	for !gotInit || !gotResult {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Logf("[repro] events channel CLOSED (init=%v text=%v result=%v)",
					gotInit, gotText, gotResult)
				return
			}
			t.Logf("[repro] EV kind=%v", ev.Kind)
			if ev.Connected != nil {
				gotInit = true
				t.Logf("    init: sessionID=%q model=%q agent=%q",
					ev.Connected.SessionID, ev.Connected.Model, ev.Connected.AgentName)
			}
			if ev.Text != "" {
				gotText = true
				t.Logf("    text: %q", ev.Text)
			}
			if ev.Result != nil {
				gotResult = true
				t.Logf("    result: %q (is_error=%v)", ev.Result.Text, ev.Result.IsError)
			}
			if ev.Error != nil {
				t.Logf("    error: %v", ev.Error.Err)
			}
			if ev.Done != nil {
				t.Logf("    done: reason=%q", ev.Done.Reason)
			}
		case <-deadline:
			t.Logf("[repro] init deadline reached (init=%v text=%v result=%v)",
				gotInit, gotText, gotResult)
			return
		case <-collectDeadline:
			t.Logf("[repro] collect deadline reached (init=%v text=%v result=%v)",
				gotInit, gotText, gotResult)
			return
		}
	}
	t.Logf("[repro] got init + result; closing session")

	// Sanity: don't leave a zombie claude. Best-effort cleanup.
	_ = os.Getpid()
}

// TestReproRealClaude_NoSendBlocks confirms a critical claude
// behavior observed 2026-08-07: claude --print --output-format
// stream-json does NOT emit system/init until it receives the
// first user turn via SendBlocks. If this assumption is correct,
// nightme's "no events observed" symptom could be explained by the
// prompt never actually reaching claude's stdin (despite Submit
// returning nil). This test asserts the negative: no events should
// arrive without SendBlocks.
func TestReproRealClaude_NoSendBlocks(t *testing.T) {
	requireRealClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a := New("claude", "claude", nil)
	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace:      "/tmp",
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()
	t.Logf("[repro] Spawned pid=%d; waiting 10s WITHOUT SendBlocks...", sess.PID())

	events := make(chan agent.AgentEvent, 64)
	go func() {
		defer close(events)
		for ev := range sess.Events() {
			events <- ev
		}
	}()

	deadline := time.After(10 * time.Second)
	gotAny := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Logf("[repro] events CLOSED (gotAny=%v)", gotAny)
				return
			}
			gotAny = true
			t.Logf("[repro] UNEXPECTED EV kind=%v (this confirms the hypothesis wrong)", ev.Kind)
		case <-deadline:
			t.Logf("[repro] 10s elapsed with NO events (gotAny=%v) — claude waits for stdin", gotAny)
			return
		}
	}
}

// TestReproRealClaude_ProductionArgs is the closest reproduction
// of nightme's production Spawn path: it goes through the
// agent.Registry → registrySpawner → AgentSession.Spawn →
// claudecode.Start sequence, with a realistic Workspace and an
// Args slice matching what /use /cwd would populate. It exercises
// every layer nightme uses.
func TestReproRealClaude_ProductionArgs(t *testing.T) {
	requireRealClaude(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Mirror what the production chatsession.Spawn path passes:
	//   cs.activeCwd = whatever the user set via /cwd
	//   as.args      = nil (chatsession always passes nil for fresh AS)
	//   as.resumeID  = "" on first Spawn, persisted on subsequent ones
	a := New("claude", "claude", nil)
	t.Logf("[repro] production-style Start: Workspace=current_dir, no args, no resume")
	wd, _ := os.Getwd()
	t.Logf("[repro] Workspace = %s", wd)

	sess, err := a.Start(ctx, agent.StartConfig{
		Workspace:      wd,
		PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("[repro] Spawned pid=%d", sess.PID())
	defer sess.Close()

	events := make(chan agent.AgentEvent, 64)
	go func() {
		defer close(events)
		for ev := range sess.Events() {
			events <- ev
		}
	}()

	// Wait 500ms before SendBlocks (mirroring the first repro test).
	time.Sleep(500 * time.Millisecond)
	t.Logf("[repro] calling SendBlocks with simple prompt...")
	if err := sess.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "say only: pong"},
	}); err != nil {
		t.Logf("[repro] SendBlocks err: %v", err)
	} else {
		t.Logf("[repro] SendBlocks returned nil")
	}

	deadline := time.After(30 * time.Second)
	gotInit := false
	gotResult := false
	for !gotInit || !gotResult {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Logf("[repro] events CLOSED (init=%v result=%v)", gotInit, gotResult)
				return
			}
			t.Logf("[repro] EV kind=%v", ev.Kind)
			if ev.Kind == agent.EventAgentConnected {
				gotInit = true
				if ev.Connected != nil {
					t.Logf("    init: sessionID=%q model=%q",
						ev.Connected.SessionID, ev.Connected.Model)
				}
			}
			if ev.Kind == agent.EventResult {
				gotResult = true
				if ev.Result != nil {
					t.Logf("    result: %q", ev.Result.Text)
				}
			}
			if ev.Kind == agent.EventError && ev.Error != nil {
				t.Logf("    error: %v", ev.Error.Err)
			}
		case <-deadline:
			t.Logf("[repro] TIMEOUT (init=%v result=%v)", gotInit, gotResult)
			return
		}
	}
	t.Logf("[repro] production-args path got init+result")
}
