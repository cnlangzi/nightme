// Package statusbar owns every piece of logic that computes or
// attaches a StatusBar to an outbound message.
//
// The data shape (messages.StatusBar, GitStatusBar, AgentStatusBar,
// UsageStatusBar) lives in internal/messages — that's the wire
// format shared with bridges and channels. This package owns the
// BEHAVIOUR that produces that shape: how to assemble one from an
// AgentSession + usage snapshot (Build), how to pre-fill it onto a
// message by source-AS (StampFromAS), how to attach one to a
// message that doesn't have one yet (AttachIfMissing), and how to
// build the per-chat lookup the outbound Emitter calls on every
// send (NewRuntimeSource).
//
// Dependency direction is one-way: statusbar → messages (and a
// few leaf packages: agent, agentsession). The package does NOT
// import chatsession, outbound, gtw, or prcache — those create
// import cycles (gtw → chatsession → outbound → statusbar;
// prcache → gtw → … same loop). Where statusbar needs a piece
// of behaviour owned by one of those packages, it takes a
// function callback (CollectGit, RefreshPR, LookupPR) or a
// plain struct (ChatInfo) that the runtime supplies at
// construction time. This avoids the interface-satisfaction
// gotchas that arise when chained interfaces (Manager.Get →
// *ChatState → ChatState) cross package boundaries with
// type-alias indirections.
package statusbar

import (
	"context"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Source produces the StatusBar attached to a chat's outbound
// messages. The runtime injects the implementation at Emitter
// construction time; statusbar itself knows nothing about
// AgentSession, chatsession, git status, etc.
//
// Returning nil means "skip the status bar this turn" — the caller
// has already populated msg.StatusBar OR there is no chat / no
// workspace for the chat. The Emitter treats nil the same way:
// don't attach, just forward.
//
// Pre-move this lived in internal/gateway/outbound as
// `StatusBarSource`. The move is a pure relocation — the
// type's behaviour, nil semantics, and signature are unchanged.
// Callers now import this type directly from internal/statusbar.
type Source func(chatID string) *messages.StatusBar

// GitCollector runs git status against cwd and returns a parsed
// snapshot. The runtime injects a closure that calls
// gtw.CollectReadiness; statusbar itself stays decoupled from
// the gtw package because gtw → chatsession → outbound →
// statusbar would close a cycle. nil-safe: nil means "git status
// unknown" and the GitBar is still emitted with a nil GitStatus.
type GitCollector func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error)

// PRRefresher triggers a background PR/MR cache refresh for the
// given (AgentSession ID, workspace). The runtime injects a
// closure that calls prcache.Cache.MaybeRefresh with the right
// HandlerDeps; same cycle-breaking rationale as GitCollector.
// nil-safe: nil means "no refresh; the cached value is whatever
// it was".
type PRRefresher func(asID, cwd string)

// PRLookup returns the cached PR / MR for an AgentSession (or
// nil if none is known). Synchronous, never blocks on network
// I/O. The runtime injects a closure that calls
// prcache.Cache.PR; same cycle-breaking rationale as the others.
// nil-safe.
type PRLookup func(asID string) *messages.PR

// Deps bundles the optional dependencies a StatusBar producer
// needs beyond the chat state itself. All fields are nil-safe:
// missing deps degrade gracefully (no PR stamping / no git
// snapshot / no background refresh) rather than failing.
//
// Pass the same Deps to every stamp site in the daemon so the
// behaviour is uniform — runtime pump, slash-command replies,
// gtw handlers, MessageState subscribers.
type Deps struct {
	// CollectGit runs `git status --porcelain --branch`
	// (or equivalent) against a workspace. nil means
	// GitBar.GitStatus is always nil — the renderer shows
	// "git status unknown" for affected messages.
	CollectGit GitCollector
	// RefreshPR triggers a background PR/MR refresh. nil
	// means no background refresh; the cached PR is read
	// as-is (possibly stale on first ever call).
	RefreshPR PRRefresher
	// LookupPR reads the cached PR / MR synchronously. nil
	// means GitBar.PullRequest is always nil.
	LookupPR PRLookup
}
