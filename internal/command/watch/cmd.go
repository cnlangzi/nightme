// Package watch implements the `/watch on|off` slash command.
//
// F-watch §3.1.1: per-chat message-watch toggle. State-only;
// does not touch activeCwd / activeAgent / pool. The actual gate
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
	"github.com/cnlangzi/nightme/internal/registry"
)

// Factory is the command.SlashCommandFactory for /watch.
type Factory struct {
	mgr            *chatsession.Manager
}

// NewFactory constructs a Factory backed by mgr.
func NewFactory(mgr *chatsession.Manager) *Factory {
	return &Factory{mgr: mgr}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "watch",
		Summary: "Toggle per-chat message-watch mode: /watch on | /watch off",
		Usage:   "/watch on | /watch off",
	}
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
if len(input.Args) < 2 {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Current watch mode: %s\nUsage: /watch on | /watch off\n"+
				"Note: in DMs this is a no-op — every DM message is processed regardless.",
			cs.WatchMode(),
		)), nil
	}

	mode, ok := chatsession.ParseWatchMode(strings.TrimSpace(input.Args[1]))
	if !ok {
		return command.Reply(ctx, rt, fmt.Sprintf(
			"Unknown watch mode %q. Usage: /watch on | /watch off",
			input.Args[1],
		)), nil
	}

	if err := cs.SetWatchMode(mode); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetWatchMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Watch mode set to %s.", mode)
	if mode == registry.WatchModeAll {
		replyText += " I will now process every message in this chat."
	} else {
		replyText += " I will only process messages that @ me or @_all."
	}
	return command.Reply(ctx, rt, replyText), nil
}