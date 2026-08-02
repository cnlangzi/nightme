// Package registry — AgentSessionEntry (v1.2 schema).
//
// See docs/SPEC.md v1.2 §1.2 and docs/feat/F-29-agent-session-pool.md
// for the full model.
package registry

import "time"

// AgentSessionEntry is the persisted form of one AgentSession.
//
// (ChatSessionID, Agent, Cwd) is unique within a ChatSession's pool;
// the storage layer enforces this so a (claude, /code/A) AgentSession
// can be looked up unambiguously within its parent ChatSession.
//
// Persistence file: agent_sessions.json
//
// Field semantics:
//
//	ID            — natural key, UUID v7 (preserved across respawn).
//	ChatSessionID — FK to ChatSessionEntry.ID; "" for legacy orphan
//	               entries (see migrate.go).
//	Agent         — IMMUTABLE agent name (claude / codex / opencode / ...).
//	Cwd           — IMMUTABLE workspace; the AgentSession cannot change
//	               cwd post-spawn. /cwd at the ChatSession level does
//	               NOT mutate this; it creates a new AgentSession or
//	               reuses an existing one with matching (agent, cwd).
//	PID           — OS process ID; 0 when not running (Detached or Exited).
//	Status        — running | detached | exited (mirrors registry.Status).
//	Args          — spawn arguments; preserved for respawn.
//	CreatedAt     — first spawn time.
//	LastRunAt     — last event time or status change.
//	ExitCode      — exit code when Status == exited; nil otherwise.
type AgentSessionEntry struct {
	ID            string    `json:"id"`
	ChatSessionID string    `json:"chatSessionId"`
	Agent         string    `json:"agent"`
	Cwd           string    `json:"cwd"`
	PID           int       `json:"pid"`
	Status        Status    `json:"status"`
	Args          []string  `json:"args,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastRunAt     time.Time `json:"lastRunAt"`
	ExitCode      *int      `json:"exitCode,omitempty"`
}

// AgentSessionFileVersion is the on-disk format version for
// agent_sessions.json.
const AgentSessionFileVersion = 1

// AgentSessionsFile is the on-disk container for agent_sessions.json.
type AgentSessionsFile struct {
	Version       int                          `json:"version"`
	AgentSessions map[string]*AgentSessionEntry `json:"agentSessions"`
}

// NewAgentSessionsFile returns an empty container at the current
// schema version.
func NewAgentSessionsFile() *AgentSessionsFile {
	return &AgentSessionsFile{
		Version:       AgentSessionFileVersion,
		AgentSessions: make(map[string]*AgentSessionEntry),
	}
}