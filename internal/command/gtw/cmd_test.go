package gtw

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
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

// --- Handle-level integration: wrap reaches downstream subprocess ---

// recordingGitRunner is the test double GitRunner for verifying the
// factory-level wrap propagates into actual subprocess calls. It
// snapshots whatever ctx it receives on the first Run so the test
// can assert the deadline was applied without spinning up a real
// `git` invocation.
type recordingGitRunner struct {
	seenCtx         context.Context
	seenHasDeadline bool
	seenTimeUntil   time.Duration
	firstSeenArgs   []string
}

func (r *recordingGitRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	if r.seenCtx == nil { // first call only
		r.seenCtx = ctx
		if d, ok := ctx.Deadline(); ok {
			r.seenHasDeadline = true
			r.seenTimeUntil = time.Until(d)
		}
		r.firstSeenArgs = append([]string(nil), args...)
	}
	// RepoRoot-style calls expect an absolute toplevel path; stub
	// something usable so the rest of runSync can proceed.
	return "/tmp/fake-repo-root", "", nil
}

// TestHandle_AppliesCommandTimeoutDownstream proves the wrap at
// Factory.Handle actually reaches an inner subprocess call — not
// just that withCommandTimeout returns a deadline'd ctx in
// isolation. Run Sync is the cheapest subcommand path that
// exercises a real Git call (RepoRoot → git rev-parse), so we
// drive Handle into runSync and have the fake GitRunner record
// what ctx it saw.
//
// Without the wrap, runSync would receive the parent ctx
// (context.Background here, no deadline) and the recording fake
// would see seenHasDeadline == false. With the wrap, the fake
// sees a deadline ~timeouts.Command in the future.
//
// Threshold note: timeouts.Command is 30 min, timeouts.CLI (the
// fallback wrap runCmd applies when no deadline is set) is 5 min.
// Distinguishing 5 from 30 is robust against scheduler slack at
// the 60-s boundaries used in the upper/lower bound checks.
func TestHandle_AppliesCommandTimeoutDownstream(t *testing.T) {
	git := &recordingGitRunner{}
	f := &Factory{
		mgr:  NewManager(),
		deps: HandlerDeps{Git: git},
	}
	cs := (&chatsession.ChatSession{}).WithEmitter(nil)
	_ = cs.SetSelectedCwd(t.TempDir())

	input := command.SlashInput{
		ChatID:    "test-chat",
		MessageID: "test-msg",
		Args:      []string{"gtw", "sync"},
	}

	if _, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, input); err != nil {
		// runSync may return an error from buildSyncReply — we
		// only care about the side-channel recording here. Any
		// error path that DIDN'T reach RepoRoot would leave
		// seenCtx==nil, which the assertion below catches.
		t.Logf("Handle returned err=%v (allowed; the test inspects the recording fake)", err)
	}

	if git.seenCtx == nil {
		t.Fatalf("repoRoot was never called — runSync path didn't exercise Git.Run; the wrap-check assertion below would be vacuous")
	}
	if !git.seenHasDeadline {
		t.Fatalf("Factory.Handle did not propagate a deadline into the inner Git subprocess call; " +
			"the wrap at Handle was lost — without this, hung /gtw subcommands can park the per-chat run lock forever")
	}
	// Lower bound: ~timeouts.Command (30 min), with a small slack
	// for the time elapsed since withCommandTimeout was called.
	// Picked well above timeouts.CLI's 5 min so we distinguish
	// "Handle wrapped to 30 min" from "runCmd's CLI fallback
	// wrapped to 5 min".
	if git.seenTimeUntil < timeouts.Command-time.Minute {
		t.Errorf("downstream ctx deadline too close: %v left, want ≥ %v (timeouts.Command); "+
			"got something closer to timeouts.CLI — the wrap at Handle wasn't applied",
			git.seenTimeUntil, timeouts.Command)
	}
	// Upper bound: must not exceed timeouts.Command by more than
	// the value the helper would add (i.e. a tiny grace margin).
	if git.seenTimeUntil > timeouts.Command+time.Second {
		t.Errorf("downstream ctx deadline too far in the future: %v left, want ≤ %v",
			git.seenTimeUntil, timeouts.Command)
	}
}
