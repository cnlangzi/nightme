// Package newcmd implements the `/new [<agent>]` slash command.
//
// F-34: `/new [<agent>]` resets the conversation context on
// AgentSessions in this chat's pool without terminating any CLI
// process or transport. Equivalent to claudecode's `/clear`,
// pi's `new_session` RPC, or ACP's `session/new` JSON-RPC — but
// issued from the IM channel.
//
// Semantics (see docs/feat/F-34-new-slash-command.md §4):
//
//	/new         → reset all AgentSessions in activeCwd
//	/new <agent> → reset only the AgentSession named <agent> in activeCwd
//
// In both cases the InputBuffer queued messages are cleared.
// Pool identity (AgentSession.ID / Cwd / Agent / args) is
// preserved.
//
// The package is named `newcmd` because `new` is a Go keyword
// and would shadow the builtin. The /new slash command Spec name
// is unchanged.
package newcmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /new.
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
		Name:    "new",
		Summary: "Reset conversation context: /new for all sessions in current workspace, /new <agent> for one",
		Usage:   "/new [<agent>]",
	}
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}
agentName := ""
	if len(input.Args) > 1 {
		agentName = strings.TrimSpace(input.Args[1])
		if agentName == "" {
			return command.Reply(ctx, rt, "Usage: /new [<agent>]"), nil
		}
	}

	matched, _, results, err := cs.NewActiveAgentSessions(ctx, agentName)

	// The queue is deliberately NOT dropped. /new resets the
	// agent's conversation context via the bridge's New() (claude
	// code's `/clear`, acp's session/new) — queued messages are
	// still owed a reply and flush into the fresh context on the
	// next TryFlush. Dropping them here would silently discard
	// work the user already submitted.

	// Usage stats are now per-turn (the runtime is a passive
	// pass-through; the next EventAgentResult / EventAgentDone naturally
	// carries the new context's snapshot), so nothing on the
	// AgentSession needs clearing here — the bridge's New() has
	// already reset the conversation context, and any future
	// result event will populate a fresh footer.
	for _, r := range results {
		if r.Session == nil || r.Error != nil {
			continue
		}
		if f.mgr != nil {
			// Persist so the new (post-bridge-New) state survives
			// daemon restart, even if no further turn completes.
			if persistErr := f.mgr.PersistAgentSession(r.Session); persistErr != nil {
				// Don't clobber the primary error if
				// NewActiveAgentSessions already returned one
				// — log and move on.
				if err == nil {
					err = persistErr
				}
			}
		}
	}

	if matched == 0 {
		if agentName != "" {
			return command.Reply(ctx, rt, fmt.Sprintf(
				"No agent session for %q in current workspace. Try /agents.",
				agentName)), nil
		}
		return command.Reply(ctx, rt,
			"No agent session in current workspace to reset. Send a message to start one."), nil
	}

	text := chatsession.FormatResetResults(results)
	if err != nil {
		text += fmt.Sprintf(" (errors: %v)", err)
	}
	return command.Reply(ctx, rt, text), nil
}