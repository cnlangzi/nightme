// Package use implements the `/use <agent> [args...]` slash
// command.
//
// /use sets the chat's selectedAgent and triggers a lazy
// LookupSelectedAgentSession (Q-B fallback order: exact →
// default → spawn). Replies with the resolved AgentSession.
//
// commit 8c: also starts the per-ChatSession readPump for the
// newly-active AgentSession (translates Events → Channel.Send).
// Old pump is implicitly stopped via /close or previous /use.
//
// Factory holds *chatsession.Manager directly.
package use

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Factory is the command.SlashCommandFactory for /use.
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
		Name:    "use",
		Summary: "Switch active agent: /use <agent-name> (lazy spawn; reuse pool if present)",
		Usage:   "/use <agent> [args...]",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Semantics:
//
//	/use claude                    → set selectedAgent, reuse/spawn (claude, cwd)
//	/use codex --auto-approve      → set selectedAgent, pass args to spawn
//	/use                           → reply "Usage: /use <agent> [args...]"
//	/use (no selectedCwd yet)        → reply "send /cwd <path> first"
//	/use unknown-agent             → reply "unknown agent"
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if len(input.Args) < 2 {
		return command.Reply(ctx, rt, "Usage: /use <agent> [args...]"), nil
	}

	agentName := strings.TrimSpace(input.Args[1])
	if agentName == "" {
		return command.Reply(ctx, rt, "Usage: /use <agent> [args...]"), nil
	}

	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	// Pure state mutation first.
	if err := cs.SetSelectedAgent(agentName); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetSelectedAgent failed: %v", err)), nil
	}

	// Lazy lookup — may spawn via the configured Spawner.
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Failed to activate agent: %v", err)), nil
	}

	// commit 8c: stop the previous pump (if any) and start a new
	// CS-AS 边界重构 Phase 1: readpump is per-AS (started by Spawn).
	// No manual StartReadPump call needed.

	source := "spawn"
	if as.Handle() != nil {
		source = "resumed"
	}

	// Stamp the new agent's identity onto the success reply so
	// the channel's StatusBar AgentBar (Line 1: 🤖: Agent ·
	// Model · SessionID) updates synchronously with the /use
	// confirmation. The plain `command.Reply` path goes through
	// Router.emitReply, which constructs the OutboundMessage with
	// ONLY ChatID/Kind/Text/ReplyTo — AgentName/Model/SessionID
	// stay empty, so formatStatusBarLines drops the entire AgentBar
	// line and the placeholder card keeps showing the previous
	// agent's identity until the next bridge EventAgentReady arrives
	// (never, on the pool-reuse path where no new spawn happens).
	//
	// Routing through SlashOutput.Outbound lets routeOutbound
	// forward a fully-populated OutboundMessage verbatim. On the
	// cold-spawn path, as.Model()/SessionID() may be empty until
	// the new bridge's EventAgentReady lands; that's fine — the
	// AgentBar will update again when that event flows through the
	// runtime's normal event translation (gateway.Translate stamps
	// AgentName/Model/SessionID onto the resulting OutboundMessage
	// from AgentEvent's top-level fields).
	return &command.SlashOutput{
		Consumed: true,
		Outbound: []messages.OutboundMessage{{
			ChatID:    input.ChatID,
			Kind:      messages.OutReply,
			Text:      fmt.Sprintf("Now using %s (pid=%d, cwd=%s, source=%s)", as.Agent, as.PID(), as.Cwd, source),
			ReplyTo:   input.MessageID,
			AgentName: as.Agent,
			Model:     as.Model(),
			SessionID: as.SessionID(),
		}},
	}, nil
}