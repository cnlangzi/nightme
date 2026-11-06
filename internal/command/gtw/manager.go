package gtw

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrAborted is the cancellation cause /gtw abort installs on the
// context of the command it cancels. It is the marker
// aborted(ctx) looks for, so a run that ends because
// timeouts.Command / timeouts.Agent expired (cause
// context.DeadlineExceeded) is never mistaken for an abort.
var ErrAborted = errors.New("aborted by /gtw abort")

// activeRun is the record of the /gtw command that currently
// holds the chat's run lock — the one blocking every later /gtw
// command in that chat.
type activeRun struct {
	// sub is the subcommand name ("pr", "commit", …) the card
	// names when the run is aborted.
	sub string
	// startedAt is when the run took the chat's chain.
	startedAt time.Time
	// cancel cancels the run's context with ErrAborted.
	cancel context.CancelCauseFunc
}

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
// it across all chats. The only per-chat state left is the run
// lock map and the active-run registry that names the command
// holding each chat's chain.
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

	// active is the per-chat registry of the command that
	// currently holds the chat's run lock — the chain holder
	// /gtw abort cancels. Keyed by chatID like runs, so an
	// abort only ever touches the chat that issued it.
	//
	// Only the chain holder is registered: beginRun runs AFTER
	// the run lock is acquired, so a queued waiter never lands
	// here. Entries are removed when the run is aborted or when
	// it releases the chain.
	active sync.Map // map[chatID]*activeRun
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
	// sync.Map.LoadOrStore returns the existing value atomically
	// when present, so a single LoadOrStore is sufficient — no
	// separate Load needed.
	mu := &sync.Mutex{}
	actual, _ := m.runs.LoadOrStore(chatID, mu)
	return actual.(*sync.Mutex)
}

// --- active run registry (per-chat chain holder) ---

// beginRun registers the run that holds chatID's chain and returns
// the release function that clears the registration. Factory.Handle
// calls it after acquiring the run lock, so the registry always
// names the chain holder and never a queued waiter.
//
// cancel cancels the run's context; /gtw abort calls it with
// ErrAborted.
//
// The release function clears the record only when it is still the
// same record (pointer compare), so a late release from an aborted
// run cannot clobber a newer run's registration.
//
// chatID == "" is a no-op, mirroring runLockFor: the returned
// release function does nothing.
func (m *Manager) beginRun(chatID, sub string, cancel context.CancelCauseFunc) func() {
	if chatID == "" {
		return func() {}
	}
	run := &activeRun{sub: sub, startedAt: time.Now(), cancel: cancel}
	m.active.Store(chatID, run)
	return func() {
		m.active.CompareAndDelete(chatID, run)
	}
}

// abortRun cancels the run holding chatID's chain and returns its
// subcommand name plus how long it had been running. ok is false
// when no run is registered for chatID.
//
// The record is removed BEFORE the cancel so a second /gtw abort
// does not report the same run again.
func (m *Manager) abortRun(chatID string) (sub string, elapsed time.Duration, ok bool) {
	if chatID == "" {
		return "", 0, false
	}
	v, found := m.active.LoadAndDelete(chatID)
	if !found {
		return "", 0, false
	}
	run, isRun := v.(*activeRun)
	if !isRun || run == nil {
		return "", 0, false
	}
	elapsed = time.Since(run.startedAt)
	if run.cancel != nil {
		run.cancel(ErrAborted)
	}
	return run.sub, elapsed, true
}
