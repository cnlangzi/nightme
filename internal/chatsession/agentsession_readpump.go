// Package chatsession — AgentSession readpump (CS-AS 边界重构 Phase 1).
//
// The readpump is the per-AS goroutine that drains `as.handle.Events()`
// and produces enriched events for ChatSession. Lifecycle:
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
//     eventQueue for ChatSession.
package chatsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

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

	// Phase 2: event loop.
	for {
		select {
		case <-as.readpumpStop:
			return
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
	as.isReady.Store(true)
	stop := as.readpumpStop
	as.asMu.Unlock()

	// Push KindPromptEnded event. Honor readpumpStop so a Shutdown
	// racing us can interrupt the push (otherwise Shutdown would
	// deadlock on readpumpDone while we're blocked on eventQueue).
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
