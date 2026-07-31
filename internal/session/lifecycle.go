package session

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// Convenience wrappers for the v0.1 chat-driven session lifecycle:
//
//   /cwd <path>          -> CreateOrUpdate(chatID, workspace, agent)
//   /run <agent> [args]  -> Run(chatID, agent, extraArgs)
//   /kill                -> KillByChat(chatID)
//
// These map cleanly to the slash commands in F-20 and keep the
// gateway handlers free of imperative session plumbing.
//
// The full Create(ctx, CreateRequest) entry point is preserved for
// CLI tools (e.g. `nightme test`) that already build a CreateRequest.

// CreateOrUpdate binds chatID to a session with the given workspace.
// If a session already exists for chatID:
//
//   - Status == StatusRunning -> error: caller must /kill first
//     (the spec forbids racing a workspace change against a live
//     agent — see F-20 §4.1).
//   - Status == StatusExited / StatusDetached -> rebind the
//     workspace in place; the session keeps its ID and history.
//
// agentName is recorded on the session but no agent is started (use
// Run for that). It is required today so the session table matches
// what /run will need.
func (m *MemoryManager) CreateOrUpdate(chatID, chatType, workspace, agentName string, args []string) (*Session, error) {
	if chatID == "" {
		return nil, errors.New("session: ChatID is required")
	}
	if workspace == "" {
		return nil, errors.New("session: Workspace is required")
	}
	if agentName == "" {
		return nil, errors.New("session: Agent is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if id, exists := m.chatIndex[chatID]; exists {
		if sess, ok := m.sessions[id]; ok {
			if sess.Status() == StatusRunning {
				return nil, fmt.Errorf("session: %w", ErrChatAlreadyBound)
			}
			// Exited / detached -> rebind workspace + agent in place.
			// Preserve ChatType from the first binding; switching a
			// DM into a group (or vice versa) is not user-driven.
			sess.Workspace = workspace
			sess.Agent = agentName
			sess.Args = append([]string(nil), args...)
			sess.LastRunAt = m.now()
			if m.reg != nil {
				status := registry.StatusDetached
				if err := m.upsertEntry(sess, status, exitCodeOr(sess, -1)); err != nil {
					return nil, fmt.Errorf("session: persist rebind: %w", err)
				}
			}
			return sess, nil
		}
	}

	now := m.now()
	sess := &Session{
		ID:        m.newID(),
		ChatID:    chatID,
		ChatType:  chatType,
		Workspace: workspace,
		Agent:     agentName,
		Args:      append([]string(nil), args...),
		StartedAt: now,
		LastRunAt: now,
	}
	sess.setLifecycle(StatusDetached, nil, 0, nil)
	m.sessions[sess.ID] = sess
	m.chatIndex[chatID] = sess.ID

	if m.reg != nil {
		if err := m.upsertEntry(sess, registry.StatusDetached, -1); err != nil {
			// Roll back the in-memory insert so the caller can retry.
			delete(m.sessions, sess.ID)
			delete(m.chatIndex, chatID)
			return nil, fmt.Errorf("session: persist: %w", err)
		}
	}
	return sess, nil
}

// Run ensures a CLI is running for chatID. It is the F-20 /run
// implementation:
//
//   - chatID has no session              -> ErrSessionNotFound
//   - session has a live agent running   -> return existing (noop)
//   - session's agent is exited/detached -> spawn a fresh agent and
//     attach it to the existing session record (workspace / history
//     preserved).
func (m *MemoryManager) Run(ctx context.Context, chatID, agentName string, extraArgs []string) (*Session, error) {
	if chatID == "" {
		return nil, errors.New("session: ChatID is required")
	}
	if agentName == "" {
		return nil, errors.New("session: Agent is required")
	}

	m.mu.RLock()
	id, exists := m.chatIndex[chatID]
	if !exists {
		m.mu.RUnlock()
		return nil, ErrSessionNotFound
	}
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return nil, ErrSessionNotFound
	}
	if sess.Status() == StatusRunning && sess.PID != 0 {
		m.mu.RUnlock()
		return sess, nil
	}
	workspace := sess.Workspace
	m.mu.RUnlock()

	// Spawn the agent outside the manager lock: agent.Start blocks
	// (PTY) and must not stall other chats' lifecycle.
	agentSession, pid, err := m.startAgent(ctx, agentName, workspace, extraArgs)
	if err != nil {
		return nil, err
	}

	// Attach the live agent handle to the existing session in place.
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok = m.sessions[id]
	if !ok {
		// Session disappeared between the lookup and the lock — close
		// the freshly-spawned agent to avoid leaking the child.
		_ = agentSession.Close()
		return nil, ErrSessionNotFound
	}
	if sess.Status() == StatusRunning && sess.PID != 0 {
		// Race: another Run won; close the duplicate.
		_ = agentSession.Close()
		return sess, nil
	}
	sess.Agent = agentName
	sess.Args = append([]string(nil), extraArgs...)
	sess.LastRunAt = m.now()
	sess.setLifecycle(StatusRunning, agentSession, pid, nil)

	pumpCtx, cancel := context.WithCancel(ctx)
	sess.setCancel(cancel)
	go m.readPump(sess, pumpCtx)

	if m.reg != nil {
		if err := m.upsertEntry(sess, registry.StatusRunning, 0); err != nil {
			// Best-effort: log via stderr; the session is live so
			// we do not roll back the spawn.
			fmt.Fprintf(os.Stderr, "session: persist run: %v\n", err)
		}
	}
	return sess, nil
}

// KillByChat stops the CLI bound to chatID. The session record is
// retained (workspace / agent / args preserved) so the user can
// /run again to restart.
//
// Returns ErrSessionNotFound when chatID has no session, and is a
// no-op when the session has no live agent.
func (m *MemoryManager) KillByChat(chatID string) error {
	m.mu.RLock()
	id, exists := m.chatIndex[chatID]
	if !exists {
		m.mu.RUnlock()
		return ErrSessionNotFound
	}
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok || sess == nil {
		return ErrSessionNotFound
	}
	return m.Kill(sess.ID)
}

// startAgent wraps the agent registry lookup + Detect + Start
// pipeline used by both Create and Run. It returns the live
// AgentSession and its PID for the caller to install.
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
