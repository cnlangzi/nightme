// Package chatsession — ActiveEvents + PumpEvents (CS-AS 边界重构 Phase 1).
//
// ActiveEvents is the CS-side accessor for the active AS's enriched
// event stream. Runtime reads from this to drive event routing.
//
// PumpEvents is the CS-side main loop consumer. It pulls events
// from cs.activeAS.Events() and dispatches by Kind:
//
//   - KindAgentEvent  → runtime event handler (translates to
//                       OutboundMessage via the existing gateway)
//   - KindPromptEnded → writebackMessageState + onPromptEnd
//                       (legacy hook for feishu reaction 🔄 → ✅/❌)
//   - KindLifecycle{StatusExited}
//                     → as.SetExited(0) so the next LookupActiveAgentSession
//                       sees StatusExited (≠ StatusRunning) and falls
//                       through to the spawn-with-resume path. Without
//                       this flip the chat-session reuses a stale
//                       (closed) bridge handle and SendBlocks silently
//                       writes to a broken pipe — the user sees the
//                       "Working..." reaction forever with no response.
//
// Lane: per-goroutine, driven by cmd/nightme/run.go instead of
// the per-CS readpump that was deleted in T13.
package chatsession

import (
	"context"
	"log/slog"
	"time"
)

// ActiveEvents (CS-AS 边界重构 Phase 1) returns the enriched event
// stream for the active AgentSession. Returns nil if no active AS.
//
// The returned channel is closed by the active AS's Shutdown
// (after its readpump exits). When /use switches activeAS, the
// next read lands on the new activeAS's channel — the old
// channel keeps accumulating events for the next time the AS
// becomes active (this is the L1.5 "offline AS tracking" basis).
func (cs *ChatSession) ActiveEvents() <-chan EnrichedEvent {
	cs.mu.RLock()
	as := cs.activeAS
	cs.mu.RUnlock()
	if as == nil {
		return nil
	}
	return as.Events()
}

// PumpEvents (CS-AS 边界重构 Phase 1) is the CS-side main loop for
// consuming the active AS's EnrichedEvent stream. Run as a
// goroutine from cmd/nightme/run.go after wiring the chat.
//
// Routes:
//   - KindAgentEvent    → cs.AgentEventBus (F-54)
//   - KindPromptEnded   → writebackMessageState + TryFlush
//   - KindLifecycle{StatusExited}
//                       → as.SetExited(0) + lifecycleBus.Publish
//
// Returns when ctx is cancelled (chat shutdown).
//
// Idle backoff (F-54 review fix): when the chat has no active AS
// (between turns, after Shutdown before the next /use, etc.) the
// inner wakeCh() returns immediately, which used to cause a tight
// spin — every idle chat burned one CPU core. We now yield to the
// runtime via time.Sleep between polls. The 100ms latency is well
// below any user-visible event latency (a new message goes through
// QueueUserMessage → TryFlush → submitPump, which is already async
// with respect to PumpEvents).
func (cs *ChatSession) PumpEvents(ctx context.Context) {
	for {
		events := cs.ActiveEvents()
		if events == nil {
			// No active AS; wait briefly for one to appear.
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Active AS closed its eventQueue. Re-evaluate
				// after a short backoff so we don't tight-spin if
				// the AS is being torn down and respawned rapidly.
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			cs.routeEvent(ev)
		}
	}
}

// routeEvent dispatches a single EnrichedEvent to the appropriate
// handler. Centralized so T14 (runtime 改流驱动) has a single
// switch to wire.
func (cs *ChatSession) routeEvent(ev EnrichedEvent) {
	switch ev.Kind {
	case KindAgentEvent:
		// Translation to OutboundMessage is the runtime's job.
		// F-54: fan out via AgentEventBus (services.Bus). The
		// runtime registers its event-translation lambda via
		// cs.AgentEventBus.Subscribe(...). nil-safe: Publish
		// on an empty bus is a no-op.
		if ev.AgentEvent == nil {
			return
		}
		// Look up the AgentSession from the pool by ID (the
		// event carries AgentSessionID as a string for cross-
		// channel safety, but the envelope carries *AgentSession
		// so subscribers don't have to).
		as := cs.lookupAS(ev.AgentSessionID)
		if as == nil {
			return
		}
		cs.AgentEventBus.Publish(AgentEventEnvelope{
			ChatID:       cs.ChatID,
			UserMsgID:    ev.UserMsgID,
			PromptID:     ev.PromptID,
			AgentSession: as,
			Event:        ev.AgentEvent,
		})
	case KindPromptEnded:
		cs.writebackMessageState(ev.Prompt)
		// After a PromptEnds, the AS is ready again. TryFlush
		// will pick up the queued messages if any.
		_ = cs.TryFlush()
	case KindLifecycle:
		// The readpumpLoop emits KindLifecycle{StatusExited} when
		// the bridge's events channel closes (claude exited —
		// natural end of a turn with stdin held open, or an
		// unexpected crash). We MUST flip the AS's Status so the
		// next LookupActiveAgentSession falls through to the
		// spawn-with-resume path; otherwise the chat-session
		// reuses the closed handle and the user sees the
		// "Working..." reaction indefinitely with no OutReply.
		//
		// emitLifecycleLocked (agentsession_readpump.go) only ever
		// emits StatusExited today — there is no other lifecycle
		// transition routed through this event kind yet.
		if ev.Lifecycle != nil && ev.Lifecycle.Status == StatusExited {
			if as := cs.lookupAS(ev.AgentSessionID); as != nil {
				as.SetExited(0)
				slog.Info("chatsession: AS marked Exited (claude process exited)",
					"chat_id", ev.ChatID, "as_id", ev.AgentSessionID)
			}
		}
	}
}

// lookupAS returns the AgentSession with the given ID, or nil if
// not in the pool. Pool scan is O(N) but the pool is small
// (typically 1-3 entries per chat).
func (cs *ChatSession) lookupAS(id string) *AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for _, as := range cs.pool {
		if as.ID == id {
			return as
		}
	}
	return nil
}