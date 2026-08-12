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
type Factory struct {
	mgr            *chatsession.Manager
}

// NewFactory constructs a Factory backed by mgr.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory(d.Manager)
	})
}

func NewFactory(mgr *chatsession.Manager) *Factory {
	return &Factory{mgr: mgr}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "think",
		Summary: "Toggle per-chat thinking visibility: /think on | /think off",
		Usage:   "/think on | /think off",
	}
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
if len(input.Args) < 2 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Current think mode: %s\nUsage: /think on | /think off",
			cs.ThinkMode(),
		)), nil
	}

	mode, ok := chatsession.ParseThinkMode(strings.TrimSpace(input.Args[1]))
	if !ok {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Unknown think mode %q. Usage: /think on | /think off",
			input.Args[1],
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