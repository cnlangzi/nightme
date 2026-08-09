// Package chatsession — AgentSession event dispatcher (multi-as Phase 1).
//
// The dispatcher is the per-AS goroutine that drains `as.eventQueue`
// (filled by the read pump) and publishes each EnrichedEvent onto
// `as.EventBus`. Subscribers (typically ChatSession's
// receiveAgentEvent) get events via the bus.
//
// Why a separate dispatcher instead of the read pump publishing
// directly: the read pump must not block on a slow subscriber. The
// dispatcher's bounded `eventQueue` already decouples the two, and
// the bus only enqueues handler calls under its own lock (Publish
// is non-blocking with respect to subscribers; see services.EventBus
// docs).
//
// Lifecycle:
//
//   - Started by `startEventDispatch` after construction. Idempotent
//     (subsequent calls are no-ops via dispatchStarted).
//   - Runs until either eventQueue is closed (Shutdown closed it)
//     OR dispatchStop is closed (Shutdown interrupts early).
//   - Returns via `defer close(dispatchDone)`.
package chatsession

// startEventDispatch launches the dispatcher goroutine if not already
// running. Single-launch guard via `dispatchStarted`.
//
// Called from `newAgentSessionRuntime` is NOT possible (goroutine
// launched inside struct literal would race); the dispatcher is
// started lazily by the first publish attempt via ensureDispatcher.
// Safe to call multiple times — only the first call has effect.
func (as *AgentSession) startEventDispatch() {
	as.asMu.Lock()
	if as.dispatchStarted {
		as.asMu.Unlock()
		return
	}
	as.dispatchStarted = true
	as.dispatchStop = make(chan struct{})
	as.dispatchDone = make(chan struct{})
	stop := as.dispatchStop
	as.asMu.Unlock()
	go as.eventDispatchLoop(stop)
}

// ensureDispatcher starts the dispatcher if it has not been started
// yet. Called lazily on the first publish so construction-time state
// stays pure.
func (as *AgentSession) ensureDispatcher() {
	as.asMu.RLock()
	started := as.dispatchStarted
	as.asMu.RUnlock()
	if !started {
		as.startEventDispatch()
	}
}

// eventDispatchLoop is the per-AS dispatcher goroutine.
//
// Lifecycle:
//
//  1. Wait for the dispatcher stop chan (closed by Shutdown).
//  2. Read each EnrichedEvent from `as.eventQueue`.
//  3. Publish onto `as.EventBus` (synchronous fan-out to
//     subscribers — see services.EventBus docs).
//  4. On eventQueue close (!ok), exit after publishing any
//     remaining events.
//
// Shutdown semantics: the inner select honors dispatchStop so a
// Shutdown during Shutdown can interrupt the dispatcher mid-publish.
func (as *AgentSession) eventDispatchLoop(stop chan struct{}) {
	defer close(as.dispatchDone)

	for {
			// Snapshot stop under asMu to avoid a Shutdown closing
			// dispatchStop while we read nil.
			as.asMu.RLock()
			stopCh := as.dispatchStop
			eq := as.eventQueue
			as.asMu.RUnlock()

			if eq == nil {
				return
			}

			select {
			case ev, ok := <-eq:
				if !ok {
					// eventQueue closed; drain complete.
					return
				}
				if as.EventBus != nil {
					as.EventBus.Publish(ev)
				}
			case <-stopCh:
				return
			}
		}
}