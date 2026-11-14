// Package inbound — tryReplyClaim: the branch that hands a message
// to the program that owns it.
//
// A message that answers a program's question belongs to that
// program, not to the agent. The claimer takes it, and the agent
// never sees it.
//
// The branch sits after the slash-command and shell branches and
// before the universal fallback: a command is a command, and a
// message nobody claims reaches the agent.

package inbound

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// tryReplyClaim hands the message to the program that owns it.
//
// A claimer that takes the message stops the chain: the message does
// not reach the agent.
//
// The claim is made inline: the claimer looks the message up and
// hands it over, which is quick. The other branches run their work
// in a goroutine because they do real work; this one does not.
func (r *Router) tryReplyClaim(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	claimer, ok := r.requireClaimer()
	if !ok {
		return false, nil, nil
	}
	claimed := false
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Default().Error("inbound: tryReplyClaim panicked",
					"chat_id", msg.ChatID, "panic", rec)
			}
		}()
		claimed = claimer.Claim(ctx, msg)
	}()
	if !claimed {
		return false, nil, nil
	}
	return true, &CommandResult{Consumed: true}, nil
}
