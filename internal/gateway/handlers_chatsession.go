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
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// RegisterChatSessionCommands installs /cwd, /use, /kill on gw,
// wired to mgr. globalPrimary is the cfg.Primary snapshot that
// becomes ChatSession.primaryAgent (and seeds activeAgent) on
// creation. channel is used to
// reply to the user.
//
// Replaces any prior registrations of the same names on gw
// (Gateway returns replaced=true in that case, but we ignore the
// bool since Register is append-only and we want a fresh handler
// pointing at this runtime's mgr).
func RegisterChatSessionCommands(gw Gateway, mgr *chatsession.Manager, channel Channel, globalPrimary string) {
	// Command names are stored WITHOUT the leading slash. The
	// Gateway strips the slash in ParseCommand before lookup
	// (see internal/gateway/parser.go), so g.cmds["cwd"] is the
	// correct key. Registering as "/cwd" silently breaks routing
	// — fix: register without slash (commit 4119e2c-fix-2).
	gw.Register(Command{
		Name: "cwd",
		Description: "Set workspace for this chat: /cwd <absolute-path>",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleCwd(ctx, mgr, channel, msg, args, globalPrimary)
		},
	})

	gw.Register(Command{
		Name: "use",
		Description: "Switch active agent: /use <agent-name> (lazy spawn; reuse pool if present)",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleUse(ctx, mgr, channel, msg, args, globalPrimary)
		},
	})

	gw.Register(Command{
		Name: "kill",
		Description: "Kill every AgentSession in this chat's pool; next message respawns",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleKill(ctx, mgr, channel, msg)
		},
	})

	// F-watch §3.1.1: per-chat message-watch toggle. State-only;
	// does not touch activeCwd / activeAgent / pool. DM chats:
	// state is persisted but the gate is a no-op.
	gw.Register(Command{
		Name: "watch",
		Description: "Toggle per-chat message-watch mode: /watch on | /watch off",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleWatch(ctx, mgr, channel, msg, args, globalPrimary)
		},
	})

	// F-34 §4: reset conversation context. /new clears the
	// conversation history on all AgentSessions in activeCwd;
	// /new <agent> narrows to one. Pool identity is preserved;
	// InputBuffer queued messages are dropped. Underlying CLI
	// processes / transports stay alive.
	gw.Register(Command{
		Name: "new",
		Description: "Reset conversation context. /new for all sessions in current workspace, /new <agent> for one.",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleNew(ctx, mgr, channel, msg, args, globalPrimary)
		},
	})
}

// RegisterChatSessionRuntime installs /cwd /use /kill and wires
// the EventHandler on every ChatSession the manager creates. The
// handler is invoked per AgentEvent from the active AgentSession's
// Events() channel — typically translates to OutboundMessage and
// sends via Channel.Send.
//
// commit 8c: in addition to registering commands, this also
// installs a manager-wide observer that:
//   - On /use: starts a readPump for the new active AgentSession
//     (the old pump was stopped by KillAll / by the prior /use).
//   - On /kill: nothing extra (KillAll stops the pump internally).
//   - On startup: ChatSessions restored from disk do not auto-spawn
//     (status=Detached); the pump is started on first /use.
func RegisterChatSessionRuntime(gw Gateway, mgr *chatsession.Manager, channel Channel, globalPrimary string, eventHandler chatsession.EventHandler) {
	RegisterChatSessionCommands(gw, mgr, channel, globalPrimary)
	// The runtime-level readPump start happens in the /use
	// handler (which has access to the resolved activeAS). See
	// handleUse below.
}

// handleCwd validates the path, sets it as the chat's activeCwd,
// and replies with the resolved absolute path. Does NOT spawn.
//
// Path resolution rules (commit fix-4):
//
//   - Leading "~" or "~/" → expand to $HOME
//   - Relative path (no leading "/" or "~") → prepend $HOME
//     (matches shell semantics where `cd code` from `~` works;
//     safer than resolving against daemon's cwd, which is wherever
//     the operator happened to invoke `go run`)
//   - Absolute path → unchanged
//
//   /cwd <path>           → set activeCwd = <resolved-abs-path>
//   /cwd (no arg)         → reply "Usage: /cwd <path>"
//   /cwd /nonexistent     → reply "Path does not exist: ..."
//   /cwd ~                → $HOME (absolute)
//   /cwd ~/foo            → $HOME/foo
//   /cwd foo              → $HOME/foo  (relative path = $HOME-relative)
//
// Existence check: we reject non-existent paths at /cwd time so
// the agent doesn't fail later with a confusing spawn error
// (commit fix-4 followup to the bug observed 2026-08-02 where
// `/cwd code/nightme` silently resolved to /home/devin/code/
// nightme/code/nightme and the subsequent spawn failed).
func handleCwd(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {
	if len(args) < 1 {
		return reply(ctx, channel, msg.ChatID, "Usage: /cwd <path>"), nil
	}

	raw := strings.TrimSpace(args[0])
	if raw == "" {
		return reply(ctx, channel, msg.ChatID, "Usage: /cwd <path>"), nil
	}

	// 1. ~ expansion
	expanded, err := expandTilde(raw)
	if err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Cannot expand ~: %v", err)), nil
	}

	// 2. Relative paths are $HOME-relative (not daemon-cwd-relative).
	if !filepath.IsAbs(expanded) {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Cannot resolve relative path: HOME unset: %v", herr)), nil
		}
		expanded = filepath.Join(home, expanded)
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Invalid path: %v", err)), nil
	}

	// 3. Existence + directory check. We reject non-existent
	// paths and file (non-directory) paths at /cwd time so the
	// user sees a clear error here, not a confusing spawn failure
	// later.
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Path does not exist: %s (resolved from %q)", abs, raw)), nil
		}
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Cannot stat %s: %v", abs, err)), nil
	}
	if !info.IsDir() {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("Not a directory: %s", abs)), nil
	}

	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)
	if err := cs.SetActiveCwd(abs); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("SetActiveCwd failed: %v", err)), nil
	}

	activeAgent := cs.ActiveAgent()
	if activeAgent == "" {
		activeAgent = globalPrimary
	}
	return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
		"Workspace set to %s.\nSession is ready (active agent: %s). Send any message to chat with it, or /use <agent> to switch. /use is optional — plain text is forwarded to the active agent automatically.",
		abs, activeAgent)), nil
}

// expandTilde expands a leading "~" or "~/" to the user's home
// directory. "~" alone becomes $HOME; "~/foo" becomes $HOME/foo.
// Returns the input unchanged if it doesn't start with "~".
//
// Errors only on `os.UserHomeDir` failure (HOME unset in some
// pathological containers); otherwise pure string manipulation.
func expandTilde(path string) (string, error) {
	if path == "" {
		return path, nil
	}
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// handleUse sets the chat's activeAgent and triggers a lazy
// LookupActiveAgentSession (Q-B fallback order: exact → default →
// spawn). Replies with the resolved AgentSession.
//
// commit 8c: also starts the per-ChatSession readPump for the
// newly-active AgentSession (translates Events → Channel.Send).
// Old pump is implicitly stopped via /kill or previous /use.
//
//   /use claude                    → set activeAgent, reuse/spawn (claude, cwd)
//   /use codex --auto-approve      → set activeAgent, pass args to spawn
//   /use                           → reply "Usage: /use <agent> [args...]"
//   /use (no activeCwd yet)        → reply "send /cwd <path> first"
//   /use unknown-agent             → reply "unknown agent"
func handleUse(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {
	if len(args) < 1 {
		return reply(ctx, channel, msg.ChatID, "Usage: /use <agent> [args...]"), nil
	}

	agentName := strings.TrimSpace(args[0])
	if agentName == "" {
		return reply(ctx, channel, msg.ChatID, "Usage: /use <agent> [args...]"), nil
	}

	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)

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

	// commit 8c: stop the previous pump (if any) and start a new
	// one for the freshly-active AgentSession. Events drain into
	// cs.eventHandler (installed by runtime at startup).
	_ = cs.StartReadPump()

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

// chatTypeFromMessage was removed in F-33 (D1). ChatSession no
// longer carries a ChatType; the field is absent from
// InboundMessage entirely. Callers that previously extracted
// chatType should pass an empty string (or restructure the call
// chain to not need a chatType).
