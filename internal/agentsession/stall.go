// Package agentsession — stall detection.
//
// A turn that stops making progress wedges the session: the
// injection path refuses new turns, the message queue never
// flushes, and the message card never settles. Nothing in the
// daemon notices on its own — the turn's state is a bit pattern the
// guard cannot read.
//
// The monitor watches the in-flight prompt's progress clock. When
// the silence crosses the threshold it asks the bridge whether its
// control channel still answers (agent.Agent.Ping). A bridge that
// answers is busy; a bridge that does not is stuck, and the user is
// told once.
//
// The monitor only reports. It never interrupts the agent, never
// ends the turn, never replaces the bridge and never respawns it —
// a quiet turn may still be a legitimate long task, and the user
// knows whether their agent is stuck. /stop and /close are the
// user's own ways out.
package agentsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Stall detection tuning.
//
// The thresholds are constants rather than configuration: the
// trade-off is fixed, and a user with an unusually slow agent can
// still end the turn with /stop.
//
//   - The silence threshold is generous because a healthy turn was
//     observed to produce no events for 5.4 seconds; a much shorter
//     threshold would fire on legitimate work.
//   - The probe deadline suits the millisecond answers the real
//     CLIs give, with room for a loaded machine.
const (
	// stallSilenceThreshold is how long the in-flight prompt may go
	// without an event before the monitor probes the bridge.
	stallSilenceThreshold = 30 * time.Minute

	// stallProbeTimeout bounds one probe round-trip.
	stallProbeTimeout = 30 * time.Second

	// stallCheckInterval is how often the monitor looks at the
	// progress clock.
	stallCheckInterval = 30 * time.Second
)

// startStallMonitor launches the stall monitor goroutine if it is
// not already running. The monitor lives for the AgentSession's
// lifetime: it watches whichever prompt is in flight.
//
// Idempotent — the second call is a no-op.
func (as *AgentSession) startStallMonitor() {
	as.asMu.Lock()
	if as.stallStop != nil || as.shuttingDown {
		as.asMu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	as.stallStop = stop
	as.stallDone = done
	as.asMu.Unlock()
	go as.stallMonitorLoop(stop, done)
}

// stallMonitorLoop is the monitor's ticker body. It wakes up every
// check interval and asks the AS whether its in-flight turn has gone
// quiet for too long.
func (as *AgentSession) stallMonitorLoop(stop <-chan struct{}, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(as.stallPeriod())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			as.checkStall()
		}
	}
}

// checkStall runs one monitor tick.
//
// It probes the bridge when the in-flight prompt has been silent
// for longer than the threshold and no probe has run inside the
// current silence window. A failed probe publishes one notice for
// the stall episode; a successful probe publishes nothing.
func (as *AgentSession) checkStall() {
	now := time.Now()

	as.asMu.Lock()
	p := as.currentPrompt
	if p == nil {
		// No turn in flight: whatever episode was running is over.
		as.resetStallLocked()
		as.asMu.Unlock()
		return
	}
	if as.stallPromptID != p.ID {
		// A new prompt is a new episode. The user has not been told
		// about this one yet.
		as.stallPromptID = p.ID
		as.stallReported = false
		as.stallProbeFailed = false
		as.stallLastProbeAt = time.Time{}
	}
	window := as.stallWindow()
	silent := now.Sub(progressSince(p))
	if silent < window {
		as.asMu.Unlock()
		return
	}
	if !as.stallLastProbeAt.IsZero() && now.Sub(as.stallLastProbeAt) < window {
		// A probe already ran inside this silence window; the next
		// one waits for the window to elapse again.
		as.asMu.Unlock()
		return
	}
	h := as.handle
	promptID := p.ID
	anchor := p.LastMessageID
	progressAt := progressSince(p)
	as.asMu.Unlock()

	if h == nil {
		// No live bridge to probe: the monitor waits for one. The
		// silence window is not consumed — there was no probe.
		return
	}

	as.asMu.Lock()
	as.stallLastProbeAt = now
	as.stallSilentSince = silent
	as.asMu.Unlock()

	ctx, cancel := context.WithTimeout(as.OpContext(), as.stallDeadline())
	err := h.Ping(ctx)
	cancel()

	switch {
	case err == nil:
		as.asMu.Lock()
		as.stallProbeFailed = false
		as.asMu.Unlock()
		slog.Debug("agentsession: stall probe answered",
			"as_id", as.ID,
			"chat_id", as.ChatSessionID,
			"silent", silent.String())
		return

	case errors.Is(err, context.Canceled):
		// The session is going away: the probe was cut short, not
		// answered badly. Nothing to report.
		return
	}

	as.asMu.Lock()
	// The turn may have moved on while the probe was in flight: the
	// agent answered the turn instead of the probe, or the turn
	// ended. A turn that finished (or made progress) is not a
	// stalled one — reporting it would tell the user to end a
	// turn that is already over.
	live := as.currentPrompt
	stillStalled := live != nil && live.ID == promptID &&
		!live.LastProgressAt.After(progressAt)
	as.stallProbeFailed = stillStalled
	report := stillStalled && !as.stallReported
	if report {
		as.stallReported = true
	}
	as.asMu.Unlock()

	if !stillStalled {
		slog.Debug("agentsession: stall probe missed a turn that moved on",
			"as_id", as.ID,
			"chat_id", as.ChatSessionID,
			"err", err.Error())
		return
	}

	slog.Warn("agentsession: stall probe failed",
		"as_id", as.ID,
		"chat_id", as.ChatSessionID,
		"silent", silent.String(),
		"err", err.Error())

	if report {
		as.publishStallNotice(promptID, anchor, silent)
	}
}

// progressSince returns the moment the in-flight prompt last showed
// progress.
//
// A prompt installed without a progress clock falls back to its
// creation time; a prompt with neither is treated as fresh, so the
// monitor does not read an unset timestamp as an eternity of
// silence.
func progressSince(p *Prompt) time.Time {
	if !p.LastProgressAt.IsZero() {
		return p.LastProgressAt
	}
	if !p.CreatedAt.IsZero() {
		return p.CreatedAt
	}
	return time.Now()
}

// resetStallLocked clears the stall episode state. Caller MUST hold
// asMu.
func (as *AgentSession) resetStallLocked() {
	as.stallPromptID = ""
	as.stallReported = false
	as.stallProbeFailed = false
	as.stallLastProbeAt = time.Time{}
	as.stallSilentSince = 0
}

// publishStallNotice tells the user that the turn stalled.
//
// The notice rides the bridge's own notice path — an
// agent.EventAgentNotice, which the outbound translation renders as
// an ordinary reply — anchored to the message whose turn stalled, so
// it lands on the card that is still spinning.
func (as *AgentSession) publishStallNotice(promptID, anchor string, silent time.Duration) {
	ev := agent.AgentEvent{
		Kind: agent.EventAgentNotice,
		Text: stallNoticeText(silent),
	}

	as.asMu.RLock()
	stop := as.stallStop
	as.asMu.RUnlock()

	as.ensureDispatcher()
	select {
	case as.eventQueue <- EnrichedEvent{
		ChatID:         as.ChatSessionID,
		AgentSessionID: as.ID,
		UserMsgID:      anchor,
		PromptID:       promptID,
		Kind:           KindAgentEvent,
		AgentEvent:     &ev,
	}:
	case <-stop:
		// Shutdown won the race; the notice is dropped.
	}
}

// injectionRefusal renders the refusal the injection path returns
// when a user turn is in flight.
//
// A turn that is merely busy is worth waiting out, so the refusal
// says so and names nothing. A turn that has stopped making
// progress AND whose probe went unanswered is worth ending, so the
// refusal names the two commands that end it.
func (as *AgentSession) injectionRefusal(stalled bool, silent time.Duration) error {
	if !stalled {
		return &InjectionRefused{Reason: "a turn is still in flight"}
	}
	return &InjectionRefused{
		Reason: fmt.Sprintf(
			"the previous turn has made no progress for %s and the agent is not responding. "+
				"Run /stop to end it, or /close and send your message again",
			stallDurationText(silent)),
		Stalled: true,
	}
}

// stallNoticeText renders the notice the user reads when the turn
// stopped making progress and the bridge stopped answering.
//
// The text is English and ASCII: the daemon speaks one language to
// its users, and the notice travels channels that render it
// verbatim.
func stallNoticeText(silent time.Duration) string {
	return fmt.Sprintf(
		"This turn has made no progress for %s and the agent is not responding. "+
			"It may be stuck. Run /stop to end the turn, or /close and send your message again.",
		stallDurationText(silent))
}

// stallDurationText renders a silence duration in whole minutes. The
// shortest wording is one minute — the notice only fires after a
// multi-minute silence, and "0 minutes" would read like a bug.
func stallDurationText(d time.Duration) string {
	minutes := int(d.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// stallWindow is how long the in-flight prompt may go without an
// event before the monitor probes the bridge. Zero means the
// production threshold.
func (as *AgentSession) stallWindow() time.Duration {
	if as.stallSilence > 0 {
		return as.stallSilence
	}
	return stallSilenceThreshold
}

// stallDeadline bounds one probe round-trip. Zero means the
// production deadline.
func (as *AgentSession) stallDeadline() time.Duration {
	if as.stallProbeTimeout > 0 {
		return as.stallProbeTimeout
	}
	return stallProbeTimeout
}

// stallPeriod is how often the monitor looks at the progress clock.
// Zero means the production period.
func (as *AgentSession) stallPeriod() time.Duration {
	if as.stallCheckInterval > 0 {
		return as.stallCheckInterval
	}
	return stallCheckInterval
}
