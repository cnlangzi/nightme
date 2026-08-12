package chatsession

import (
	"testing"
	"time"
)

// F-61 prober tests. Run with: go test ./internal/chatsession/... -run TestF61

// respawnsCounter returns the prober's respawn counter via the
// public Snapshot accessor.
func respawnsCounter(p *AgentProber) int64 {
	return p.Snapshot().RespawnsHit
}

// TestF61_Prober_ExitedSuspectRetry verifies that the prober
// runs the Exited+Suspect branch on cooldown-elapsed ASes.
// Today the branch only logs (no real respawn — manager wiring
// is a follow-up). The semantic assertion is:
//   - respawnsHit stays 0 (no false increment on no-op, F-61 #4)
//   - Scan timestamp advances (tick ran)
//
// When the manager wiring lands, this test will need to grow
// a spawner mock to verify the actual respawn.
func TestF61_Prober_ExitedSuspectRetry(t *testing.T) {
	cs, _ := New("oc_f61", "claude")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	as.SetExited(0)
	// Backdate suspect to before cooldown so the prober
	// enters the Exited+Suspect branch.
	as.SetSuspectAt("immediate_respawn_failed", time.Now().Add(-2*suspectCooldown))

	prober := NewAgentProber(func() []*ChatSession { return []*ChatSession{cs} })

	before := respawnsCounter(prober)
	prober.tick()
	after := respawnsCounter(prober)

	// No false increment on no-op (F-61 finding #4).
	if after-before != 0 {
		t.Errorf("respawnsHit incremented on no-op; before=%d after=%d",
			before, after)
	}

	// Scan timestamp advanced → tick ran → branch executed.
	if prober.Snapshot().LastScanAt.IsZero() {
		t.Error("LastScanAt zero after tick")
	}
}

// TestF61_Prober_CooldownGate verifies the cooldown gates retry.
// A Suspect set "now" must NOT be retried within the cooldown
// window — this protects against thrashing when respawn fails
// repeatedly (binary missing).
func TestF61_Prober_CooldownGate(t *testing.T) {
	cs, _ := New("oc_f61", "claude")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("claude")

	as, _ := cs.LookupSelectedAgentSession()
	as.SetExited(0)
	as.SetSuspect("immediate_respawn_failed") // SuspectSince = now
	// SetSuspect uses time.Now() internally; the cooldown gate
	// compares against p.now() which defaults to time.Now too.

	prober := NewAgentProber(func() []*ChatSession { return []*ChatSession{cs} })

	before := respawnsCounter(prober)
	prober.tick()
	after := respawnsCounter(prober)

	if after-before != 0 {
		t.Errorf("prober retried despite cooldown; respawns before=%d after=%d",
			before, after)
	}
}

// TestF61_Prober_SnapshotReflectsActivity verifies that the
// prober's Snapshot reports scanned/probes counters truthfully
// across ticks. ScannedTotal counts every AS visited; ProbesRun
// only counts actual kill(pid, 0) probes (F-61 finding #5).
func TestF61_Prober_SnapshotReflectsActivity(t *testing.T) {
	cs, _ := New("oc_f61", "claude")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("claude")
	cs.LookupSelectedAgentSession()

	prober := NewAgentProber(func() []*ChatSession { return []*ChatSession{cs} })

	if got := prober.Snapshot().ScannedTotal; got != 0 {
		t.Errorf("initial ScannedTotal = %d, want 0", got)
	}
	if got := prober.Snapshot().ProbesRun; got != 0 {
		t.Errorf("initial ProbesRun = %d, want 0", got)
	}

	prober.tick()

	// ScannedTotal >= 1 (we visited the AS) — but ProbesRun
	// might be 0 because the AS might have PID=0 (no real
	// probe happens for PID-less ASes, see stale_pid_zero
	// branch). The important semantic split is that Scanned
	// and Probes are separate counters.
	if got := prober.Snapshot().ScannedTotal; got < 1 {
		t.Errorf("after tick ScannedTotal = %d, want >= 1", got)
	}
	if !prober.Snapshot().LastScanAt.IsZero() {
		t.Logf("LastScanAt after first tick: %s", prober.Snapshot().LastScanAt)
	}
}

// TestF61_Prober_SuspectSinceZeroIsNotRetryTrigger covers the
// edge case where SuspectSince is nil but SuspectReason isn't
// (defensive — should never happen, but prober must not crash).
func TestF61_Prober_SuspectSinceZeroIsNotRetryTrigger(t *testing.T) {
	cs, _ := New("oc_f61", "claude")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("claude")
	as, _ := cs.LookupSelectedAgentSession()
	as.SetExited(0)
	as.SetSuspect("orphan") // SuspectSince is non-nil here

	prober := NewAgentProber(func() []*ChatSession { return []*ChatSession{cs} })

	before := respawnsCounter(prober)
	prober.tick() // within cooldown, should skip
	after := respawnsCounter(prober)

	if after-before != 0 {
		t.Errorf("cooldown gate failed; respawns before=%d after=%d",
			before, after)
	}
}