// Package tools implements the `/tools on|off` slash command.
//
// F-38 §3.1.3: per-chat tool-event visibility toggle. State-only;
// does not touch activeCwd / activeAgent / pool. The actual gate
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

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /tools.
type Factory struct {
	mgr            *chatsession.Manager
	defaultPrimary string
}

// NewFactory constructs a Factory backed by mgr.
func NewFactory(mgr *chatsession.Manager, defaultPrimary string) *Factory {
	return &Factory{mgr: mgr, defaultPrimary: defaultPrimary}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "tools",
		Summary: "Toggle per-chat tool-call visibility: /tools on | /tools off",
		Usage:   "/tools on | /tools off",
	}
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
if len(input.Args) < 2 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Current tools mode: %s\nUsage: /tools on | /tools off",
			cs.ToolsMode(),
		)), nil
	}

	mode, ok := agent.ParseToolsMode(strings.TrimSpace(input.Args[1]))
	if !ok {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Unknown tools mode %q. Usage: /tools on | /tools off",
			input.Args[1],
		)), nil
	}

	if err := cs.SetToolsMode(mode); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetToolsMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Tools mode set to %s.", mode)
	if mode == agent.ToolsModeShow {
		replyText += " Tool calls will appear in the message thread (one reply per tool, call + result merged)."
	} else {
		replyText += " Tool calls will be hidden; only the final answer will be shown."
	}
	return command.Reply(ctx, rt, replyText), nil
}