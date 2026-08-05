package chatsession

import "time"

// This file holds the F-45 (teamflow /gtw) accessors and reaction
// router that hang off ChatSession. Kept in a separate file from
// gtw_state.go so the type definitions (which need to be referenced
// from internal/gtw via type aliases) don't have to share a file
// with the runtime methods.

// GTWContext returns a value-typed copy of the in-flight /gtw fix
// snapshot, or the zero value (with State == "") when no fix is
// active. The struct is small (int + 3 strings + time.Time ≈ 80B
// on amd64) so the copy is essentially free; returning by value
// instead of pointer avoids a reader/writer race on the pointed-to
// fields that would otherwise require a full lock at the call site.
//
// Callers detect "no active fix" by checking the returned value
// against the zero value: `if cs.GTWContext().State == "" { ... }`.
func (cs *ChatSession) GTWContext() GTWContext {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.gtwContext == nil {
		return GTWContext{}
	}
	return *cs.gtwContext
}

// SetGTWContext replaces the in-flight /gtw fix snapshot. Pass the
// zero value (GTWContext{}) to clear. The previous value (if any)
// is dropped without notification — callers are expected to drive
// the FSM themselves.
func (cs *ChatSession) SetGTWContext(ctx GTWContext) {
	cs.mu.Lock()
	if ctx == (GTWContext{}) {
		cs.gtwContext = nil
	} else {
		// Copy through a fresh allocation so the caller cannot
		// mutate the stored struct by holding their own pointer.
		stored := ctx
		cs.gtwContext = &stored
	}
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
}

// ClearGTWContext is a sugar wrapper for SetGTWContext(GTWContext{}).
func (cs *ChatSession) ClearGTWContext() {
	cs.SetGTWContext(GTWContext{})
}

// HasGTWContext reports whether a /gtw fix is currently in flight
// for this chat. Equivalent to `cs.GTWContext().Issue != 0`.
func (cs *ChatSession) HasGTWContext() bool {
	return cs.GTWContext().Issue != 0
}

// GTWDraft returns the pending draft keyed by userMsgID, or nil
// if no draft is registered. Used by reaction handlers to look up
// the context of the user's reaction target.
func (cs *ChatSession) GTWDraft(userMsgID string) *GTWDraft {
	if userMsgID == "" {
		return nil
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.gtwDrafts[userMsgID]
}

// StoreGTWDraft registers a draft under userMsgID. Overwrites any
// previous draft under the same key (rare; reactions are usually
// one-shot per card).
func (cs *ChatSession) StoreGTWDraft(userMsgID string, d *GTWDraft) {
	if userMsgID == "" || d == nil {
		return
	}
	cs.mu.Lock()
	cs.gtwDrafts[userMsgID] = d
	cs.lastInteractionAt = time.Now()
	cs.mu.Unlock()
}

// TakeGTWDraft atomically reads and deletes the draft. Returns
// nil if no draft was registered. Used by reaction handlers to
// ensure a single ✅ / 🆕 / 🔗 / ❌ / 🔄 is acted on exactly once.
func (cs *ChatSession) TakeGTWDraft(userMsgID string) *GTWDraft {
	if userMsgID == "" {
		return nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	d := cs.gtwDrafts[userMsgID]
	delete(cs.gtwDrafts, userMsgID)
	return d
}

// ListGTWDrafts returns a snapshot of all currently-pending drafts.
// Order is unspecified. Used by `nightme gtw list` (F-51, not v1).
func (cs *ChatSession) ListGTWDrafts() []*GTWDraft {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]*GTWDraft, 0, len(cs.gtwDrafts))
	for _, d := range cs.gtwDrafts {
		out = append(out, d)
	}
	return out
}

// GTWDraftCount returns the number of pending drafts. Cheap; used
// by diagnostics and tests.
func (cs *ChatSession) GTWDraftCount() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.gtwDrafts)
}

