// Package chatsession — AgentSession readpump (CS-AS 边界重构 Phase 1).
//
// The readpump is the per-AS goroutine that drains `as.handle.Events()`
// and produces enriched events. Events go into `as.eventQueue`; a
// separate dispatcher goroutine (see agentsession_dispatch.go) drains
// the queue and publishes onto `as.EventBus`, where ChatSession
// subscribers receive them. Lifecycle:
//
//   - Started by `Activate` on first call (idempotent; subsequent
//     calls are no-ops).
//   - Runs until either the handle's Events() channel closes
//     (process death) or `Shutdown` closes readpumpStop.
//   - On handle close (process death), calls `endPrompt(ProcessDied)`
//     to fix the F-53 deadlock and emits `KindLifecycle{StatusExited}`.
//   - On EventAgentDone / EventAgentError, calls `endPrompt(Clean | Error)`
//     and emits `KindPromptEnded`.
//   - Other events are wrapped as `KindAgentEvent` and pushed to
//     eventQueue for the dispatcher.
package agentsession

import (
	"log/slog"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// readpumpStallThreshold (fix-bridge-stuck) is the per-loop "no
// events for N seconds while in-flight" detector. When the readpump
// is mid-Prompt (IsReady=false, Status=Running) and no bridge event
// has arrived for this duration, we mark the AS suspect with reason
// "readpump_stalled" so the prober can revive it.
//
// Why this exists: HungPrompt (5min) is the watchdog's last-resort
// recovery for a "bridge alive but protocol deadlocked" scenario.
// readpumpStallThreshold is a much earlier signal — typically
// triggered while the bridge is still in early thinking phases —
// that gives the prober a Suspect-marked AS to act on instead of
// waiting for the full HungPrompt window.
//
// Tuning: 60s. Shorter risks false positives on slow-but-correct
// turns (Claude Opus thinking can take 60-90s before the first
// EventAgentText). Longer loses the value of this detector.
// Empirically: a healthy bridge emits at least an EventAgentReady
// or first EventAgentText within 30s of Submit; 60s is twice that.
const readpumpStallThreshold = 60 * time.Second

// startReadPump launches the readpump goroutine if not already running.
// Single-launch guard via `readpumpStarted`.
//
// Called by `Activate` on first invocation. Safe to call multiple
// times — only the first call has effect.
func (as *AgentSession) startReadPump() {
	as.asMu.Lock()
	if as.readpumpStarted {
		as.asMu.Unlock()
		return
	}
	as.readpumpStarted = true
	as.readpumpStop = make(chan struct{})
	as.readpumpDone = make(chan struct{})
	as.asMu.Unlock()
	// Lazily start the dispatcher too — it drains eventQueue into
	// EventBus and must be running before any events are pushed.
	as.startEventDispatch()
	go as.readpumpLoop()
}

// readpumpLoop is the per-AS event drain loop. Single goroutine per AS.
//
// Lifecycle:
//
//  1. Wait for `as.handle` to be non-nil (Spawn completes some time
//     after Activate). Polls at 50ms — Spawn is called in the same
//     goroutine that called Activate, so the wait is short.
//  2. Once handle is set, switch to the event loop. Read each
//     `agent.AgentEvent`, enrich it, and push to eventQueue.
//  3. On EventAgentDone / EventAgentError, call `endPrompt(reason)` to
//     terminate the in-flight Prompt.
//  4. On channel close (`!ok`), call `endPrompt(ProcessDied)`
//     (fixes the F-53 deadlock), emit `KindLifecycle{StatusExited}`.
//  5. Return on either readpumpStop close (Shutdown) or handler exit.
//
// Shutdown semantics: every push to eventQueue is `select`'d against
// readpumpStop — so a Shutdown during a saturated eventQueue
// (256 events buffered, no reader) can interrupt the readpump and
// drain the queue. The lost event is acceptable for graceful
// shutdown.
func (as *AgentSession) readpumpLoop() {
	defer close(as.readpumpDone)

	// Phase 1: snapshot handle. Spawn guarantees it's set before
	// startReadPump; the defensive path below handles the rare
	// race where startReadPump runs before Spawn has populated
	// as.handle (only seen in tests that wire them out of order).
	as.asMu.RLock()
	handle := as.handle
	as.asMu.RUnlock()
	if handle == nil {
		select {
		case <-as.readpumpStop:
			return
		case <-time.After(100 * time.Millisecond):
		}
		as.asMu.RLock()
		handle = as.handle
		as.asMu.RUnlock()
		if handle == nil {
			return // nothing to do; abort
		}
	}
	events := handle.Events()

	// fix-bridge-stuck: per-loop stall detector. Reset on every
	// observed event so a healthy-but-slow turn (long thinking,
	// tool calls) does not trip the threshold. Only when the
	// select genuinely sees nothing for readpumpStallThreshold
	// AND the AS is mid-Prompt do we mark Suspect.
	stallTimer := time.NewTimer(readpumpStallThreshold)
	defer stallTimer.Stop()

	// Phase 2: event loop.
	for {
		select {
		case <-as.readpumpStop:
			return
		case <-stallTimer.C:
			// fix-bridge-stuck: silence detector. Only fires
			// when in-flight (IsReady=false) AND the bridge is
			// supposed to be alive (StatusRunning). /close
			// path (StatusExited) and idle (IsReady=true) are
			// not stalls — those are intentional steady
			// states.
			if !as.IsReady() && as.Status() == StatusRunning {
				slog.Warn("agentsession: readpump stalled (no events in threshold while in-flight); marking suspect",
					"as_id", as.ID,
					"threshold", readpumpStallThreshold)
				as.SetSuspect("readpump_stalled")
			}
			// Reset so we keep flagging — the prober is the
			// one with the actual cooldown / respawn decision.
			stallTimer.Reset(readpumpStallThreshold)
		case ev, ok := <-events:
			if !ok {
				// Channel closed: process exited (or Shutdown
				// closed the underlying handle). Either way,
				// the in-flight Prompt needs to end — this is
				// the F-53 deadlock fix.
				as.endPrompt(PromptEndProcessDied)
				as.emitLifecycleLocked(StatusExited)
				return
			}

			// Activity observed — reset stall timer.
			if !stallTimer.Stop() {
				select {
				case <-stallTimer.C:
				default:
				}
			}
			stallTimer.Reset(readpumpStallThreshold)

			// Enrich event with anchor info from currentPrompt.
			as.asMu.RLock()
			prompt := as.currentPrompt
			as.asMu.RUnlock()
			var userMsgID, promptID string
			if prompt != nil {
				userMsgID = prompt.LastMessageID
				promptID = prompt.ID
			}

			// CRITICAL: copy ev to heap before taking its address.
			// Go's range loop gives each iteration its own ev
			// variable, but the address is still on the readpump's
			// stack frame. When the next iteration starts, ev is
			// overwritten — and the runtime, which may still be
			// reading the previous event via the old &ev pointer,
			// would see the new event's bytes (silent data
			// corruption). Forcing evCopy to escape via the
			// channel-send keeps the pointer stable across the
			// readpump's lifetime.
			evCopy := ev
			as.ensureDispatcher()
			select {
			case as.eventQueue <- EnrichedEvent{
				ChatID:         as.ChatSessionID,
				AgentSessionID: as.ID,
				UserMsgID:      userMsgID,
				PromptID:       promptID,
				Prompt:         prompt,
				Kind:           KindAgentEvent,
				AgentEvent:     &evCopy,
			}:
			case <-as.readpumpStop:
				return
			}

			// Terminal handling: EventAgentDone / EventAgentError ends the
			// in-flight Prompt. The PromptEnded event is emitted
			// by endPrompt itself.
			if ev.Kind == agent.EventAgentDone || ev.Kind == agent.EventAgentError {
				reason := PromptEndClean
				if ev.Kind == agent.EventAgentError {
					reason = PromptEndError
				}
				as.endPrompt(reason)
			}
		}
	}
}

// emitLifecycleLocked pushes a KindLifecycle event to eventQueue,
// if eventQueue is still open. Safe to call from any context
// (readpump or any other goroutine). Honors readpumpStop on
// shutdown.
//
// Caller does NOT need to hold asMu.
func (as *AgentSession) emitLifecycleLocked(status Status) {
	as.asMu.RLock()
	pid := as.pid
	stop := as.readpumpStop
	as.asMu.RUnlock()

	as.ensureDispatcher()
	select {
	case as.eventQueue <- EnrichedEvent{
		ChatID:         as.ChatSessionID,
		AgentSessionID: as.ID,
		Kind:           KindLifecycle,
		Lifecycle: &LifecycleChange{
			PID:    pid,
			Status: status,
		},
	}:
	case <-stop:
		// shutdown won the race; abandon the event
	}
}

// endPrompt (CS-AS 边界重构 Phase 1) is the AS-internal Prompt
// terminator. Called by:
//
//   - readpumpLoop, on EventAgentDone → PromptEndClean
//   - readpumpLoop, on EventAgentError → PromptEndError
//   - readpumpLoop, on channel close → PromptEndProcessDied (F-53
//     deadlock fix)
//
// Phase 0's `cs.endPrompt` did the same thing but was tied to
// cs.mu and routed through a callback. Phase 1 moves it to AS
// (where the Prompt lifecycle naturally lives) and routes through
// eventQueue (unified with the rest of the per-AS event stream).
//
// The CS side writes the message-state bookkeeping back to
// `messagesByID` via the KindPromptEnded event — see
// `ChatSession.writebackMessageState`.
//
// Re-entrant safe: a second call while currentPrompt is already
// nil is a no-op (the channel-closed path may legitimately call
// endPrompt twice — once for the in-flight Prompt, once for the
// followup nothing).
func (as *AgentSession) endPrompt(reason PromptEndReason) {
	as.asMu.Lock()
	if as.currentPrompt == nil {
		as.asMu.Unlock()
		return
	}
	p := as.currentPrompt
	p.EndedAt = time.Now()
	p.EndReason = reason
	as.currentPrompt = nil
	// Mirror inFlightMessages with currentPrompt — invariant:
	// currentPrompt non-nil ⇔ inFlightMessages non-nil.
	as.inFlightMessages = nil
	as.isReady.Store(true)
	stop := as.readpumpStop
	as.asMu.Unlock()

	// Best-effort persistence of the cleared state. Mirrors the
	// Submit-side comment: must not roll back the prompt-end
	// event we just queued. Failures fall through — the next
	// status transition will retry. See Submit for the
	// Submit↔endPrompt persist race window rationale.
	if as.persist != nil {
		if err := as.persist(as.Entry()); err != nil {
			slog.Warn("agentsession: persist after endPrompt failed; entry may be stale on restart",
				"as_id", as.ID, "reason", reason, "err", err)
		}
	}

	// Push KindPromptEnded event. Honor readpumpStop so a Shutdown
	// racing us can interrupt the push (otherwise Shutdown would
	// deadlock on readpumpDone while we're blocked on eventQueue).
	as.ensureDispatcher()
	select {
	case as.eventQueue <- EnrichedEvent{
		ChatID:         as.ChatSessionID,
		AgentSessionID: as.ID,
		UserMsgID:      p.LastMessageID,
		PromptID:       p.ID,
		Prompt:         p,
		Kind:           KindPromptEnded,
		PromptEnded: &PromptEndedChange{
			EndedAt:   p.EndedAt,
			EndReason: reason,
		},
	}:
	case <-stop:
		// shutdown wins; event lost
	}
}

// EndPromptForTest is the public version of endPrompt for tests
// that need to simulate prompt-end events. Same semantics as the
// internal endPrompt — pushes a KindPromptEnded event and clears
// currentPrompt.
func (as *AgentSession) EndPromptForTest(reason PromptEndReason) {
	as.endPrompt(reason)
}

// EndPrompt (fix-bridge-stuck) is the public, control-class entry
// point for ending the in-flight Prompt. The /stop and /close
// command paths drive this synchronously so the local state
// machine (IsReady=true, currentPrompt=nil) updates without
// waiting for the bridge protocol to emit a terminal event. See
// StopSelectedAgent (internal/command/stop) and AgentSession.Close
// for the call paths; behavior is identical to the internal
// endPrompt — push KindPromptEnded onto eventQueue, interrupt
// on readpumpStop to avoid deadlock with Shutdown.
//
// Internal callers should keep using endPrompt directly; this is
// just the exported surface for cross-package control commands.
func (as *AgentSession) EndPrompt(reason PromptEndReason) {
	as.endPrompt(reason)
}

// StartReadPumpForTest is the public test-only version of
// startReadPump. Idempotent (gated by readpumpStarted). Production
// code MUST NOT use this — Spawn / Activate start the readpump.
func (as *AgentSession) StartReadPumpForTest() {
	as.startReadPump()
}
