// Package chatsession — global AgentSessionPool (docs/CHATSTORE.md).
//
// AgentSessionPool holds live/warm AgentSessions keyed by
// chatID + cwd + agent. ChatSession.cs.pool is only the active
// cwd working set; leaving a cwd parks the AS here without
// Close / asFile.Delete / EventBus unsubscribe.
package chatsession

import (
	"path/filepath"
	"sync"
)

// asPoolKey is the canonical AgentSessionPool map key.
// Order is chatID → cwd → agent (docs/CHATSTORE.md §6.1).
type asPoolKey struct {
	ChatID string
	Cwd    string
	Agent  string
}

// AgentSessionPool is the process-wide live/warm AS registry.
// Safe for concurrent use. Multiple channel Managers share one
// instance; chatIDs are channel-prefixed so keys do not collide.
type AgentSessionPool struct {
	mu    sync.RWMutex
	byKey map[asPoolKey]*AgentSession
	byID  map[string]*AgentSession // secondary index — FindByID is O(1)

	// resolveMu serializes cold Lookup create+spawn per key so
	// concurrent miss paths cannot Put distinct AS objects or
	// double-Spawn the same (chatID,cwd,agent).
	resolveMu sync.Map // asPoolKey → *sync.Mutex
}

// NewAgentSessionPool returns an empty pool.
func NewAgentSessionPool() *AgentSessionPool {
	return &AgentSessionPool{
		byKey: make(map[asPoolKey]*AgentSession),
		byID:  make(map[string]*AgentSession),
	}
}

func makeAsPoolKey(chatID, cwd, agent string) asPoolKey {
	return asPoolKey{
		ChatID: chatID,
		Cwd:    filepath.Clean(cwd),
		Agent:  agent,
	}
}

// lockResolve holds the per-key resolve mutex. Unlock via the
// returned function (always safe to call, including on nil pool).
func (p *AgentSessionPool) lockResolve(chatID, cwd, agent string) (unlock func()) {
	if p == nil {
		return func() {}
	}
	key := makeAsPoolKey(chatID, cwd, agent)
	v, _ := p.resolveMu.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// Get returns the AS for (chatID, cwd, agent), or nil.
func (p *AgentSessionPool) Get(chatID, cwd, agent string) *AgentSession {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byKey[makeAsPoolKey(chatID, cwd, agent)]
}

// Put registers as under (chatID, as.Cwd, as.Agent). Nil as or
// empty chatID is a no-op. Prefer GetOrPut on the cold Lookup path.
func (p *AgentSessionPool) Put(chatID string, as *AgentSession) {
	if p == nil || as == nil || chatID == "" {
		return
	}
	key := makeAsPoolKey(chatID, as.Cwd, as.Agent)
	p.mu.Lock()
	p.byKey[key] = as
	if as.ID != "" {
		p.byID[as.ID] = as
	}
	p.mu.Unlock()
}

// GetOrPut returns the existing AS for as's key, or inserts as and
// returns it. Concurrent cold creators share one winner.
func (p *AgentSessionPool) GetOrPut(chatID string, as *AgentSession) *AgentSession {
	if as == nil {
		return nil
	}
	if p == nil || chatID == "" {
		return as
	}
	key := makeAsPoolKey(chatID, as.Cwd, as.Agent)
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.byKey[key]; existing != nil {
		return existing
	}
	p.byKey[key] = as
	if as.ID != "" {
		p.byID[as.ID] = as
	}
	return as
}

// Delete removes the entry for (chatID, cwd, agent) if present.
func (p *AgentSessionPool) Delete(chatID, cwd, agent string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if existing, ok := p.byKey[makeAsPoolKey(chatID, cwd, agent)]; ok {
		delete(p.byKey, makeAsPoolKey(chatID, cwd, agent))
		if existing != nil && existing.ID != "" {
			delete(p.byID, existing.ID)
		}
	}
	p.mu.Unlock()
}

// FindByID returns the AS with the given ID, or nil. Used when an
// event carries AgentSessionID but the AS may be warm (not in cs.pool).
func (p *AgentSessionPool) FindByID(id string) *AgentSession {
	if p == nil || id == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byID[id]
}

// ListByChatCwd returns every AS for chatID whose Cwd matches cwd
// (after Clean). Order is unspecified. The returned slice is a
// snapshot; callers may mutate it.
func (p *AgentSessionPool) ListByChatCwd(chatID, cwd string) []*AgentSession {
	if p == nil || chatID == "" || cwd == "" {
		return nil
	}
	want := filepath.Clean(cwd)
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*AgentSession, 0)
	for k, as := range p.byKey {
		if k.ChatID == chatID && k.Cwd == want && as != nil {
			out = append(out, as)
		}
	}
	return out
}
