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
	"log"
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
//   - KindAgentEvent    → cs.eventHandler (the runtime-installed
//                         callback, kept for F-44 backward compat)
//   - KindPromptEnded   → cs.writebackMessageState(p) +
//                         TryFlush (next batch if queue non-empty)
//   - KindLifecycle{StatusExited}
//                       → as.SetExited(0) — see package doc
//
// Returns when ctx is cancelled (chat shutdown).
func (cs *ChatSession) PumpEvents(ctx context.Context) {
	for {
		events := cs.ActiveEvents()
		if events == nil {
			// No active AS; wait briefly for one to appear.
			select {
			case <-ctx.Done():
				return
			case <-wakeCh():
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Active AS closed its eventQueue. Re-evaluate.
				select {
				case <-ctx.Done():
					return
				case <-wakeCh():
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
		// Phase 1 keeps the existing EventHandler callback (set
		// via SetEventHandler) so we don't break the existing
		// wiring in cmd/nightme/run.go.
		if cs.eventHandler == nil || ev.AgentEvent == nil {
			return
		}
		// Look up the AgentSession from the pool by ID (the
		// event carries AgentSessionID as a string for cross-
		// channel safety, but the legacy EventHandler signature
		// expects *AgentSession).
		as := cs.lookupAS(ev.AgentSessionID)
		if as == nil {
			return
		}
		cs.eventHandler(cs.ChatID, as, *ev.AgentEvent, ev.UserMsgID)
	case KindPromptEnded:
		cs.writebackMessageState(ev.Prompt)
		// After a PromptEnds, the AS is ready again. TryFlush
		// will pick up the queued messages if any.
		_ = cs.TryFlush()
	case KindLifecycle:
		// T-alive (2026-08-07): the readpumpLoop emits
		// KindLifecycle{StatusExited} when the bridge's events
		// channel closes (claude exited — natural at end of a
		// --print single-turn, or unexpected crash). We MUST
		// flip the AS's Status so the next LookupActiveAgentSession
		// falls through to the spawn-with-resume path; otherwise
		// the chat-session reuses the closed handle and the user
		// sees the "Working..." reaction indefinitely with no
		// OutReply. Main branch did this in readpump.go; feat/alive
		// moved the loop into per-AS readpump but missed wiring
		// the consumer side.
		if ev.Lifecycle != nil && ev.Lifecycle.Status == StatusExited {
			if as := cs.lookupAS(ev.AgentSessionID); as != nil {
				as.SetExited(0)
				log.Printf("chatsession: AS %s marked Exited (claude process exited)", ev.AgentSessionID)
			}
		} else {
			log.Printf("chatsession: lifecycle event chat=%s as=%s %v", ev.ChatID, ev.AgentSessionID, ev.Lifecycle)
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

// wakeCh returns a small-channel-then-close pattern used as a
// poll-tick. Avoids busy-spin when no active AS is available.
func wakeCh() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}