// Package tools implements the `/tools on|off` slash command.
//
// F-38 §3.1.3: per-chat tool-event visibility toggle. State-only;
// does not touch selectedCwd / selectedAgent / pool. The actual gate
// (drop OutToolStart / OutToolEnd vs pass-through) lives in the
// runtime's EventHandler closure, which reads cs.ToolsMode() after
// Translate + ReplyTo stamping and before ch.Send.
//
// Default direction is OPPOSITE of /think: /think defaults to
// ThinkModeShow (preserve existing F-thread-route behavior);
// /tools defaults to ToolsModeHide (quiet by default; users opt
// in to see tool calls).
//
// Factory holds *chatsession.Manager directly.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /tools.
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
		Name:    "tools",
		Summary: "Toggle per-chat tool-call visibility: /tools on | /tools off",
		Usage:   "/tools on | /tools off",
	}
}

// toolsSpec declares /tools' argv grammar for the shared lexer
// (issue #291): no flags, at most one positional mode token.
// Bare `/tools` reports the current mode, so MinArgs stays 0.
//
// If /tools ever grows a flag, declare it here — the lexer
// already rejects every undeclared flag, so a typo can never
// silently fall through to ParseToolsMode.
var toolsSpec = command.CmdSpec{
	Name:    "/tools",
	Usage:   "/tools on | /tools off",
	MinArgs: 0,
	MaxArgs: 1,
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	args, err := command.ParseCmdArgs(input.Args[1:], toolsSpec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if args.NArgs() == 0 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Current tools mode: %s\nUsage: /tools on | /tools off",
			cs.ToolsMode(),
		)), nil
	}

	mode, ok := chatsession.ParseToolsMode(strings.TrimSpace(args.Arg(0)))
	if !ok {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Unknown tools mode %q. Usage: /tools on | /tools off",
			args.Arg(0),
		)), nil
	}

	if err := cs.SetToolsMode(mode); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetToolsMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Tools mode set to %s.", mode)
	if mode == chatsession.ToolsModeShow {
		replyText += " Tool calls will appear in the message thread (one reply per tool, call + result merged)."
	} else {
		replyText += " Tool calls will be hidden; only the final answer will be shown."
	}
	return command.Reply(ctx, rt, replyText), nil
}