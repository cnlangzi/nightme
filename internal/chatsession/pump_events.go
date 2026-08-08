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
	"reflect"
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
// F-54 fix (2026-08-09): PumpEvents now drains the eventQueue of
// EVERY running AS in the pool, not just activeAS. The previous
// shape (read from activeAS.Events() only) caused a producer/
// consumer deadlock when the user had two running ASes (e.g. /use
// pi → /use claude → /new). The non-active AS's per-AS readpump
// kept pushing to its eventQueue, but no consumer was draining
// it. The eventQueue filled (cap 256), the per-AS readpump
// blocked on push, the bridge's events channel filled (cap 64),
// the bridge's deliver hit its 1s timeout and dropped events —
// including the post-/new EventAgentReady the runtime was waiting
// on. The bridge's new_session RPC then timed out at 10s.
//
// With reflect.Select on every AS's Events() channel, the
// consumer is always moving the buffer regardless of which AS
// is active. The per-AS readpump never blocks, the bridge's
// events channel never fills, and the deliver never drops.
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
		// Snapshot the pool under RLock; the select reads from
		// a fixed list of channels for one iteration. Any AS
		// spawned AFTER this snapshot is picked up on the next
		// iteration (within ~100ms latency on the slow path).
		cs.mu.RLock()
		ases := make([]*AgentSession, 0, len(cs.pool))
		for _, as := range cs.pool {
			ases = append(ases, as)
		}
		cs.mu.RUnlock()

		if len(ases) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		// Build a select with one case per AS's Events() channel,
		// plus ctx.Done() at index 0. reflect.Select is the runtime
		// primitive that lets a runtime-sized slice of channels be
		// waited on without a fixed-cap select.
		cases := make([]reflect.SelectCase, 0, len(ases)+1)
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		})
		for _, as := range ases {
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(as.Events()),
			})
		}

		chosen, value, ok := reflect.Select(cases)
		if chosen == 0 {
			return // ctx.Done()
		}
		as := ases[chosen-1]
		if !ok {
			// The AS closed its eventQueue (Shutdown / process
			// death). Drop it from the pool so we don't re-select
			// on the closed channel on the next iteration.
			cs.mu.Lock()
			delete(cs.pool, agentCwdKey{Agent: as.Agent, Cwd: as.Cwd})
			cs.mu.Unlock()
			continue
		}
		ev, _ := value.Interface().(EnrichedEvent)
		cs.routeEvent(ev)
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