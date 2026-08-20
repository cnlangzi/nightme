package gtw

// Test fixtures shared by /gtw push, /gtw commit, and /gtw pr
// dispatch tests.
//
// setupReadiness(rig, snap) is the shared helper that mocks the
// git calls those dispatchers make end-to-end:
//   - `git status --porcelain --branch` (runtime footer render
//     path; dispatch paths no longer read the body after the
//     2-gate refactor)
//   - `git ls-remote --heads origin <branch>` (gate 1 of /gtw pr)
//   - `git symbolic-ref --short refs/remotes/origin/HEAD`
//     (DefaultBranch, used by push + pr)
//   - the two `git rev-parse` calls loadDispatchContext makes in
//     the non-worktree fallback
//
// Tests that need to exercise gate-1 fail override the
// ls-remote mock with an empty-string response. setupPRGit
// adds a GitHub-shaped origin URL so resolveProvider succeeds
// without Detect firing its HTTP probe.

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
		case snap.UpstreamGone:
			// [gone] is mutually exclusive with ahead/behind in real
			// git (no tracking ref → nothing to diff against), so it
			// takes precedence here. See parseBranchHeader.
			header += " [gone]"
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
	//
	// Conflict entries: emit one `UU` line per Conflicts entry
	// so the parser populates Conflicts (not Modified). The
	// F-57 readiness tests rely on HasConflicts=true reaching
	// the parser; setting Conflicts>=1 covers both HasConflicts
	// (boolean mirror) and the numeric Conflicts field in one
	// shot. Always emit at least one `UU` line when HasConflicts
	// is set so a {Modified: 0, Conflicts: 0, HasConflicts: true}
	// snapshot still surfaces as conflicting — the parser
	// derives HasConflicts from Conflicts, but tests can also
	// set the flag directly to bypass Conflicts plumbing.
	for i := 0; i < snap.Conflicts; i++ {
		body += "UU conflict.txt\n"
	}
	if snap.HasConflicts && snap.Conflicts == 0 {
		// Defensive fallback for tests that set HasConflicts
		// without populating Conflicts: emit one conflict line
		// so the parser surfaces the flag.
		body += "UU conflict.txt\n"
	}
	// F-XX (status-bar split): emit one porcelain line per
	// category so parsePorcelainBranchStatus populates the
	// matching Added/Deleted/Modified field. The fixture uses
	// canonical XY positions for each category (X=='A', Y=='D',
	// Y=='M') so the parser's switch lands in the right arm.
	for i := 0; i < snap.Added; i++ {
		body += "A  file.txt\n"
	}
	for i := 0; i < snap.Deleted; i++ {
		body += " D file.txt\n"
	}
	for i := 0; i < snap.Modified; i++ {
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
// /gtw pr's two readiness gates + DefaultBranch +
// loadDispatchContext's non-worktree fallback. It does NOT
// configure push execution (programmaticPushWithRetry uses
// different argv keys; tests that drive push to completion must
// register those separately).
//
// What it does mock:
//   - `git status --porcelain --branch --untracked-files=normal`
//     → porcelainFromSnapshot(snap) — runtime footer render path
//     reads this; dispatch paths no longer do.
//   - `git ls-remote --heads origin <branch>`
//     → "abc1234\trefs/heads/<branch>" (gate 1 of /gtw pr;
//     tests that want to exercise gate-1 fail override with "")
//   - `git symbolic-ref --short refs/remotes/origin/HEAD`
//     → "origin/main" (DefaultBranch)
//   - the two `git rev-parse` calls loadDispatchContext makes in
//     the non-worktree fallback
//
// Tests pass `branch` to control the upstream ref (snap.Branch
// is what the porcelain header says; the worktree branch name
// from the yml is what /gtw pr sees in `c.Branch`). For the
// standard case they're identical; tests that need to exercise
// the "branch in yml != header" edge case can pass different
// values.
func setupReadiness(rig *prTestRig, branch string, snap messages.GitStatusSnapshot) {
	// Runtime footer render path — ChatSession.GitStatus rebuilds
	// the snapshot on every stamp, so it still needs the
	// porcelain mock. Dispatch paths don't read the body.
	rig.git.onArgs(statusCmd, porcelainFromSnapshot(snap), "", nil)

	// Gate 1 of /gtw pr: origin/<branch> ref exists. Default to
	// a non-empty response so existing tests (which assume the
	// upstream really exists on origin) keep passing. Tests that
	// want to exercise gate-1 fail override this with "".
	rig.git.onArgs([]string{"ls-remote", "--heads", "origin", branch},
		"abc1234	refs/heads/"+branch, "", nil)

	// DefaultBranch (used by dispatchPR for the PR base ref).
	// DefaultBranch does strings.TrimPrefix(out, "origin/") so
	// the mock must NOT include the "refs/remotes/" prefix.
	rig.git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"origin/main", "", nil)

	// Non-worktree fallback: loadDispatchContext reads these
	// when there's no .nightme/gtw.yml. Tests that go through
	// setupPRWorktree (yml path) don't hit them; tests that
	// don't, do.
	rig.git.onArgs([]string{"rev-parse", "--show-toplevel"}, "/w", "", nil)
	rig.git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, branch, "", nil)
}

// itoa10 lives in push_test.go — we reuse it here for the
// readiness fixture.

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
func setupPushMocks(git *pushGit, _ string, snap messages.GitStatusSnapshot) {
	git.onArgs(statusCmd, porcelainFromSnapshot(snap), "", nil)
}
