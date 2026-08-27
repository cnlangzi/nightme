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
// a transient stat failure must NOT clear slot / selectedCwd /
// the AS pool, and that's easier to lock in via a stub than by
// chmod races (chmod 000 is silently bypassed for root on
// Linux, and the test would race with later reads anyway).
// Production uses os.Stat directly; tests restore via
// withStatStub in close_test.go.
var statPath = os.Stat

// maxDirtyFilesReported caps how many uncommitted paths we
// surface in the "worktree is dirty" error reply. 10 is the
// sweet spot the user tested with: enough to identify the
// problem (top of the porcelain output is the most-recent
// changes), short enough that a Feishu reply doesn't get
// truncated by the channel's own message-size limits.
const maxDirtyFilesReported = 10

// RunClose is the entry point for `/gtw close`. Mirrors RunFix
// in shape (ctx, cs, slot, deps, chatID, messageID; returns
// *Result, error) so the factory can wire it through the same
// dispatcher pipeline.
//
// Flow (wip/gtw.md §14.5):
//
//  1. Look for `<cs.SelectedCwd()>/.nightme/gtw.yml` (no walk-up
//     by design — see §14.4 design rationale).
//  2. Refuse if missing — there's no active fix in this chat.
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
//     spawns in the main repo.
//  7. Clear the in-memory Context.
//  8. Emit close's own success card.
//  9. Invoke /new on the new cwd (c.RepoRoot) to drop any AS
//     left there of its accumulated conversation context. This
//     mirrors the user's manual workflow ("close the fix, then
//     clear the chat") — context reset BEFORE upstream refresh
//     so the next turn spawned by sync's pull summary (if any)
//     starts cold. matched==0 means no AS survives in repoRoot
//     — the common case after step 2.5 — and no extra card is
//     sent.
//  10. Run `git pull --rebase origin <default>` on repoRoot and
//     emit the same sync card /gtw sync uses. Sync runs LAST
//     so the user sees the full sequence: close → /new → sync.
//     If sync errors (dirty main, rebase conflict), its own
//     error card surfaces the cause — close's card above is not
//     retracted because the local fix session genuinely ended.
//
// On any error before step 7 (yml missing / dirty / worktree-
// remove / branch-delete fail) we leave the chat's active cwd
// and the in-memory Context untouched, so the user can retry
// once they fix the underlying problem. Steps 9 (/new) and 10
// (sync) run unconditionally after step 8 — neither error
// undoes the local-fix tear-down, and neither skips the
// other. Step 9's matched==0 path silently skips its card so
// the empty-pool case stays a two-card story (close + sync).
func RunClose(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
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
	//       at a path that does not exist; the in-memory slot
	//       carries the matching Worktree; the yml is gone with
	//       the worktree.
	//
	//   (b) The chat just /cwd'd into a directory that someone
	//       else later removed (e.g. another chat's /gtw close
	//       on a sibling /gtw fix). selectedCwd is dangling;
	//       slot is empty; the user never ran /gtw fix here.
	//
	//   (c) selectedCwd is empty (cmd.go:364-389 does not
	//       preflight this). Without this guard, ReadGTWYml("")
	//       would resolve to "./.nightme/gtw.yml" relative to
	//       wherever the daemon happens to be running — not
	//       what the user expects. Same reply text as
	//       command.RequireActiveCwd (internal/command/preflight.go:30)
	//       for consistency.
	//
	// All three are resolved by closing+dropping the ASes in
	// the cwd (when there is one), clearing the dangling state,
	// and telling the user to /cwd again. The successful close
	// path (step 5.5 below) does the same AS cleanup so the
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
		cur := slot.Load()
		// F-PATHUTIL-001 §5.2: same-case/same-slash comparison
		// bug as the fix.go call sites — the slot was written
		// with backslash form (WorktreePath → pathutil) but
		// selectedCwd may carry forward slashes (auto-restored
		// or pre-migration yml). pathutil.Equal collapses both
		// axes so the "is this the chat's active worktree?"
		// decision stays correct on Windows.
		slotMatched := cur.Worktree != "" &&
			pathutil.Equal(cur.Worktree, selectedCwd)
		// Always tear down ASes pinned to the unreachable path —
		// they are orphaned regardless of slot state.
		droppedN, _ := cs.EvictAgentSessionsInCwd(selectedCwd)
		if slotMatched {
			slot.Store(Context{}) // Manager.ClearContext via cmd.go:655-661 shim
		}
		cs.ClearSelectedCwd()
		agentsLine := ""
		if n := droppedN; n > 0 {
			agentsLine = fmt.Sprintf("dropped %d orphaned agent session(s); ", n)
		}
		var body string
		if slotMatched {
			body = fmt.Sprintf(
				"⚠️ worktree directory is unreachable: %s\n"+
					"another /gtw close (or an external `git worktree remove`) "+
					"tore down this chat's fix worktree.\n"+
					"%scleared the in-flight fix and the dangling cwd.\n"+
					"hint: run /cwd <path> to point this chat at a directory again.",
				selectedCwd, agentsLine)
		} else {
			body = fmt.Sprintf(
				"⚠️ directory is unreachable: %s\n"+
					"it was deleted out from under this chat (e.g. by another "+
					"/gtw close running `git worktree remove`).\n"+
					"%scleared the dangling cwd.\n"+
					"hint: run /cwd <path> to point this chat at a directory again.",
				selectedCwd, agentsLine)
		}
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

	// --- step 2.5: close ASes pinned to the about-to-be-gone worktree ---
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
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
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

	// --- step 7: clear in-memory state ----------------------------
	slot.Store(Context{})

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
	// Symmetric to step 2.5's AS cleanup at the dead worktree:
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
	// common path after step 2.5 — and we skip the extra card so
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
