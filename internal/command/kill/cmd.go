// Package kill implements the `/kill` slash command.
//
// /kill clears the ChatSession's AgentSession pool. The
// ChatSession itself is preserved (activeCwd / activeAgent
// remain). The next message triggers a fresh spawn via the
// configured Spawner.
//
// F-42 §6.1: replies are a per-entry list (see
// chatsession.FormatKillResults) so the user sees which agents
// were killed, which were already dead, and which (if any)
// failed — not a bare count.
//
// Factory holds *chatsession.Manager directly.
package kill

import (
	"context"
	"fmt"

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
		Summary: "Kill every AgentSession in this chat's pool; next message respawns",
		Usage:   "/kill",
	}
}

// Handle implements command.SlashCommandFactory.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	input command.SlashInput) (*command.SlashOutput, error) {

	cs := f.mgr.Get(input.ChatID)
	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session to kill."), nil
	}

	results, err := cs.KillAll()
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Kill failed: %v", err)), nil
	}

	// F-53: drop any queued messages that were waiting for the
	// killed AgentSession. ClearBuffer flips each to MessageDropped
	// and wire-emits MessageDropped — the user sees them as
	// "explicitly cleared" rather than dangling forever. See
	// docs/feat/message_lifecycle.md §5.1 (MessageDropped
	// trigger paths).
	_ = cs.ClearBuffer()

	// F-53 P2 follow-up: reset the InputBuffer FSM to Idle.
	// KillAll nukes the pool + activeAS but does NOT flip the
	// FSM; without this, the next user message after /kill would
	// see state=Busy, get queued, and never be flushed (the new
	// AS spawned on the next LookupActiveAgentSession can't drive
	// a flushPending that the dead pump never fires). Calling
	// SetIdle here puts the buffer back in a state where the
	// next message can dispatch immediately.
	cs.SetIdle()

	return command.Reply(ctx, rt, chatsession.FormatKillResults(results)), nil
}