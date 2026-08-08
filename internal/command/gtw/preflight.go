package gtw

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// preflightOrphanYml refuses /gtw fix when there's an unfinished
// /gtw fix on this repo. Two cases produce an orphan yml:
//
//  1. ActiveCwd IS the fix worktree (i.e. the user is sitting
//     inside a worktree that /gtw fix created, and forgot to
//     /gtw close before running another /gtw fix).
//  2. A sibling worktree under the same repo holds an orphan
//     .nightme/gtw.yml from a previous /gtw fix whose /gtw close
//     never ran. The slot is empty (in-memory state was cleared
//     by a daemon restart or a different chat), but the on-disk
//     yml survives.
//
// Without this check, /gtw fix #2 would silently overwrite
// worktree #2 onto a fresh path, ignore the existing yml
// (ErrGtwYmlExists is warn-only inside completeFixAndDispatch),
// and leave the user with in-memory state pointing at the new
// fix while the yml describes the old one. /gtw close then
// reads the old yml and tries to remove a now-non-existent
// worktree. See wip/gtw.md §14.10 risk table for the original
// discussion.
//
// Returns nil if no orphan is detected, or a user-friendly
// error reply text otherwise.
func preflightOrphanYml(ctx context.Context, activeCwd string, git GitRunner) error {
	// Case 1: ActiveCwd itself is a fix worktree.
	if _, err := os.Stat(filepath.Join(activeCwd, nightmeDirName, gtwYmlName)); err == nil {
		return fmt.Errorf(
			"❌ %s already exists\n"+
				"  this directory is the current /gtw fix worktree.\n"+
				"  fix: run /gtw close here (or /cwd into another worktree)",
			filepath.Join(activeCwd, nightmeDirName, gtwYmlName),
		)
	}

	// Case 2: a sibling worktree under the same repo holds an
	// orphan yml. `git worktree list --porcelain` lists ALL
	// linked worktrees (main repo + every worktree).
	out, _, err := git.Run(ctx, activeCwd, "worktree", "list", "--porcelain")
	if err != nil {
		// Not a git repo / git unavailable — let downstream
		// preflight (RepoRoot / PreflightWorktreeCreate) catch
		// the real failure. Don't fail on our scan.
		return nil
	}

	for _, p := range parseWorktreePaths(out) {
		if filepath.Clean(p) == filepath.Clean(activeCwd) {
			continue // already checked in case 1
		}
		if _, err := os.Stat(filepath.Join(p, nightmeDirName, gtwYmlName)); err == nil {
			return fmt.Errorf(
				"❌ found unfinished /gtw fix in sibling worktree %s\n"+
					"  fix: /cwd into that worktree and run /gtw close first",
				p,
			)
		}
	}
	return nil
}

// parseWorktreePaths extracts the absolute paths from
// `git worktree list --porcelain` output. Each worktree entry
// starts with `worktree <path>`; entries are separated by blank
// lines. Only paths are returned — branch / HEAD lines are
// ignored.
//
// Example input:
//
//	worktree /home/user/repo
//	HEAD abc123
//	branch refs/heads/main
//
//	worktree /home/user/repo.wt-fix-42
//	HEAD def456
//	branch refs/heads/fix/42-foo
//
// Output: ["/home/user/repo", "/home/user/repo.wt-fix-42"]
func parseWorktreePaths(porcelain string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(porcelain))
	for sc.Scan() {
		line := sc.Text()
		const prefix = "worktree "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		out = append(out, strings.TrimPrefix(line, prefix))
	}
	return out
}