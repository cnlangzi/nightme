// Package think implements the `/think on|off` slash command.
//
// F-think §3.1.2: per-chat thinking-content visibility toggle.
// State-only; does not touch selectedCwd / selectedAgent / pool.
// The actual gate (drop OutThinking vs pass-through) lives in the
// runtime's EventHandler closure, which reads cs.ThinkMode() after
// Translate + ReplyTo stamping and before ch.Send.
//
// Factory holds *chatsession.Manager directly.
package think

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /think.
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
		Name:    "think",
		Summary: "Toggle per-chat thinking visibility: /think on | /think off",
		Usage:   "/think on | /think off",
	}
}

// thinkSpec declares /think's argv grammar for the shared
// lexer (issue #291): no flags, at most one positional mode
// token. Bare `/think` reports the current mode, so MinArgs
// stays 0.
//
// If /think ever grows a flag, declare it here — the lexer
// already rejects every undeclared flag, so a typo can never
// silently fall through to ParseThinkMode.
var thinkSpec = command.CmdSpec{
	Name:    "/think",
	Usage:   "/think on | /think off",
	MinArgs: 0,
	MaxArgs: 1,
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := command.ParseCmdArgs(input.Args[1:], thinkSpec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if args.NArgs() == 0 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Current think mode: %s\nUsage: /think on | /think off",
			cs.ThinkMode(),
		)), nil
	}

	mode, ok := chatsession.ParseThinkMode(strings.TrimSpace(args.Arg(0)))
	if !ok {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Unknown think mode %q. Usage: /think on | /think off",
			args.Arg(0),
		)), nil
	}

	if err := cs.SetThinkMode(mode); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetThinkMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Think mode set to %s.", mode)
	if mode == chatsession.ThinkModeShow {
		replyText += " Agent reasoning will appear in the message thread."
	} else {
		replyText += " Agent reasoning will be hidden; only final answers and tool summaries will be shown."
	}
	return command.Reply(ctx, rt, replyText), nil
}