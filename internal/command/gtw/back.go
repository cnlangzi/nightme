package gtw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// RunBack is the entry point for `/gtw back`. The non-destructive
// counterpart to RunClose: switch CWD back to the main repo
// without removing the worktree, the branch, or the .nightme/
// gtw.yml — and then run `gtw sync` on the main repo so the next
// /gtw fix has a clean baseline.
//
// Flow (mirrors RunClose, drops the destructive steps):
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
//   - Step 2.6 EvictAgentSessionsInCwd(c.Worktree): the worktree
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
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply,
			command.FailedOutcome("no_cwd", "the command needs an active workspace")), nil
	}

	// --- step 1+2: locate the snapshot ---------------------------
	c, err := ReadGTWYml(selectedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				"❌ no active fix to back out of in this chat\n"+
					"hint: /cwd into the /gtw fix worktree first (its "+
					"`.nightme/gtw.yml` is the back source of truth).",
				command.FailedOutcome("no_active_fix", "there is no active fix to leave")), nil
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err),
			command.FailedOutcome("bad_state", "the gtw state file could not be read")), nil
	}

	if c.RepoRoot == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: repoRoot is empty",
			command.FailedOutcome("bad_state", "the gtw state file is malformed")), nil
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
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("⚠️ SetSelectedCwd(%s) failed: %v\n"+
				"worktree at %s is unchanged; run `/cwd %s` manually.",
				c.RepoRoot, err, c.Worktree, c.RepoRoot),
			messages.Outcome{
				Status:  messages.OutcomeIncomplete,
				Summary: "the chat stayed in the worktree",
				Data: map[string]string{
					"reason":    "cwd_not_changed",
					"worktree":  c.Worktree,
					"repo_root": c.RepoRoot,
				},
			}), nil
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
	syncBody, _, syncErr := buildSyncReply(ctx, c.RepoRoot, deps)
	if syncErr != nil {
		// Same pattern as RunClose's step 9: stamp a ❌ prefix
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

	// The move out of the worktree happened; the sync that follows
	// is a separate step. A failed sync leaves the chat in the
	// main repo but the main branch unrefreshed.
	if syncErr != nil {
		reportOutcome(ctx, messages.Outcome{
			Status:  messages.OutcomeIncomplete,
			Summary: "the chat left the worktree but the main branch could not be synced",
			Data: map[string]string{
				"reason":    "sync_failed",
				"worktree":  c.Worktree,
				"repo_root": c.RepoRoot,
			},
		})
		return &Result{Consumed: true}, nil
	}
	reportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "the chat left the worktree",
		Data: map[string]string{
			"worktree":  c.Worktree,
			"repo_root": c.RepoRoot,
		},
	})
	return &Result{Consumed: true}, nil
}

// worktreeMatchError is the failure of resolveWorktreeBranch: the
// argument matched no worktree branch, or matched more than one.
type worktreeMatchError struct {
	Reason    string // "not_found" | "ambiguous"
	Arg       string // the argument the user typed
	Matches   []WorktreeEntry
	Available []WorktreeEntry
}

// Error implements error. The text doubles as the outcome summary.
func (e *worktreeMatchError) Error() string {
	if e.Reason == "ambiguous" {
		return fmt.Sprintf("%s matches several worktree branches", e.Arg)
	}
	return fmt.Sprintf("no worktree branch matches %s", e.Arg)
}

// candidates returns the entries the failure card lists: the prefix
// matches for an ambiguous prefix, every worktree branch for a
// miss.
func (e *worktreeMatchError) candidates() []WorktreeEntry {
	if e.Reason == "ambiguous" {
		return e.Matches
	}
	return e.Available
}

// branchNames returns the branch names of the entries.
func branchNames(entries []WorktreeEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Branch)
	}
	return names
}

// resolveWorktreeBranch resolves arg — a branch-name prefix — to the
// worktree of this repository that holds the matching branch.
//
// entries is the repository's worktree listing in git's order (the
// primary checkout comes first); primary is that checkout's path.
//
// The candidate set is every worktree of the repository except the
// primary checkout, minus the detached ones: the branch is the
// handle, and a hand-made worktree (`git worktree add ../scratch -b
// scratch`) is otherwise unreachable. The primary checkout is never a
// candidate — `/gtw back main` must not "enter" the directory the
// chat already sits in.
//
// An exact match wins over longer branches that share the prefix.
// An ambiguous prefix is an error, not a guess: landing the chat in
// the wrong worktree is worse than asking.
func resolveWorktreeBranch(entries []WorktreeEntry, primary, arg string) (WorktreeEntry, *worktreeMatchError) {
	var candidates, available []WorktreeEntry
	for _, e := range entries {
		if sameWorktreePath(e.Path, primary) {
			continue
		}
		if e.Branch == "" {
			// Detached HEAD (or a bare repo): no branch to match.
			continue
		}
		available = append(available, e)
		if strings.HasPrefix(e.Branch, arg) {
			candidates = append(candidates, e)
		}
	}
	for _, c := range candidates {
		if c.Branch == arg {
			return c, nil
		}
	}
	switch len(candidates) {
	case 0:
		return WorktreeEntry{}, &worktreeMatchError{
			Reason: "not_found", Arg: arg, Available: available,
		}
	case 1:
		return candidates[0], nil
	default:
		return WorktreeEntry{}, &worktreeMatchError{
			Reason: "ambiguous", Arg: arg, Matches: candidates,
		}
	}
}

// renderWorktreeMatchError renders the failure card of a
// resolveWorktreeBranch miss — no branch matched the argument, or
// several did. One helper so every caller of the resolver words the
// two failures the same way.
func renderWorktreeMatchError(e *worktreeMatchError) string {
	var b strings.Builder
	if e.Reason == "ambiguous" {
		fmt.Fprintf(&b, "❌ %q matches %d worktree branches:\n", e.Arg, len(e.Matches))
		for _, m := range e.Matches {
			fmt.Fprintf(&b, "→ %s\n", m.Branch)
		}
		b.WriteString("hint: type the full branch name")
		return b.String()
	}
	fmt.Fprintf(&b, "❌ no worktree branch matches %q\n", e.Arg)
	if len(e.Available) == 0 {
		b.WriteString("this repository has no worktrees — run `/gtw fix <issue-id>` " +
			"or `/gtw fix --name <branch>` to create one")
		return b.String()
	}
	b.WriteString("worktree branches:\n")
	for _, a := range e.Available {
		fmt.Fprintf(&b, "→ %s\n", a.Branch)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RunBackToWorktree is the entry point for `/gtw back <branch>`.
// The mirror of RunBack: jump from the chat's main repo root
// into the fix worktree that holds the matching branch, repairing a
// missing .nightme/gtw.yml when needed.
//
// `branch` is a PREFIX of a worktree branch name — the whole branch
// is not required. When exactly one branch matches, that worktree is
// entered; when several match and one of them equals the argument
// exactly, that one wins; when the choice is still ambiguous the
// command fails and lists the branches, so the next attempt is
// unambiguous.
//
// The candidate set is every worktree of the repository, not only
// the ones under `<repo>.nightme/`: a worktree the user made by hand
// is reachable the same way.
//
// The new path is the strict inverse of the existing RunBack
// path:
//
//   - RunBack requires cwd to be IN a worktree (refuses when
//     at the repo root — no worktree to step out of).
//   - RunBackToWorktree requires cwd to BE the primary checkout
//     (refuses when inside a worktree — caller must `/gtw back`
//     first).
//
// These two gates make the two directions non-overlapping: a
// user inside a worktree types `/gtw back` to leave it; only
// from the main repo does `/gtw back <branch>` make sense.
//
// yml repair (when <wt>/.nightme/gtw.yml is missing):
//
//  1. `branch   ← the branch the worktree holds, per the worktree
//     listing` (no second git call — the listing already says it)
//  2. `worktree ← the resolved path itself`
//  3. `mode     = ModeLocal` (we cannot recover Issue/Repo/
//     Provider from git alone; ModeLocal is the only safe
//     default that lets `/gtw close` work without trying to
//     call GitHub/GitLab to clear a WIP label)
//  4. `repoRoot = the primary checkout` (the first record of the
//     worktree listing — DO NOT re-query git from inside the
//     worktree: `git rev-parse --show-toplevel` run in a worktree
//     returns the worktree's own toplevel, which IS
//     `worktreePath`. Writing that into the yml would make
//     later `/gtw close` `SetSelectedCwd(c.RepoRoot)` to the
//     just-removed path and trigger the "dangling cwd" branch
//     in RunClose step 0.5.)
//
// The repair only touches a worktree under `<repo>.nightme/` — one
// nightme created. The yml is the token `/gtw close` tears a
// worktree and its branch down with, and nightme does not claim a
// worktree the user made by hand.
//
// The success card surfaces whether the yml was repaired so
// the user can tell a ModeLocal stub apart from a yml that
// survived from the original `/gtw fix`.
func RunBackToWorktree(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, branch string,
) (*Result, error) {
	selectedCwd := cs.SelectedCwd()
	if selectedCwd == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply,
			command.FailedOutcome("no_cwd", "the command needs an active workspace")), nil
	}

	// --- the chat must be in a git repository -------------------
	// `rev-parse --show-toplevel` is the probe for that — and
	// nothing more: inside a linked worktree it reports the
	// WORKTREE's own root, so it cannot tell the primary checkout
	// from a linked one.
	if _, err := RepoRoot(ctx, selectedCwd, deps.Git); err != nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first.",
			command.FailedOutcome("not_a_repo", "the workspace is not inside a git repository")), nil
	}

	// --- the worktrees of this repository -----------------------
	// Git lists the main worktree first, so entries[0] is the
	// primary checkout: the directory `/gtw back` runs from, the
	// one `/gtw close` tears the branch down from, and the one
	// that is never a candidate.
	entries, lerr := ListWorktrees(ctx, selectedCwd, deps.Git)
	if lerr != nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ git worktree list: %v", lerr),
			command.FailedOutcome("error", "the worktrees could not be listed")), nil
	}
	if len(entries) == 0 {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first.",
			command.FailedOutcome("not_a_repo", "the workspace is not inside a git repository")), nil
	}
	primary := entries[0].Path

	// --- gate: must be at the primary checkout -----------------
	// Symmetric to RunBack's "must be inside a worktree" gate.
	// The two together keep the directions from being able to
	// fire in the wrong context.
	//
	// The comparison is against the primary checkout from the
	// listing, not against `rev-parse --show-toplevel`: inside a
	// linked worktree that command reports the worktree's own
	// root, so a cwd sitting exactly at a linked worktree's root
	// would compare equal to it and slip through.
	if !sameWorktreePath(selectedCwd, primary) {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ /gtw back <branch> requires cwd at the main repo root.\n"+
				"current cwd is inside a worktree; run `/gtw back` (no arg) first.",
			command.FailedOutcome("wrong_cwd", "the chat is not at the main repo root")), nil
	}

	// --- resolve the branch prefix → worktree ------------------
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ /gtw back: branch name is empty",
			command.FailedOutcome("usage", "no branch name was given")), nil
	}
	entry, matchErr := resolveWorktreeBranch(entries, primary, branch)
	if matchErr != nil {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			renderWorktreeMatchError(matchErr),
			command.FailedOutcomeData(matchErr.Reason, matchErr.Error(),
				map[string]string{
					"candidates": strings.Join(branchNames(matchErr.candidates()), ", "),
				})), nil
	}
	worktreePath := entry.Path

	// --- the worktree must still be on disk --------------------
	if info, serr := os.Stat(worktreePath); serr != nil {
		if os.IsNotExist(serr) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ worktree for %q is gone: %s\n"+
					"hint: run `git worktree prune` to drop the stale entry",
					branch, worktreePath),
				command.FailedOutcome("not_found", "the worktree directory is gone")), nil
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ stat worktree path: %v", serr),
			command.FailedOutcome("error", "the worktree path could not be read")), nil
	} else if !info.IsDir() {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ %s exists but is not a directory", worktreePath),
			command.FailedOutcome("error", "the worktree path is not a directory")), nil
	}

	// --- yml: skip if present, repair from git if missing -----
	ymlPath := gtwYmlPath(worktreePath)
	repaired := false
	// The yml is the token `/gtw close` tears a worktree (and its
	// branch) down with, so nightme only stamps it into a worktree
	// it owns — one under this repository's nightme layout. A
	// worktree the user made by hand stays untouched: nightme
	// must not claim it, and `/gtw close` must not be able to
	// delete a branch nightme never created.
	owned := underNightmeLayout(primary, worktreePath)
	if _, serr := os.Stat(ymlPath); serr != nil {
		if !os.IsNotExist(serr) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ stat .nightme/gtw.yml: %v", serr),
				command.FailedOutcome("bad_state", "the gtw state file could not be read")), nil
		}
		if owned {
			// Recover a qualified stub from git.
			//
			// RepoRoot is the primary checkout — NOT a second
			// git invocation inside the worktree.
			// `git -C <worktree> rev-parse --show-toplevel`
			// returns the worktree's OWN toplevel (==
			// worktreePath), not the parent repo root, which
			// would poison later /gtw close.
			//
			// The branch comes from the worktree listing the
			// resolver already read — one git call fewer, and no
			// chance for the two sources to disagree.
			werr := WriteGTWYml(worktreePath, Context{
				Mode:     ModeLocal,
				Branch:   entry.Branch,
				Worktree: worktreePath,
				RepoRoot: primary,
				State:    StateFixing,
			}, deps.Now)
			if werr != nil && !errors.Is(werr, ErrGtwYmlExists) {
				return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
					fmt.Sprintf("❌ repair .nightme/gtw.yml: %v", werr),
					command.FailedOutcome("bad_state", "the gtw state file could not be repaired")), nil
			}
			// ErrGtwYmlExists: another chat raced ahead and wrote
			// the yml between our stat and write. The desired
			// state is already on disk; surface as preserved (not
			// repaired) so the user sees the right success card.
			repaired = true
		}
	}

	// --- switch cwd into the worktree --------------------------
	if serr := cs.SetSelectedCwd(worktreePath); serr != nil {
		slog.Default().Warn("gtw: SetSelectedCwd into worktree failed",
			"worktree", worktreePath,
			"err", serr)
		// If the yml was just repaired, the worktree now carries
		// a ModeLocal stub; subsequent /gtw close / /cwd back
		// into the worktree will see the stub. Surface that in
		// the recovery hint so the user is not surprised.
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("⚠️ SetSelectedCwd(%s) failed: %v\n"+
				"worktree at %s is intact%s; run `/cwd %s` manually.",
				worktreePath, serr, worktreePath,
				ymlRepairSuffix(repaired), worktreePath),
			messages.Outcome{
				Status:  messages.OutcomeIncomplete,
				Summary: "the chat stayed outside the worktree",
				Data: map[string]string{
					"reason":   "cwd_not_changed",
					"worktree": worktreePath,
				},
			}), nil
	}

	// --- success card -----------------------------------------
	// Mirrors RunBack's row-style card so the two directions
	// read the same shape at a glance. The last line is the
	// only visual hint that distinguishes a ModeLocal stub from
	// a yml that survived a previous `/gtw fix` — and from a
	// worktree nightme does not own, which gets no yml at all.
	ymlLine := "→ .nightme/gtw.yml (preserved)"
	switch {
	case repaired:
		ymlLine = "→ .nightme/gtw.yml (repaired — ModeLocal stub; re-dispatch any issue manually)"
	case !owned:
		ymlLine = "→ .nightme/gtw.yml (not created — this worktree is not a /gtw fix worktree)"
	}
	body := fmt.Sprintf("✅ back into `%s`\n→ worktree: %s\n→ branch: %s\n%s",
		worktreePath, worktreePath, entry.Branch, ymlLine)
	reply(ctx, cs.Emitter(), chatID, messageID, body)

	reportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: fmt.Sprintf("the chat entered the worktree %s", worktreePath),
		Data: map[string]string{
			"worktree":  worktreePath,
			"repo_root": primary,
			"branch":    entry.Branch,
		},
	})
	return &Result{Consumed: true}, nil
}

// underNightmeLayout reports whether worktreePath lives in the
// nightme worktree directory of the repository at primary —
// <parent>/<repoName>.nightme/ — the directory `/gtw fix` creates
// worktrees in.
//
// A worktree outside it is one the user made by hand. nightme did
// not create it and must not claim it: the `.nightme/gtw.yml` it
// would stamp is the token `/gtw close` tears the worktree and its
// branch down with.
//
// The comparison resolves symlinks on both sides: on macOS the
// same directory is spelled /var/... by one caller and
// /private/var/... by the other.
func underNightmeLayout(primary, worktreePath string) bool {
	layout := WorktreeLayoutDir(primary)
	if pathutil.IsUnder(worktreePath, layout) {
		return true
	}
	realLayout, err := filepath.EvalSymlinks(layout)
	if err != nil {
		return false
	}
	realWorktree, err := filepath.EvalSymlinks(worktreePath)
	if err != nil {
		return false
	}
	return pathutil.IsUnder(realWorktree, realLayout)
}

// ymlRepairSuffix returns a short parenthetical describing
// whether the yml was just (re)written. Used by the
// SetSelectedCwd-failed recovery hint so the user knows
// whether a follow-up /gtw close will see a stub. Empty when
// the yml was untouched (the common case).
func ymlRepairSuffix(repaired bool) string {
	if repaired {
		return " (yml was created as a ModeLocal stub; /gtw close will read it)"
	}
	return ""
}
