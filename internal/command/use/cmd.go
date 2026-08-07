// Package use implements the `/use <agent> [args...]` slash
// command.
//
// /use sets the chat's activeAgent and triggers a lazy
// LookupActiveAgentSession (Q-B fallback order: exact →
// default → spawn). Replies with the resolved AgentSession.
//
// commit 8c: also starts the per-ChatSession readPump for the
// newly-active AgentSession (translates Events → Channel.Send).
// Old pump is implicitly stopped via /kill or previous /use.
//
// Factory holds *chatsession.Manager directly.
package use

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /use.
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
		Name:    "use",
		Summary: "Switch active agent: /use <agent-name> (lazy spawn; reuse pool if present)",
		Usage:   "/use <agent> [args...]",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Semantics:
//
//	/use claude                    → set activeAgent, reuse/spawn (claude, cwd)
//	/use codex --auto-approve      → set activeAgent, pass args to spawn
//	/use                           → reply "Usage: /use <agent> [args...]"
//	/use (no activeCwd yet)        → reply "send /cwd <path> first"
//	/use unknown-agent             → reply "unknown agent"
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	input command.SlashInput) (*command.SlashOutput, error) {

	if len(input.Args) < 2 {
		return command.Reply(ctx, rt, "Usage: /use <agent> [args...]"), nil
	}

	agentName := strings.TrimSpace(input.Args[1])
	if agentName == "" {
		return command.Reply(ctx, rt, "Usage: /use <agent> [args...]"), nil
	}

	cs := f.mgr.GetOrCreate(input.ChatID, f.defaultPrimary)

	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	// Pure state mutation first.
	if err := cs.SetActiveAgent(agentName); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetActiveAgent failed: %v", err)), nil
	}

	// Lazy lookup — may spawn via the configured Spawner.
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Failed to activate agent: %v", err)), nil
	}

	// commit 8c: stop the previous pump (if any) and start a new
	// one for the freshly-active AgentSession. Events drain into
	// cs.eventHandler (installed by runtime at startup).
	_ = cs.StartReadPump()

	source := "spawn"
	if as.Handle() != nil {
		source = "resumed"
	}

	return command.Reply(ctx, rt, fmt.Sprintf("Now using %s (pid=%d, cwd=%s, source=%s)",
		as.Agent, as.PID(), as.Cwd, source)), nil
}