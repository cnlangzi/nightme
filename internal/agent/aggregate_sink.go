// aggregate_sink.go — multi-RunOnce event aggregator for /review.
//
// Background (docs/REVIEW.md §2.5): when Tier 2 (ocr delegate) emits
// multiple rule groups, DelegateReview splits the review into N parallel
// RunOnce jobs (one per group). Each job emits its own Ready → Text →
// Result lifecycle to its sink. Without aggregation, the chat channel's
// StatusBar / heartbeat would observe N interleaved review runs
// simultaneously — four jobs would produce four "ready" events, four
// terminal events, and a confused receipt header.
//
// eventAggregator turns N per-job sink streams into ONE outer sink
// stream with the following invariants:
//
//   1. The first per-job EventAgentReady is forwarded as the outer
//      Ready; subsequent per-job Ready events are SUPPRESSED (the chat
//      already knows a review is running — we don't want N redundant
//      "🤖: agent X ready" header flips).
//   2. Per-job INTERMEDIATE events (EventAgentText, EventAgentToolStart/
//      End, EventAgentPermission, EventAgentTask*) are DROPPED at the
//      aggregator. They were observation-only in single-job mode;
//      shipping them as N interleaved streams would clutter the chat
//      (e.g., four tools calling grep simultaneously on screen). The
//      chat channel only needs the lifecycle markers.
//   3. Per-job TERMINAL events (EventAgentResult / EventAgentError)
//      increment a done counter. When done == expected, the aggregator
//      emits ONE synthetic outer EventAgentResult to close the
//      lifecycle — guaranteeing every outer Ready is paired with
//      exactly one outer Result (the same pairing the long-lived
//      runtime.NewEventHandler relies on for StatusBar / receipt
//      rendering).
//
// The merged review TEXT is delivered via FormatReviewMessage from the
// /review dispatcher (internal/command/review/cmd.go) — NOT via the
// synthetic outer Result. The synthetic event is a lifecycle marker
// only; Result.Text is intentionally empty.
//
// Thread-safety: wrapJob closures are called concurrently from N
// goroutines; all shared state is mu-guarded.
package agent

import (
	"sync"
)

// maxConcurrentReviewJobs caps how many parallel RunOnce jobs a single
// /review may spawn. 4 is the documented ceiling (REVIEW.md §2.5.3):
// too high risks token / API-rate blow-up, too low starves large
// changesets. The cap is enforced by a semaphore inside DelegateReview
// — this package only does the event-side aggregation.
const maxConcurrentReviewJobs = 4

// eventAggregator merges N per-job AgentEvent streams into one outer
// stream. Constructed with the outer sink callback (typically the one
// from outbound.StreamRunOnceToEmitter) and the expected number of
// jobs. Returns via newEventAggregator.
type eventAggregator struct {
	outer    func(AgentEvent)
	expected int

	mu         sync.Mutex
	firstReady bool // first per-job Ready forwarded as the outer Ready
	done       int  // per-job terminals observed
	finalSent  bool // synthetic outer Result emitted
}

// newEventAggregator constructs an aggregator. outer is the outer sink
// callback (passed to WithEventSink by the /review dispatcher); expected
// is how many per-job sinks wrapJob will be called for. expected must
// be ≥ 1 — callers gate on multi-group detection before constructing.
func newEventAggregator(outer func(AgentEvent), expected int) *eventAggregator {
	return &eventAggregator{outer: outer, expected: expected}
}

// wrapJob returns a per-job sink. src is unused at the event layer
// (per-job intermediate events are dropped — see package doc) but is
// accepted for symmetry with multi-job debug logging; a future
// iteration MAY re-introduce per-job Text forwarding with Source
// stamped, in which case src becomes meaningful again.
//
// Each returned closure is safe to call concurrently from its own
// goroutine; all aggregator state is mu-guarded.
func (a *eventAggregator) wrapJob(src string) func(AgentEvent) {
	return func(ev AgentEvent) {
		switch ev.Kind {
		case EventAgentReady:
			// First per-job Ready IS the outer review-ready signal.
			// Subsequent per-job Ready events are redundant (the
			// chat channel's StatusBar would otherwise flip N
			// times). Source is reset to "" so the outer sink
			// sees a clean "outer" Ready.
			a.mu.Lock()
			first := !a.firstReady
			a.firstReady = true
			a.mu.Unlock()
			if first {
				a.outer(AgentEvent{
					Kind:      EventAgentReady,
					SessionID: ev.SessionID,
					Model:     ev.Model,
					AgentName: ev.AgentName,
					Workspace: ev.Workspace,
					Branch:    ev.Branch,
					Source:    "",
				})
			}
		case EventAgentResult, EventAgentError:
			// Per-job terminal — DROP at the aggregator level and
			// only count toward the done counter. The per-job
			// RunResult.Text is already captured by DelegateReview
			// (it sees the RunOnce return value directly), so no
			// information is lost; we just don't echo it back
			// through the sink.
			a.mu.Lock()
			a.done++
			allDone := a.done >= a.expected && !a.finalSent
			if allDone {
				a.finalSent = true
			}
			a.mu.Unlock()
			if allDone {
				// Close the outer lifecycle with one synthetic
				// EventAgentResult. Source="" marks it as the
				// aggregator's own emission. Result.Text is
				// intentionally empty: the merged review text
				// is delivered via FormatReviewMessage from
				// the /review dispatcher, not via the sink.
				a.outer(AgentEvent{
					Kind:   EventAgentResult,
					Source: "",
				})
			}
		default:
			// Per-job intermediate events (Text, ToolStart/End,
			// Permission, TaskCreate/Update, etc.): dropped.
			// Shipping N interleaved intermediate streams would
			// clutter the chat (e.g., four tools calling grep
			// simultaneously on screen) — and the chat channel
			// doesn't have a renderer for "[group-N] …"
			// prefixes anyway. Per-job RUN-RESULT TEXT is still
			// captured by DelegateReview via the RunOnce return
			// value, so the final deliverable is unaffected.
		}
	}
}
