// Package registry — ChatSessionEntry (v1.2 schema).
//
// v1.2 splits the v1.1 single Entry into two persistent types:
//   - ChatSessionEntry (this file) — per-chat session context
//   - AgentSessionEntry (next file) — per-CLI-process handle
//
// See docs/SPEC.md v1.2 §1.2 (three FSMs, three owners) and
// docs/feat/F-27-chatsession.md for the full model.
package registry

import (
	"encoding/json"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ChatSessionEntry is the persisted form of one ChatSession.
//
// One ChatSession is bound 1:1 to an IM chat (chat_id), enforced
// by the unique ChatID constraint at the storage layer. Each
// ChatSession owns a pool of AgentSessions (see AgentSessionEntry)
// keyed by (agent, cwd); the active one is identified by
// SelectedAgentSessionID.
//
// Persistence file: chat_sessions.json
//
// Field semantics:
//
//	ChatID               — IM channel chat identifier; UNIQUE.
//	                       Chat type classification is no longer
//	                       carried on this struct (F-33 D1): nightme
//	                       treats all chats as opaque string IDs.
//	                       Old files written before F-33 contain a
//	                       `chatType` field; json.Unmarshal tolerates
//	                       unknown fields and the field is ignored.
//	SelectedCwd            — workspace currently bound; "" → ChatSession
//	                       exists but user has not yet /cwd'd.
//	SelectedAgent          — agent name currently selected; "" → not yet
//	                       /use'd. Immutable per AgentSession.
//	                       At construction, seeded from cfg.Primary
//	                       (snapshot, see PrimaryAgent).
//	PrimaryAgent         — snapshot of cfg.Primary at ChatSession
//	                       creation time. Read-only post-construction
//	                       (Q-A: no per-chat override). Init-time
//	                       seeds SelectedAgent.
//	AgentSessionIDs      — pool index (in-order; unordered semantics).
//	SelectedAgentSessionID — pointer into AgentSessionIDs[]; nil → no
//	                       active AgentSession (need /cwd + /use).
//	CreatedAt            — when ChatSession was first bound.
//	LastInteractionAt    — last user message; used for idle expiry
//	                       decisions (future).
//	WatchMode            — F-watch per-chat message-watch mode.
//	                       0 = WatchModeMention (default; safe),
//	                       1 = WatchModeAll. Persisted as int; old
//	                       chat_sessions.json files lacking the
//	                       field decode to zero == WatchModeMention.
//	ThinkMode            — F-think per-chat thinking-content
//	                       visibility toggle. 0 = ThinkModeShow
//	                       (default; preserve F-thread-route
//	                       behavior), 1 = ThinkModeHide (runtime
//	                       drops OutThinking at EventHandler gate).
//	                       Persisted as int; old chat_sessions.json
//	                       files lacking the field decode to zero
//	                       == ThinkModeShow.
//	ToolsMode            — F-38 per-chat tool-event visibility
//	                       toggle. 0 = ToolsModeHide (default;
//	                       runtime drops OutToolStart / OutToolEnd
//	                       at EventHandler gate — tool spam is the
//	                       loudest part of the agent stream and
//	                       most users do not want it by default),
//	                       1 = ToolsModeShow (Feishu adapter merges
//	                       each pair into a single thread reply via
//	                       PATCH on the same message_id). Persisted
//	                       as int; old chat_sessions.json files
//	                       lacking the field decode to zero ==
//	                       ToolsModeHide. Direction is OPPOSITE of
//	                       ThinkMode: ThinkMode's default is Show
//	                       (preserve existing F-thread-route UX);
//	                       ToolsMode's default is Hide (quiet by
//	                       default; opt in to see tool calls).
type ChatSessionEntry struct {
	ID                     string    `json:"id"`
	ChatID                 string    `json:"chatId"`
	SelectedCwd            string    `json:"selectedCwd"`
	SelectedAgent          string    `json:"selectedAgent"`
	PrimaryAgent           string    `json:"primaryAgent,omitempty"`
	AgentSessionIDs        []string  `json:"agentSessionIds"`
	SelectedAgentSessionID *string   `json:"selectedAgentSessionId,omitempty"`
	CreatedAt              time.Time `json:"createdAt"`
	LastInteractionAt      time.Time `json:"lastInteractionAt"`
	WatchMode              WatchMode        `json:"watchMode,omitempty"`
	ThinkMode              ThinkMode        `json:"thinkMode,omitempty"`
	ToolsMode              agent.ToolsMode  `json:"toolsMode,omitempty"`
}

// UnmarshalJSON reads a ChatSessionEntry, transparently migrating
// from legacy field names:
//
//   - `defaultAgent` (v1.2-early)            → PrimaryAgent
//   - `activeCwd`     (v1.2 early naming)   → SelectedCwd
//   - `activeAgent`   (v1.2 early naming)   → SelectedAgent
//   - `activeAgentSessionId` (v1.2 early)    → SelectedAgentSessionID
//
// Old chat_sessions.json files written before the renames still
// carry the legacy keys; we copy each into the canonical field on
// read. New writes only emit the canonical names. Migration is
// one-shot and runs at first read.
//
// On write the canonical fields are used; no migration file is left
// behind. Subsequent reads see only the canonical fields.
func (e *ChatSessionEntry) UnmarshalJSON(data []byte) error {
	type alias ChatSessionEntry
	aux := struct {
		*alias
		LegacyDefaultAgent       string  `json:"defaultAgent,omitempty"`
		LegacyActiveCwd          string  `json:"activeCwd,omitempty"`
		LegacyActiveAgent        string  `json:"activeAgent,omitempty"`
		LegacyActiveAgentSessID  *string `json:"activeAgentSessionId,omitempty"`
	}{
		alias: (*alias)(e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if e.PrimaryAgent == "" && aux.LegacyDefaultAgent != "" {
		e.PrimaryAgent = aux.LegacyDefaultAgent
	}
	if e.SelectedCwd == "" && aux.LegacyActiveCwd != "" {
		e.SelectedCwd = aux.LegacyActiveCwd
	}
	if e.SelectedAgent == "" && aux.LegacyActiveAgent != "" {
		e.SelectedAgent = aux.LegacyActiveAgent
	}
	if e.SelectedAgentSessionID == nil && aux.LegacyActiveAgentSessID != nil {
		e.SelectedAgentSessionID = aux.LegacyActiveAgentSessID
	}
	return nil
}

// ChatSessionFileVersion is the on-disk format version for
// chat_sessions.json. The PrimaryAgent rename (v1.2-final) is
// transparent via UnmarshalJSON migration, so the on-disk
// envelope version stays at 1.
const ChatSessionFileVersion = 1