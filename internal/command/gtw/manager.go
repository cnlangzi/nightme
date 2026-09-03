package gtw

import (
	"sync"
)

// Manager owns the per-chat run lock used to serialise /gtw
// subcommand execution. v1.5 stripped the rest:
//
//   - Manager.states (per-chat "active fix" Context map) was
//     removed. /gtw fix / close now key off the cwd-scoped yml
//     at <worktree>/.nightme/gtw.yml; there is no parallel
//     in-memory copy.
//   - Manager.drafts + the reaction-routing entry point
//     (HandleReaction / SetHandlerDeps) were removed when the
//     /gtw fix retry mechanism (DraftFixWorktreeFail kind, the
//     §5.3.3 worktree-fail card) was retired. Failure paths now
//     reply with the error text directly.
//
// What survives: the per-chat run lock, because F-59 made every
// slash command async (a fresh goroutine per inbound) and two
// /gtw fix / push / pr calls landing in quick succession would
// still race on the worktree directory, cs.SelectedCwd, and the
// agent session. The chatID is the natural serialisation
// boundary — two chats must remain independent.
//
// The runtime instantiates one Manager per process and shares
// it across all chats. There is no per-chat substate left to
// key by chatID; the run lock map is the only field.
type Manager struct {
	// runs is the per-chat run lock that serialises /gtw
	// subcommand execution. Acquired at the top of
	// Factory.Handle (cmd.go), released on Handle return.
	//
	// Rationale (F-59): two /gtw calls landing back-to-back
	// would race on the worktree directory, cs.SelectedCwd,
	// and the agent session. The chatID is the natural
	// serialisation boundary — two chats must remain
	// independent — so we lazy-allocate one mutex per chatID
	// and never free it. Never-freeing matches the
	// chatsession.Manager.hintLocks policy: freeing a
	// *sync.Mutex while another goroutine is blocked on it
	// would race.
	//
	// Memory: one *sync.Mutex + sync.Map overhead (~80 bytes)
	// per chatID seen. A busy daemon with thousands of chats
	// sees sub-megabyte footprint; cleanup isn't worth the
	// race risk.
	runs sync.Map // map[chatID]*sync.Mutex
}

// NewManager returns an empty Manager. The run lock map is
// lazy-allocated (LoadOrStore on first use), so the constructor
// has nothing to initialise.
func NewManager() *Manager {
	return &Manager{}
}

// --- run lock (per-chat serialisation) ---

// runLockFor returns the per-chat mutex that serialises /gtw
// subcommand execution for chatID. Factory.Handle acquires it
// before the subcommand switch and releases it via defer on
// every return path (early validation, unknown subcommand,
// normal completion).
//
// chatID == "" returns nil so the caller can no-op safely in
// tests and synthetic inputs that drive Handle directly
// without a ChatID. Callers MUST nil-check the return value
// before Lock; the defer Unlock must also be guarded:
//
//	if mu := mgr.runLockFor(input.ChatID); mu != nil {
//	    mu.Lock()
//	    defer mu.Unlock()
//	}
//
// Per-chat (not Manager-wide) so a slow /gtw commit in chat A
// never blocks /gtw sync in chat B. Lazily allocated via
// sync.Map.LoadOrStore; entries are never freed (see the runs
// field doc for the rationale).
func (m *Manager) runLockFor(chatID string) *sync.Mutex {
	if chatID == "" {
		return nil
	}
	if v, ok := m.runs.Load(chatID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := m.runs.LoadOrStore(chatID, mu)
	return actual.(*sync.Mutex)
}
