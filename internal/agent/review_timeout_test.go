package agent

// The silence threshold of a review group.
//
// A group whose subagent stops producing anything must be closed on
// its own: the run cannot finish without it, and the group must stay
// resumable so the work is not lost.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDefaultReviewGroupTimeout: the built-in threshold is 15 minutes.
func TestDefaultReviewGroupTimeout(t *testing.T) {
	if DefaultReviewGroupTimeout != 15*time.Minute {
		t.Errorf("DefaultReviewGroupTimeout = %s, want 15m", DefaultReviewGroupTimeout)
	}
}

// TestGroupTimeoutError_IsATimeout: the error carries the group and
// the threshold, and it answers to ErrGroupTimeout.
func TestGroupTimeoutError_IsATimeout(t *testing.T) {
	err := &GroupTimeoutError{Group: "nightme code reviewer", Timeout: 15 * time.Minute}
	if !errors.Is(err, ErrGroupTimeout) {
		t.Error("the timeout error is not ErrGroupTimeout")
	}
	want := `review group "nightme code reviewer" timed out after 15m0s without a heartbeat`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// TestGroupWatchdog_ClosesTheSilentGroup: the watchdog cancels the
// group's context with the timeout sentinel when the silence reaches
// the threshold.
func TestGroupWatchdog_ClosesTheSilentGroup(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	w := newGroupWatchdog(20*time.Millisecond, cancel)
	defer w.stop()

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrGroupTimeout) {
			t.Fatalf("the group's context carries the cause %v, want ErrGroupTimeout",
				context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog did not close the silent group")
	}
}

// TestGroupWatchdog_AHeartbeatResetsTheTimer: a group that says
// something inside the window is not closed.
func TestGroupWatchdog_AHeartbeatResetsTheTimer(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// The window is wide enough that a scheduling hiccup does not
	// close the group, and the loop runs past it: a watchdog that
	// ignored the heartbeats would close the group.
	w := newGroupWatchdog(150*time.Millisecond, cancel)
	defer w.stop()

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.beat()
		time.Sleep(20 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("a group that keeps beating was closed: %v", context.Cause(ctx))
	}
}

// TestGroupWatchdog_StopDisarmsIt: a group that ended in time is
// never closed by the timer.
func TestGroupWatchdog_StopDisarmsIt(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	w := newGroupWatchdog(20*time.Millisecond, cancel)
	w.stop()

	time.Sleep(80 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("a stopped watchdog closed the group: %v", context.Cause(ctx))
	}
}

// TestWithReviewGroupTimeout_ReachesTheConfig: the threshold travels
// as an option, and "disabled" stays distinguishable from "not set".
func TestWithReviewGroupTimeout_ReachesTheConfig(t *testing.T) {
	cfg := ParseRunOnceOptions(nil)
	if cfg.ReviewGroupTimeout != nil {
		t.Errorf("a run without the option carries a threshold: %v", *cfg.ReviewGroupTimeout)
	}

	cfg = ParseRunOnceOptions([]RunOnceOption{WithReviewGroupTimeout(5 * time.Minute)})
	if cfg.ReviewGroupTimeout == nil {
		t.Fatal("the option did not reach the config")
	}
	if *cfg.ReviewGroupTimeout != 5*time.Minute {
		t.Errorf("the threshold = %s, want 5m", *cfg.ReviewGroupTimeout)
	}

	cfg = ParseRunOnceOptions([]RunOnceOption{WithReviewGroupTimeout(0)})
	if cfg.ReviewGroupTimeout == nil {
		t.Fatal("a disabled timeout did not reach the config")
	}
	if *cfg.ReviewGroupTimeout != 0 {
		t.Errorf("the threshold = %s, want 0", *cfg.ReviewGroupTimeout)
	}
}
