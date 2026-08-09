// Package kill implements the `/kill` slash command.
//
// /kill clears the ChatSession's AgentSession pool. The
// ChatSession itself is preserved (selectedCwd / selectedAgent
// remain). The next message triggers a fresh spawn via the
// configured Spawner.
//
// Semantics:
//
//	/kill             → chatsession.KillAllAgents(cmd) — terminate
//	                    every entry whose Cwd == activeCwd
//	/kill <agent>     → chatsession.KillAgent(cmd, <agent>) —
//	                    terminate the (agent, activeCwd) entry
//
// Per-entry outcomes are rendered via chatsession.FormatKillResults.
// The slash command never touches entries in other cwds — use
// `/cwd <other>` + `/kill` there.
//
// Kill orchestration lives in chatsession (kill.go) as
// package-level functions, not methods on ChatSession. This
// keeps the kill surface to two entry points and lets the
// implementation access CS private state without leaking
// public accessors.
//
// Daemon shutdown (cmd/nightme/run.go) does NOT call these —
// agents survive nightme restart via the Detached registry
// state.
//
// Factory holds *chatsession.Manager directly.
package kill

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /kill.
type Factory struct {
	mgr *chatsession.Manager
}

// NewFactory constructs a Factory backed by mgr.
func NewFactory(mgr *chatsession.Manager) *Factory {
	return &Factory{mgr: mgr}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "kill",
		Summary: "Kill every agent in this workspace (/kill) or one agent (/kill <agent>).",
		Usage:   "/kill [<agent>]",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//  1. Look up the ChatSession for this chat. Reject if absent.
//  2. RequireActiveCwd preflight (every cmd preflights its own).
//  3. Resolve which kill to run: /kill <agent> → KillAgent;
//     /kill (no args) → KillAllAgents.
//  4. Wrap the per-entry KillResult slice with FormatKillResults
//     and reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session to kill."), nil
	}

	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	cmd := &Cmd{CS: cs, Ctx: ctx}

	var (
		single  *Result
		results []Result
	)

	if len(input.Args) > 1 {
		// /kill <agent>
		candidate := strings.TrimSpace(input.Args[1])
		if candidate == "" {
			// Treat empty trailing arg as a usage error rather
			// than silently falling back to "kill all in cwd" —
			// a typo on the agent name shouldn't quietly widen
			// the blast radius.
			return command.Reply(ctx, rt, "Usage: /kill [<agent>]"), nil
		}
		r, err := KillAgent(cmd, candidate)
		if err != nil {
			if errors.Is(err, chatsession.ErrAgentNotFound) {
				return command.Reply(ctx, rt, fmt.Sprintf(
					"No %s session in %s to kill. Try /agents.",
					candidate, cs.SelectedCwd())), nil
			}
			return command.Reply(ctx, rt, fmt.Sprintf("Kill failed: %v", err)), nil
		}
		single = &r
	} else {
		// /kill (no args): kill every entry in activeCwd
		rs, err := KillAllAgents(cmd)
		if err != nil {
			return command.Reply(ctx, rt, fmt.Sprintf("Kill failed: %v", err)), nil
		}
		results = rs
	}

	if single != nil {
		results = []Result{*single}
	}
	return command.Reply(ctx, rt, FormatKillResults(results)), nil
}