//go:build !windows

// Real-binary smoke for the pi bridge.
//
// File: session_real_test.go
//
// Counterpart to session_test.go (which drives
// internal/testdata/pi_mock.sh). The mock is fast and
// deterministic, but it cannot reproduce the production failure
// mode that motivated this file: a real `pi` binary that starts,
// completes the get_state handshake (so Spawn returns), accepts
// the prompt (so SendBlocks returns nil and ChatSession flips
// MessageState to Forwarded), and then NEVER surfaces an
// agent_settled event — the F-32 2026-08-06 incident, where
// feishu showed nothing after /use pi + a plain "hi".
//
// Tests in this file SKIP when the `pi` binary is not on PATH so
// the default `go test ./internal/bridge/pi` run is unaffected
// on dev machines / CI runners without the dependency.
//
// Run explicitly:
//
//	go test ./internal/bridge/pi -run Real -v
//
// Out of scope here (covered by the mock-driven session_test.go):
//
//   - handshake timeout — TestSession_HandshakeTimeout
//   - tool lifecycle, extension_ui auto-cancel, /new round-trip
//   - prompt-deadline enforcement — that needs the session.go fix
//     and is unit-tested via the mock.

package pi

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// promptDeadline bounds the wait for EventAgentDone after SendBlocks.
// Sized for a real model response on a healthy dev box; a hung
// pi (the failure mode we guard against) blows past this and the
// test fails with an elapsed-time message instead of stalling
// the runner.
//
// handshakeWindow bounds the wait for EventAgentReady; the bridge's
// own handshakeTimeout is 10s but real-pi cold start (model
// warm-up, plugin load) routinely takes longer.
const (
	handshakeWindow  = 30 * time.Second
	promptDeadline   = 60 * time.Second
	closeGraceWindow = 8 * time.Second
)

// TestSession_RealPi_E2E_ReceiveInputAndReply is the
// end-to-end proof that the bridge drives a real `pi` process
// all the way through:
//
//	input  → bridge.SendBlocks   → pi reads it   →
//	                                    ↓
//	                            pi emits text + agent_settled  →
//	                                    ↓
//	output ← bridge.Events()  ← translator    ←
//
// What it asserts (each step corresponds to one assertion in
// the test body so a failure tells you exactly which leg of
// the e2e chain broke):
//
//  1. Start + get_state handshake complete and emit exactly one
//     EventAgentReady carrying a non-empty SessionID — so the runtime
//     can persist the resume id (input pipeline reaches pi).
//  2. SendBlocks(prompt-with-marker) returns nil within promptDeadline
//     and the stream terminates in EventAgentDone with Reason:"settled".
//  3. The aggregated reply text contains the unique marker we
//     embedded in our prompt — this is the *positive* proof that
//     pi received our specific input (not just "got any reply"):
//     the LLM had to read our marker to echo it back.
//  4. A second turn with a different marker succeeds and the
//     reply contains THAT marker — proves the session stays
//     alive across turns and state is preserved (input still
//     reaches the same pi process).
//  5. The events channel is still open after both turns —
//     long-lived session contract (closing is reserved for
//     process exit / Close).
//  6. Close returns within closeGraceWindow so the bridge's
//     teardown watchdog has not regressed (F-32 follow-up).
//
// Both replies are printed via t.Logf so a developer running
// this test sees the bridge talking to the real binary end-to-end:
// the goal is "I get a human-readable answer", not just "the
// stream terminated".
func TestSession_RealPi_E2E_ReceiveInputAndReply(t *testing.T) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		t.Skipf("pi binary not on PATH; skipping real-binary e2e: %v", err)
	}
	t.Logf("using pi binary at %s", bin)

	workspace := t.TempDir()
	a := NewStarter("pi", bin, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v (binary at %q)", err, bin)
	}

	// Start bounds — handshakeWindow covers real-pi cold start.
	startCtx, startCancel := context.WithTimeout(context.Background(), handshakeWindow)
	defer startCancel()

	startedAt := time.Now()
	sess, err := a.Start(startCtx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v (elapsed=%s)", err, time.Since(startedAt))
	}
	t.Logf("Start ok in %s, pid=%d", time.Since(startedAt), sess.PID())

	// Bound Close separately — it has its own watchdog path that
	// is the subject of a different test (F-32 close watchdog).
	defer func() {
		closeDone := make(chan error, 1)
		go func() { closeDone <- sess.Close() }()
		select {
		case cerr := <-closeDone:
			if cerr != nil {
				t.Logf("Close returned: %v (informational)", cerr)
			}
		case <-time.After(closeGraceWindow):
			t.Errorf("Close did not return within %s; bridge teardown watchdog may have regressed", closeGraceWindow)
		}
	}()

	// Step 1: handshake → EventAgentReady.
	init := mustFirstEventOfKind(t, sess, agent.EventAgentReady, handshakeWindow)
	if init.SessionID == "" {
		t.Errorf("Init.SessionID is empty; runtime would fail to persist resume id (events seen so far are %v)",
			init.Kind)
	}
	t.Logf("handshake ok: session=%q model=%q", init.SessionID, init.Model)

	// Step 2 + 3: first turn — send a marker-bearing prompt and
	// verify pi echoes the marker back. This is the strongest
	// "pi received our input" assertion we can make without
	// instrumenting pi itself: the LLM only produces the marker
	// string if it actually saw it on its stdin.
	marker1 := "MIBLRE-CANARY-" + t.Name() + "-T1"
	prompt1 := fmt.Sprintf("hi — please repeat this id back to me verbatim: %s", marker1)
	if err := driveTurn(t, sess, prompt1, promptDeadline, "turn-1"); err != nil {
		// driveTurn already logged a failure with elapsed time; nothing to add.
		return
	}

	// Step 4: second turn with a different marker. Confirms the
	// session is long-lived (channel not closed, pi process
	// alive) and that a fresh input still reaches the same pi
	// process and gets a fresh, on-topic reply.
	marker2 := "MIBLRE-CANARY-" + t.Name() + "-T2"
	prompt2 := fmt.Sprintf("thanks — and please repeat this second id back to me verbatim: %s", marker2)
	if err := driveTurn(t, sess, prompt2, promptDeadline, "turn-2"); err != nil {
		return
	}

	// Step 5: events channel still open — long-lived contract.
	select {
	case _, ok := <-sess.Events():
		if !ok {
			t.Fatal("events channel closed after two turns; long-lived session contract violated")
		}
	default:
		// No buffered event ready; channel not closed. Expected.
	}
}

// driveTurn sends prompt via SendBlocks, waits up to deadline for
// EventAgentDone with Reason:"settled", and asserts:
//
//   - SendBlocks did not error (transport write succeeded);
//   - the stream terminated in EventAgentDone with the right reason;
//   - at least one EventAgentText or EventAgentResult was emitted;
//   - the aggregated reply text contains marker — i.e. the LLM
//     read our specific input (proves the input reached pi).
//
// The marker check is the assertion that makes this an e2e test
// rather than just "did anything come back". The reply text is
// logged in full (truncated at 240 chars for readability) so the
// developer sees the actual pi response.
//
// turnLabel is only used in t.Logf to disambiguate turn-1 vs
// turn-2 in the test output.
func driveTurn(t *testing.T, sess *agent.Agent, prompt string, deadline time.Duration, turnLabel string) error {
	t.Helper()
	promptStartedAt := time.Now()
	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	}); err != nil {
		t.Errorf("%s: SendBlocks returned %v (elapsed=%s)", turnLabel, err, time.Since(promptStartedAt))
		return err
	}
	turn := drainEventsUntilDone(t, sess, deadline)
	if turn.Done == nil {
		t.Fatalf("%s: no EventAgentDone within %s; events seen: %v (elapsed=%s) — F-32 2026-08-06 production hang",
			turnLabel, deadline, turn.Kinds, time.Since(promptStartedAt))
		return nil
	}
	if turn.Done.Reason != "settled" {
		t.Errorf("%s: Done.Reason = %q, want \"settled\" (events=%v)", turnLabel, turn.Done.Reason, turn.Kinds)
	}

	// At least one text-bearing event in the turn.
	if !slices.Contains(turn.Kinds, agent.EventAgentText) &&
		!slices.Contains(turn.Kinds, agent.EventAgentResult) {
		t.Errorf("%s: no EventAgentText / EventAgentResult in turn; events=%v", turnLabel, turn.Kinds)
	}

	// Aggregate reply text and assert non-empty + marker presence.
	reply := aggregateReplyText(turn.Events)
	if reply == "" {
		t.Errorf("%s: no text reply at all; events=%v (prompt=%q)", turnLabel, turn.Kinds, prompt)
		return nil
	}

	// Extract the MIBLRE-CANARY-* marker from the prompt and
	// require it in the reply. This is the input-receipt proof:
	// the LLM only produces this exact string if it actually read
	// our stdin.
	marker := extractMarker(prompt)
	if marker == "" {
		t.Logf("%s: no CANARY marker in prompt (skipping input-receipt check): %q", turnLabel, prompt)
	} else if !strings.Contains(reply, marker) {
		t.Errorf("%s: pi did NOT receive our input — reply lacks marker %q. reply=%q events=%v",
			turnLabel, marker, reply, turn.Kinds)
	}

	preview := reply
	if len(preview) > 240 {
		preview = preview[:240] + "...(+" + itoa(len(reply)-240) + " chars)"
	}
	t.Logf("%s: pi received our input and replied (%d chars) in %s: %q",
		turnLabel, len(reply), time.Since(promptStartedAt), preview)
	return nil
}

// extractMarker pulls the MIBLRE-CANARY-* token out of a prompt.
// Returns "" if the prompt has no marker (caller can fall back
// to a generic "did anything come back" check).
func extractMarker(prompt string) string {
	const prefix = "MIBLRE-CANARY-"
	i := strings.Index(prompt, prefix)
	if i < 0 {
		return ""
	}
	// Marker ends at the next whitespace or end-of-string.
	rest := prompt[i+len(prefix):]
	for j, r := range rest {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return prompt[i : i+len(prefix)+j]
		}
	}
	return prompt[i:]
}

// aggregateReplyText returns the assistant's reply as a single
// string, concatenating:
//   - the streaming EventAgentText chunks (real pi emits one or more),
//   - the final EventAgentResult.Result text, if present.
//
// Order is preserved as events arrived on the channel.
func aggregateReplyText(events []agent.AgentEvent) string {
	var sb strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentText:
			sb.WriteString(ev.Text)
		case agent.EventAgentResult:
			if ev.Result != nil {
				sb.WriteString(ev.Result.Text)
			}
		}
	}
	return sb.String()
}

// itoa avoids importing strconv just for this one truncation
// marker in the test log.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}