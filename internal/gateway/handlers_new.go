// F-34: `/new [<agent>]` slash command handler.
//
// Resets the conversation context on AgentSessions in this chat's
// pool without terminating any CLI process or transport. Equivalent
// to claudecode's `/clear`, pi's `new_session` RPC, or ACP's
// `session/new` JSON-RPC — but issued from the IM channel.
//
// Semantics (see docs/feat/F-34-new-slash-command.md §4):
//   /new            → reset all AgentSessions in activeCwd
//   /new <agent>    → reset only the AgentSession named <agent> in activeCwd
//
// In both cases the InputBuffer queued messages are cleared. Pool
// identity (AgentSession.ID / Cwd / Agent / args) is preserved.
package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// handleNew resets conversation context on AgentSessions in the
// current chat's pool. See package docs above for full semantics.
//
//   /new                 → reply "Reset N/M agent session(s)."
//   /new <agent>         → same, narrowed to one AgentSession
//   /new (no activeCwd)  → reply "Send /cwd <path> first."
//   /new <agent> (no AS) → reply "No agent session for <agent> in current workspace."
//   /new (no AS at all)  → reply "No agent session in current workspace to reset."
func handleNew(ctx context.Context, mgr *chatsession.Manager, channel Channel,
	msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {

	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)
	if cs.ActiveCwd() == "" {
		return reply(ctx, channel, msg.ChatID,
			"No active workspace. Send /cwd <path> first."), nil
	}

	agentName := ""
	if len(args) > 0 {
		agentName = strings.TrimSpace(args[0])
		if agentName == "" {
			return reply(ctx, channel, msg.ChatID,
				"Usage: /new [<agent>]"), nil
		}
	}

	matched, _, results, err := cs.NewActiveAgentSessions(ctx, agentName)

	// F-45 §1.7: /new is the ONLY event that clears cumulative
	// token / cost stats on the AgentSession. Bridge New()
	// already reset the conversation context; runtime resets the
	// counter so the footer starts from zero on the next reply.
	// PersistAgentSession is called immediately so the cleared
	// state survives daemon restart even if no further turn
	// completes (and thus no EventDone-triggered persist fires).
	for _, r := range results {
		if r.Session == nil || r.Error != nil {
			continue
		}
		r.Session.ResetCumulative()
		if mgr != nil {
			if persistErr := mgr.PersistAgentSession(r.Session); persistErr != nil {
				// Don't clobber the primary error if NewActiveAgentSessions
				// already returned one — log and move on.
				if err == nil {
					err = persistErr
				}
			}
		}
	}

	if matched == 0 {
		if agentName != "" {
			return reply(ctx, channel, msg.ChatID,
				fmt.Sprintf("No agent session for %q in current workspace. Try /agents.",
					agentName)), nil
		}
		return reply(ctx, channel, msg.ChatID,
			"No agent session in current workspace to reset. Send a message to start one."), nil
	}

	text := chatsession.FormatResetResults(results)
	if err != nil {
		text += fmt.Sprintf(" (errors: %v)", err)
	}
	return reply(ctx, channel, msg.ChatID, text), nil
}