package gtw

// F-57 readiness test fixtures.
//
// setupReadiness(rig, snap) is the shared helper both /gtw push
// and /gtw pr tests use to mock a single `git status --porcelain
// --branch --untracked-files=normal` invocation. It derives the
// porcelain output from a messages.GitStatusSnapshot and also
// stubs the auxiliary probes (DefaultBranch / countBaseAhead /
// rev-parse for the non-worktree path) so dispatchPush and
// dispatchPR can run end-to-end without the test author having
// to assemble the porcelain string by hand.
//
// Per the F-57 migration §9 step 5: "testharness_test.go (或新
// 文件)" — choosing a dedicated file so the readiness fixtures
// can grow alongside the §8 test matrix without bloating the
// existing setupPRGit / pushGit helpers.

import (
	"github.com/cnlangzi/nightme/internal/messages"
)

// porcelainFromSnapshot renders a `git status --porcelain
// --branch --untracked-files=normal` output equivalent to the
// given snapshot. The dispatchReadiness chain in
// git_status.go::parsePorcelainBranchStatus parses this format
// verbatim — keep these in lock-step.
//
// Format reference:
//   - branch header: "## <branch>[...<upstream>[ [ahead N][, behind M]]]"
//   - empty body when nothing in the working tree
//   - one line per porcelain entry (modified / untracked / etc.)
//
// We deliberately keep this function deterministic and trivial
// to debug: a snapshot field flips, the produced porcelain flips
// with it. Anything more elaborate (e.g. simulating partial
// staged/unstaged states) belongs in a per-test override.
func porcelainFromSnapshot(snap messages.GitStatusSnapshot) string {
	var header string
	switch {
	case snap.Branch == "":
		// Detached HEAD or fresh repo. The two cases git reports
		// are distinguishable in real life ("HEAD (no branch)" vs
		// "(HEAD detached at <sha>)") but for the readiness gate
		// both parse to Branch="", which is what the gate cares
		// about.
		header = "## HEAD (no branch)"
	default:
		header = "## " + snap.Branch
		if snap.HasUpstream {
			header += "...origin/" + snap.Branch
		}
		switch {
		case snap.AheadOfRemote > 0 && snap.BehindRemote > 0:
			header += " [ahead " + itoa10(snap.AheadOfRemote) +
				", behind " + itoa10(snap.BehindRemote) + "]"
		case snap.AheadOfRemote > 0:
			header += " [ahead " + itoa10(snap.AheadOfRemote) + "]"
		case snap.BehindRemote > 0:
			header += " [behind " + itoa10(snap.BehindRemote) + "]"
		}
	}

	body := ""
	// Order matters only for human-readability of test failures;
	// parsePorcelainBranchStatus scans lines independently.
	for i := 0; i < snap.Uncommitted; i++ {
		if snap.HasConflicts && i == 0 {
			// Surface the conflict marker on the FIRST uncommitted
			// line so the parser picks it up via isConflictXY.
			body += "UU conflict.txt\n"
			continue
		}
		body += " M file.txt\n"
	}
	for i := 0; i < snap.Untracked; i++ {
		body += "?? newfile.txt\n"
	}

	if body == "" {
		return header + "\n"
	}
	return header + "\n" + body
}

// statusCmd is the canonical argv slice for the readiness git
// status call. Exposed so tests that need to register additional
// responses (e.g. sequencing with a post-push re-snapshot) can
// reference the same key.
var statusCmd = []string{"status", "--porcelain", "--branch", "--untracked-files=normal"}

// setupReadiness configures a rig's pushGit with responses for
// the readiness pre-flight. It does NOT configure push execution
// (programmaticPushWithRetry uses different argv keys; tests that
// drive push to completion must register those separately).
//
// What it does mock:
//   - `git status --porcelain --branch --untracked-files=normal`
//     → porcelainFromSnapshot(snap)
//   - `git symbolic-ref --short refs/remotes/origin/HEAD`
//     → "origin/main" (DefaultBranch)
//   - `git rev-list --count main..HEAD`
//     → "5" (countBaseAhead default — happy path; tests that
//     exercise the "nothing new to PR yet" branch override)
//   - the two `git rev-parse` calls loadDispatchContext makes in
//     the non-worktree fallback
//
// Tests pass `branch` to control the upstream ref (snap.Branch is
// what the porcelain header says; the worktree branch name from
// the yml is what /gtw pr sees in `c.Branch`). For the standard
// case they're identical; tests that need to exercise the
// "branch in yml != header" edge case can pass different values.
func setupReadiness(rig *prTestRig, branch string, snap messages.GitStatusSnapshot) {
	// CollectReadiness — the F-57 single source of truth.
	rig.git.onArgs(statusCmd, porcelainFromSnapshot(snap), "", nil)

	// DefaultBranch (used by both dispatchPush and dispatchPR for
	// the success-card revRange / PR base ref). DefaultBranch does
	// strings.TrimPrefix(out, "origin/") so the mock must NOT
	// include the "refs/remotes/" prefix.
	rig.git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"origin/main", "", nil)

	// countBaseAhead — only /gtw pr reaches this. Default to 5
	// (happy path "has stuff to PR"). Tests that want the
	// "nothing new to PR yet" reply override via onArgs directly.
	rig.git.onArgs([]string{"rev-list", "--count", "main..HEAD"}, "5", "", nil)

	// Non-worktree fallback: loadDispatchContext reads these
	// when there's no .nightme/gtw.yml. Tests that go through
	// setupPRWorktree (yml path) don't hit them; tests that
	// don't, do.
	rig.git.onArgs([]string{"rev-parse", "--show-toplevel"}, "/w", "", nil)
	rig.git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, branch, "", nil)
}

// itoa10 lives in commit_push_test.go (older code) — we reuse it
// here for the readiness fixture.

// setupPushMocks is the push-test equivalent of setupReadiness.
// The F-56 push tests already wire their own deps.Git and chat
// session; this helper registers the readiness-related git
// responses on the supplied *pushGit so they compose with the
// rest of the test's per-test setup.
//
// Unlike setupReadiness, this does NOT register countBaseAhead
// or the DefaultBranch mock — those are /gtw pr concerns only.
// For push tests, only `status --porcelain --branch
// --untracked-files=normal` matters at the dispatch entry point.
//
// Tests that need post-agent re-snapshot behaviour should
// register an onSeq on statusCmd before calling
// dispatchPush.
func setupPushMocks(git *pushGit, branch string, snap messages.GitStatusSnapshot) {
	git.onArgs(statusCmd, porcelainFromSnapshot(snap), "", nil)
}