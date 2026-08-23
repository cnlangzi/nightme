// emitter_sink.go — RunOnce/Review event sink helpers.
//
// StreamRunOnceToEmitter is the canonical pattern for callers that
// want to surface a Starter.RunOnce / Starter.Review call's
// intermediate AgentEvents to a chat channel via the outbound
// Emitter. It bridges two concerns with opposite threading models:
//
//   - The bridge (dsh, acp, …) drives its event drain on its own
//     goroutine. AgentEvent.Agent().Events() is consumed synchronously
//     there; calling a slow sink would stall the bridge.
//   - The Emitter (Feishu, telegram, …) may itself block — e.g.
//     Feishu rate-limits card sends. It MUST NOT be called on the
//     bridge goroutine.
//
// The pattern:
//
//	┌─────────────────┐    chan AgentEvent     ┌──────────────────┐
//	│  bridge drain   │ ───────────────────► │  drain goroutine  │
//	│  (sink callback)│   buffered (cap=64)  │ (Translate+Send)  │
//	└─────────────────┘                       └──────────────────┘
//
// The bridge sees a non-blocking enqueue (drops via select on
// ctx.Done if the chan is full + ctx is cancelled, which is the
// well-defined backpressure signal). The drain goroutine runs at
// its own pace and translates every event through outbound.Translate
// before handing off to the Emitter.
//
// IMPORTANT: StreamRunOnceToEmitter returns the sink callback. The
// caller passes it as agent.WithEventSink(...) to Starter.RunOnce /
// Starter.Review. The drain goroutine stays alive until ctx is
// cancelled (or the process exits). Tying the goroutine to ctx is
// intentional: for /gtw commit and /gtw pr the ctx outlives the
// one-shot call (it carries timeouts.Agent), so the drain finishes
// naturally on return.
//
// One-shot calls run with full-access permission mode (no
// Permission event handling), so the drain never has to wait for
// user response — Translate drops Permission to OutChoice /
// permission-set cards and the user can act on them, but the
// bridge's drain does NOT block on ResponseCh.
package outbound

import (
	"context"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// sinkBufferSize is the capacity of the bridge → drain chan. 64
// events covers a typical tool-heavy turn (Ready + several text
// chunks + tool start/end pairs + Result + Done). Overflow drops
// the event with a debug log — the bridge never blocks.
const sinkBufferSize = 64

// StreamRunOnceToEmitter returns a sink callback that forwards every
// AgentEvent the bridge emits during a RunOnce / Review call to
// em (after Translate). chatID / replyTo / agentName are stamped
// onto every translated OutboundMessage so the user sees a coherent
// thread. ctx controls the drain goroutine's lifetime — cancel
// it to stop draining.
//
// The returned sink is non-blocking from the bridge's perspective:
// when the internal chan is full and ctx is not cancelled, the
// sink logs and drops the event (debug level) so the bridge's
// wire parser / drain loop never stalls on a slow channel.
//
// Returns nil when em is nil — callers may use this to avoid
// guarding every call site.
func StreamRunOnceToEmitter(ctx context.Context, em Emitter, chatID, replyTo, agentName string) func(agent.AgentEvent) {
	if em == nil {
		return func(agent.AgentEvent) {}
	}

	ch := make(chan agent.AgentEvent, sinkBufferSize)

	// Drain goroutine: pulls from the bridge's sink-chan,
	// translates to OutboundMessage, and hands off to the Emitter.
	// Decoupled from the bridge's drain loop so the bridge never
	// waits on the Emitter (Feishu rate-limits, etc.).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				dispatchSinkEvent(ctx, em, chatID, replyTo, agentName, ev)
			}
		}
	}()

	// Sink callback handed to the bridge. Synchronous on the
	// bridge's goroutine; non-blocking on the internal chan.
	return func(ev agent.AgentEvent) {
		select {
		case <-ctx.Done():
			// Bridge context cancelled; the drain goroutine has
			// already exited (or is about to). Drop silently — the
			// bridge's defer Close will fire and tear down the
			// session regardless.
		case ch <- ev:
			// Common path: event queued for drain.
		default:
			// Bridge is faster than the Emitter. Drop with a
			// debug log so a slow / stalled Emitter doesn't stall
			// the bridge's wire parser / drain loop. The terminal
			// EventAgentResult always gets through (it's the last
			// event before the bridge returns and its drain loop
			// exits) — Translate still sees it on the same
			// goroutine via deliverToSink.
			slog.Default().Debug("outbound: sink buffer full; dropping event",
				"kind", ev.Kind.String(),
				"chat_id", chatID,
			)
		}
	}
}

// dispatchSinkEvent translates one AgentEvent to an OutboundMessage
// and emits it. Mirrors the runtime.NewEventHandler path so the
// chat sees the same shape for one-shot / review calls as it does
// for primary chat sessions.
func dispatchSinkEvent(ctx context.Context, em Emitter, chatID, replyTo, agentName string, ev agent.AgentEvent) {
	out, ok := Translate(chatID, ev)
	if !ok {
		return // Translate drops events that don't surface to the channel
	}
	out.ReplyTo = replyTo
	if out.AgentName == "" {
		out.AgentName = agentName
	}
	if err := em.Send(context.Background(), out); err != nil {
		// Channel-side errors are not the caller's problem — log
		// and continue. The bridge's RunResult carries the text
		// independently so /gtw commit still has its outcome even
		// if a few intermediate cards fail to render.
		slog.Default().Warn("outbound: sink send failed",
			"kind", ev.Kind.String(),
			"chat_id", chatID,
			"err", err.Error())
	}
	_ = ctx // ctx is reserved for future per-event cancellation hooks
	_ = messages.OutboundMessage{} // import anchor — messages is used by Translate above
}
