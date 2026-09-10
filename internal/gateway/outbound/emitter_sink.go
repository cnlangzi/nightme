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
// the caller's ctx.Done if the chan is full + ctx is cancelled,
// which is the well-defined backpressure signal). The drain
// goroutine runs at its own pace and translates every event
// through outbound.Translate before handing off to the Emitter.
//
// IMPORTANT: StreamRunOnceToEmitter returns a sink callback AND
// a finalize function. The caller passes the sink as
// agent.WithEventSink(...) to Starter.RunOnce / Starter.Review
// and defers finalize() so the terminal OutHeartbeat lands on
// the receipt card BEFORE the dispatcher's defer cancel() fires:
//
//	sink, finalize := outbound.StreamRunOnceToEmitter(ctx, em, cs, ...)
//	defer finalize()                                      // blocks until drain exits
//	res, err := a.RunOnce(ctx, blocks, WithEventSink(sink))
//
// Two contexts matter:
//
//	ctx        caller's; consulted only by the sink callback's
//	           drop check (caller-shutdown backpressure signal).
//	drainCtx   sink-internal; survives caller cancel and is
//	           canceled by finalize() once the channel is drained.
//
// Tying the drain goroutine to the caller's ctx is wrong for
// one-shot dispatchers (/gtw commit, /gtw pr, /review): the
// dispatcher's WithTimeout(ctx, timeouts.Agent) defers cancel(),
// which races the receipt's 300ms PATCH throttle inside
// renderLocked. When defer cancel fires mid-throttle, renderLocked
// returns ctx.Err() and the receipt's terminal ✅ PATCH never
// lands — the card stays stuck on the pre-terminal ⏱ / 💭 / 🔧
// header. Decoupling the drain's ctx from the caller's lets the
// terminal OutHeartbeat finish rendering before dispatchCommit
// returns and the WithTimeout cancel fires.
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
// dropKinds is the set of OutboundKinds the sink should SILENTLY
// skip after Translate + policy gate + heartbeat observe — the
// kind's wire event is still observed for the heartbeat tracker
// (so /think off / /tools off counters stay accurate) and still
// passes through the think/tools gate (so a suppressed kind
// doesn't accidentally leak), but the resulting OutboundMessage
// never reaches em. /gtw pr and /gtw commit pass
// {OutResult} here: their success path produces its own
// result card via replyAgent (OutReply to the receipt), so
// the agent's terminal OutResult card would be a redundant
// duplicate on top of the dispatcher's success card. The agent's
// RunResult.Text (carried on the bridge layer's RunResult, not
// the sink's OutboundMessage) is unaffected, so /gtw pr's
// parsePRReply / /gtw commit's verifyAgentCommitted still see
// the parseable text. All other callers omit dropKinds and the
// existing behavior is preserved.
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
	dropKinds ...messages.OutboundKind,
) (sink func(agent.AgentEvent), finalize func()) {
	if em == nil {
		return func(agent.AgentEvent) {}, func() {}
	}

	ch := make(chan agent.AgentEvent, sinkBufferSize)
	drainDone := make(chan struct{})

	// F-63 follow-up (fix-gtw-command-done): the drain goroutine
	// runs on drainCtx, NOT on the caller's ctx. Decoupling matters
	// for one-shot dispatchers (/gtw commit, /gtw pr, /review)
	// whose defer cancel() at function return would otherwise
	// cancel the terminal OutHeartbeat's renderLocked while it is
	// still in flight. The receipt's 300ms PATCH throttle can keep
	// renderLocked blocked on its timer for hundreds of ms after
	// the last counter bump; defer cancel at dispatcher return
	// races that timer and produces the
	// "feishu receipt: heartbeat render failed err="context
	// canceled"" warning, leaving the receipt card stuck on the
	// pre-terminal ⏱ / 💭 / 🔧 header.
	//
	// The caller's ctx is consulted only by the sink callback's
	// drop check (caller-shutdown backpressure signal). drainCtx
	// is canceled by finalize() once the channel is closed and
	// drained.
	drainCtx, drainCancel := context.WithCancel(context.Background())

	// Drain goroutine: pulls from the bridge's sink-chan,
	// translates to OutboundMessage, and hands off to the Emitter.
	// Decoupled from the bridge's drain loop so the bridge never
	// waits on the Emitter (Feishu rate-limits, etc.).
	go func() {
		defer close(drainDone)
		defer drainCancel()
		for {
			select {
			case <-drainCtx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				dispatchSinkEvent(drainCtx, em, cs, logger, chatID, replyTo, agentName, ev, dropKinds)
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
	sink = func(ev agent.AgentEvent) {
		select {
		case <-ctx.Done():
			// Caller context cancelled; the bridge may still emit
			// but we drop silently. The drain goroutine continues
			// processing any events already enqueued.
		case ch <- ev:
			// Common path: event queued for drain.
		default:
			slog.Default().Debug("outbound: sink buffer full; dropping event",
				"kind", ev.Kind.String(),
				"chat_id", chatID,
			)
		}
	}

	// finalize closes ch (signals drain to drain remaining events
	// and exit) and blocks until the drain goroutine returns.
	// Callers MUST defer finalize() right after the sink is
	// obtained; that way finalize runs when runAgentFor returns,
	// closes the channel, and blocks until the terminal
	// OutHeartbeat's PATCH has landed on the receipt card.
	// Without this guarantee, the dispatcher's defer cancel()
	// could race renderLocked's 300ms throttle timer and cancel
	// the terminal render mid-flight (see drainCtx comment).
	finalize = func() {
		close(ch)
		<-drainDone
	}

	return sink, finalize
}

// dispatchSinkEvent translates one AgentEvent to an OutboundMessage
// and emits it. Mirrors the runtime.NewEventHandler path so the
// chat sees the same shape for one-shot / review calls as it does
// for primary chat sessions. cs may be nil (one-shot without a
// chat session) — the policy chain short-circuits and the
// heartbeat observe is skipped, matching pre-Plan-B behavior.
//
// dropKinds is the set of OutboundKinds the caller wants dropped
// AFTER Translate / policy gate / heartbeat observe but BEFORE
// em.Send. See StreamRunOnceToEmitter's doc for the rationale.
func dispatchSinkEvent(
	ctx context.Context,
	em messages.Emitter,
	cs *chatsession.ChatSession,
	logger *slog.Logger,
	chatID, replyTo, agentName string,
	ev agent.AgentEvent,
	dropKinds []messages.OutboundKind,
) {
	out, ok := Translate(chatID, ev)
	if !ok {
		return // Translate drops events that don't surface to the channel
	}
	out.ReplyTo = replyTo
	if out.AgentName == "" {
		out.AgentName = agentName
	}
	// Identity fallback: when the bridge didn't stamp Model /
	// SessionID / Workspace on the translated event (e.g. the
	// event arrived before the bridge parsed its wire frames),
	// fall back to the chat's current selectedAS. For /gtw
	// run-once this is the chat primary's identity, which
	// matches the placeholder the runtime subscriber already
	// rendered — so streaming chunks stay consistent with the
	// placeholder card. The terminal OutReply for the run
	// carries the run-once agent's identity stamped directly
	// by replyAgent (not through the sink), so the final
	// standalone card has the right identity regardless of
	// what selectedAS points to.
	if cs != nil {
		if as := cs.SelectedAgentSession(); as != nil {
			if out.AgentName == "" {
				out.AgentName = as.Agent
			}
			if out.Model == "" {
				out.Model = as.Model()
			}
			if out.SessionID == "" {
				out.SessionID = as.SessionID()
			}
			if out.Workspace == "" {
				out.Workspace = as.Cwd
			}
		}
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
		// 1. Observe FIRST — heartbeat counter increments AND
		//    terminal verdict (OutResult → Done/Error by msg.Err)
		//    flip happen in the same choke point. Critical for
		//    /gtw commit / /gtw pr: those dispatchers drop
		//    OutResult later in this function (caller opted out
		//    via dropKinds so the dispatcher's own success card
		//    isn't shadowed), but the terminal OutHeartbeat
		//    follow-up emit fires here, BEFORE the drop check —
		//    so the receipt's ⏱ / 💭 N · 🔧 M header still
		//    PATCHes to ✅ Done on turn end. Without this the
		//    GTW receipt would stay "🤖 Working" past the
		//    actual finish.
		if hb := cs.Heartbeat(); hb != nil && replyTo != "" {
			if hb.Observe(replyTo, out) {
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

	if isDroppedKind(out.Kind, dropKinds) {
		// Caller opted out of this kind (e.g. /gtw pr drops
		// OutResult so the dispatcher's success card isn't
		// shadowed by the agent's standalone result card). The
		// Translate / policy / observe work above still ran, so
		// downstream observers stay consistent — only the
		// user-visible surface is suppressed.
		return
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

// isDroppedKind reports whether kind is in the caller's drop set.
// Empty drop set = no drops (the common case). Linear scan is fine
// — drop sets are tiny (typically 0 or 1 element for the /gtw pr /
// commit use case) and the call site is per-event, not per-message.
func isDroppedKind(kind messages.OutboundKind, dropKinds []messages.OutboundKind) bool {
	for _, k := range dropKinds {
		if k == kind {
			return true
		}
	}
	return false
}
