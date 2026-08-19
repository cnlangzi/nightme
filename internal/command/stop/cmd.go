// Package stop implements the `/stop` slash command.
//
// /stop halts execution of the in-flight turn on the chat's
// selectedAgent. Distinct from /close:
//
//   - /close — cwd-scoped (every AgentSession whose Cwd ==
//              activeCwd), destructive (terminates processes +
//              drops AgentSession entries from the pool +
//              agent_sessions.json). Next message triggers a
//              fresh spawn via the configured Spawner.
//   - /stop  — singleAgent-scoped (only the selectedAgent),
//              non-destructive (signals the bridge to stop its
//              current work; the AgentSession entry stays in
//              the pool; the chat layer's TryFlush picks up the
//              next queued prompt once the bridge settles).
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
)

// Factory is the command.SlashCommandFactory for /stop.
type Factory struct {}

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
		Name:    "stop",
		Summary: "Stop the in-flight turn on the selected agent (next prompt takes over).",
		Usage:   "/stop",
		Category: "session",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//  1. Look up the ChatSession for this chat. Reject if absent.
//  2. RequireActiveCwd preflight (every cmd preflights its own).
//  3. Reject trailing args (the surface is /stop with no args).
//  4. Resolve the selectedAgentSession via chatsession and call
//     StopSelectedAgent. Wrap the per-call Result with
//     FormatStopResult and reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}

	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	if len(input.Args) > 1 {
		// Trailing args are rejected: /stop is single-action with
		// no subcommands. Treating a typo'd arg as "still stop"
		// would mask user error.
		return command.Reply(ctx, rt, "Usage: /stop"), nil
	}

	cmd := &Cmd{CS: cs, Ctx: ctx}
	result, err := StopSelectedAgent(cmd)
	if err != nil {
		if errors.Is(err, chatsession.ErrNoSelectedAgent) {
			return command.Reply(ctx, rt,
				"No active agent to stop. Use /use <agent> first."), nil
		}
		return command.Reply(ctx, rt, fmt.Sprintf("Stop failed: %v", err)), nil
	}
	return command.Reply(ctx, rt, FormatStopResult(result)), nil
}