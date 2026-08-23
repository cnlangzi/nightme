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

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
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
//
// F-CODEX-RUNONCE-REVIEW-EVENT: cs + logger are threaded so
// dispatchSinkEvent can apply the same DefaultPolicies chain
// (think gate / tools gate) and HeartbeatTracker.Observe that
// the long-lived runtime.NewEventHandler applies. Both paths
// call outbound.DefaultPolicies / cs.Heartbeat() directly — single
// source of truth. Pass cs=nil + logger=nil to fall back to the
// pre-Plan-B behavior (no gates, no heartbeat observe).
func StreamRunOnceToEmitter(
	ctx context.Context,
	em messages.Emitter,
	cs *chatsession.ChatSession,
	logger *slog.Logger,
	chatID, replyTo, agentName string,
) func(agent.AgentEvent) {
	if em == nil {
		return func(agent.AgentEvent) {}
	}

	ch := make(chan agent.AgentEvent, sinkBufferSize)

	// Drain goroutine: pulls from the bridge's sink-chan,
	// translates to OutboundMessage, and hands off to the Emitter.
	// Decoupled from the bridge's drain loop so the bridge never
	// waits on the Emitter (Feishu rate-limits, etc.).
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				dispatchSinkEvent(ctx, em, cs, logger, chatID, replyTo, agentName, ev)
			}
		}
	}()

	// Sink callback handed to the bridge. Synchronous on the
	// bridge's goroutine; non-blocking on the internal chan.
	//
	// Backpressure policy: when the internal chan is full we DROP
	// the event and log at debug level. The terminal
	// EventAgentResult is therefore NOT guaranteed to reach the
	// Emitter under heavy load — only the bridge's RunResult
	// Text carries that, so callers must not rely on observing
	// the result via the sink alone. This trade-off keeps the
	// bridge's wire parser / drain loop from blocking on a slow
	// Feishu card send.
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
// for primary chat sessions. cs may be nil (one-shot without a
// chat session) — the policy chain short-circuits and the
// heartbeat observe is skipped, matching pre-Plan-B behavior.
func dispatchSinkEvent(
	ctx context.Context,
	em messages.Emitter,
	cs *chatsession.ChatSession,
	logger *slog.Logger,
	chatID, replyTo, agentName string,
	ev agent.AgentEvent,
) {
	out, ok := Translate(chatID, ev)
	if !ok {
		return // Translate drops events that don't surface to the channel
	}
	out.ReplyTo = replyTo
	if out.AgentName == "" {
		out.AgentName = agentName
	}

	// F-CODEX-RUNONCE-REVIEW-EVENT: apply the same think / tools
	// gate the long-lived runtime.NewEventHandler applies
	// (shared via outbound.DefaultPolicies — moved out of runtime
	// in this change).
	//
	// Order: Translate → Observe (heartbeat) + OutHeartbeat
	// follow-up emit → Policy gate → em.Send. The Observe-before-
	// Policy invariant is what makes /think off / /tools off
	// still increment counters in the receipt header (F-63 §3.2)
	// — even when the gate drops the rendering, the counter
	// reflects the agent's real activity. Mirrors the runtime
	// handler's order exactly (runtime/handler.go:219-250).
	if cs != nil {
		env := agentsession.AgentEventEnvelope{
			ChatID:    chatID,
			UserMsgID: replyTo,
		}
		// 1. Observe FIRST — heartbeat counter increments even when
		//    the policy gate below drops the message.
		if hb := cs.Heartbeat(); hb != nil && replyTo != "" {
			if hb.Observe(replyTo, out.Kind) {
				snap := hb.Snapshot(replyTo)
				if !snap.Empty() {
					_ = em.Send(ctx, messages.OutboundMessage{
						ChatID:    chatID,
						Kind:      messages.OutHeartbeat,
						ReplyTo:   replyTo,
						Heartbeat: &snap,
					})
				}
			}
		}
		// 2. THEN policy gate — may drop the message; the heartbeat
		//    update above already reached the channel.
		for _, pol := range DefaultPolicies(chatsession.GitStatusDeps{}, cs, logger) {
			if pol.Apply(&out, env) {
				return // dropped by gate; counter already incremented
			}
		}
	}

	if err := em.Send(ctx, out); err != nil {
		// Channel-side errors are not the caller's problem — log
		// and continue. The bridge's RunResult carries the text
		// independently so /gtw commit still has its outcome even
		// if a few intermediate cards fail to render.
		slog.Default().Warn("outbound: sink send failed",
			"kind", ev.Kind.String(),
			"chat_id", chatID,
			"err", err.Error())
	}
}
