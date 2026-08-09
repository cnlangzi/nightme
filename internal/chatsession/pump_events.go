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

// detachAgentSubscription removes the subscription record for as.
// Idempotent. The actual bus handler still fires on Publish until
// the AS is shut down (in practice Shutdown follows this call and
// closes the bus).
func (cs *ChatSession) detachAgentSubscription(as *AgentSession) {
	if as == nil {
		return
	}
	cs.mu.Lock()
	delete(cs.subs, as.ID)
	cs.mu.Unlock()
}

// detachAgentSubscriptionLocked is detachAgentSubscription without
// the lock acquire. MUST be called with cs.mu held (write).
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
		// After a PromptEnds, the AS is ready again. TryFlush
		// will pick up the queued messages if any.
		_ = cs.TryFlush()
	case KindLifecycle:
		if ev.Lifecycle != nil && ev.Lifecycle.Status == StatusExited {
			as.SetExited(0)
			slog.Info("chatsession: AS marked Exited (claude process exited)",
				"chat_id", ev.ChatID, "as_id", ev.AgentSessionID)
		}
	}
}