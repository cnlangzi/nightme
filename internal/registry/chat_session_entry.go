// Package registry — ChatSessionEntry (v1.2 schema).
//
// v1.2 splits the v1.1 single Entry into two persistent types:
//   - ChatSessionEntry (this file) — per-chat session context
//   - AgentSessionEntry (next file) — per-CLI-process handle
//
// See docs/SPEC.md v1.2 §1.2 (three FSMs, three owners) and
// docs/feat/F-27-chatsession.md for the full model.
package registry

import "time"

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
//	ChatType             — "p2p" | "group" | "topic_group" | "" (legacy).
//	ActiveCwd            — workspace currently bound; "" → ChatSession
//	                       exists but user has not yet /cwd'd.
//	ActiveAgent          — agent name currently selected; "" → not yet
//	                       /use'd. Immutable per AgentSession.
//	DefaultAgent         — per-chat default agent (overrides global
//	                       defaults.agent from config). Optional.
//	AgentSessionIDs      — pool index (in-order; unordered semantics).
//	ActiveAgentSessionID — pointer into AgentSessionIDs[]; nil → no
//	                       active AgentSession (need /cwd + /use).
//	CreatedAt            — when ChatSession was first bound.
//	LastInteractionAt    — last user message; used for idle expiry
//	                       decisions (future).
type ChatSessionEntry struct {
	ID                   string    `json:"id"`
	ChatID               string    `json:"chatId"`
	ChatType             string    `json:"chatType"`
	ActiveCwd            string    `json:"activeCwd"`
	ActiveAgent          string    `json:"activeAgent"`
	DefaultAgent         string    `json:"defaultAgent,omitempty"`
	AgentSessionIDs      []string  `json:"agentSessionIds"`
	ActiveAgentSessionID *string   `json:"activeAgentSessionId,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	LastInteractionAt    time.Time `json:"lastInteractionAt"`
}

// ChatSessionFileVersion is the on-disk format version for
// chat_sessions.json. Increment when the schema changes in a
// breaking way (existing migration logic should handle older
// versions).
const ChatSessionFileVersion = 1

// ChatSessionsFile is the on-disk container for chat_sessions.json.
type ChatSessionsFile struct {
	Version      int                          `json:"version"`
	ChatSessions map[string]*ChatSessionEntry `json:"chatSessions"`
}

// NewChatSessionsFile returns an empty container at the current
// schema version.
func NewChatSessionsFile() *ChatSessionsFile {
	return &ChatSessionsFile{
		Version:      ChatSessionFileVersion,
		ChatSessions: make(map[string]*ChatSessionEntry),
	}
}