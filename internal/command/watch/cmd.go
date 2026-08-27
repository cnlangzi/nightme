// Package watch implements the `/watch on|off` slash command.
//
// F-watch §3.1.1: per-chat message-watch toggle. State-only;
// does not touch selectedCwd / selectedAgent / pool. The actual gate
// (drop non-mention messages vs pass-through) lives in
// gateway.Handle — this handler only mutates state and replies.
//
// Factory holds *chatsession.Manager directly.
package watch

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /watch.
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
		Name:    "watch",
		Summary: "Toggle per-chat message-watch mode: /watch on | /watch off",
		Usage:   "/watch on | /watch off",
	}
}

// watchSpec declares /watch's argv grammar for the shared lexer
// (issue #291): no flags, at most one positional mode token
// (on / off / all / mention — ParseWatchMode owns the alias
// set). Bare `/watch` reports the current mode, so MinArgs
// stays 0.
//
// If /watch ever grows a flag, declare it here — the lexer
// already rejects every undeclared flag, so a typo can never
// silently fall through to ParseWatchMode.
var watchSpec = command.CmdSpec{
	Name:    "/watch",
	Usage:   "/watch on | /watch off",
	MinArgs: 0,
	MaxArgs: 1,
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := command.ParseCmdArgs(input.Args[1:], watchSpec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if args.NArgs() == 0 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Current watch mode: %s\nUsage: /watch on | /watch off\n"+
				"Note: in DMs this is a no-op — every DM message is processed regardless.",
			cs.WatchMode(),
		)), nil
	}

	mode, ok := chatsession.ParseWatchMode(strings.TrimSpace(args.Arg(0)))
	if !ok {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Unknown watch mode %q. Usage: /watch on | /watch off",
			args.Arg(0),
		)), nil
	}

	if err := cs.SetWatchMode(mode); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetWatchMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Watch mode set to %s.", mode)
	if mode == chatsession.WatchModeAll {
		replyText += " I will now process every message in this chat."
	} else {
		replyText += " I will only process messages that @ me or @_all."
	}
	return command.Reply(ctx, rt, replyText), nil
}