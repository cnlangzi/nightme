package gtw

import (
	"fmt"
	"os"
	"path/filepath"
)

// preflightOrphanYml guards /gtw fix against one specific case:
// SelectedCwd is itself the worktree of an in-flight fix (i.e.
// .nightme/gtw.yml exists at SelectedCwd). This means the user
// /cwd'd into a /gtw fix worktree and then started a new /gtw
// fix without running /gtw close first — the new fix would
// inherit the old fix's slot/yml and immediately contradict it.
//
// What this function does NOT guard (v1.x, by design):
//
//   - Other sibling worktrees under the same repo holding ymls.
//     gtw supports N parallel /gtw fix in the same repo, each
//     owning its own worktree + yml. The yml is the per-worktree
//     source of truth; /gtw close reads it from CWD and tears
//     down only that worktree. The whole point of git worktrees
//     is to enable parallel branches on one machine — a global
//     "one fix per repo" gate would defeat the tool's value
//     proposition.
//
//   - Stale ymls in worktrees whose chat was lost (daemon
//     restart, different chat ID, etc.). They are per-worktree
//     state, not a cross-worktree hazard. /gtw close from the
//     owning worktree (or /gtw close --force from anywhere with
//     that worktree as CWD) reconciles them. If the user really
//     wants to abandon a stale worktree, they can `git worktree
//     remove --force` it; the yml goes with the directory.
//
// History: v1 carried a "sibling yml" check that rejected any
// second /gtw fix under the same repo. It was removed because
// (a) the failure scenario it described relied on the yml being
// shared state, which it never was — each worktree has its own —
// and (b) blocking parallel worktrees defeated the tool's
// purpose. See commit history for the removal.
//
// Returns nil if SelectedCwd is fine, or a user-friendly error
// reply text otherwise.
func preflightOrphanYml(selectedCwd string) error {
	target := filepath.Join(selectedCwd, nightmeDirName, gtwYmlName)
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf(
			"❌ %s already exists\n"+
				"  this directory is the current /gtw fix worktree.\n"+
				"  fix: run /gtw close here (or /cwd into another worktree)",
			target,
		)
	}
	return nil
}