// review_timeout.go — the silence threshold of a review group.
//
// A review fan-out runs one subagent per group. A subagent that wedges
// — its process is alive but it stopped producing anything — would hold
// its session open forever: the run never finishes, the group's
// findings never arrive, and the group is never recorded as failed, so
// `/review --continue` has nothing to pick up.
//
// The threshold is a silence window, not a deadline: every event on the
// group's session stream is a heartbeat that resets it. A group that
// says nothing for longer than the window is closed, the run reports
// it, and the group stays resumable.
//
// The watchdog lives here, next to the fan-out core, because "one group
// = one subagent" only exists there. The shared session drain
// (drainSessionEvents) serves /gtw commit and /gtw pr as well, and
// must not learn about review semantics.

package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultReviewGroupTimeout is the silence threshold a review group
// gets when the caller does not pass one.
//
// It is long enough to cover the silent window of one LLM call plus a
// tool round, and short enough to tell a wedged subagent from a slow
// one.
const DefaultReviewGroupTimeout = 15 * time.Minute

// ErrGroupTimeout marks a review group whose subagent went silent.
//
// It is the sentinel errors.Is looks for: a group's failure is a
// timeout when the group's own context was closed by the watchdog.
var ErrGroupTimeout = errors.New("review group timed out")

// GroupTimeoutError reports that one review group's subagent produced
// no heartbeat for longer than the threshold.
type GroupTimeoutError struct {
	// Group is the group's reader-facing name (see ReviewGroupName).
	Group string
	// Timeout is the silence threshold that fired.
	Timeout time.Duration
}

// Error renders the timeout as the group's failure. It is what the
// merged review text carries inline, and what the main agent reads.
func (e *GroupTimeoutError) Error() string {
	return fmt.Sprintf("review group %q timed out after %s without a heartbeat",
		e.Group, e.Timeout)
}

// Unwrap makes errors.Is(err, ErrGroupTimeout) true for the error.
func (e *GroupTimeoutError) Unwrap() error { return ErrGroupTimeout }

// WithReviewGroupTimeout sets the per-group silence threshold of a
// review fan-out.
//
// d <= 0 disables the timeout. Not passing the option keeps
// DefaultReviewGroupTimeout.
//
// The option is consumed by the fan-out core and is not forwarded to
// the group's turn: the threshold belongs to the fan-out, not to the
// turn it runs.
func WithReviewGroupTimeout(d time.Duration) RunOnceOption {
	return func(c *runOnceConfig) { c.ReviewGroupTimeout = &d }
}

// groupWatchdog closes a review group whose subagent went silent.
//
// The timer is armed once and re-armed by its own fire handler: a beat
// inside the window pushes the next expiry to lastBeat + timeout, so
// the group is closed exactly when the silence reaches the threshold
// and not one timer tick later.
type groupWatchdog struct {
	timeout time.Duration
	cancel  context.CancelCauseFunc

	mu sync.Mutex
	// timer is the AfterFunc timer that fires the check.
	timer *time.Timer
	// lastBeat is when the group was last heard from.
	lastBeat time.Time
	// stopped marks a disarmed watchdog: the group ended in time, or
	// the timeout already fired.
	stopped bool
}

// newGroupWatchdog arms the watchdog of one group.
func newGroupWatchdog(timeout time.Duration, cancel context.CancelCauseFunc) *groupWatchdog {
	w := &groupWatchdog{timeout: timeout, cancel: cancel}
	w.lastBeat = time.Now()
	w.timer = time.AfterFunc(timeout, w.fire)
	return w
}

// beat records a heartbeat: the group's silence window starts over.
//
// It runs on the group's drain goroutine, so it must not block.
func (w *groupWatchdog) beat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.lastBeat = time.Now()
}

// stop disarms the watchdog. A group that ended in time is never
// closed by the timer.
func (w *groupWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	w.timer.Stop()
}

// fire is the timer callback: it closes the group when the silence
// reached the threshold.
//
// The timer was armed before the last beat, so a beat inside the window
// means the group is still alive — the handler re-arms for the time
// that is left instead of closing it.
//
// The cancel happens under the lock, so the watchdog and the caller
// that stops it agree on who won: whichever of them takes the lock
// first decides whether the group is closed.
func (w *groupWatchdog) fire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	if left := w.timeout - time.Since(w.lastBeat); left > 0 {
		w.timer.Reset(left)
		return
	}
	w.stopped = true
	w.cancel(ErrGroupTimeout)
}
