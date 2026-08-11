// Package close implements the `/close` slash command.
//
// /close forcibly terminates the bridge processes backing the
// ChatSession's AgentSession pool. The AgentSession entries
// themselves are preserved — selectedCwd / selectedAgent / pool /
// sessionID / agent_sessions.json rows all stay intact — so the
// next user message triggers a respawn that replays
// `--resume <sessionID>` to continue the conversation.
//
// Semantics:
//
//	/close             → chatsession.CloseAllAgents(cmd) — terminate
//	                    every bridge process whose Cwd == activeCwd
//	/close <agent>     → chatsession.CloseAgent(cmd, <agent>) —
//	                    terminate the bridge process backing the
//	                    (agent, activeCwd) entry
//
// Per-entry outcomes are rendered via FormatResults. The slash
// command never touches entries in other cwds — use `/cwd <other>`
// + `/close` there.
//
// Compare to sibling commands:
//
//   - /stop  — signals the bridge to halt the in-flight turn;
//              bridge process may or may not exit. Pool entry
//              preserved.
//   - /close — kills the bridge process (graceful: close stdin +
//              SIGINT + 2s grace + SIGKILL fallback). Pool entry
//              preserved; sessionID kept; respawn resumes the
//              conversation.
//   - /new   — invokes the bridge's in-place context reset
//              (claude's `/clear`, pi's `new_session`, etc.).
//              Process stays; conversation history cleared.
//
// To fully discard a session (kill process AND forget sessionID),
// the user can delete the corresponding row in `agent_sessions.json`
// (the runtime will then fresh-spawn on the next message without
// --resume) or wait for daemon shutdown to reap stale Detached
// entries.
//
// Close orchestration lives in this package as package-level
// functions, not methods on ChatSession. This keeps the close
// surface to two entry points and lets the implementation access
// CS private state without leaking public accessors.
//
// Daemon shutdown (cmd/nightme/run.go) does NOT call these —
// agents survive nightme restart via the Detached registry state.
//
// Factory holds *chatsession.Manager directly.
package close

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /close.
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
		Name:    "close",
		Summary: "Close every agent session in this workspace (/close) or one agent (/close <agent>).",
		Usage:   "/close [<agent>]",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//  1. Look up the ChatSession for this chat. Reject if absent.
//  2. RequireActiveCwd preflight (every cmd preflights its own).
//  3. Resolve which close to run: /close <agent> → CloseAgent;
//     /close (no args) → CloseAllAgents.
//  4. Wrap the per-entry Result slice with FormatResults and reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session to close."), nil
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
		// /close <agent>
		candidate := strings.TrimSpace(input.Args[1])
		if candidate == "" {
			// Treat empty trailing arg as a usage error rather
			// than silently falling back to "close all in cwd" —
			// a typo on the agent name shouldn't quietly widen
			// the blast radius.
			return command.Reply(ctx, rt, "Usage: /close [<agent>]"), nil
		}
		r, err := CloseAgent(cmd, candidate)
		if err != nil {
			if errors.Is(err, chatsession.ErrAgentNotFound) {
				return command.Reply(ctx, rt, fmt.Sprintf(
					"No %s session in %s to close. Try /agents.",
					candidate, cs.SelectedCwd())), nil
			}
			return command.Reply(ctx, rt, fmt.Sprintf("Close failed: %v", err)), nil
		}
		single = &r
	} else {
		// /close (no args): close every entry in activeCwd
		rs, err := CloseAllAgents(cmd)
		if err != nil {
			return command.Reply(ctx, rt, fmt.Sprintf("Close failed: %v", err)), nil
		}
		results = rs
	}

	if single != nil {
		results = []Result{*single}
	}
	return command.Reply(ctx, rt, FormatResults(results)), nil
}