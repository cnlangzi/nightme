// Repro: the user-reported scenario
//
//   1) start pi RPC bridge, send a prompt (pi responds)
//   2) simulate "switch to claude" — pi process keeps running,
//      bridge handle untouched
//   3) call bridge.New() (this is what /new does to every
//      running AgentSession — see chatsession.go NewActiveAgentSessions)
//   4) send another prompt → does pi still respond?
//
// Run:
//   go test ./internal/bridge/pi -run TestReproNewAfterSwitch -v
//
// SKIPPED when `pi` is not on PATH.

package pi

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestReproNewAfterSwitch(t *testing.T) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		t.Skipf("pi binary not on PATH: %v", err)
	}
	t.Logf("using pi binary at %s", bin)

	workspace := t.TempDir()
	a := NewStarter("pi", bin, []string{
		"--provider", "anthropic",
		"--model", "claude-sonnet",
		"--no-session",
	})
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), handshakeWindow)
	defer startCancel()
	sess, err := a.Start(startCtx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	// Boot handshake — first EventAgentReady
	boot := mustFirstEventOfKind(t, sess, agent.EventAgentReady, handshakeWindow)
	if boot.SessionID == "" {
		t.Fatal("boot SessionID empty")
	}
	t.Logf("step 0: handshake ok session=%q model=%q", boot.SessionID, boot.Model)

	// STEP 1: first prompt
	marker1 := fmt.Sprintf("REPRO-T1-%d", time.Now().UnixNano())
	if err := driveTurn(t, sess, "Reply with the single token: "+marker1, promptDeadline, "turn-1"); err != nil {
		t.Fatal(err)
	}

	// STEP 2: simulate "switched to claude" — bridge stays alive,
	// pi process stays alive, nothing else happens for a moment.
	time.Sleep(500 * time.Millisecond)

	// STEP 3: bridge.New() — this is what /new does to the
	// running AgentSession (per internal/chatsession/chatsession.go
	// NewActiveAgentSessions). It should send new_session + get_state
	// and emit a fresh EventAgentReady carrying the new SessionID.
	if err := sess.New(context.Background()); err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	t.Log("step 3: bridge.New() returned nil")

	// STEP 3b: did we get a FRESH EventAgentReady?
	// If not, the runtime will keep the OLD SessionID — the runtime
	// will not know the conversation was reset, and downstream
	// --resume on daemon restart will replay the dead session.
	init2 := mustFirstEventOfKind(t, sess, agent.EventAgentReady, 10*time.Second)
	if init2.SessionID == "" {
		t.Fatal("step 3b: post-reset EventAgentReady has empty SessionID")
	}
	if init2.SessionID == boot.SessionID {
		t.Fatalf("step 3b: post-reset SessionID == boot SessionID (%q) — bridge did not actually reset", init2.SessionID)
	}
	t.Logf("step 3b: fresh EventAgentReady session=%q (was %q)", init2.SessionID, boot.SessionID)

	// STEP 4: another prompt — does pi still respond?
	marker2 := fmt.Sprintf("REPRO-T2-%d", time.Now().UnixNano())
	if err := driveTurn(t, sess, "Reply with the single token: "+marker2, promptDeadline, "turn-2-after-new"); err != nil {
		t.Fatal(err)
	}
	t.Log("step 4: pi responded to a fresh prompt after /new — bridge path is OK")
}