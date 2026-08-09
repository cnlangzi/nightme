package gtw

import (
	"fmt"
	"os"
	"path/filepath"
)

// preflightOrphanYml is the single guard for /gtw fix. gtw is
// per-directory: every directory is its own island, and the
// .nightme/gtw.yml state at <directory>/.nightme/gtw.yml is
// fully scoped to that directory. A yml in some other directory
// (a sibling worktree, the main repo, anywhere else) is
// irrelevant to /gtw fix starting from here.
//
// The ONE case this function blocks: SelectedCwd's own
// .nightme/gtw.yml exists, meaning the user is standing inside
// a worktree that holds an in-flight /gtw fix. The user got
// here by /cwd'ing into a previous fix's worktree and then
// running /gtw fix again without /gtw close first. The new fix
// would inherit the old fix's slot + yml and immediately
// contradict it. The recovery is to either /gtw close here, or
// /cwd into ANY other directory (a sibling worktree, the main
// repo, a brand-new worktree) — /gtw fix then proceeds normally.
//
// What this function does NOT guard (v1.x, by design):
//
//   - Sibling worktrees under the same repo holding ymls. They
//     are independent state, not a hazard. A yml in worktree A
//     tells you nothing about what should happen in worktree B.
//     Parallel /gtw fix across separate worktrees is the
//     explicit design — git worktrees exist to enable parallel
//     branches on one machine.
//
//   - Stale ymls in worktrees whose chat was lost (daemon
//     restart, different chat ID, etc.). They are per-directory
//     state, not a cross-directory hazard. /gtw close from the
//     owning directory (or /gtw close --force from anywhere with
//     that directory as CWD) reconciles them. To abandon a stale
//     worktree, `git worktree remove --force` it; the yml goes
//     with the directory.
//
// History: v1 carried a "sibling yml" check that rejected any
// second /gtw fix under the same repo. It conflated "this
// directory has a yml" (real hazard) with "some sibling has a
// yml" (no hazard, just independent state). The sibling check
// was removed; the per-directory check is what remained. See
// commit history for the removal.
//
// Returns nil if SelectedCwd has no yml, or a user-friendly
// error reply text otherwise.
func preflightOrphanYml(selectedCwd string) error {
	target := filepath.Join(selectedCwd, nightmeDirName, gtwYmlName)
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf(
			"❌ %s already exists\n"+
				"  this directory is the current /gtw fix worktree.\n"+
				"  fix: /gtw close here, or /cwd into another directory",
			target,
		)
	}
	return nil
}