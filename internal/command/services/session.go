// Package services holds the service interfaces (SessionService,
// ReactionRouter) that slash command implementations depend on.
//
// The interfaces here are pure contracts; the concrete
// implementations live in the runtime layer
// (cmd/nightme/session_adapter.go for SessionService,
// services/reaction.go for ReactionRouter). This package does
// NOT import chatsession / gtw / gateway / channel — it is the
// "lowest layer" of the command stack, depended on by every
// command package but depending on nothing project-specific.
package services

import (
	"context"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// SessionService is the chat-side session surface commands
// depend on. The implementation lives in
// `cmd/nightme/session_adapter.go` as a `sessionAdapter`
// wrapping *chatsession.Manager — see F-51 doc §1.2.5 for why
// the adapter MUST NOT live in this package (otherwise this
// package would import chatsession, breaking the dependency
// arrow).
type SessionService interface {
	// Get returns the Session for chatID, or nil if absent.
	// Commands that need to detect "no active session" use
	// this; commands that want to lazily create one use
	// GetOrCreate.
	Get(chatID string) Session

	// GetOrCreate returns the Session for chatID, creating it
	// lazily with the given primary agent name. primaryAgent
	// typically comes from cfg.Primary; commands don't read
	// config directly.
	GetOrCreate(chatID, primaryAgent string) Session
}

// Session is the per-chat state surface that slash commands
// need. Wraps *chatsession.ChatSession but exposes only the
// methods commands actually call. Commands MUST go through
// this interface (NOT the concrete *ChatSession) so the command
// layer has zero chatsession dependency.
type Session interface {
	// Agent / cwd
	ActiveCwd() string
	SetActiveCwd(cwd string) error
	ActiveAgent() string
	SetActiveAgent(name string) error
	PrimaryAgent() string

	// AgentSession pool
	LookupActiveAgentSession() (AgentSession, error)
	SetActiveAgentSession(as AgentSession)
	KillAll() ([]KillResult, error)
	NewActiveAgentSessions(ctx context.Context, agentName string) (matched int, sessions []AgentSession, results []ResetResult, err error)

	// Per-chat toggle modes
	WatchMode() WatchMode
	SetWatchMode(WatchMode) error
	ThinkMode() ThinkMode
	SetThinkMode(ThinkMode) error
	ToolsMode() ToolsMode
	SetToolsMode(ToolsMode) error
}

// AgentSession is the command-package's view of one running
// (or detached) agent process. *chatsession.AgentSession
// satisfies this interface (the runtime adapter wires it up).
//
// Method surface is minimal — only what commands actually call.
// Things like agent-specific I/O streams are NOT exposed here
// (commands don't drive the agent directly; they configure
// state and the runtime pumps events through the bridge).
type AgentSession interface {
	// Agent returns the agent name (e.g. "claude" / "codex").
	Agent() string
	// PID returns the OS process id; 0 if the process is not
	// running (Detached / Exited status).
	PID() int
	// Cwd returns the workspace this session is bound to.
	Cwd() string
	// IsRunning reports whether the process is currently alive
	// (equivalent to the concrete AgentSession's Handle() != nil
	// check, but without exposing the bridge.Handle type to the
	// command layer).
	IsRunning() bool
	// ResetCumulative clears cumulative token / cost stats.
	// Called by /new after the bridge New() reset so the
	// footer starts from zero.
	ResetCumulative()
}

// KillResult is one row of the /kill reply. Mirrors
// chatsession.KillResult but with command-package-typed fields
// (no chatsession.Status, no *chatsession.AgentSession pointer
// — callers use Agent + Cwd as the identifying tuple).
type KillResult struct {
	Agent       string
	Cwd         string
	BeforeState string // "running" | "detached" | "exited"
	Action      string // "killed" | "stale-cleared"
	Error       error
}

// ResetResult is one row of the /new reply. Mirrors
// chatsession.ResetResult with command-package-typed fields.
type ResetResult struct {
	Agent       string
	Cwd         string
	BeforeState string
	Action      string
	Error       error
	// Session is the underlying AgentSession — populated so
	// the caller (handleNew in F-45) can perform per-row
	// follow-up actions such as ResetCumulative without
	// re-walking the pool. nil only when targets were empty
	// (matched == 0); always set otherwise.
	Session AgentSession
}

// WatchMode / ThinkMode / ToolsMode are re-declared here as
// type aliases to the canonical types in internal/registry and
// internal/agent. This keeps the command layer free of
// chatsession dependency (chatsession is the original aliaser
// of these types) while letting command callers use the same
// values and constants the registry/agent packages define.
type (
	WatchMode = registry.WatchMode
	ThinkMode = registry.ThinkMode
	ToolsMode = agent.ToolsMode
)
