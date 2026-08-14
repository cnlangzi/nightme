// F-CLAUDE-PRINT-002 + fix-status-bar-git: GitStatus collection
// deps live with ChatSession (their sole consumer). The runtime
// constructs Deps at startup and wires it to every ChatSession
// via Manager.WithGitStatusDeps. ChatSession.GitStatus reads
// these deps on every call to build a fresh snapshot — there
// is no per-chat cache layer; freshness is the explicit goal.
//
// All fields are nil-safe: missing deps degrade gracefully
// (Snapshot nil / PullRequest nil) rather than failing the
// outbound pipeline. See ChatSession.GitStatus for the
// failure path.

package chatsession

import (
	"context"

	"github.com/cnlangzi/nightme/internal/messages"
)

// GitCollector runs git status against cwd and returns a parsed
// snapshot. The runtime injects a closure that calls
// gtw.CollectReadiness; chatsession itself stays decoupled from
// the gtw package because gtw → chatsession → outbound would
// close a cycle. nil-safe: nil means "git status unknown" and
// the GitStatus is still emitted with a nil Snapshot.
type GitCollector func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error)

// PRLookup returns the cached PR / MR for an AgentSession (or
// nil if none is known). Synchronous, never blocks on network
// I/O. The runtime injects a closure that wraps prcache.Cache:
// it triggers prcache.Cache.MaybeRefresh(cwd, deps) first
// (sync, no I/O, conditionally spawns a background refresh
// when the 60s TTL has elapsed) and then reads prcache.Cache
// .PR(). PR caching lives in the dedicated prcache.Cache with
// its own 60s TTL + failureBackoff + background refresh —
// the chatsession layer just owns the per-stamp trigger. The
// `cwd` argument lets the prcache refresh resolve the current
// head branch from the same workspace the chat is sitting on.
// nil-safe (returns nil when prCache == nil).
type PRLookup func(asID, cwd string) *messages.PR

// GitStatusDeps bundles the optional dependencies a ChatSession
// uses to build its GitStatus on every pull-on-read call
// (workspace path + git status + open PR). All fields are
// nil-safe.
//
// Pass the same Deps to every chatsession at startup so the
// behaviour is uniform: every ChatSession.GitStatus call uses
// the same CollectGit / LookupPR closures.
type GitStatusDeps struct {
	// CollectGit runs `git status --porcelain --branch`
	// (or equivalent) against a workspace. nil means the
	// GitStatus.Snapshot stays nil — Channel renders "git
	// status unknown" for the affected line.
	CollectGit GitCollector
	// LookupPR reads the cached PR / MR synchronously. nil
	// means GitStatus.PullRequest is always nil.
	LookupPR PRLookup
}
