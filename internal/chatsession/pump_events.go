// Package chatsession — PumpEvents + routeEvent (multi-as Phase 1).
//
// PumpEvents is the CS-side main loop. It periodically scans the
// pool and attaches an EventBus subscription to every AgentSession
// that doesn't have one yet. The subscriber forwards EnrichedEvents
// to routeEvent.
//
// Lane: per-goroutine, driven by cmd/nightme/run.go.
package chatsession

import (
	"context"
	"log/slog"
	"time"
)

// PumpEvents (multi-as Phase 1) is the CS-side main loop. It
// subscribes to every AgentSession's EventBus (via
// attachAgentSubscription) and routes EnrichedEvents to routeEvent
// as subscribers receive them. Returns when ctx is cancelled.
//
// The scan is a cheap idle loop (100ms) when the pool is stable;
// subscription is idempotent so the periodic scan is safe.
func (cs *ChatSession) PumpEvents(ctx context.Context) {
	cs.attachAllPendingSubscriptions()

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
			cs.attachAllPendingSubscriptions()
		}
	}
}

// attachAllPendingSubscriptions ensures every AS in the pool has an
// active EventBus subscription. Idempotent.
func (cs *ChatSession) attachAllPendingSubscriptions() {
	cs.mu.RLock()
	ases := make([]*AgentSession, 0, len(cs.pool))
	for _, as := range cs.pool {
		ases = append(ases, as)
	}
	cs.mu.RUnlock()

	for _, as := range ases {
		cs.attachAgentSubscription(as)
	}
}

// attachAgentSubscription installs one EventBus subscription on as
// (if not already present) that forwards every EnrichedEvent to
// cs.routeEvent. Idempotent.
func (cs *ChatSession) attachAgentSubscription(as *AgentSession) {
	if as == nil || as.EventBus == nil {
		return
	}
	cs.mu.Lock()
	if _, ok := cs.subs[as.ID]; ok {
		cs.mu.Unlock()
		return
	}
	cs.subs[as.ID] = struct{}{}
	cs.mu.Unlock()

	// Subscribe OUTSIDE cs.mu.
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		cs.routeEvent(as, ev)
		return false
	})
}

// detachAgentSubscriptionLocked removes the subscription record for
// as. Idempotent. MUST be called with cs.mu held (write). The
// actual bus handler still fires on Publish until the AS is
// shut down (in practice Shutdown follows this call and closes
// the bus).
//
// The non-Locked wrapper that lived here was deleted in F-46:
// every caller is already under cs.mu (DropAgentSession's lock
// section), so the `Lock()/Unlock()` pair inside the wrapper
// was a no-op that round-tripped the same mutex.
func (cs *ChatSession) detachAgentSubscriptionLocked(as *AgentSession) {
	if as == nil {
		return
	}
	delete(cs.subs, as.ID)
}

// routeEvent dispatches a single EnrichedEvent to the appropriate
// handler. The AgentSession source is provided by the subscription
// closure; no second pool lookup is needed.
//
// Concurrency: handlers may run synchronously from EventBus.Publish,
// which is invoked from the AS dispatcher goroutine. routeEvent
// must NOT block on slow channel.Send operations — it publishes
// onto ChatSession's business buses (AgentEventBus /
// PromptEndBus / MessageStateBus), which the runtime subscribes to
// and handles asynchronously.
func (cs *ChatSession) routeEvent(as *AgentSession, ev EnrichedEvent) {
	if as == nil {
		return
	}
	switch ev.Kind {
	case KindAgentEvent:
		if ev.AgentEvent == nil {
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
		cs.writebackMessageState(as, ev.Prompt)
		// F-61: arm HungPrompt + clear suspect on clean turn end.
		// The synchronous respawn path (KindLifecycle below) handles
		// bridge deaths; this timer catches the rarer "bridge alive
		// but hung" case where no event ever fires.
		cs.Watchdog().disarmHungPrompt()
		as.ClearSuspect()
		// After a PromptEnds, the AS is ready again. TryFlush
		// will pick up the queued messages if any.
		_ = cs.TryFlush()
	case KindLifecycle:
		if ev.Lifecycle != nil && ev.Lifecycle.Status == StatusExited {
			as.SetExited(0)
			// F-61: disarm HungPrompt. The bridge died without
			// emitting KindPromptEnded, so the 5min timer armed
			// at Submit is still live. Without this, it would
			// fire later and mark the freshly-respawned bridge
			// as Suspect("hung_prompt") — wrong: the respawned
			// bridge just started, the previous one died.
			cs.Watchdog().disarmHungPrompt()
			slog.Info("chatsession: AS marked Exited (claude process exited)",
				"chat_id", ev.ChatID, "as_id", ev.AgentSessionID)
			// F-61: immediate respawn right after marking Exited.
			// Synchronous spawn — the new bridge starts with
			// --resume <sessionID> and re-processes the
			// in-flight user message from its own JSONL
			// history. No need to wait for the async watchdog
			// prober. The user sees a brief delay (1-2s for
			// fork+exec+handshake) and then a normal reply.
			//
			// /close path is excluded via as.closedByUser
			// (set by Close()). Respawn failures fall through
			// to the watchdog/prober retry under the 5min
			// cooldown.
			if cs.spawner != nil {
				if err := as.RestartFromDeath(cs.ctx, cs.spawner); err != nil {
					slog.Warn("chatsession: immediate respawn after bridge death failed; watchdog will retry",
						"chat_id", ev.ChatID, "as_id", ev.AgentSessionID,
						"err", err)
					as.SetSuspect("immediate_respawn_failed")
				} else {
					// F-61: drain queue with the freshly-
					// respawned AS. The in-flight message is
					// already covered by --resume on the
					// bridge side; any further queued user
					// messages (submitted after this death
					// was detected but before respawn
					// finished) need TryFlush to land.
					_ = cs.TryFlush()
				}
			} else {
				slog.Warn("chatsession: cannot respawn after bridge death — no spawner on chat session",
					"chat_id", ev.ChatID, "as_id", ev.AgentSessionID)
				as.SetSuspect("immediate_respawn_no_spawner")
			}
		}
	}
}