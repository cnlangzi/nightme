// Package chatsession — Spawner abstraction (commit 7).
//
// Spawner is the seam between ChatSession (which knows about chat
// state and pools) and the agent package (which knows how to fork
// a CLI subprocess). ChatSession calls Spawner.Spawn when
// LookupSelectedAgentSession finds a missing (agent, cwd) pair.
//
// In production, the runtime wires an agent.Registry-backed Spawner.
// In tests, a fake Spawner returns synthetic handles without
// actually forking. This keeps the chat-session package free of any
// direct dependency on internal/agent or internal/bridge.
package agentsession

import (
	"context"
	"errors"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Spawner abstracts the agent bring-up pipeline used to materialize
// an AgentSession from a (agent, cwd) tuple. Implementations:
//
//   - Look up the agent by name (e.g., via agent.Registry).
//   - Run Detect to verify the binary exists / SDK is available.
//   - Call Start(ctx, StartConfig{Workspace: cwd, Args: args,
//     SessionID: sessionID}) to fork the child and obtain a live
//     agent.Agent.
//
// The sessionID is the agent's own session id from the previous run
// (e.g. Claude Code's `system/init.session_id`); bridges that
// support resume (Claude Code) translate it into their native flag
// (`--resume <id>`); bridges that don't (ACP / Pi / PTY) ignore it.
// Empty means "no --resume; start a fresh session".
//
// Spawn returns the live bridge-level handle. The caller (typically
// ChatSession.LookupSelectedAgentSession) wraps it in an AgentSession
// and stores it in the pool.
type Spawner interface {
	Spawn(ctx context.Context, agentName, cwd string, args []string, sessionID string) (*agent.Agent, error)
}

// ErrSpawnerNotSet is returned by ChatSession.LookupSelectedAgentSession
// when the chat has no Spawner wired (typical in tests, or before
// runtime bootstrapping). The caller can choose to either inject a
// Spawner or treat the lookup as "AgentSession created but not
// started" (status=Detached).
var ErrSpawnerNotSet = errors.New("chatsession: spawner not configured")

// registrySpawner is the production Spawner backed by an agent.Registry.
// It mirrors what v1.1's MemoryManager.startAgent did, extracted into
// a small interface implementation so the chatsession package never
// imports agent.Registry directly (keeping the dependency arrow
// chatsession -> agent.Registry one-way).
type registrySpawner struct {
	agents *agent.Registry
	// scope is the daemon-scoped process scope: the parent context
	// of every agent child process. Cancelling it terminates the
	// children — see spawnContext.
	scope context.Context
}

// NewRegistrySpawner returns a Spawner that delegates to an
// agent.Registry. The registry is consulted for Detect and Start.
//
// scope is the daemon's process scope: the parent context of every
// agent child process this spawner starts. Cancelling it terminates
// those children, so the daemon can guarantee no agent process
// outlives it. A nil scope means "no scope" — the caller's context
// is then the only lifetime.
func NewRegistrySpawner(reg *agent.Registry, scope context.Context) Spawner {
	return &registrySpawner{agents: reg, scope: scope}
}

func (s *registrySpawner) Spawn(ctx context.Context, agentName, cwd string, args []string, sessionID string) (*agent.Agent, error) {
	if s.agents == nil {
		return nil, errors.New("registrySpawner: nil registry")
	}
	s2, err := s.agents.Get(agentName)
	if err != nil {
		return nil, err
	}
	if err := s2.Detect(); err != nil {
		return nil, err
	}
	return s2.Start(spawnContext(s.scope, ctx), agent.StartConfig{Workspace: cwd, Args: args, SessionID: sessionID})
}

// spawnContext combines the process scope with the caller's context
// so that either one being cancelled tears the child down.
//
// The merged context is a child of the scope, so cancelling the scope
// propagates. A context cannot have two parents, so the caller's
// cancellation is wired in with context.AfterFunc — "either one
// cancels" is the contract.
//
// The caller's context must be a long-lived one (the daemon context
// or the chat context), not a per-operation context: a per-agent
// operation context is cancelled on an agent switch, and the agent
// process must survive that. The daemon's process scope is the only
// thing that ends an agent's life.
//
// The merged context is deliberately NOT cancelled when Spawn
// returns: the child process outlives the spawn call, and
// exec.CommandContext ties the process lifetime to the context it was
// handed.
func spawnContext(scope, caller context.Context) context.Context {
	if scope == nil {
		scope = context.Background()
	}
	if caller == nil {
		caller = context.Background()
	}
	ctx, cancel := context.WithCancel(scope)
	context.AfterFunc(caller, cancel)
	return ctx
}
