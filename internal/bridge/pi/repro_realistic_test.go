// Repro for the user-reported scenario, more realistic flavor:
// 1) start pi with default args (no --no-session) → pi creates a real session
// 2) send a prompt and capture the SessionID
// 3) simulate "switch to claude" — long idle, bridge handle untouched
// 4) reopen the same bridge config but with --session-id <captured> to
//    simulate the runtime's "resume" path on next agent spawn (the
//    user actually has only one bridge alive, but the session id is
//    preserved across the bridge's lifetime — so the running pi
//    process is still bound to the original session).
//    -> skip this step; the live bridge is what /new targets.
//
// The point of this test is to assert: after a long idle, does
// bridge.New() on the LIVE bridge still round-trip new_session?
//
// Run: go test ./internal/bridge/pi -run TestReproNewAfterSwitch_Realistic -v

package pi

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestReproNewAfterSwitch_Realistic(t *testing.T) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		t.Skipf("pi binary not on PATH: %v", err)
	}
	t.Logf("using pi binary at %s", bin)

	workspace := t.TempDir()
	// Mirror the runtime's spawn for pi exactly: no extra args, no
	// --no-session. Pi decides whether to persist sessions.
	a := NewStarter("pi", bin, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sess, err := a.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	boot := mustFirstEventOfKind(t, sess, agent.EventAgentReady, 30*time.Second)
	sessionID := boot.SessionID
	if sessionID == "" {
		t.Fatal("boot SessionID empty")
	}
	t.Logf("step 0: handshake ok session=%q model=%q", sessionID, boot.Model)

	// 1) first turn like the user did
	if err := driveTurn(t, sess, "Reply with the single token: HELLO-1", 60*time.Second, "turn-1"); err != nil {
		t.Fatal(err)
	}

	// 2) simulate "switch to claude" — long idle, NOTHING touching
	//    the bridge
	idle := 5 * time.Second
	t.Logf("step 2: idle for %s (simulate switch to claude)", idle)
	time.Sleep(idle)

	// 3) bridge.New() — what /new does on the running AgentSession
	startNew := time.Now()
	if err := sess.New(context.Background()); err != nil {
		t.Fatalf("step 3: bridge.New failed in %s: %v", time.Since(startNew), err)
	}
	t.Logf("step 3: bridge.New() returned in %s", time.Since(startNew))

	// 3b) post-reset EventAgentReady with FRESH sessionId
	init2 := mustFirstEventOfKind(t, sess, agent.EventAgentReady, 10*time.Second)
	if init2.SessionID == "" {
		t.Fatal("step 3b: post-reset SessionID empty")
	}
	if init2.SessionID == sessionID {
		t.Fatalf("step 3b: post-reset SessionID == boot SessionID (%q) — bridge did not actually reset", sessionID)
	}
	t.Logf("step 3b: fresh EventAgentReady session=%q (was %q)", init2.SessionID, sessionID)

	// 4) another prompt — does the bridge still drive pi?
	if err := driveTurn(t, sess, "Reply with the single token: HELLO-2", 60*time.Second, "turn-2-after-new"); err != nil {
		t.Fatal(err)
	}
	t.Log("step 4: pi responded to a fresh prompt after /new — bridge path is OK")
}
