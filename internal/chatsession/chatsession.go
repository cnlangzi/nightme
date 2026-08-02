// Package chatsession — ChatSession (v1.2 per-chat session context).
//
// ChatSession owns the persistent per-chat state and the pool of
// AgentSessions. It replaces v1.1's Session + Gateway BindingEntry
// pair with a single coherent structure.
//
// See docs/SPEC.md v1.2 §1.1 and docs/feat/F-27-chatsession.md.
package chatsession

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// ChatSession is the persistent per-chat session context.
//
// One ChatSession is bound 1:1 to an IM chat (chatID), enforced by
// the UNIQUE constraint on registry.ChatSessionEntry.ChatID. Each
// ChatSession owns a pool of AgentSessions keyed by (agent, cwd);
// the active one (or nil) is tracked in activeAS.
//
// Concurrency: state fields (activeCwd, activeAgent, defaultAgent,
// pool, activeAS) are guarded by mu. Reads take RLock; writes take
// Lock. /use / /cwd take RLock for the mutation + Lock for the
// pool mutation when an AgentSession is added.
type ChatSession struct {
	ID       string
	ChatID   string
	ChatType string

	mu sync.RWMutex

	// Active routing state (mutable via /cwd /use /default).
	activeCwd    string
	activeAgent  string
	defaultAgent string

	// Pool of AgentSessions keyed by (agent, cwd).
	pool map[agentCwdKey]*AgentSession

	// Currently active AgentSession (pointer into pool). nil means
	// no active session (ChatSession exists but no /cwd + /use yet).
	activeAS *AgentSession

	// Timestamps.
	createdAt        time.Time
	lastInteractionAt time.Time

	// Persistence handles (optional — nil means no persistence).
	csFile *registry.ChatSessionFile
	asFile *registry.AgentSessionFile
}

// New creates a fresh ChatSession in memory. The caller is
// responsible for persisting via Persist().
func New(chatID, chatType, defaultAgent string) *ChatSession {
	return &ChatSession{
		ID:               deriveIDFromChatID(chatID),
		ChatID:           chatID,
		ChatType:         chatType,
		defaultAgent:     defaultAgent,
		pool:             make(map[agentCwdKey]*AgentSession),
		createdAt:        time.Now(),
		lastInteractionAt: time.Now(),
	}
}

// WithPersistence attaches registry stores. Both can be nil (no
// persistence); typically both are non-nil in production.
func (cs *ChatSession) WithPersistence(csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile) *ChatSession {
	cs.mu.Lock()
	cs.csFile = csFile
	cs.asFile = asFile
	cs.mu.Unlock()
	return cs
}

// deriveIDFromChatID produces a deterministic ID from the chat ID
// for the 1:1 invariant. Real implementation should use a hash; for
// commit 6 we use a plain prefix to keep things readable.
func deriveIDFromChatID(chatID string) string {
	if chatID == "" {
		return ""
	}
	return "cs_" + chatID
}

// ErrNoActiveCwd is returned by LookupActiveAgentSession when the
// ChatSession has no activeCwd set (user has not /cwd'd yet).
var ErrNoActiveCwd = errors.New("chatsession: no active workspace (send /cwd first)")

// ErrUnknownAgent indicates the requested agent is not registered.
// (Validation should happen at the gateway layer; this is a safety net.)
var ErrUnknownAgent = errors.New("chatsession: unknown agent")

// SetActiveCwd changes the active workspace. Does NOT spawn or kill
// any AgentSession; the pool is preserved. Next message triggers
// LookupActiveAgentSession which may spawn or reuse.
func (cs *ChatSession) SetActiveCwd(cwd string) error {
	if cwd == "" {
		return errors.New("chatsession: empty cwd")
	}
	cs.mu.Lock()
	cs.activeCwd = cwd
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetActiveAgent changes the active agent name. Does NOT spawn or
// kill; caller must invoke LookupActiveAgentSession to materialize.
func (cs *ChatSession) SetActiveAgent(agent string) error {
	if agent == "" {
		return errors.New("chatsession: empty agent")
	}
	cs.mu.Lock()
	cs.activeAgent = agent
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// SetDefaultAgent sets the per-chat default agent. Used as fallback
// during LookupActiveAgentSession when (activeAgent, activeCwd) is
// not in pool.
func (cs *ChatSession) SetDefaultAgent(agent string) error {
	if agent == "" {
		return errors.New("chatsession: empty default agent")
	}
	cs.mu.Lock()
	cs.defaultAgent = agent
	cs.mu.Unlock()
	cs.persistChatEntry()
	return nil
}

// ActiveCwd returns the current active workspace.
func (cs *ChatSession) ActiveCwd() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.activeCwd
}

// ActiveAgent returns the current active agent name.
func (cs *ChatSession) ActiveAgent() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.activeAgent
}

// DefaultAgent returns the per-chat default agent (or "").
func (cs *ChatSession) DefaultAgent() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.defaultAgent
}

// Pool returns a snapshot of all AgentSessions in the pool.
func (cs *ChatSession) Pool() []*AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]*AgentSession, 0, len(cs.pool))
	for _, as := range cs.pool {
		out = append(out, as)
	}
	return out
}

// ActiveAgentSession returns the current active AgentSession (or nil).
func (cs *ChatSession) ActiveAgentSession() *AgentSession {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.activeAS
}

// LookupInPool returns the AgentSession matching (agent, cwd) if
// present in the pool (regardless of status). Returns
// ErrAgentNotFound if not in pool.
func (cs *ChatSession) LookupInPool(agent, cwd string) (*AgentSession, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	as, ok := cs.pool[agentCwdKey{Agent: agent, Cwd: cwd}]
	if !ok {
		return nil, ErrAgentNotFound
	}
	return as, nil
}

// LookupActiveAgentSession resolves the active AgentSession per the
// Q-B fallback order:
//
//  1. pool[(activeAgent, activeCwd)] if present → return it
//     (regardless of status; caller decides respawn)
//  2. pool[(defaultAgent, activeCwd)] if present → return it
//     (fallback; activeAgent not mutated)
//  3. spawn a new AgentSession with (activeAgent, activeCwd) and
//     add it to the pool
//
// Returns ErrNoActiveCwd if activeCwd is empty.
//
// In commit 6 (this commit), step 3 only creates the AgentSession
// data structure and adds it to the pool; it does NOT fork a child
// process. Actual fork-exec lands in commit 7.
func (cs *ChatSession) LookupActiveAgentSession() (*AgentSession, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.activeCwd == "" {
		return nil, ErrNoActiveCwd
	}

	// Step 1: exact match.
	if as, ok := cs.pool[agentCwdKey{Agent: cs.activeAgent, Cwd: cs.activeCwd}]; ok {
		cs.activeAS = as
		return as, nil
	}

	// Step 2: default fallback.
	if cs.defaultAgent != "" && cs.defaultAgent != cs.activeAgent {
		if as, ok := cs.pool[agentCwdKey{Agent: cs.defaultAgent, Cwd: cs.activeCwd}]; ok {
			cs.activeAS = as
			return as, nil
		}
	}

	// Step 3: spawn new (commit 6: data-only; commit 7: real fork).
	newAS := NewAgentSession(
		newAgentSessionID(),
		cs.ID,
		cs.activeAgent,
		cs.activeCwd,
		nil, // args — populated by spawn layer
	)
	cs.pool[agentCwdKey{Agent: cs.activeAgent, Cwd: cs.activeCwd}] = newAS
	cs.activeAS = newAS

	// Persist the new AgentSession entry.
	if cs.asFile != nil {
		_ = cs.asFile.Upsert(newAS.Entry())
	}
	cs.persistChatEntryLocked()

	return newAS, nil
}

// KillAll kills every AgentSession in the pool and clears the pool.
// activeAS is set to nil. Old receipts (if any) are NOT touched
// (they're gateway-managed and will be disposed by the gateway on
// next EventError from this chat).
//
// v1.2 commit 6: this is a data-only operation — no actual signal
// is sent (commit 7 will wire SIGTERM).
func (cs *ChatSession) KillAll() error {
	cs.mu.Lock()
	cs.pool = make(map[agentCwdKey]*AgentSession)
	cs.activeAS = nil
	cs.mu.Unlock()

	if cs.asFile != nil {
		// Best-effort: clear all entries owned by this ChatSession.
		for _, e := range cs.asFile.GetByChatPool(cs.ID) {
			_ = cs.asFile.Delete(e.ID)
		}
	}
	cs.persistChatEntry()
	return nil
}

// Entry returns a snapshot of this ChatSession as a registry entry.
func (cs *ChatSession) Entry() *registry.ChatSessionEntry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.entryLocked()
}

// CreatedAt returns when this ChatSession was first created.
func (cs *ChatSession) CreatedAt() time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.createdAt
}

// LastInteractionAt returns when this ChatSession last had user
// interaction.
func (cs *ChatSession) LastInteractionAt() time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.lastInteractionAt
}

// entryLocked is the same as Entry but assumes cs.mu is held.
func (cs *ChatSession) entryLocked() *registry.ChatSessionEntry {
	agentIDs := make([]string, 0, len(cs.pool))
	var activeASID *string
	for _, as := range cs.pool {
		agentIDs = append(agentIDs, as.ID)
		if cs.activeAS != nil && as.ID == cs.activeAS.ID {
			id := as.ID
			activeASID = &id
		}
	}
	return &registry.ChatSessionEntry{
		ID:                   cs.ID,
		ChatID:               cs.ChatID,
		ChatType:             cs.ChatType,
		ActiveCwd:            cs.activeCwd,
		ActiveAgent:          cs.activeAgent,
		DefaultAgent:         cs.defaultAgent,
		AgentSessionIDs:      agentIDs,
		ActiveAgentSessionID: activeASID,
		CreatedAt:            cs.createdAt,
		LastInteractionAt:    cs.lastInteractionAt,
	}
}

// persistChatEntry writes the ChatSessionEntry to disk (if persistence
// is configured). Best-effort: errors are returned but not propagated
// through call sites (logged at higher level).
func (cs *ChatSession) persistChatEntry() {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	cs.persistChatEntryLocked()
}

// persistChatEntryLocked writes ChatSessionEntry. Caller must hold
// cs.mu (RLock or Lock).
func (cs *ChatSession) persistChatEntryLocked() {
	if cs.csFile == nil {
		return
	}
	_ = cs.csFile.Upsert(cs.entryLocked())
}

// newAgentSessionID returns a unique ID for an AgentSession. v1.2
// commit 6 uses a simple counter-based scheme for testability;
// commit 7 may swap to UUID v7.
var agentSessionCounter atomic.Uint64

func newAgentSessionID() string {
	n := agentSessionCounter.Add(1)
	return fmt.Sprintf("as_%d_%d", time.Now().UnixNano(), n)
}