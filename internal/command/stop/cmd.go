// Package stop implements the `/stop` slash command.
//
// /stop halts execution of the in-flight turn on the chat's
// selectedAgent. Distinct from /close:
//
//   - /close — cwd-scoped (every AgentSession whose Cwd ==
//     activeCwd), destructive (terminates processes +
//     drops AgentSession entries from the pool +
//     agent_sessions.json). Next message triggers a
//     fresh spawn via the configured Spawner.
//   - /stop  — singleAgent-scoped (only the selectedAgent),
//     non-destructive (signals the bridge to stop its
//     current work; the AgentSession entry stays in
//     the pool; the chat layer's TryFlush picks up the
//     next queued prompt once the bridge settles).
//
// Use /stop for "I changed my mind / want to redirect" — keep the
// session, drop the in-flight generation. Use /close for "this
// session is wedged / I want a clean slate".
//
// Factory holds *chatsession.Manager directly.
package stop

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Factory is the command.SlashCommandFactory for /stop.
type Factory struct{}

// NewFactory constructs a Factory. command/* factories do not
// receive a *chatsession.Manager — cs comes from the dispatcher
// parameter at Handle time.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

func NewFactory() *Factory {
	return &Factory{}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "stop",
		Summary:  "Stop the in-flight turn on the selected agent (next prompt takes over).",
		Usage:    "/stop",
		Category: "session",
	}
}

// stopSpec declares /stop's argv grammar for the shared lexer
// (issue #291): no flags, no positional args. Routing through
// ParseCmdArgs (rather than a bare `len(input.Args) > 1` check)
// buys the flag/positional distinction for free — `/stop --typo`
// now says "unknown flag" while `/stop extra-word` says
// "unexpected positional argument", instead of both collapsing
// into one opaque usage line.
//
// If /stop ever grows a flag, declare it in Flags here; nothing
// else in this handler needs to change.
var stopSpec = command.CmdSpec{
	Name:    "/stop",
	Usage:   "/stop",
	MinArgs: 0,
	MaxArgs: 0,
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//  1. Look up the ChatSession for this chat. Reject if absent.
//  2. RequireActiveCwd preflight (every cmd preflights its own).
//  3. Reject trailing args / any flag (the surface is /stop with
//     no args) via the shared CLI lexer.
//  4. Resolve the selectedAgentSession via chatsession and call
//     StopSelectedAgent. Wrap the per-call Result with
//     FormatStopResult and reply.
//
// Reply kind: OutReply (not OutCommandReply). The /stop outcome
// is the agent's continuation of the in-flight turn — the stop
// signal was sent, the next prompt takes over — so it folds into
// the same rolling-log card the agent is streaming on. Feishu
// PATCHes the placeholder created at MessageQueued time, Slack
// streams into the same turnStream. Mirrors /steer and /queue;
// only /cwd /run /help /kill /agents remain on OutCommandReply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_session", "the chat has no session"))
		return command.Reply(ctx, rt, "No active chat session."), nil
	}

	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("no_cwd", "the command needs an active workspace"))
		return failOut, nil
	}

	// Trailing args are rejected: /stop is single-action with no
	// subcommands. Treating a typo'd arg as "still stop" would
	// mask user error.
	if _, err := command.ParseCmdArgs(input.Args[1:], stopSpec); err != nil {
		chatsession.ReportOutcome(ctx, command.FailedOutcome("usage", "the command line is not valid"))
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	cmd := &Cmd{CS: cs, Ctx: ctx}
	result, err := StopSelectedAgent(cmd)
	if err != nil {
		if errors.Is(err, chatsession.ErrNoSelectedAgent) {
			chatsession.ReportOutcome(ctx, command.FailedOutcome("no_agent", "no agent is selected"))
			return command.OutReply(input,
				"No active agent to stop. Use /use <agent> first."), nil
		}
		chatsession.ReportOutcome(ctx, command.FailedOutcome("error", "the agent could not be stopped"))
		return command.OutReply(input, fmt.Sprintf("Stop failed: %v", err)), nil
	}
	chatsession.ReportOutcome(ctx, stopOutcome(result))
	return command.OutReply(input, FormatStopResult(result)), nil
}

// stopOutcome maps the stop result onto the structured outcome.
//
// The stopped turn belongs to an EARLIER inbound message — that one
// settles as cancelled. This command's own message reports what the
// stop itself did.
func stopOutcome(result Result) messages.Outcome {
	data := map[string]string{}
	if result.Agent != "" {
		data["agent"] = result.Agent
	}
	switch result.Action {
	case "stopped":
		return messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "the agent was stopped",
			Data:    data,
		}
	case "noop":
		return messages.Outcome{
			Status:  messages.OutcomeNoop,
			Summary: "no turn was in flight",
			Data:    data,
		}
	case "not-supported":
		data["reason"] = "not_supported"
		return messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the agent does not support /stop",
			Data:    data,
		}
	default:
		data["reason"] = "error"
		return messages.Outcome{
			Status:  messages.OutcomeFailed,
			Summary: "the agent could not be stopped",
			Data:    data,
		}
	}
}
