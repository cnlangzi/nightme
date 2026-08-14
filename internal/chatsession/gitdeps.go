// F-CLAUDE-PRINT-002 + fix-status-bar-git: GitStatus collection
// deps live with ChatSession (their sole consumer). The runtime
// constructs Deps at startup and wires it to every ChatSession
// via Manager.WithGitStatusDeps. Each ChatSession uses it in
// RefreshGitStatus (and in the pull-on-read fallback in GitStatus).
//
// All fields are nil-safe: missing deps degrade gracefully
// (Snapshot nil / PullRequest nil) rather than failing the
// outbound pipeline. See ChatSession.RefreshGitStatus for the
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

// PRRefresher triggers a background PR/MR cache refresh for the
// given (AgentSession ID, workspace). The runtime injects a
// closure that calls prcache.Cache.MaybeRefresh with the right
// HandlerDeps; same cycle-breaking rationale as GitCollector.
// nil-safe: nil means "no refresh; the cached value is whatever
// it was".
type PRRefresher func(asID, cwd string)

// PRLookup returns the cached PR / MR for an AgentSession (or
// nil if none is known). Synchronous, never blocks on network
// I/O. The runtime injects a closure that calls prcache.Cache.PR;
// same cycle-breaking rationale as the others. nil-safe.
type PRLookup func(asID string) *messages.PR

// GitStatusDeps bundles the optional dependencies a chatsession
// uses to refresh its GitStatus (workspace + git status + open
// PR). All fields are nil-safe.
//
// Pass the same Deps to every chatsession at startup so the
// behaviour is uniform — initial refresh + per-`/gtw commit` /
// per-`/gtw pr` refresh use the same source.
type GitStatusDeps struct {
	// CollectGit runs `git status --porcelain --branch`
	// (or equivalent) against a workspace. nil means the
	// GitStatus.Snapshot stays nil — Channel renders "git
	// status unknown" for the affected line.
	CollectGit GitCollector
	// RefreshPR triggers a background PR/MR refresh. nil
	// means no background refresh; the cached PR is read
	// as-is (possibly stale on first ever call).
	RefreshPR PRRefresher
	// LookupPR reads the cached PR / MR synchronously. nil
	// means GitStatus.PullRequest is always nil.
	LookupPR PRLookup
}
