package gtw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/newcmd"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// statPath is the os.Stat function used by RunClose to detect
// unreachable cwd. Exposed as a package-level var so tests can
// swap in a stub that returns non-IsNotExist errors (EACCES,
// EIO, ESTALE, etc.) without chmod'ing the worktree directory —
// a transient stat failure must NOT clear selectedCwd / the AS
// pool, and that's easier to lock in via a stub than by chmod
// races (chmod 000 is silently bypassed for root on Linux, and
// the test would race with later reads anyway). Production uses
// os.Stat directly; tests restore via withStatStub in close_test.go.
var statPath = os.Stat

// maxDirtyFilesReported caps how many uncommitted paths we
// surface in the "worktree is dirty" error reply. 10 is the
// sweet spot the user tested with: enough to identify the
// problem (top of the porcelain output is the most-recent
// changes), short enough that a Feishu reply doesn't get
// truncated by the channel's own message-size limits.
const maxDirtyFilesReported = 10

// RunClose is the entry point for `/gtw close`. Mirrors RunFix
// in shape (ctx, cs, deps, chatID, messageID; returns
// *Result, error) so the factory can wire it through the same
// dispatcher pipeline.
//
// Flow (wip/gtw.md §14.5):
//
//  1. Look for `<cs.SelectedCwd()>/.nightme/gtw.yml` (no walk-up
//     by design — see §14.4 design rationale).
//  2. Refuse if missing — there's no active fix in this chat.
//  2.5. Remove the nightme/wip label from the originating issue
//     (ModeRemote only). Runs BEFORE any local cleanup so a
//     platform-API failure (auth expired, network down, repo
//     moved) leaves the worktree + branch + ASes fully intact
//     — the yml is still the recovery source of truth and the
//     user can retry /gtw close once the upstream call works.
//     ModeLocal has c.Issue == -1 and is skipped — there is no
//     remote label to clean up.
//
//     Symmetric compensation: if a SUBSEQUENT local step (3
//     dirty / 4 worktree remove / 5 branch -D) fails after this
//     one succeeded, the platform "in flight" marker is re-
//     added so a retry converges on the same starting state.
//     Without this compensation the platform would show "out of
//     WIP" while local still has the worktree / branch / yml,
//     and a retry would face the divergence instead of the same
//     pre-close state.
//  3. Refuse if the worktree has uncommitted changes (`git
//     status --porcelain` non-empty). Close is intentionally
//     all-or-nothing: commit, stash, or discard before
//     re-running. No --force escape hatch (deliberate — see
//     cmd.go runClose doc).
//  4. Run `git worktree remove <path>` from inside the main
//     repo (git refuses to run that command from inside a
//     worktree itself). The yml file goes away with the
//     worktree — no explicit unlink needed.
//  5. `git branch -D <branch>` from repoRoot (always force-
//     delete — close is a "tear down the experiment" action
//     and the local branch was created by /gtw fix, never
//     published; the -d (lowercase) safe-delete would refuse
//     on unmerged, which is hostile here).
//  6. SetSelectedCwd back to repoRoot so the next agent message
//     spawns in the main repo. v1.5 removed the in-memory slot
//     clear that lived in this slot in older versions; nothing
//     to do beyond SetSelectedCwd (the yml disappears with the
//     worktree at step 4).
//  7. Emit close's own success card.
//  8. Invoke /new on the new cwd (c.RepoRoot) to drop any AS
//     left there of its accumulated conversation context. This
//     mirrors the user's manual workflow ("close the fix, then
//     clear the chat") — context reset BEFORE upstream refresh
//     so the next turn spawned by sync's pull summary (if any)
//     starts cold. matched==0 means no AS survives in repoRoot
//     — the common case after step 2.6 — and no extra card is
//     sent.
//  9. Run `git pull --rebase origin <default>` on repoRoot and
//     emit the same sync card /gtw sync uses. Sync runs LAST
//     so the user sees the full sequence: close → /new → sync.
//     If sync errors (dirty main, rebase conflict), its own
//     error card surfaces the cause — close's card above is not
//     retracted because the local fix session genuinely ended.
//
// On any error before step 6 (yml missing / dirty / worktree-
// remove / branch-delete fail) we leave the chat's active cwd
// untouched, so the user can retry once they fix the underlying
// problem. Steps 8 (/new) and 9 (sync) run unconditionally after
// step 7 — neither error undoes the local-fix tear-down, and
// neither skips the other. Step 8's matched==0 path silently
// skips its card so the empty-pool case stays a two-card story
// (close + sync).
func RunClose(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (*Result, error) {
	selectedCwd := cs.SelectedCwd()

	// --- step 0.5: dangling-cwd safety net -------------------------
	// Three failure modes the step 1+2 IsNotExist branch would
	// otherwise misreport as "no active fix to close":
	//
	//   (a) Another chat's /gtw close ran `git worktree remove`
	//       against THIS chat's fix worktree (or some external
	//       `rm -rf` ate the directory). selectedCwd now points
	//       at a path that does not exist; the yml is gone with
	//       the worktree.
	//
	//   (b) The chat just /cwd'd into a directory that someone
	//       else later removed (e.g. another chat's /gtw close
	//       on a sibling /gtw fix). selectedCwd is dangling;
	//       the user never ran /gtw fix here.
	//
	//   (c) selectedCwd is empty — short-circuited separately
	//       at lines 129-132 (just below) with
	//       command.NoActiveCwdReply; never reaches the
	//       dangling-dir cleanup below.
	//
	// Cases (a) and (b) are resolved by closing+dropping the
	// ASes in the cwd, clearing the dangling state, and telling
	// the user to /cwd again. The successful close path
	// (step 2.6 below) does the same AS cleanup so the
	// principle generalises: a /gtw close that tears down a
	// worktree must kill the agent processes pinned to it,
	// otherwise they accumulate as orphans and drag the daemon
	// down.
	if selectedCwd == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply), nil
	}
	// Stat failure handling: split the permanent case from the
	// transient case. IsNotExist means another /gtw close (or an
	// external `git worktree remove`) tore down this chat's
	// fix worktree — the path is gone, recovery is to clear
	// the dangling state and ask the user to /cwd again.
	// Anything else (EACCES, EIO, ESTALE, ENOTDIR, ENOTCONN) is
	// a transient or permission failure on a path that may
	// still be there in a moment — preserve all state, surface
	// the stat error as an IM reply, and let the user retry.
	//
	// F-PATHUTIL-001 §5.2: Normalize selectedCwd before stat so
	// a SelectedCwd stored with forward slashes (the common
	// form coming from `git rev-parse --show-toplevel` on
	// Windows, and what auto-restored ChatSessions may carry)
	// reaches os.Stat in canonical form. Without this, a path
	// like "F:/foo" might stat-fail on a case-sensitive Win32
	// API even though the directory exists.
	normalizedCwd := selectedCwd
	if n, err := pathutil.NormalizeForOS(selectedCwd); err == nil {
		normalizedCwd = n
	}
	if _, statErr := statPath(normalizedCwd); statErr != nil {
		if !os.IsNotExist(statErr) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ cannot reach workspace: %s\n(stat: %v)\n"+
					"hint: this may be transient (e.g. NFS hiccup, briefly-unmounted volume). "+
					"retry /gtw close once the path is reachable again — the in-flight fix and "+
					"agent sessions are left intact.", selectedCwd, statErr)), nil
		}
		// Always tear down ASes pinned to the unreachable path —
		// they are orphaned regardless of why the cwd vanished.
		droppedN, _ := cs.EvictAgentSessionsInCwd(selectedCwd)
		cs.ClearSelectedCwd()
		agentsLine := ""
		if n := droppedN; n > 0 {
			agentsLine = fmt.Sprintf("dropped %d orphaned agent session(s); ", n)
		}
		body := fmt.Sprintf(
			"⚠️ directory is unreachable: %s\n"+
				"it was deleted out from under this chat (e.g. by another "+
				"/gtw close running `git worktree remove`, or an external `rm -rf`).\n"+
				"%scleared the dangling cwd.\n"+
				"hint: run /cwd <path> to point this chat at a directory again.",
			selectedCwd, agentsLine)
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// --- step 1+2: locate the snapshot ---------------------------
	c, err := ReadGTWYml(selectedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				"❌ no active fix to close in this chat\n"+
					"hint: /cwd into the /gtw fix worktree first (its "+
					"`.nightme/gtw.yml` is the close source of truth)."), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err)), nil
	}

	if c.Worktree == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: worktree is empty"), nil
	}
	if c.RepoRoot == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: repoRoot is empty"), nil
	}

	// --- step 2.5: remove nightme/wip label (issue-backed fixes) ---
	// GATES every local cleanup step that follows. /gtw fix
	// stamped nightme/wip onto the originating issue when the
	// fix started (fix.go:410); /gtw close is the symmetric
	// teardown that lifts that label so the issue drops out of
	// the "in flight" board on the platform side. Local cleanup
	// (worktree remove / branch delete / AS drop) is destructive
	// and one-way; if it ran first and the label-removal API
	// call then failed, the issue would stay stuck at nightme/wip
	// forever with no local evidence pointing back to it. By
	// running label removal first and aborting on failure, the
	// yml remains the recovery source of truth and the user can
	// retry once the platform call works.
	//
	// The helper short-circuits when there's no issue to clean
	// up: ModeLocal always writes Issue == -1 (types.go:164), and
	// legacy /gtw fix snapshots that predate the Mode field still
	// have a positive Issue number (per the empty-Mode-treats-as-
	// ModeRemote convention in types.go:131-132) — so `c.Issue > 0`
	// alone is the right gate. c.RepoRoot != "" is the defensive
	// guard for the RepoRoot-missing malformed-yml case (caught
	// by the explicit check above, but cheap to repeat).
	labelRemoved, labelProvider, labelOwner, labelRepo, labelAborted := removeWIPLabel(ctx, c, deps, cs, chatID, messageID)
	if labelAborted {
		// Helper already sent a user-facing error reply. Bail
		// before any local cleanup so the yml-driven state
		// (worktree + branch + ASes) stays fully intact.
		return &Result{Consumed: true}, nil
	}

	// --- step 2.6: close ASes pinned to the about-to-be-gone worktree ---
	// Every AgentSession whose Cwd == c.Worktree is orphaned the
	// moment `git worktree remove` (step 4) succeeds — its cwd
	// is gone. Without this step, the orphan agent processes
	// accumulate across fixes and drag the daemon down.
	//
	// Runs BEFORE the dirty check (step 3) so a live agent
	// cannot dirty the worktree in the small window between
	// the dirty check and `git worktree remove`; with the
	// agent still alive, that window was a TOCTOU where
	// step 4's worktree remove could fail with "contains
	// modified or untracked files" and the orphan bridge
	// survived every retry.
	//
	// Mirrors the cleanup the step 0.5 safety net performs.
	// The result is fed into the step 8 success card so the
	// user sees an `→ agents: N dropped ...` line when
	// applicable; the no-agents happy path keeps its existing
	// 4-line card body.
	droppedN, _ := cs.EvictAgentSessionsInCwd(c.Worktree)

	// --- step 3: dirty check --------------------------------------
	// Close is intentionally all-or-nothing — the user must
	// commit / stash / discard before re-running. Neither
	// /gtw close nor /gtw fix exposes a --force; the yml
	// snapshot is the recovery source of truth for close,
	// and stale worktree paths are cleaned manually via
	// `git worktree remove --force <path>`.
	if err := assertWorktreeClean(ctx, c.Worktree, deps); err != nil {
		body := err.Error()
		if labelRemoved {
			// Local state untouched — restore the platform
			// "in flight" marker so a retry converges on the
			// same starting state.
			body += "\n" + compensateWIPLabel(ctx, labelProvider, c, labelOwner, labelRepo)
		}
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// --- step 4: git worktree remove ------------------------------
	if err := WorktreeRemove(ctx, c.RepoRoot, c.Worktree, false, deps.Git); err != nil {
		// Don't roll back — the yml still reflects the (still-
		// attached) fix. User can retry once they understand the
		// git error.
		stderr := ""
		if we, ok := errors.AsType[*WorktreeError](err); ok {
			stderr = tailLines(we.Stderr, 10)
		}
		body := fmt.Sprintf("❌ git worktree remove failed: %v", err)
		if stderr != "" {
			body += "\n[git stderr tail]\n" + stderr
		}
		if labelRemoved {
			// Local state untouched — restore the platform
			// "in flight" marker so a retry converges.
			body += "\n" + compensateWIPLabel(ctx, labelProvider, c, labelOwner, labelRepo)
		}
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// --- step 5: delete the local branch --------------------------
	// `-D` (uppercase) is force-delete — refuses to keep the
	// branch even if it's not fully merged. Always force: the
	// branch was created by /gtw fix and the user is asking for
	// a full close, so a "branch not merged" refusal would be
	// hostile. Recovery remains possible via reflog if needed.
	// Mirrors the rollback path in fix.go:440.
	if _, brStderr, brErr := deps.Git.Run(ctx, c.RepoRoot, "branch", "-D", c.Branch); brErr != nil {
		body := fmt.Sprintf(
			"❌ git branch -D %s failed: %v\n"+
				"[git stderr tail]\n%s\n"+
				"hint: worktree at %s is already removed; clean up the branch manually with `git branch -D %s`.",
			c.Branch, brErr, tailLines(brStderr, 10), c.Worktree, c.Branch,
		)
		if labelRemoved {
			// Worktree is gone but the branch ref still
			// exists, so the fix is half-torn-down. Restore
			// the platform "in flight" marker so a retry
			// sees a consistent state (and so the issue
			// doesn't drift while the user investigates).
			body += "\n" + compensateWIPLabel(ctx, labelProvider, c, labelOwner, labelRepo)
		}
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// --- step 6: switch CWD back to repoRoot ----------------------
	if err := cs.SetSelectedCwd(c.RepoRoot); err != nil {
		// The worktree IS removed at this point. Failing to
		// switch CWD is awkward (user is stranded in a path
		// that no longer exists) but we surface the error so
		// they can run `/cwd <repoRoot>` manually.
		slog.Default().Warn("gtw: SetSelectedCwd back to repoRoot failed",
			"repo_root", c.RepoRoot,
			"err", err)
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("⚠️ worktree removed but SetSelectedCwd(%s) failed: %v\n"+
				"run `/cwd %s` manually.", c.RepoRoot, err, c.RepoRoot)), nil
	}

	// --- step 6.5: clear PR cache for the chat's AS pool ---------
	// The dead branch's ASes must drop their cached PR (the
	// branch is being deleted; a refresh would be wasted work)
	// and any repoRoot ASes that still hold a stale "PR for
	// the old branch" entry get cleared too — the next stamp's
	// lazy MaybeRefresh will fetch fresh from scratch.
	if deps.PRCache != nil {
		for _, as := range cs.Pool() {
			if as == nil {
				continue
			}
			deps.PRCache.WritePR(as.ID, nil)
		}
	}

	// --- step 8: close's own success card -------------------------
	// IM-friendly layout that mirrors the fix success card:
	// `✅` headline naming the branch, then one `→` row per
	// side effect (worktree torn down, branch deleted, yml gone
	// with the worktree, cwd switched back). The sync card that
	// follows in step 9 has its own format — close does not
	// preview or summarise it here, to keep card ownership clean.
	body := fmt.Sprintf(
		"✅ closed `%s`\n"+
			"→ worktree: %s (removed)\n"+
			"→ branch: %s (deleted)\n"+
			"→ .nightme/gtw.yml (removed with worktree)\n"+
			"→ cwd → %s",
		c.Branch, c.Worktree, c.Branch, c.RepoRoot,
	)
	if labelRemoved {
		// Surface the platform-side teardown so the user sees
		// both halves of the close: local (worktree/branch) +
		// remote (label lifted off the issue). labelOwner /
		// labelRepo come from the helper's re-detected values,
		// not c.Repo — the yml's Repo field can be empty on
		// legacy snapshots, but label-removal success proves we
		// successfully resolved owner/repo from the live remote.
		body += fmt.Sprintf("\n→ issue: %s/%s#%d %s removed",
			labelOwner, labelRepo, c.Issue, LabelWIP)
	}
	if n := droppedN; n > 0 {
		// Surface the agent-process cleanup only when it
		// actually fired. The no-agents happy path keeps
		// its existing 4-line card body.
		body += fmt.Sprintf("\n→ agents: %d dropped (orphaned by worktree removal)", n)
	}
	// Send close's own success card. The reply() return is
	// intentionally discarded — returning here would skip step 9's
	// separate sync card. Every other reply path in this file
	// uses `return reply(...), nil` because they're terminal;
	// step 8 is mid-flow by design.
	reply(ctx, cs.Emitter(), chatID, messageID, body)

	// --- step 9: /new (clear agent context in repoRoot) ---------
	// Mirrors the user's manual workflow: after /gtw close wipes
	// the worktree they fire /new to drop the agent's accumulated
	// conversation context for the new cwd. We do it here so they
	// don't have to.
	//
	// Symmetric to step 2.6's AS cleanup at the dead worktree:
	// the worktree-pinned ASes are already dropped. Any AS left
	// in c.RepoRoot (the new cwd after step 6) survives with
	// stale context; /new forces them cold so the next turn in
	// the main repo doesn't inherit the fix's conversation.
	//
	// Runs BEFORE sync (step 10) so any agent spawned by sync's
	// pull summary (if the bridge echoes one) starts cold —
	// reset first, refresh second.
	//
	// matched==0 means the pool is empty in c.RepoRoot — the
	// common path after step 2.6 — and we skip the extra card so
	// close + sync stays a two-card story for that case. The
	// queue is deliberately NOT dropped (same contract as
	// /new: queued messages are still owed a reply and flush
	// into the fresh context on the next TryFlush).
	matched, _, results, newErr := cs.NewActiveAgentSessions(ctx, "")
	if matched > 0 {
		body := newcmd.FormatResetResults(results)
		if newErr != nil {
			body += fmt.Sprintf("\n(errors: %v)", newErr)
		}
		reply(ctx, cs.Emitter(), chatID, messageID, body)
	}

	// --- step 10: sync main (separate card) ----------------------
	// buildSyncReply shares the /gtw sync card formatter and
	// respects deps.SkipRefreshDefaultBranch (test seam). If
	// sync fails the error surfaces its own card with a ❌ prefix
	// (RefreshDefaultBranch's own errors sometimes lack one);
	// the close + /new cards above are NOT retracted because the
	// local fix session is genuinely over regardless.
	//
	// Single terminal step: all three sync branches (error /
	// normal / skip) fall through to the same return.
	syncBody, syncErr := buildSyncReply(ctx, c.RepoRoot, deps)
	if syncErr != nil {
		reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ sync failed: "+syncErr.Error())
	} else if syncBody != "" {
		reply(ctx, cs.Emitter(), chatID, messageID, syncBody)
	}
	// else: SkipRefreshDefaultBranch set (test-only); no sync
	// card. close's card (and /new card when applicable) still
	// stand.

	return &Result{Consumed: true}, nil
}

// removeWIPLabel drives the platform-side teardown that pairs
// with the local cleanup in RunClose. It resolves the provider
// from the yml's c.Repo / c.Provider shortcut (when populated
// by F-XX+) or falls back to RemoteOriginURL + Detect + ParseRepoOwner,
// then asks the provider to drop the nightme/wip label from
// issue c.Issue.
//
// Returns:
//
//   - (true, provider, owner, repo, false): LabelWIP was removed.
//     provider / owner / repo are threaded back to RunClose so a
//     later step can compensate (re-add the label) if a local
//     cleanup step fails — without paying for a second Detect
//     round-trip. owner/repo come from resolveProvider (yml-
//     cached c.Repo split on '/', or live remote URL).
//   - (false, nil, "", "", false): No work was needed (ModeLocal
//     always writes Issue == -1; c.RepoRoot == "" catches
//     malformed snapshots). RunClose continues to local cleanup.
//   - (false, nil, "", "", true): A user-facing error reply was
//     sent; RunClose must abort BEFORE any local cleanup so the
//     yml remains the recovery source of truth.
//
// The helper sends its own error replies (with ❌ prefixes that
// match RunClose's existing style) so RunClose's main flow stays
// linear — it only needs to inspect the `aborted` flag, not format
// error text.
func removeWIPLabel(
	ctx context.Context,
	c Context,
	deps HandlerDeps,
	cs *chatsession.ChatSession,
	chatID, messageID string,
) (removed bool, provider GitProvider, owner, repo string, aborted bool) {
	// ModeLocal always writes Issue == -1 (types.go:164), and
	// legacy yml snapshots that predate the Mode field still
	// have a positive Issue number per the empty-Mode-treats-
	// as-ModeRemote convention in types.go:131-132 — so
	// `c.Issue > 0` alone is the right gate. c.RepoRoot != ""
	// catches malformed snapshots (already filtered above but
	// repeated here as a defence-in-depth).
	if c.Issue <= 0 || c.RepoRoot == "" {
		return false, nil, "", "", false
	}

	// Reuse the same detection pipeline as /gtw pr. resolveProvider
	// honours the yml-cached c.Repo / c.Provider shortcut when
	// present (no git / network round-trip) and falls back to
	// RemoteOriginURL + Detect + ParseRepoOwner otherwise — both
	// /gtw pr and /gtw close now share that policy.
	prov, owner, repo, resolveErr := resolveProvider(ctx, c, deps)
	if resolveErr != nil || prov == nil {
		reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ cannot reach GitHub/GitLab to remove the nightme/wip label: "+
				resolveErr.Error())
		return false, nil, "", "", true
	}

	if rmErr := prov.RemoveIssueLabel(ctx, owner, repo, c.Issue, LabelWIP); rmErr != nil {
		reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ could not remove label %q from issue #%d: %v\n"+
				"worktree and branch are left intact — retry /gtw close once "+
				"the platform call succeeds (auth / network / repo moved).",
				LabelWIP, c.Issue, rmErr))
		return false, nil, "", "", true
	}

	return true, prov, owner, repo, false
}

// compensateWIPLabel is the symmetric rollback to removeWIPLabel.
// Called from RunClose when a local cleanup step (dirty check,
// worktree remove, branch -D) fails AFTER the label was already
// removed — the platform would otherwise show "out of WIP" while
// local state still has the worktree / branch / yml, breaking
// the next retry. Re-adding the label restores the platform
// "in flight" state so a retry converges on the same starting
// point.
//
// Best-effort: returns a status line the caller appends to the
// error reply. A re-add failure is communicated but does NOT
// escalate to a second top-level error — the user already has
// the original local error to act on.
//
// NOT called for steps 6 (SetSelectedCwd), 9 (/new), or 10
// (sync): by those points the local close is functionally
// complete and the label correctly reflects "out of WIP".
func compensateWIPLabel(
	ctx context.Context,
	provider GitProvider,
	c Context,
	owner, repo string,
) string {
	if err := provider.AddIssueLabel(ctx, owner, repo, c.Issue, LabelWIP); err != nil {
		return fmt.Sprintf(
			"(could not restore %s to %s/%s#%d: %v — issue may now be in an inconsistent state; "+
				"manually re-add the label if a retry is needed)",
			LabelWIP, owner, repo, c.Issue, err)
	}
	return fmt.Sprintf(
		"(%s restored to %s/%s#%d — fix the local state above and retry /gtw close)",
		LabelWIP, owner, repo, c.Issue)
}

// assertWorktreeClean returns a user-friendly error if `dir` has
// any porcelain-visible changes (modified / added / deleted /
// renamed / copied / unmerged conflicts / untracked). Returns
// nil if the worktree is clean.
//
// Implemented as `git -C <dir> status --porcelain` — same
// porcelain format GitStatusSnapshot uses elsewhere in gtw, so
// the semantics are consistent across the package.
func assertWorktreeClean(ctx context.Context, dir string, deps HandlerDeps) error {
	stdout, _, err := deps.Git.Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("❌ git status in %s failed: %w", dir, err)
	}
	if stdout == "" {
		return nil
	}

	// Split + filter in one pass. strings.SplitSeq tolerates a
	// trailing \n; we drop empties because git occasionally
	// pads with blank lines (e.g. after a section header in
	// some configs — defensive, even though --porcelain
	// shouldn't produce any).
	lines := make([]string, 0, 4)
	for line := range strings.SplitSeq(stdout, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}

	preview := lines
	if len(preview) > maxDirtyFilesReported {
		preview = preview[:maxDirtyFilesReported]
	}
	var body strings.Builder
	body.WriteString("❌ worktree has uncommitted changes — commit or stash before closing:\n")
	fmt.Fprintf(&body, "  %s\n", pathutil.Join(dir, "(worktree)"))
	for _, line := range preview {
		fmt.Fprintf(&body, "  %s\n", line)
	}
	if len(lines) > maxDirtyFilesReported {
		fmt.Fprintf(&body, "\n  ... and %d more\n", len(lines)-maxDirtyFilesReported)
	}
	body.WriteString("hint: this command does not support `--force`; clean the worktree first.")
	return errors.New(body.String())
}
