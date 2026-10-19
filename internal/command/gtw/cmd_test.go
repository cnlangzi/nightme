package gtw

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/timeouts"
)

// TestWithCommandTimeout_AppliesCommandBudget covers the
// "caller has no deadline" branch: a fresh context.Background()
// must come back deadline'd at timeouts.Command. Without this,
// the safety net is silently disabled and a hung /gtw
// subcommand can park the per-chat run lock forever.
//
// We don't actually wait 30 minutes — instead we read the
// deadline back off the returned ctx and compare it to
// timeouts.Command. The slack window is wide enough to absorb
// clock drift while still catching "applied CLI (5 min) by
// mistake" / "applied nothing at all".
func TestWithCommandTimeout_AppliesCommandBudget(t *testing.T) {
	ctx, cancel := withCommandTimeout(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("withCommandTimeout(context.Background()) produced no deadline; got ok=false")
	}
	left := time.Until(deadline)
	// Lower bound: leave room for the test scheduler — anything
	// significantly short of timeouts.Command means we applied a
	// smaller number (e.g. CLI's 5 min) by accident.
	if left < timeouts.Command-time.Minute {
		t.Errorf("deadline too close: %v left, want ≈ %v (timeouts.Command)",
			left, timeouts.Command)
	}
	// Upper bound: must not exceed the policy value by more than
	// a tiny clock-slop margin. Anything significantly above
	// means we applied a larger number (e.g. 1 hour) by accident.
	if left > timeouts.Command+time.Second {
		t.Errorf("deadline too far in the future: %v left, want ≈ %v",
			left, timeouts.Command)
	}
}

// TestWithCommandTimeout_RespectsCallerDeadline mirrors
// TestRunCmd_RespectsCallerDeadline: the contract is "caller's
// deadline always wins". A 100 ms parent deadline must come back
// unchanged — if we accidentally overrode with timeouts.Command
// the test would block for 30 min and the deadline check would
// show ~30 min remaining, not ~100 ms.
//
// Counter-test to TestWithCommandTimeout_AppliesCommandBudget:
// together they pin the two-branch behaviour.
func TestWithCommandTimeout_RespectsCallerDeadline(t *testing.T) {
	const callerBudget = 100 * time.Millisecond
	parent, parentCancel := context.WithTimeout(context.Background(), callerBudget)
	defer parentCancel()

	ctx, cancel := withCommandTimeout(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("withCommandTimeout dropped the caller's deadline: got ok=false")
	}
	left := time.Until(deadline)
	// Caller's 100 ms deadline should propagate. Anything well
	// above 100 ms means we silently applied a 30-min wrap on
	// top of the caller's budget — the worst-case failure mode
	// of this contract.
	if left > 5*callerBudget {
		t.Errorf("caller deadline overridden: ctx has %v left, want ≈ %v",
			left, callerBudget)
	}

	// Returned cancel must be safe to defer even when the caller
	// already set a deadline (otherwise every run* would need a
	// conditional cancel). The helper returns a no-op in this
	// branch — we cannot observe "no-op" directly, but deferring
	// it must not produce a panic and must not extend the
	// deadline (which parent's own cancel already covers).
	cancel()
}
