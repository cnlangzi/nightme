package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// startAgent wraps the agent registry lookup + Detect + Start
// pipeline used by Manager.Create. It returns the live
// AgentSession and its PID for the caller to install.
//
// v1.1: this is the only agent-bring-up path left in the session
// package. CreateOrUpdate / Run / GetByChat / KillByChat lived
// here in v0.x but moved out — see F-26 §6 commit 2 (and commit 3
// for the full move into the Gateway).
func (m *MemoryManager) startAgent(ctx context.Context, agentName, workspace string, args []string) (agent.AgentSession, int, error) {
	if m.agents == nil {
		return nil, 0, errors.New("session: agent registry is nil")
	}
	a, err := m.agents.Get(agentName)
	if err != nil {
		return nil, 0, fmt.Errorf("session: %w", err)
	}
	if err := a.Detect(); err != nil {
		return nil, 0, fmt.Errorf("session: detect %s: %w", agentName, err)
	}
	agentSession, err := a.Start(ctx, agent.StartConfig{
		Workspace: workspace,
		Args:      args,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("session: start %s: %w", agentName, err)
	}
	return agentSession, agentSession.PID(), nil
}

// upsertEntry is implemented in manager.go; this file exists to
// host helpers that historically belonged to the chat-keyed
// lifecycle methods (CreateOrUpdate / Run / KillByChat). Those
// methods lived in cmd/nightme's runtime bridge (commit 2
// transition) and then moved into the Gateway itself (commit 3).
// See internal/gateway/gateway.go for the current binding
// implementation.
//
// The remaining helpers (startAgent) are session-package-internal
// and have no chat_id awareness, which is exactly what v1.1 asks
// for: keep the Manager as a pure process factory.

// upsertEntry re-export is needed by lifecycle_test.go and any
// future caller; it's defined in manager.go.
var _ = registry.StatusRunning // anchor the registry import for files
                                // that only need startAgent
                                // (keeps goimports from removing it)