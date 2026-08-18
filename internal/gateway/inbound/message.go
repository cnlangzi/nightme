// Package inbound — tryMessageDispatch: the universal fallback.
// Always claims (handled=true) so the chain always terminates
// with a non-nil result.
//
// The "default branch" of the inboundDispatcher: plain text,
// attachments, unrecognised "/foo" — anything not consumed
// by action / command / shell. Hands the message to
// chatsession.Manager.HandleInbound (PR3b), which owns the
// WatchMode gate + per-chat GetOrCreate + AgentSession
// resolution + InputBuffer queue. The full message-handling
// pipeline lives next to its state; the dispatcher just
// points at it.
//
// F-59: HandleInbound runs in a spawned goroutine so a slow
// agent turn (claude / codex streaming a long response) no
// longer blocks the dispatchLoop monitor. Pre-F-59, every
// inbound message waited synchronously for HandleInbound to
// resolve (which in turn only enqueued the message — the
// actual turn latency was off the readPump path) — but the
// dispatchLoop still paid for the GetOrCreate / WatchMode
// gate / InputBuffer queue-allocation cost inline. Moving
// the call into a goroutine keeps that off the hot path and
// matches the async treatment of tryCommand / tryAction.
package inbound

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// tryMessageDispatch is the universal fallback. Always
// claims (handled=true) so the chain always terminates with a
// non-nil result, but the actual HandleInbound call runs in a
// spawned goroutine (F-59) so a slow inbound pipeline can
// never block the monitor.
func (r *Router) tryMessageDispatch(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	// F-59 async dispatch — HandleInbound may resolve a
	// ChatSession (GetOrCreate), evaluate WatchMode, walk the
	// InputBuffer FSM, and resolve / spawn an AgentSession.
	// Most of that is fast but it's not free; running it in a
	// goroutine keeps the monitor loop tight under inbound
	// load (chatty groups, multi-channel storms).
	r.execWg.Add(1)
	// F-59 ctx policy: goroutine outlives inbound-ctx cancellation
	// (daemon shutdown). See inbound/command.go's tryCommandDispatch
	// for the full rationale.
	//
	// v1.3+ multi-channel: capture the per-call mgr so the
	// goroutine routes HandleInbound to the channel's own
	// chatsession.Manager (which carries the per-channel
	// Emitter). Using r.csMgr here would silently black-hole
	// every message from non-primary channels.
	localMgr := mgr
	go func() {
		defer r.execWg.Done()
		r.runMessage(context.Background(), localMgr, msg)
	}()
	// Consumed=false (zero value of the struct) is the v0.x
	// contract: the message branch is a "fire and forget" for
	// the agent loop, not a claim like a slash command reply.
	// Tests in dispatch_fallthrough_test.go + dispatch_shell_test.go
	// rely on this signal to distinguish "the message reached
	// the agent" from "a slash command claimed it". Reply is
	// always empty (the agent loop emits its own responses via
	// the readPump, never through dispatchLoop).
	return true, &CommandResult{}, nil
}

// runMessage is the goroutine body for the message branch.
// Calls csMgr.HandleInbound and logs any error (matches the
// pre-F-59 warn-and-continue behaviour from tryMessageDispatch).
//
// Panic-safe so a misbehaving ChatSession.HandleInbound can't
// crash the daemon — same guard F-59 adds to the other run*
// helpers.
func (r *Router) runMessage(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("inbound: runMessage panicked",
				"chat_id", msg.ChatID, "panic", rec)
		}
	}()
	// v1.3+ multi-channel: use the per-channel mgr passed in
	// rather than r.csMgr (which is the noOpMgr stub set in
	// inbound.New). The per-channel mgr has the per-channel
	// Emitter wired, so cs.Emitter() inside HandleInbound's
	// error paths posts to the right channel.
	if err := mgr.HandleInbound(ctx, msg); err != nil {
		slog.Default().Warn("inbound: HandleInbound failed",
			"chat_id", msg.ChatID, "err", err)
	}
}