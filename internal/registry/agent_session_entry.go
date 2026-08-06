// Package registry — AgentSessionEntry (v1.2 schema).
//
// See docs/SPEC.md v1.2 §1.2 and docs/feat/F-29-agent-session-pool.md
// for the full model.
package registry

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
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
//	ResumeID        — agent's own session id (e.g. Claude Code's
//	                 `system/init.session_id`); persisted so a future
//	                 respawn can pass `--resume <id>` back to the agent.
//	                 Empty if the agent has no resume semantics (ACP / Pi
//	                 / PTY) or has not yet emitted its init event.
//	CreatedAt       — first spawn time.
//	LastRunAt       — last event time or status change.
//	ExitCode        — exit code when Status == exited; nil otherwise.
//	Model           — F-45: model captured on first EventInit (e.g.
//	                 "claude-opus-4-5-20250929"). Empty before the
//	                 first init event lands. Stable for the session
//	                 identity's lifetime; reset only when bridge New()
//	                 re-emits EventInit with a new model (post-/new).
//	CumulativeUsage — F-45: per-AgentSession running total of token /
//	                 cost stats. Persists across daemon restarts;
//	                 cleared only by /new (handleNew resets + persists).
//	                 Legacy entries written before F-45 lack this field;
//	                 Go JSON unmarshal tolerates missing keys and yields
//	                 nil pointer (= "never ran", cumulative starts at 0
//	                 on first EventUsage). Non-nil pointer with all-zero
//	                 values means "ran but token counts were 0".
//
// CompactionCount is the cumulative number of completed context-
// compaction cycles observed on this AgentSession. F-49 addition.
// 0 = never compacted. Legacy entries written before F-49 lack this
// field; Go JSON unmarshal yields zero value, the safe default
// ("never compacted"). See docs/feat/F-49-compaction-counter.md
// §1.4 / §4.1.
type AgentSessionEntry struct {
	ID              string          `json:"id"`
	ChatSessionID   string          `json:"chatSessionId"`
	Agent           string          `json:"agent"`
	Cwd             string          `json:"cwd"`
	PID             int             `json:"pid"`
	Status          Status          `json:"status"`
	Args            []string        `json:"args,omitempty"`
	ResumeID        string          `json:"resumeId,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	LastRunAt       time.Time       `json:"lastRunAt"`
	ExitCode        *int            `json:"exitCode,omitempty"`
	Model           string          `json:"model,omitempty"`
	CumulativeUsage *agent.UsageInfo `json:"cumulativeUsage,omitempty"`
	CompactionCount int             `json:"compactionCount,omitempty"`
}

// AgentSessionFileVersion is the on-disk format version for
// agent_sessions.json. Bumped to 2 in F-49 to mark the addition of
// the CompactionCount field; loaders remain permissive (zero value
// on missing field) so v1 files load transparently as v2 entries
// with CompactionCount=0.
const AgentSessionFileVersion = 2