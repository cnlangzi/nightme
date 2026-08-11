// Package inbound — tryActionDispatch: routes msg.Reaction /
// msg.Action events through the wired services.ReactionRouter.
//
// Priority 1 because action events carry empty Text — letting
// them fall through to tryCommandDispatch would either
// consume them as plain text (matched nothing) or as an empty
// slash command (also nothing), and the gtw draft pipeline
// would never see its reaction.
//
// For non-action messages returns (false, nil, nil) so the
// chain continues to the next tryDispatch.
package inbound

import (
	"context"

	"github.com/cnlangzi/nightme/internal/messages"
)

// tryActionDispatch claims Reaction / Action events. The
// pattern (msg.Reaction != nil || msg.Action != nil) lives
// HERE so the chain itself never inspects the message shape.
func (r *Router) tryActionDispatch(ctx context.Context, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	if msg.Reaction == nil && msg.Action == nil {
		return false, nil, nil
	}
	return true, r.dispatchAction(ctx, msg), nil
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
func (r *Router) dispatchAction(ctx context.Context, msg *messages.InboundMessage) *CommandResult {
	router, ok := r.requireAction()
	if !ok {
		return &CommandResult{Consumed: true, Dropped: true}
	}
	if msg.Reaction == nil {
		// msg.Action (card button click) is not currently routed
		// through ReactionRouter. Drop silently — the F-46 PATCH
		// path owns that branch.
		return &CommandResult{Consumed: true, Dropped: true}
	}
	if router.Handle(ctx, msg.ChatID, *msg.Reaction) {
		return &CommandResult{Consumed: true}
	}
	return &CommandResult{Consumed: true, Dropped: true}
}
