// Package gitbase resolves the base branch of a git workspace — the
// branch a review diffs against — and repairs a missing
// refs/remotes/origin/HEAD.
//
// The resolver is the single place that decides the base branch: both
// /review's precompute paths and /me's upstream facts read it. It
// never returns an error; "could not be determined" is reported
// through Resolution.Source / Resolution.Reason and each caller
// decides what that means.
//
// Why the repair exists: refs/remotes/origin/HEAD is created by
// `git clone` and by `git remote set-head`, and by `git fetch` on
// some git versions — so a repository set up with `git remote add`
// + push often has no such ref. Every base resolution would
// otherwise depend on a slow remote query, and a query that times
// out silently degrades a review into "there is nothing to review".
package gitbase

import (
	"context"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// Source records how the base branch was determined.
type Source int

const (
	// SourceUnknown means the base branch could not be determined.
	SourceUnknown Source = iota
	// SourceSymbolicRef means refs/remotes/origin/HEAD already
	// existed — no remote was contacted and nothing was written.
	SourceSymbolicRef
	// SourceRemoteQuery means the branch was learned from the remote
	// and no ref was written.
	SourceRemoteQuery
	// SourceRepaired means the branch was learned from the remote and
	// refs/remotes/origin/HEAD was created.
	SourceRepaired
)

func (s Source) String() string {
	switch s {
	case SourceSymbolicRef:
		return "symbolic-ref"
	case SourceRemoteQuery:
		return "remote-query"
	case SourceRepaired:
		return "repaired"
	default:
		return "unknown"
	}
}

// Reason explains why the base branch could not be determined. Empty
// when the branch was determined.
type Reason string

const (
	// ReasonNoRemote means the repository has no "origin" remote.
	ReasonNoRemote Reason = "no_origin_remote"
	// ReasonRemoteUnreachable means the remote did not answer.
	ReasonRemoteUnreachable Reason = "remote_unreachable"
	// ReasonMissingTrackingRef means the remote answered, but the
	// local remote-tracking ref for that branch is absent — so
	// `origin/<branch>` does not resolve and no diff can be taken.
	ReasonMissingTrackingRef Reason = "missing_tracking_ref"
)

// Resolution is the outcome of one resolution attempt.
type Resolution struct {
	// Branch is the bare branch name (e.g. "main"), "" when unknown.
	Branch string
	// Source records how the branch was determined.
	Source Source
	// HasRemote reports whether the repository has an "origin" remote.
	HasRemote bool
	// WroteHead reports whether this call created refs/remotes/origin/HEAD.
	WroteHead bool
	// Reason explains an unknown Branch.
	Reason Reason
}

// Options configures one resolution.
type Options struct {
	// Repair allows creating a missing refs/remotes/origin/HEAD. The
	// value written is always the remote's authoritative default
	// branch — never a locally guessed one.
	Repair bool
	// QueryTimeout bounds the remote query. Zero means
	// DefaultQueryTimeout.
	QueryTimeout time.Duration
}

// DefaultQueryTimeout bounds the remote query. The measured SSH round
// trip on a slow host is 4-6 s, so a tighter budget fails
// intermittently and leaves the base unknown.
const DefaultQueryTimeout = 20 * time.Second

// gitTimeout caps one git invocation. The remote query's own budget
// is layered on top as the parent context, so the earlier deadline
// wins.
const gitTimeout = 30 * time.Second

// Resolve determines the base branch of the repository at workspace.
//
// The ladder:
//
//  1. refs/remotes/origin/HEAD — offline, writes nothing.
//  2. the repository has no "origin" remote → no base, and the
//     working tree is the only scope.
//  3. the remote's default branch, queried with a bounded budget.
//  4. repair: write refs/remotes/origin/HEAD so later resolutions
//     succeed offline.
func Resolve(ctx context.Context, workspace string, opts Options) Resolution {
	if strings.TrimSpace(workspace) == "" {
		return Resolution{Reason: ReasonNoRemote}
	}
	timeout := opts.QueryTimeout
	if timeout <= 0 {
		timeout = DefaultQueryTimeout
	}

	if branch, ok := symbolicRefHead(ctx, workspace); ok {
		return Resolution{Branch: branch, Source: SourceSymbolicRef, HasRemote: true}
	}
	if !hasOriginRemote(ctx, workspace) {
		return Resolution{Reason: ReasonNoRemote}
	}
	branch, ok := remoteDefaultBranch(ctx, workspace, timeout)
	if !ok {
		return Resolution{HasRemote: true, Reason: ReasonRemoteUnreachable}
	}
	if !trackingRefExists(ctx, workspace, branch) {
		// The base ref `origin/<branch>` only resolves when the local
		// remote-tracking ref exists. The repair writes exactly one
		// ref — creating the tracking ref is a fetch, which is the
		// user's call, not ours.
		return Resolution{HasRemote: true, Reason: ReasonMissingTrackingRef}
	}

	res := Resolution{Branch: branch, Source: SourceRemoteQuery, HasRemote: true}
	if opts.Repair && writeOriginHead(ctx, workspace, branch) {
		res.Source = SourceRepaired
		res.WroteHead = true
	}
	return res
}

// symbolicRefHead resolves refs/remotes/origin/HEAD without contacting
// the remote.
func symbolicRefHead(ctx context.Context, workspace string) (string, bool) {
	out, err := runGit(ctx, workspace, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", false
	}
	ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "ref:"))
	const prefix = "refs/remotes/origin/"
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	branch := strings.TrimPrefix(ref, prefix)
	if branch == "" {
		return "", false
	}
	return branch, true
}

// hasOriginRemote reports whether the repository has an "origin"
// remote.
func hasOriginRemote(ctx context.Context, workspace string) bool {
	out, err := runGit(ctx, workspace, "remote")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "origin" {
			return true
		}
	}
	return false
}

// remoteDefaultBranch asks the remote for its default branch.
//
// The single-ref query is tried first because it asks for one ref;
// `git remote show` is the fallback because it reports the default
// branch on servers that do not answer --symref. Both share one
// budget, so the whole remote phase stays bounded by timeout.
func remoteDefaultBranch(ctx context.Context, workspace string, timeout time.Duration) (string, bool) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := runGit(c, workspace, "ls-remote", "--symref", "origin", "HEAD")
	if err == nil {
		if branch, ok := parseSymrefHead(out); ok {
			return branch, true
		}
	}
	out, err = runGit(c, workspace, "remote", "show", "origin")
	if err != nil {
		return "", false
	}
	return parseRemoteShowHead(out)
}

// parseSymrefHead reads the `ref: refs/heads/<branch>` line of
// `git ls-remote --symref origin HEAD`.
func parseSymrefHead(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref:"))
		if len(fields) == 0 {
			continue
		}
		const prefix = "refs/heads/"
		if !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		branch := strings.TrimPrefix(fields[0], prefix)
		if branch != "" {
			return branch, true
		}
	}
	return "", false
}

// parseRemoteShowHead reads the `HEAD branch: <branch>` line of
// `git remote show origin`.
func parseRemoteShowHead(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "HEAD branch:") {
			continue
		}
		branch := strings.TrimSpace(strings.TrimPrefix(line, "HEAD branch:"))
		if branch != "" && branch != "(unknown)" {
			return branch, true
		}
	}
	return "", false
}

// trackingRefExists reports whether the local remote-tracking ref for
// the branch exists — the base ref `origin/<branch>` only resolves
// when it does.
func trackingRefExists(ctx context.Context, workspace, branch string) bool {
	_, err := runGit(ctx, workspace, "rev-parse", "--verify", "--quiet",
		"refs/remotes/origin/"+branch)
	return err == nil
}

// writeOriginHead points refs/remotes/origin/HEAD at the branch's
// remote-tracking ref.
//
// An existing ref is left alone: a wrong origin/HEAD is worse than a
// missing one, because `git checkout` without arguments, `git pull`
// and `git remote show origin` would then act on the wrong branch.
func writeOriginHead(ctx context.Context, workspace, branch string) bool {
	if originHeadExists(ctx, workspace) {
		return false
	}
	_, err := runGit(ctx, workspace, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
	return err == nil
}

// originHeadExists reports whether refs/remotes/origin/HEAD exists in
// any form — symbolic or not. for-each-ref lists the ref itself, so
// it answers for both.
func originHeadExists(ctx context.Context, workspace string) bool {
	out, err := runGit(ctx, workspace, "for-each-ref",
		"--format=%(refname)", "refs/remotes/origin/HEAD")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// runGit runs a git command in workspace and returns its trimmed
// stdout. The error is returned so the caller can tell "the command
// failed" from "the command succeeded with no output".
func runGit(ctx context.Context, workspace string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := proc.New(c, "git", append([]string{"-C", workspace}, args...)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
