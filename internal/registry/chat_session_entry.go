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
//	                       Stored as int. See the field-level
//	                       comment below for the numeric mapping
//	                       and the chatsession.WatchMode enum
//	                       (the authoritative source of meaning).
//	ThinkMode            — F-think per-chat thinking-content
//	                       visibility toggle. Stored as int. See
//	                       the field-level comment + chatsession.
//	                       ThinkMode.
//	ToolsMode            — F-38 per-chat tool-event visibility
//	                       toggle. Stored as int. See the field-
//	                       level comment + chatsession.ToolsMode.
//	WatcherHintEmitted   — F-watch hint-emitted tombstone. True
//	                       once the one-time `/watch on` hint has
//	                       been sent for this chat. See the field-
//	                       level comment + chatsession.Manager.
//	                       maybeEmitWatcherHint.
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
	// Per-chat toggles. Stored as bare int so the registry layer
	// doesn't depend on the enum types (which live next to their
	// reader — chatsession — not here). The meaning of each
	// numeric value is documented on the chatsession-side
	// declarations (WatchMode / ThinkMode / ToolsMode):
	//
	//	WatchMode 0 = chatsession.WatchModeMention (default; safe),
	//	          1 = chatsession.WatchModeAll.
	//	ThinkMode 0 = chatsession.ThinkModeShow (default;
	//	          preserve F-thread-route behaviour),
	//	          1 = chatsession.ThinkModeHide.
	//	ToolsMode 0 = chatsession.ToolsModeHide (default; quiet),
	//	          1 = chatsession.ToolsModeShow.
	//
	// The omitempty tag still drops zero values from the JSON
	// output, preserving on-disk compatibility across upgrades.
	WatchMode int `json:"watchMode,omitempty"`
	ThinkMode int `json:"thinkMode,omitempty"`
	ToolsMode int `json:"toolsMode,omitempty"`

	// WatcherHintEmitted records whether the one-time `/watch on`
	// hint has already been sent to this chat. Defaults to false
	// (hint not yet sent). Set to true the first time the drop
	// branch in chatsession.Manager.HandleInbound fires a hint, so
	// the user is never spammed across subsequent drops or daemon
	// restarts. Persisted on the ChatSessionEntry so the flag
	// survives in-memory evictions and is independent of whether a
	// full ChatSession has been allocated for this chat yet (the
	// hint can fire on the very first non-mention group message,
	// before any @-mention has caused GetOrCreate to allocate
	// state).
	WatcherHintEmitted bool `json:"watcherHintEmitted,omitempty"`
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