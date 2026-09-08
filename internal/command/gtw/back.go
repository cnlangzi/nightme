package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// RunBack is the entry point for `/gtw back`. The non-destructive
// counterpart to RunClose: switch CWD back to the main repo
// without removing the worktree, the branch, or the .nightme/
// gtw.yml — and then run `gtw sync` on the main repo so the next
// /gtw fix has a clean baseline.
//
// Flow (mirrors RunClose, drops the destructive + reset steps):
//
//  1. Read `<cs.SelectedCwd()>/.nightme/gtw.yml` (same source
//     of truth RunClose uses — no walk-up by design, see
//     wip/gtw.md §14.4).
//  2. Refuse if missing — there's no active fix to back out of.
//  3. SetSelectedCwd back to repoRoot (RunClose's step 6).
//     The worktree, the branch, and the yml stay on disk — user
//     can /cwd back into the worktree to resume, or run
//     /gtw fix <other-id> from the main repo.
//  4. Emit a one-line "back" card naming the repoRoot we landed
//     on (NOT a close-style teardown card — the worktree was not
//     touched). The worktree still exists; just the chat's
//     SelectedCwd has moved.
//  5. Run buildSyncReply on repoRoot and emit its card. Errors
//     surface with ❌ prefix; sync is the LAST step so the user
//     reads "back to root" first, then "synced" / "sync failed".
//
// Skipped vs RunClose (each with the why):
//
//   - Step 2.5 EvictAgentSessionsInCwd(c.Worktree): the worktree
//     is NOT being removed, so any ASes pinned to it must stay
//     alive. The user may `/cwd` back into the worktree to
//     continue.
//   - Step 3 assertWorktreeClean: back is a temporary cwd swap,
//     not a teardown. A dirty worktree is fine — the user's
//     uncommitted work is exactly the point of being able to
//     come back. (RunClose is hard-refusal because removing a
//     dirty worktree is destructive; back never removes
//     anything.)
//   - Step 4 WorktreeRemove / Step 5 branch -D: the whole
//     non-destructive premise.
//   - Step 6.5 PR cache clear: the branch is still alive;
//     caching is still valid.
//   - Step 7 slot.Store(Context{}): the fix is still in flight
//     per the yml; reaction handlers for the same worktree
//     should keep routing. (Clearing would silently break
//     reactions if the user /cwd's back into the worktree.)
//   - Step 9 /new: back is reversible — the user might resume
//     the fix in minutes. Forcing a context reset would be
//     hostile. RunClose does /new because its whole purpose is
//     "tear down the experiment, start fresh".
func RunBack(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (*Result, error) {
	selectedCwd := cs.SelectedCwd()

	// --- step 0: defensive empty-cwd guard ----------------------
	// runBack's handler-level preflight (cmd.go runBack) is the
	// primary gate and produces the standard "no active
	// workspace" reply via RequireActiveCwd. This guard is the
	// belt-and-suspenders second line for direct callers of
	// RunBack (tests, future call sites) — without it,
	// ReadGTWYml("") resolves gtwYmlPath("") to
	// pathutil.Join("", ".nightme", "gtw.yml") which either
	// silently finds a stale yml under the daemon's CWD or
	// surfaces the misleading "no active fix to back out of"
	// when the yml genuinely doesn't exist. Same wording as
	// RequireActiveCwd so users see one consistent reply no
	// matter which path catches the empty-cwd case.
	if selectedCwd == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply), nil
	}

	// --- step 1+2: locate the snapshot ---------------------------
	c, err := ReadGTWYml(selectedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				"❌ no active fix to back out of in this chat\n"+
					"hint: /cwd into the /gtw fix worktree first (its "+
					"`.nightme/gtw.yml` is the back source of truth)."), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err)), nil
	}

	if c.RepoRoot == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: repoRoot is empty"), nil
	}
	// Worktree may legitimately be empty for a freshly-started
	// fix that hasn't reached §5.2.④ yet — back is still safe
	// (we just SetSelectedCwd back). We surface c.Worktree in
	// the card only when set so a half-formed yml doesn't show
	// a misleading "(empty)" path.

	// --- step 3: switch CWD back to repoRoot ---------------------
	// Same pattern as RunClose's step 6, but with a different
	// failure-mode story: nothing on disk has changed, so the
	// worst case is "the chat is still pointing at the
	// worktree" — much less urgent than the close-path
	// equivalent where the worktree IS gone.
	if err := cs.SetSelectedCwd(c.RepoRoot); err != nil {
		slog.Default().Warn("gtw: SetSelectedCwd back to repoRoot failed",
			"repo_root", c.RepoRoot,
			"err", err)
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("⚠️ SetSelectedCwd(%s) failed: %v\n"+
				"worktree at %s is unchanged; run `/cwd %s` manually.",
				c.RepoRoot, err, c.Worktree, c.RepoRoot)), nil
	}

	// --- step 4: back's own success card ------------------------
	// Single-line card that mirrors the close-card format (✅
	// headline + `→` rows) but says "back to root" instead of
	// "closed X". Listing c.Worktree as "preserved" makes the
	// non-destructive intent obvious — the user shouldn't have
	// to wonder whether `back` is a synonym for `close`.
	//
	// Rows are built conditionally so a partially-written yml
	// (Worktree set but Branch empty — the "fix didn't reach
	// §5.2.④" half-window where Worktree was captured but the
	// branch name hadn't been assigned yet) doesn't render as
	// "→ branch:  (preserved)" with a blank slot.
	var rows []string
	if c.Worktree != "" {
		rows = append(rows, fmt.Sprintf("→ worktree: %s (preserved)", c.Worktree))
		if c.Branch != "" {
			rows = append(rows, fmt.Sprintf("→ branch: %s (preserved)", c.Branch))
		}
		rows = append(rows,
			fmt.Sprintf("→ .nightme/gtw.yml (preserved — `/cwd %s` to resume)", c.Worktree))
	} else {
		// No worktree recorded yet — fix didn't reach §5.2.④.
		// Don't reference c.Branch at all here; it could be
		// empty for the same reason Worktree is, or populated
		// from an even-earlier step that means nothing to the
		// user. Just surface the state we know about.
		rows = append(rows, "→ worktree: (not yet recorded — fix didn't reach §5.2.④)")
		rows = append(rows, "→ .nightme/gtw.yml (preserved)")
	}
	body := "✅ back to `" + c.RepoRoot + "`\n" + strings.Join(rows, "\n")
	// Mid-flow by design — the sync card that follows in
	// step 5 is a separate reply, matching the close-path
	// two-card shape (close + sync, back + sync).
	reply(ctx, cs.Emitter(), chatID, messageID, body)

	// --- step 5: sync main (separate card) ----------------------
	// buildSyncReply is the same helper runSync + runClose use;
	// sharing the formatter keeps the "synced" / "already up to
	// date" / "❌ sync failed" surface identical across the three
	// commands. Respect deps.SkipRefreshDefaultBranch as a
	// short-circuit (test-only).
	//
	// Single terminal step: all three branches (error / normal /
	// skip) fall through to the same return.
	syncBody, syncErr := buildSyncReply(ctx, c.RepoRoot, deps)
	if syncErr != nil {
		// Same pattern as RunClose's step 10: stamp a ❌ prefix
		// unconditionally so the user can tell the sync failure
		// apart from the back-success card above. RefreshDefaultBranch
		// returns plain fmt.Errorf values whose messages already
		// include git stderr tails + user-facing hints — the prefix
		// is the only wrapping RunClose / RunBack do.
		reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ sync failed: "+syncErr.Error())
	} else if syncBody != "" {
		reply(ctx, cs.Emitter(), chatID, messageID, syncBody)
	}
	// else: SkipRefreshDefaultBranch set (test-only); no sync
	// card. The back-success card above still stands.

	return &Result{Consumed: true}, nil
}

// RunBackToWorktree is the entry point for `/gtw back <slug>`.
// The mirror of RunBack: jump from the chat's main repo root
// into the named fix worktree, repairing a missing
// .nightme/gtw.yml when needed.
//
// `slug` is the basename of the worktree directory under
// `<parent>/<repoName>.nightme/<slug>` (the layout WorktreePath
// builds). It is NOT a branch name and NOT an absolute path —
// resolving anything else would let a typo silently land in the
// wrong directory, while the layout's basename is what every
// existing /gtw fix call writes.
//
// The new path is the strict inverse of the existing RunBack
// path:
//
//   - RunBack requires cwd to be IN a worktree (refuses when
//     at the repo root — no worktree to step out of).
//   - RunBackToWorktree requires cwd to BE the repo root
//     (refuses when inside a worktree — caller must `/gtw back`
//     first).
//
// These two gates make the two directions non-overlapping: a
// user inside a worktree types `/gtw back` to leave it; only
// from the main repo does `/gtw back <name>` make sense.
//
// yml repair (when <wt>/.nightme/gtw.yml is missing):
//
//  1. `branch     ← git -C <wt> branch --show-current`
//  2. `repoRoot   ← git -C <wt> rev-parse --show-toplevel`
//  3. `worktree   ← the resolved path itself`
//  4. `mode       = ModeLocal` (we cannot recover Issue/Repo/
//     Provider from git alone; ModeLocal is the only safe
//     default that lets `/gtw close` work without trying to
//     call GitHub/GitLab to clear a WIP label)
//
// The success card surfaces whether the yml was repaired so
// the user can tell a ModeLocal stub apart from a yml that
// survived from the original `/gtw fix`.
func RunBackToWorktree(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, slug string,
) (*Result, error) {
	selectedCwd := cs.SelectedCwd()
	if selectedCwd == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply), nil
	}

	// --- resolve the main repo root from chat cwd --------------
	repoRoot, err := RepoRoot(ctx, selectedCwd, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	if n, nerr := pathutil.NormalizeForOS(repoRoot); nerr == nil {
		repoRoot = n
	}
	if n, nerr := pathutil.NormalizeForOS(selectedCwd); nerr == nil {
		selectedCwd = n
	}

	// --- gate: must be at the repo root, not inside a worktree -
	// Symmetric to RunBack's "must be inside a worktree" gate.
	// The two together keep the directions from being able to
	// fire in the wrong context.
	if selectedCwd != repoRoot {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ /gtw back <worktree> requires cwd at the main repo root.\n"+
				"current cwd is inside a worktree; run `/gtw back` (no arg) first."), nil
	}

	// --- resolve slug → worktree path --------------------------
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ /gtw back: worktree name is empty"), nil
	}
	worktreePath := WorktreePath(repoRoot, slug)

	// path must exist and be a known worktree of this repo
	if info, serr := os.Stat(worktreePath); serr != nil {
		if os.IsNotExist(serr) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ worktree %q not found at %s", slug, worktreePath)), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ stat worktree path: %v", serr)), nil
	} else if !info.IsDir() {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %s exists but is not a directory", worktreePath)), nil
	}
	known, kerr := IsKnownWorktree(ctx, repoRoot, worktreePath, deps.Git)
	if kerr != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ git worktree list: %v", kerr)), nil
	}
	if !known {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %s is not a git worktree of this repository", worktreePath)), nil
	}

	// --- yml: skip if present, repair from git if missing -----
	ymlPath := gtwYmlPath(worktreePath)
	repaired := false
	if _, serr := os.Stat(ymlPath); serr != nil {
		if !os.IsNotExist(serr) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ stat .nightme/gtw.yml: %v", serr)), nil
		}
		// Recover a qualified stub from git.
		branch := ""
		if b, _, berr := deps.Git.Run(ctx, worktreePath,
			"branch", "--show-current"); berr == nil {
			branch = strings.TrimSpace(b)
		}
		wtRepoRoot := repoRoot
		if r, _, rerr := deps.Git.Run(ctx, worktreePath,
			"rev-parse", "--show-toplevel"); rerr == nil {
			if n, nerr := pathutil.NormalizeForOS(strings.TrimSpace(r)); nerr == nil && n != "" {
				wtRepoRoot = n
			}
		}
		if werr := WriteGTWYml(worktreePath, Context{
			Mode:     ModeLocal,
			Branch:   branch,
			Worktree: worktreePath,
			RepoRoot: wtRepoRoot,
			State:    StateFixing,
		}, deps.Now); werr != nil {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ repair .nightme/gtw.yml: %v", werr)), nil
		}
		repaired = true
	}

	// --- switch cwd into the worktree --------------------------
	if serr := cs.SetSelectedCwd(worktreePath); serr != nil {
		slog.Default().Warn("gtw: SetSelectedCwd into worktree failed",
			"worktree", worktreePath,
			"err", serr)
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("⚠️ SetSelectedCwd(%s) failed: %v", worktreePath, serr)), nil
	}

	// --- success card -----------------------------------------
	// Mirrors RunBack's row-style card so the two directions
	// read the same shape at a glance. The `repaired` line is the
	// only visual hint that distinguishes a ModeLocal stub from
	// a yml that survived a previous `/gtw fix`.
	ymlLine := "→ .nightme/gtw.yml (preserved)"
	if repaired {
		ymlLine = "→ .nightme/gtw.yml (repaired — ModeLocal stub; re-dispatch any issue manually)"
	}
	body := fmt.Sprintf("✅ back into `%s`\n→ worktree: %s\n%s",
		worktreePath, worktreePath, ymlLine)
	reply(ctx, cs.Emitter(), chatID, messageID, body)

	return &Result{Consumed: true}, nil
}
