// Package inbound — tryShellDispatch: mirrors tryCommandDispatch
// for the feat-shell "!" route.
//
// Priority 3 (between command and message dispatch): slash
// takes priority over shell — a "/foo" should never
// accidentally hit the shell, even though the text would also
// fail parseShell.
//
// The shell package owns its own prefix detection
// (parseShell); we call Dispatcher.Handle and translate the
// result. The shim — translation *messages.InboundMessage →
// shell.InboundRequest — is one struct literal.
package inbound

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/shell"
)

// tryShellDispatch claims !-prefixed text via the wired
// shell.Dispatcher. Like tryCommandDispatch, the prefix check
// lives in the dispatch target (parseShell) — we just
// translate and call.
//
// GetOrCreate failure (chat not yet known, registry not
// loaded) falls through to tryMessageDispatch so the user
// can still talk to the agent. Same contract as the v0.x
// runtime shim.
func (r *Router) tryShellDispatch(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	sh, ok := r.requireShell()
	if !ok {
		return false, nil, nil
	}
	cs, err := r.csMgr.GetOrCreate(msg.ChatID, r.primary)
	if err != nil || cs == nil {
		slog.Default().Warn("inbound: GetOrCreate failed in tryShellDispatch",
			"chat_id", msg.ChatID, "err", err)
		return false, nil, nil
	}
	result, handled := sh.Handle(r.csMgr, cs, shell.InboundRequest{
		Request: shell.Request{
			Text: msg.Text,
			Cwd:  cs.SelectedCwd(),
		},
		ChatID:    msg.ChatID,
		MessageID: msg.MessageID,
	})
	// Same fall-through contract as tryCommandDispatch: a
	// non-claiming shell result (handled=false) means "not a
	// shell command" and the chain continues to tryMessageDispatch.
	// shell.Handle guarantees result != nil whenever handled=true,
	// so the dereference is safe.
	if !handled || result == nil || !result.Consumed {
		return false, nil, nil
	}
	return true, &CommandResult{Consumed: true}, nil
}
