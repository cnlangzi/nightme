// Package inbound — tryActionDispatch: routes msg.Reaction to
// the wired services.ReactionRouter, and msg.Action (card button
// clicks) to Manager.SendPermission for pending approvals /
// AskUserQuestion.
//
// Priority 1 because action events carry empty Text — letting
// them fall through to tryCommandDispatch would either
// consume them as plain text (matched nothing) or as an empty
// slash command (also nothing), and the gtw draft pipeline
// would never see its reaction.
//
// For non-action messages returns (false, nil, nil) so the
// chain continues to the next tryDispatch.
//
// F-59: the actual dispatchAction call now runs in a spawned
// goroutine so reaction handling (which may invoke a card
// PATCH through Emitter.Send (Kind=OutChoicePatch) or call into a downstream
// gtw / permission handler) never blocks the dispatchLoop
// monitor. The sync pattern check (Reaction / Action != nil)
// stays inline so the chain's handled decision is synchronous.
package inbound

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
)

// tryActionDispatch claims Reaction / Action events. The
// pattern (msg.Reaction != nil || msg.Action != nil) lives
// HERE so the chain itself never inspects the message shape.
// Match is synchronous; actual dispatch runs in a goroutine.
func (r *Router) tryActionDispatch(ctx context.Context, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	if msg.Reaction == nil && msg.Action == nil {
		return false, nil, nil
	}
	// F-59 async dispatch — ReactionRouter.Handle may invoke a
	// long path (gtw draft lookup + handler chain, or future
	// permission / interactive-prompt handlers). Running it
	// sync would let a misbehaving handler block the entire
	// dispatchLoop; the goroutine keeps the monitor unblocked
	// while still letting gtw reaction cards drive their
	// downstream state machine in the background.
	r.execWg.Add(1)
	// F-59 ctx policy: goroutine outlives inbound-ctx cancellation
	// (daemon shutdown). See inbound/command.go's tryCommandDispatch
	// for the full rationale — same shell.Dispatcher pattern.
	go func() {
		defer r.execWg.Done()
		r.runAction(context.Background(), msg)
	}()
	return true, &CommandResult{Consumed: true}, nil
}

// runAction is the goroutine body for action dispatch. Walks
// the same routing logic as the pre-F-59 dispatchAction, but
// does not return its result to the dispatch chain — the
// action branch's only output is a (consumed=true) signal
// and any reply that needs to surface is the downstream
// ReactionRouter handler's responsibility (gtw, permission,
// etc., post their own cards via the wired Emitter).
//
// ReactionRouter handlers are intentionally synchronous from
// the handler's POV — they receive a ReactionEvent and decide
// whether to consume it; the goroutine is only here to
// isolate the dispatchLoop from handler latency.
//
// runAction is panic-safe (the pre-F-59 dispatchAction had
// no such guard; F-59 adds it so a panic in gtw's
// HandleReaction can't take down the daemon). Matches the
// behaviour of runCommand / runMessage — slog.Default().Error
// surfaces the panic to the daemon log so it's never silent.
func (r *Router) runAction(ctx context.Context, msg *messages.InboundMessage) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("inbound: runAction panicked",
				"chat_id", msg.ChatID, "panic", rec)
		}
	}()
	r.dispatchAction(ctx, msg)
}

// dispatchAction hands the reaction / action to the wired
// ReactionRouter. Returns a consumed result with Dropped=true
// when no router is wired — that's the v1 pre-F-45 default
// and lets the runtime come up before the reaction branch is
// ready.
//
// A handler returning consumed=false still results in
// Consumed=true (DispatchInbound has "owned" the event;
// re-routing it to dispatchMessage would either send a
// confusing empty text to the agent or re-enter the WatchMode
// gate).
//
// Kept as a separate method (vs inlining into runAction) so
// the routing logic is testable without spawning goroutines,
// and so any future sync caller (tests, debug fixture) can
// still invoke it directly.
func (r *Router) dispatchAction(ctx context.Context, msg *messages.InboundMessage) *CommandResult {
	if msg.Action != nil {
		r.dispatchPermissionClick(msg)
		return &CommandResult{Consumed: true}
	}
	router, ok := r.requireAction()
	if !ok {
		return &CommandResult{Consumed: true, Dropped: true}
	}
	if router.Handle(ctx, msg.ChatID, *msg.Reaction) {
		return &CommandResult{Consumed: true}
	}
	return &CommandResult{Consumed: true, Dropped: true}
}

// permissionSender is the optional chatsession.Manager surface used
// to feed a card-button label back to the selected agent's
// SendPermission. Declared here so inbound.MessageHandler does not
// grow a method that PTY/test stubs never need; production Manager
// implements it.
type permissionSender interface {
	SendPermission(chatID, option string) error
}

// dispatchPermissionClick routes a Feishu opt:<label> (or other
// channel Action.Option) to the selected agent's pending
// approval / AskUserQuestion. Does not fall through to
// HandleInbound — that would enqueue a new prompt while dsh is
// still blocked on question/requested.
func (r *Router) dispatchPermissionClick(msg *messages.InboundMessage) {
	option := strings.TrimSpace(msg.Action.Option)
	if option == "" {
		slog.Default().Warn("inbound: card action missing option",
			"chat_id", msg.ChatID)
		return
	}
	sender, ok := r.csMgr.(permissionSender)
	if !ok {
		slog.Default().Warn("inbound: MessageHandler does not support SendPermission",
			"chat_id", msg.ChatID)
		return
	}
	if err := sender.SendPermission(msg.ChatID, option); err != nil {
		slog.Default().Warn("inbound: SendPermission failed",
			"chat_id", msg.ChatID, "err", err)
	}
}
