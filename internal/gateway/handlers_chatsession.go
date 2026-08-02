// Package gateway — v1.2 ChatSession-based slash command handlers.
//
// These replace v1.1's /cwd + /run + /kill handlers (which operated
// on session.MemoryManager). The new handlers operate on
// chatsession.Manager, calling ChatSession.SetActiveCwd /
// SetActiveAgent / LookupActiveAgentSession / KillAll.
//
// The handlers DO NOT spawn directly. /cwd is pure state mutation
// (no spawn). /use triggers a lazy lookup via Spawner; /kill
// clears the pool without removing the ChatSession.
//
// `RegisterChatSessionCommands` is the convenience entry point that
// wires all three commands on a Gateway. The runtime calls this
// once at startup; v1.1 MemoryManager-based commands can be
// retained as fallbacks (deprecated path).
package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// RegisterChatSessionCommands installs /cwd, /use, /kill on gw,
// wired to mgr. globalDefault is the cfg.Primary snapshot that
// becomes ChatSession.defaultAgent on creation. channel is used to
// reply to the user.
//
// Replaces any prior registrations of the same names on gw
// (Gateway returns replaced=true in that case, but we ignore the
// bool since Register is append-only and we want a fresh handler
// pointing at this runtime's mgr).
func RegisterChatSessionCommands(gw Gateway, mgr *chatsession.Manager, channel Channel, globalDefault string) {
	gw.Register(Command{
		Name: "/cwd",
		Description: "Set workspace for this chat: /cwd <absolute-path>",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleCwd(ctx, mgr, channel, msg, args, globalDefault)
		},
	})

	gw.Register(Command{
		Name: "/use",
		Description: "Switch active agent: /use <agent-name> (lazy spawn; reuse pool if present)",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleUse(ctx, mgr, channel, msg, args, globalDefault)
		},
	})

	gw.Register(Command{
		Name: "/kill",
		Description: "Kill every AgentSession in this chat's pool; next message respawns",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleKill(ctx, mgr, channel, msg)
		},
	})
}

// handleCwd validates the path, sets it as the chat's activeCwd,
// and replies with the resolved absolute path. Does NOT spawn.
//
//   /cwd <path>           → set activeCwd = <path>
//   /cwd (no arg)         → reply "Usage: /cwd <path>"
//   /cwd /nonexistent     → reply "no such directory"
//   /cwd ~               → ~ expanded (or rejected if no home)
func handleCwd(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalDefault string) (*CommandResult, error) {
	if len(args) < 1 {
		return reply(ctx, channel, msg.ChatID, "Usage: /cwd <absolute-path>"), nil
	}

	raw := strings.TrimSpace(args[0])
	if raw == "" {
		return reply(ctx, channel, msg.ChatID, "Usage: /cwd <absolute-path>"), nil
	}

	// Path validation: must be absolute. We don't require the
	// directory to exist (offline machines may have a deferred
	// mount); we DO reject ~ that doesn't expand (e.g., HOME unset).
	abs, err := filepath.Abs(raw)
	if err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Invalid path: %v", err)), nil
	}

	cs := mgr.GetOrCreate(msg.ChatID, chatTypeFromMessage(msg), globalDefault)
	if err := cs.SetActiveCwd(abs); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("SetActiveCwd failed: %v", err)), nil
	}

	return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Workspace set to %s", abs)), nil
}

// handleUse sets the chat's activeAgent and triggers a lazy
// LookupActiveAgentSession (Q-B fallback order: exact → default →
// spawn). Replies with the resolved AgentSession.
//
//   /use claude                    → set activeAgent, reuse/spawn (claude, cwd)
//   /use codex --auto-approve      → set activeAgent, pass args to spawn
//   /use                           → reply "Usage: /use <agent> [args...]"
//   /use (no activeCwd yet)        → reply "send /cwd <path> first"
//   /use unknown-agent             → reply "unknown agent"
func handleUse(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalDefault string) (*CommandResult, error) {
	if len(args) < 1 {
		return reply(ctx, channel, msg.ChatID, "Usage: /use <agent> [args...]"), nil
	}

	agentName := strings.TrimSpace(args[0])
	if agentName == "" {
		return reply(ctx, channel, msg.ChatID, "Usage: /use <agent> [args...]"), nil
	}

	cs := mgr.GetOrCreate(msg.ChatID, chatTypeFromMessage(msg), globalDefault)

	if cs.ActiveCwd() == "" {
		return reply(ctx, channel, msg.ChatID, "No active workspace. Send /cwd <path> first."), nil
	}

	// Pure state mutation first.
	if err := cs.SetActiveAgent(agentName); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("SetActiveAgent failed: %v", err)), nil
	}

	// Lazy lookup — may spawn via the configured Spawner.
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		// ErrNoActiveCwd shouldn't reach here (we checked above),
		// but pass through any spawn error.
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Failed to activate agent: %v", err)), nil
	}

	source := "spawn"
	if as.Handle() != nil {
		source = "resumed"
	}

	return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Now using %s (pid=%d, cwd=%s, source=%s)",
		as.Agent, as.PID(), as.Cwd, source)), nil
}

// handleKill clears the ChatSession's AgentSession pool. The
// ChatSession itself is preserved (activeCwd / activeAgent remain).
// The next message triggers a fresh spawn via the configured Spawner.
func handleKill(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage) (*CommandResult, error) {
	cs := mgr.Get(msg.ChatID)
	if cs == nil {
		return reply(ctx, channel, msg.ChatID, "No active chat session to kill."), nil
	}

	poolSize := len(cs.Pool())
	if err := cs.KillAll(); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Kill failed: %v", err)), nil
	}

	return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Killed %d agent session(s). Send a message to start fresh.", poolSize)), nil
}

// reply sends a text reply on channel and returns a consumed
// CommandResult. Errors from channel.Send are swallowed (logged at
// the runtime layer).
func reply(ctx context.Context, channel Channel, chatID, text string) *CommandResult {
	_ = channel.Send(ctx, OutboundMessage{ChatID: chatID, Text: text})
	return &CommandResult{Consumed: true}
}

// chatTypeFromMessage extracts chat_type from the InboundMessage.
// Falls back to "p2p" when unknown (current Feishu reality).
func chatTypeFromMessage(msg *InboundMessage) string {
	if msg == nil || string(msg.ChatType) == "" {
		return "p2p"
	}
	return string(msg.ChatType)
}