// Package chatsession — Spawner abstraction (commit 7).
//
// Spawner is the seam between ChatSession (which knows about chat
// state and pools) and the agent package (which knows how to fork
// a CLI subprocess). ChatSession calls Spawner.Spawn when
// LookupActiveAgentSession finds a missing (agent, cwd) pair.
//
// In production, the runtime wires an agent.Registry-backed Spawner.
// In tests, a fake Spawner returns synthetic handles without
// actually forking. This keeps the chat-session package free of any
// direct dependency on internal/agent or internal/bridge.
package chatsession

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
//   - Call Start(ctx, StartConfig{Workspace: cwd, Args: args}) to
//     fork the child and obtain a live agent.AgentSession.
//
// Spawn returns the live bridge-level handle. The caller (typically
// ChatSession.LookupActiveAgentSession) wraps it in an AgentSession
// and stores it in the pool.
type Spawner interface {
	Spawn(ctx context.Context, agentName, cwd string, args []string) (agent.AgentSession, error)
}

// ErrSpawnerNotSet is returned by ChatSession.LookupActiveAgentSession
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
}

// NewRegistrySpawner returns a Spawner that delegates to an
// agent.Registry. The registry is consulted for Detect and Start.
func NewRegistrySpawner(reg *agent.Registry) Spawner {
	return &registrySpawner{agents: reg}
}

func (s *registrySpawner) Spawn(ctx context.Context, agentName, cwd string, args []string) (agent.AgentSession, error) {
	if s.agents == nil {
		return nil, errors.New("registrySpawner: nil registry")
	}
	a, err := s.agents.Get(agentName)
	if err != nil {
		return nil, err
	}
	if err := a.Detect(); err != nil {
		return nil, err
	}
	return a.Start(ctx, agent.StartConfig{Workspace: cwd, Args: args})
}