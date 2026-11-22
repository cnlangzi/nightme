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
//   - The silence threshold is half an hour because a healthy turn
//     can be quiet for a long time: a turn that runs a long tool
//     call or a long generation produces no events while it works
//     (a healthy turn was observed to produce no events for 5.4
//     seconds, and a 30-minute generation is possible). A turn that
//     has been quiet for half an hour is not doing visible work.
//     The 60-second threshold the removed readpump stall detector
//     used was sized for an early signal, not for a termination
//     condition.
//   - The probe deadline suits the millisecond answers the real
//     CLIs give (measured: 90 ms for a warm Claude Code, 89 ms for
//     the codex app-server, 1-70 ms for the rest), with room for a
//     loaded machine.
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
		// The turn is making progress again. Whatever the last
		// probe concluded no longer holds: the agent is visibly
		// working, so the "not responding" verdict must not outlive
		// the silence that produced it.
		as.stallProbeFailed = false
		as.stallSilentSince = 0
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
	stop := as.stallStop
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
	err := as.runProbe(ctx, h, stop)
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

// runProbe asks the bridge whether it still answers.
//
// The probe runs in its own goroutine so the monitor never blocks on
// it: a bridge whose stdin pipe is full can wedge the probe's write
// (a pipe write is not context-aware), and the monitor has to be
// able to stop when the session shuts down — a blocked probe would
// otherwise hang /close. The abandoned goroutine ends when the
// bridge closes its transport.
//
// A shutdown during the probe reads as context.Canceled: the probe
// was cut short, not answered badly.
func (as *AgentSession) runProbe(ctx context.Context, h *agent.Agent, stop <-chan struct{}) error {
	answered := make(chan error, 1)
	go func() { answered <- h.Ping(ctx) }()
	select {
	case err := <-answered:
		return err
	case <-stop:
		return context.Canceled
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
		Reason:  "the previous turn " + stallClause(silent) + ". " + stallAdvice,
		Stalled: true,
	}
}

// stallAdvice is what the user can do about a turn that stopped
// making progress. Both user-facing texts end with it: the notice
// the monitor publishes and the refusal the injection path returns.
const stallAdvice = "Run /stop to end the turn, or /close and send your message again."

// stallClause renders the shared clause: how long the turn has been
// silent, and that the agent is not answering.
//
// The notice and the refusal say the same thing about the turn, so
// they share the wording.
func stallClause(silent time.Duration) string {
	return fmt.Sprintf("has made no progress for %s and the agent is not responding",
		stallDurationText(silent))
}

// stallNoticeText renders the notice the user reads when the turn
// stopped making progress and the bridge stopped answering.
//
// The text is English and ASCII: the daemon speaks one language to
// its users, and the notice travels channels that render it
// verbatim.
func stallNoticeText(silent time.Duration) string {
	return "This turn " + stallClause(silent) + ". It may be stuck. " + stallAdvice
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

// orDefault returns v when it is set, and the production value
// otherwise. The monitor's timings are zero until a test installs
// them; zero means "use the default".
func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// stallWindow is how long the in-flight prompt may go without an
// event before the monitor probes the bridge. Zero means the
// production threshold.
func (as *AgentSession) stallWindow() time.Duration {
	return orDefault(as.stallSilence, stallSilenceThreshold)
}

// stallDeadline bounds one probe round-trip. Zero means the
// production deadline.
func (as *AgentSession) stallDeadline() time.Duration {
	return orDefault(as.stallProbeTimeout, stallProbeTimeout)
}

// stallPeriod is how often the monitor looks at the progress clock.
// Zero means the production period.
func (as *AgentSession) stallPeriod() time.Duration {
	return orDefault(as.stallCheckInterval, stallCheckInterval)
}
