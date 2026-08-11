// Package inbound — tryCommandDispatch: routes /-prefixed
// text through the wired command.Commander.
//
// Priority 2: runs after tryActionDispatch so reaction events
// never reach the commander, and before tryMessageDispatch so
// recognised slash commands short-circuit the per-chat
// WatchMode gate (otherwise applied by chatsession inside
// chatsession.Manager.HandleInbound). The gate is a no-op for
// slash commands — recognised commands return here,
// unrecognised /-inputs fall through to the message branch
// with the original text intact.
//
// Unlike the v0.x shim, this implementation calls the
// commander directly. The translation *messages.InboundMessage
// → command.SlashInput happens inline (the field shapes are
// intentionally close to avoid a separate adapter type).
package inbound

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// tryCommandDispatch claims /-prefixed text. The prefix
// detection lives in command.Commander.parseCommand (NOT
// here) — the commander returns handled=false for non-slash
// text, which we translate to (false, nil, nil) so the chain
// continues.
func (r *Router) tryCommandDispatch(ctx context.Context, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	commander, ok := r.requireCommander()
	if !ok {
		return false, nil, nil
	}
	cs, err := r.csMgr.GetOrCreate(msg.ChatID, r.primary)
	if err != nil || cs == nil {
		slog.Default().Warn("inbound: GetOrCreate failed in tryCommandDispatch",
			"chat_id", msg.ChatID, "err", err)
		return false, nil, nil
	}
	out, handled, err := commander.Dispatch(ctx, command.RuntimeServices{
		// RuntimeServices currently carries only Logger + Config
		// (see command/runtime.go). The runtime wires the
		// concrete services it needs (primary agent, …) at
		// construction; here we just pass the same shape.
		// F-58 TODO: thread primary agent through RuntimeServices
		// explicitly; current Commander implementations don't
		// need it (they read cs.SelectedAgent()).
	}, cs, command.SlashInput{
		ChatID:     msg.ChatID,
		UserID:     msg.UserID,
		Text:       msg.Text,
		MessageID:  msg.MessageID,
		HasMention: msg.HasMention,
	})
	if err != nil {
		return true, &CommandResult{Consumed: true, Reply: "❌ " + err.Error()}, nil
	}
	// F-51 fall-through contract:
	//   - handled=false  → input wasn't a slash command at all;
	//                      the chain continues to tryShellDispatch.
	//   - handled=true + Consumed=false  → slash command attempt
	//                      but no factory matched; the chain
	//                      continues to tryShellDispatch (and
	//                      from there to tryMessageDispatch) so
	//                      the original text reaches the agent
	//                      loop. Preserves the pre-F-51
	//                      passthrough characteristic for inputs
	//                      like "/etc/passwd" or "/@everyone".
	if !handled || (out != nil && !out.Consumed && !out.Dropped) {
		return false, nil, nil
	}
	return true, &CommandResult{
		Consumed: out.Consumed,
		Dropped:  out.Dropped,
		Reply:    out.Reply,
	}, nil
}
