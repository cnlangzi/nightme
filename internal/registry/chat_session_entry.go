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
)

// ChatSessionEntry is the persisted form of one ChatSession.
//
// One ChatSession is bound 1:1 to an IM chat (chat_id), enforced
// by the unique ChatID constraint at the storage layer. Each
// ChatSession owns a pool of AgentSessions (see AgentSessionEntry)
// keyed by (agent, cwd); the active one is identified by
// ActiveAgentSessionID.
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
//	ActiveCwd            — workspace currently bound; "" → ChatSession
//	                       exists but user has not yet /cwd'd.
//	ActiveAgent          — agent name currently selected; "" → not yet
//	                       /use'd. Immutable per AgentSession.
//	                       At construction, seeded from cfg.Primary
//	                       (snapshot, see PrimaryAgent).
//	PrimaryAgent         — snapshot of cfg.Primary at ChatSession
//	                       creation time. Read-only post-construction
//	                       (Q-A: no per-chat override). Init-time
//	                       seeds ActiveAgent.
//	AgentSessionIDs      — pool index (in-order; unordered semantics).
//	ActiveAgentSessionID — pointer into AgentSessionIDs[]; nil → no
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
type ChatSessionEntry struct {
	ID                   string    `json:"id"`
	ChatID               string    `json:"chatId"`
	ActiveCwd            string    `json:"activeCwd"`
	ActiveAgent          string    `json:"activeAgent"`
	PrimaryAgent         string    `json:"primaryAgent,omitempty"`
	AgentSessionIDs      []string  `json:"agentSessionIds"`
	ActiveAgentSessionID *string   `json:"activeAgentSessionId,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	LastInteractionAt    time.Time `json:"lastInteractionAt"`
	WatchMode            WatchMode `json:"watchMode,omitempty"`
	ThinkMode            ThinkMode `json:"thinkMode,omitempty"`
}

// UnmarshalJSON reads a ChatSessionEntry, transparently migrating
// from the legacy field name `defaultAgent` (v1.2-early naming)
// to the canonical `primaryAgent` (v1.2-final). Old chat_sessions.json
// files written before the rename still carry `defaultAgent`; we
// copy it into PrimaryAgent on read. New writes only emit
// `primaryAgent`. Migration is one-shot and runs at first read.
//
// On write the canonical field is used; no migration file is left
// behind. Subsequent reads see only the canonical field.
func (e *ChatSessionEntry) UnmarshalJSON(data []byte) error {
	type alias ChatSessionEntry
	aux := struct {
		*alias
		LegacyDefaultAgent string `json:"defaultAgent,omitempty"`
	}{
		alias: (*alias)(e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if e.PrimaryAgent == "" && aux.LegacyDefaultAgent != "" {
		e.PrimaryAgent = aux.LegacyDefaultAgent
	}
	return nil
}

// ChatSessionFileVersion is the on-disk format version for
// chat_sessions.json. The PrimaryAgent rename (v1.2-final) is
// transparent via UnmarshalJSON migration, so the on-disk
// envelope version stays at 1.
const ChatSessionFileVersion = 1