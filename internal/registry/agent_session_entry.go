// Package registry — AgentSessionEntry (v3 schema).
//
// See docs/SPEC.md v1.2 §1.2 and docs/feat/F-29-agent-session-pool.md
// for the full model.
//
// v3 changes (SessionID rename + compaction removal):
//   - ResumeID field renamed to SessionID; JSON key "resumeId" → "sessionId".
//   - CompactionCount field removed entirely (compression tracking
//     deleted across the runtime; no consumer remains).
//   - Load compat: legacy JSON with "resumeId" key is silently
//     ignored (SessionID defaults to "" → next spawn starts fresh).
//     Legacy JSON with "compactionCount" key is silently ignored.
package registry

import (
	"time"
)

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
//	ID              — natural key, UUID v7 (preserved across respawn).
//	ChatSessionID   — FK to ChatSessionEntry.ID; "" for legacy orphan
//	                 entries (see migrate.go).
//	Agent           — IMMUTABLE agent name (claude / codex / opencode / ...).
//	Cwd             — IMMUTABLE workspace; the AgentSession cannot change
//	                 cwd post-spawn. /cwd at the ChatSession level does
//	                 NOT mutate this; it creates a new AgentSession or
//	                 reuses an existing one with matching (agent, cwd).
//	PID             — OS process ID; 0 when not running (Detached or Exited).
//	Status          — running | detached | exited (mirrors registry.Status).
//	Args            — spawn arguments; preserved for respawn.
//	SessionID       — agent's own session id (e.g. Claude Code's
//	                 `system/init.session_id`); persisted so a future
//	                 respawn can pass `--resume <id>` back to the agent.
//	                 Empty if the agent has no resume semantics (ACP / Pi
//	                 / PTY) or has not yet emitted its init event.
//	CreatedAt       — first spawn time.
//	LastRunAt       — last event time or status change.
//	ExitCode        — exit code when Status == exited; nil otherwise.
//	Model           — F-45: model captured on first EventAgentReady (e.g.
//	                 "claude-opus-4-5-20250929"). Empty before the
//	                 first init event lands. Stable for the session
//	                 identity's lifetime; reset only when bridge New()
//	                 re-emits EventAgentReady with a new model (post-/new).
type AgentSessionEntry struct {
	ID            string    `json:"id"`
	ChatSessionID string    `json:"chatSessionId"`
	Agent         string    `json:"agent"`
	Cwd           string    `json:"cwd"`
	PID           int       `json:"pid"`
	Status        Status    `json:"status"`
	Args          []string  `json:"args,omitempty"`
	SessionID     string    `json:"sessionId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastRunAt     time.Time `json:"lastRunAt"`
	ExitCode      *int      `json:"exitCode,omitempty"`
	Model         string    `json:"model,omitempty"`
}

// AgentSessionFileVersion is the on-disk format version for
// agent_sessions.json. Bumped to 3 to mark the SessionID rename +
// CompactionCount removal. Load is permissive (unknown fields are
// silently ignored by default), so v2 files load as v3 entries with
// SessionID="" and no CompactionCount.
const AgentSessionFileVersion = 3