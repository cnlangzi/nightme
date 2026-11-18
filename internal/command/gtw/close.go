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
	"github.com/cnlangzi/nightme/internal/messages"
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
// dispatcher pipeline. The trailing reset flag is the `--reset`
// switch: clear the issue's state labels without marking the work
// done.
//
// The close tears the fix down on both sides: locally (worktree,
// branch, cwd) and on the tracker (the issue's `nightme/*` labels).
// The default mode finalises the issue — every `nightme/*` label
// but the queue marker comes off and `nightme/done` goes on; the
// `reset` mode only clears the state labels and marks nothing, for
// dropping the work without calling it done. A local fix has no
// issue, so the label step is skipped entirely.
//
// Flow (wip/gtw.md §14.5):
//
//  1. Look for `<cs.SelectedCwd()>/.nightme/gtw.yml` (no walk-up
//     by design — see §14.4 design rationale).
//
//  2. Refuse if missing — there's no active fix in this chat.
//
//     2.5. Finalise the issue's labels (ModeRemote only). Runs
//     BEFORE any local cleanup so a platform-API failure (auth
//     expired, network down, repo moved) leaves the worktree +
//     branch + ASes fully intact — the yml is still the recovery
//     source of truth and the user can retry /gtw close once the
//     upstream call works.
//
//     Symmetric compensation: if a SUBSEQUENT local step (3
//     dirty / 4 worktree remove / 5 branch -D) fails after the
//     labels changed, the issue is put back the way it was, so a
//     retry converges on the same starting state. Without this
//     compensation the platform would say "done" while the local
//     side still has the worktree / branch / yml, and the retry
//     would face that divergence.
//
//  3. Refuse if the worktree has uncommitted changes (`git
//     status --porcelain` non-empty). Close is intentionally
//     all-or-nothing: commit, stash, or discard before
//     re-running. No --force escape hatch (deliberate — see
//     cmd.go runClose doc).
//
//  4. Run `git worktree remove <path>` from inside the main
//     repo (git refuses to run that command from inside a
//     worktree itself). The yml file goes away with the
//     worktree — no explicit unlink needed.
//
//  5. `git branch -D <branch>` from repoRoot (always force-
//     delete — close is a "tear down the experiment" action
//     and the local branch was created by /gtw fix, never
//     published; the -d (lowercase) safe-delete would refuse
//     on unmerged, which is hostile here).
//
//  6. SetSelectedCwd back to repoRoot so the next agent message
//     spawns in the main repo. v1.5 removed the in-memory slot
//     clear that lived in this slot in older versions (the
//     retired step 7); nothing to do beyond SetSelectedCwd (the
//     yml disappears with the worktree at step 4).
//
//  8. Emit close's own success card.
//
//  9. Run `git pull --rebase origin <default>` on repoRoot and
//     emit the same sync card /gtw sync uses. Sync runs LAST
//     so the user sees the full sequence: close → sync. If sync
//     errors (dirty main, rebase conflict), its own error card
//     surfaces the cause — close's card above is not retracted
//     because the local fix session genuinely ended.
//
// On any error before step 6 (yml missing / dirty / worktree-
// remove / branch-delete fail) we leave the chat's active cwd
// untouched, so the user can retry once they fix the underlying
// problem. Step 9 (sync) runs unconditionally after step 8 — a
// sync error does not undo the local-fix tear-down.
//
// The chat's agent conversation in repoRoot is left alone: the
// fix conversation lived in the worktree's AgentSession, which
// step 2.6 drops together with the worktree. The main repo's own
// AgentSession keeps its context so the next user message
// continues the same thread.
func RunClose(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	reset bool,
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
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply,
			command.FailedOutcome("no_cwd", "the command needs an active workspace")), nil
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
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ cannot reach workspace: %s\n(stat: %v)\n"+
					"hint: this may be transient (e.g. NFS hiccup, briefly-unmounted volume). "+
					"retry /gtw close once the path is reachable again — the in-flight fix and "+
					"agent sessions are left intact.", selectedCwd, statErr),
				command.FailedOutcome("error", "the workspace could not be reached")), nil
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
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID, body,
			messages.Outcome{
				Status:  messages.OutcomeIncomplete,
				Summary: "the workspace is gone; the dangling state was cleared",
				Data:    map[string]string{"reason": "workspace_unreachable"},
			}), nil
	}

	// --- step 1+2: locate the snapshot ---------------------------
	c, err := ReadGTWYml(selectedCwd)
	if err != nil {
		if os.IsNotExist(err) {
			return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
				"❌ no active fix to close in this chat\n"+
					"hint: /cwd into the /gtw fix worktree first (its "+
					"`.nightme/gtw.yml` is the close source of truth).",
				command.FailedOutcome("no_active_fix", "there is no active fix to close")), nil
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ failed to read .nightme/gtw.yml: %v", err),
			command.FailedOutcome("bad_state", "the gtw state file could not be read")), nil
	}

	if c.Worktree == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: worktree is empty",
			command.FailedOutcome("bad_state", "the gtw state file is malformed")), nil
	}
	if c.RepoRoot == "" {
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ .nightme/gtw.yml is malformed: repoRoot is empty",
			command.FailedOutcome("bad_state", "the gtw state file is malformed")), nil
	}

	// --- step 2.5: finalize the issue's labels (issue-backed fixes) ---
	// GATES every local cleanup step that follows. The local cleanup
	// (worktree remove / branch delete / AS drop) is destructive and
	// one-way; if it ran first and the label call then failed, the
	// issue would keep a stale state label forever with no local
	// evidence pointing back at it. By running the label step first
	// and aborting on failure, the yml remains the recovery source
	// of truth and the user can retry once the platform call works.
	labels, aborted, labelWarning := finalizeIssueLabels(ctx, c, deps, cs, chatID, messageID, reset)
	if aborted {
		// The helper already sent a user-facing error reply. Bail
		// before any local cleanup so the yml-driven state
		// (worktree + branch + ASes) stays fully intact.
		reportOutcome(ctx, command.FailedOutcome("label_error",
			"the issue labels could not be finalized"))
		return &Result{Consumed: true}, nil
	}
	// A failed label-catalog check is a warning, not a failure: the
	// close still runs to its normal result. The warning rides
	// along on every reply the close itself produces — its success
	// card and its failure cards — so the user learns about it
	// wherever the close ends. The sync card that follows is a
	// separate step and does not repeat it.
	withWarning := func(body string) string {
		return prependWarning(body, labelWarning)
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
		if labels.changed() {
			// Local state untouched — put the issue's labels back
			// the way they were so a retry converges on the same
			// starting state.
			body += "\n" + restoreIssueLabels(ctx, labels)
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID, withWarning(body),
			command.FailedOutcome("dirty", "the worktree has uncommitted changes")), nil
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
		if labels.changed() {
			// Local state untouched — put the issue's labels back
			// so a retry converges on the same starting state.
			body += "\n" + restoreIssueLabels(ctx, labels)
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID, withWarning(body),
			command.FailedOutcome("error", "the worktree could not be removed")), nil
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
		if labels.changed() {
			// Worktree is gone but the branch ref still exists, so
			// the fix is half-torn-down. Put the issue's labels back
			// so a retry sees a consistent state.
			body += "\n" + restoreIssueLabels(ctx, labels)
		}
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID, withWarning(body),
			command.FailedOutcome("error", "the branch could not be deleted")), nil
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
		return replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			withWarning(fmt.Sprintf("⚠️ worktree removed but SetSelectedCwd(%s) failed: %v\n"+
				"run `/cwd %s` manually.", c.RepoRoot, err, c.RepoRoot)),
			messages.Outcome{
				Status:  messages.OutcomeIncomplete,
				Summary: "the worktree was removed but the chat stayed in it",
				Data: map[string]string{
					"reason":    "cwd_not_restored",
					"worktree":  c.Worktree,
					"repo_root": c.RepoRoot,
				},
			}), nil
	}

	// --- step 6.5: clear PR cache for the workspace ----------------
	// The dead branch's PR is gone — write nil so the next
	// stamp on this cwd reflects that immediately. The branch
	// is being deleted, so a MaybeRefresh round-trip would be
	// wasted; clearing directly skips the network.
	if deps.PRCache != nil {
		// prcache is keyed by cwd (per-workspace). Clearing
		// the workspace's PR entry applies to every AS that
		// has ever stamped on this path.
		deps.PRCache.WritePR(c.Worktree, nil)
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
	// The platform half of the close: what came off the issue and
	// what went on. The row is absent when the close touched no
	// issue (a local fix, or an issue the tracker no longer knows).
	if line := labels.cardLine(); line != "" {
		body += "\n" + line
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
	reply(ctx, cs.Emitter(), chatID, messageID, withWarning(body))

	// --- step 9: sync main (separate card) ----------------------
	// buildSyncReply shares the /gtw sync card formatter and
	// respects deps.SkipRefreshDefaultBranch (test seam). If
	// sync fails the error surfaces its own card with a ❌ prefix
	// (RefreshDefaultBranch's own errors sometimes lack one);
	// the close card above is NOT retracted because the local fix
	// session is genuinely over regardless.
	//
	// Single terminal step: all three sync branches (error /
	// normal / skip) fall through to the same return.
	syncBody, _, syncErr := buildSyncReply(ctx, c.RepoRoot, deps)
	if syncErr != nil {
		reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ sync failed: "+syncErr.Error())
	} else if syncBody != "" {
		reply(ctx, cs.Emitter(), chatID, messageID, syncBody)
	}
	// else: SkipRefreshDefaultBranch set (test-only); no sync
	// card. close's card still stands.

	// The teardown itself succeeded; the sync that follows it is a
	// separate step. A failed sync leaves the work closed but the
	// default branch unrefreshed — half of the command's work
	// happened.
	if syncErr != nil {
		reportOutcome(ctx, messages.Outcome{
			Status:  messages.OutcomeIncomplete,
			Summary: "the worktree was closed but the main branch could not be synced",
			Data: map[string]string{
				"reason":    "sync_failed",
				"branch":    c.Branch,
				"worktree":  c.Worktree,
				"repo_root": c.RepoRoot,
			},
		})
		return &Result{Consumed: true}, nil
	}
	reportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: fmt.Sprintf("closed the fix on branch %s", c.Branch),
		Data: map[string]string{
			"branch":    c.Branch,
			"worktree":  c.Worktree,
			"repo_root": c.RepoRoot,
		},
	})
	return &Result{Consumed: true}, nil
}

// labelFinalize records what the label step did to the issue, so the
// success card can report it and a failed local step can put the
// issue back the way it was.
type labelFinalize struct {
	// removed lists the labels this close took off the issue, by the
	// name the platform reported.
	removed []string

	// added is the label this close put on the issue — LabelDone in
	// the default mode. Empty in the --reset mode, which marks
	// nothing.
	added string

	// provider / owner / repo / issue identify the issue the labels
	// live on. provider is nil when the label step did not run.
	provider GitProvider
	owner    string
	repo     string
	issue    int
}

// changed reports whether the label step changed the issue at all.
// The compensation runs only then: there is nothing to put back when
// no label moved.
func (l labelFinalize) changed() bool {
	return len(l.removed) > 0 || l.added != ""
}

// cardLine renders the issue row of the close card: one `-<label>`
// entry per label that came off the issue, one `+<label>` entry per
// label that went on. Empty when the close touched no issue.
func (l labelFinalize) cardLine() string {
	if l.provider == nil {
		return ""
	}
	parts := make([]string, 0, len(l.removed)+1)
	for _, label := range l.removed {
		parts = append(parts, "-"+label)
	}
	if l.added != "" {
		parts = append(parts, "+"+l.added)
	}
	if len(parts) == 0 {
		parts = append(parts, "no nightme/* labels")
	}
	return fmt.Sprintf("→ issue: %s/%s#%d %s",
		l.owner, l.repo, l.issue, strings.Join(parts, " "))
}

// finalizeIssueLabels drives the platform half of the close: it
// brings the issue's `nightme/*` labels to the state the close means.
//
// The default mode marks the work done — every `nightme/*` label but
// the queue marker comes off and `nightme/done` goes on. The reset
// mode only clears: the state labels come off, nothing goes on.
//
// The removal set comes from the issue's own labels, not from
// AllLabels: a hand-made `nightme/custom` is a nightme label too,
// and the platform reports the name it knows. The queue marker
// `nightme/assigned` is never touched — it belongs to the queue, not
// to the close.
//
// The step GATES the local cleanup: the worktree remove and the
// branch delete are one-way, so a label failure must abort the close
// before them. The yml then stays the recovery source of truth and
// the user retries once the platform call works.
//
// Returns:
//
//   - (labels, false, warning): the labels the step changed (empty
//     when the step did not run) and the warning to ride along on
//     the close's replies.
//   - (labels, true, warning): a user-facing error reply was sent;
//     the close must abort BEFORE any local cleanup.
//
// The helper sends its own error replies (with the ❌ prefix the
// rest of the close uses) so RunClose's main flow stays linear — it
// only inspects the `aborted` flag.
func finalizeIssueLabels(
	ctx context.Context,
	c Context,
	deps HandlerDeps,
	cs *chatsession.ChatSession,
	chatID, messageID string,
	reset bool,
) (labelFinalize, bool, string) {
	// ModeLocal always writes Issue == -1 (types.go), and legacy yml
	// snapshots that predate the Mode field still have a positive
	// issue number per the empty-Mode-treats-as-ModeRemote
	// convention — so `c.Issue > 0` alone is the right gate.
	// c.RepoRoot == "" catches malformed snapshots (already filtered
	// by RunClose, but cheap to repeat here).
	if c.Issue <= 0 || c.RepoRoot == "" {
		return labelFinalize{}, false, ""
	}

	// The same resolution policy as /gtw pr: the yml-cached
	// c.Repo / c.Provider shortcut when the snapshot carries one,
	// the live remote URL otherwise.
	prov, owner, repo, err := resolveProvider(ctx, c, deps)
	if err != nil || prov == nil {
		reason := "provider detection returned no result"
		if err != nil {
			reason = err.Error()
		}
		reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ cannot reach GitHub/GitLab to finalize the issue labels: "+reason)
		return labelFinalize{}, true, ""
	}

	// The label catalog is verified before the labels change, so the
	// repository keeps the gtw labels the next fix needs — and
	// `nightme/done` has to exist before it can be attached. A failed
	// check only warns: the attach that follows reports its own
	// failure if the label really is missing.
	var warning string
	if _, err := ensureRepoLabels(ctx, deps, prov, owner, repo); err != nil {
		warning = labelCheckWarning(owner, repo, err)
	}

	issue, err := prov.GetIssue(ctx, owner, repo, c.Issue)
	if err != nil {
		if errors.Is(err, ErrIssueNotFound) {
			// A deleted issue can never be labelled, and a close
			// that refused to run would strand the worktree for
			// good. Warn and carry on with the local cleanup.
			return labelFinalize{}, false, prependWarning(warning, fmt.Sprintf(
				"⚠️ issue #%d not found in %s/%s — no label was changed",
				c.Issue, owner, repo))
		}
		reply(ctx, cs.Emitter(), chatID, messageID, prependWarning(fmt.Sprintf(
			"❌ could not read issue #%d: %v\n"+
				"worktree and branch are left intact — retry /gtw close once "+
				"the platform call succeeds (auth / network / repo moved).",
			c.Issue, err), warning))
		return labelFinalize{}, true, warning
	}

	// The removal set: every nightme label the issue carries but the
	// queue marker. The names are the platform's own, so a label the
	// catalog does not know still comes off.
	toRemove := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if !isNightmeLabel(label) {
			continue
		}
		if strings.EqualFold(label, LabelAssigned) {
			continue
		}
		if !reset && strings.EqualFold(label, LabelDone) {
			// The default mode puts `nightme/done` on the issue
			// right after this list is computed; taking it off
			// again would undo that. A stale `nightme/done` stays
			// — which is the state the close wants.
			continue
		}
		toRemove = append(toRemove, label)
	}

	// Add before remove: a failed add leaves the issue with the
	// state labels it had, which is a state the user can read. The
	// reverse order would strip the issue bare and then fail.
	added := ""
	if !reset {
		if err := prov.AddIssueLabel(ctx, owner, repo, c.Issue, LabelDone); err != nil {
			reply(ctx, cs.Emitter(), chatID, messageID, prependWarning(fmt.Sprintf(
				"❌ could not add label %q to issue #%d: %v\n"+
					"worktree and branch are left intact — retry /gtw close once "+
					"the platform call succeeds (auth / network / repo moved).",
				LabelDone, c.Issue, err), warning))
			return labelFinalize{}, true, warning
		}
		added = LabelDone
	}

	for _, label := range toRemove {
		if err := prov.RemoveIssueLabel(ctx, owner, repo, c.Issue, label); err != nil {
			// The labels already taken off stay off; a retry reads
			// the issue again and converges on the same end state.
			reply(ctx, cs.Emitter(), chatID, messageID, prependWarning(fmt.Sprintf(
				"❌ could not remove label %q from issue #%d: %v\n"+
					"worktree and branch are left intact — retry /gtw close once "+
					"the platform call succeeds (auth / network / repo moved).",
				label, c.Issue, err), warning))
			return labelFinalize{}, true, warning
		}
	}

	return labelFinalize{
		removed:  toRemove,
		added:    added,
		provider: prov,
		owner:    owner,
		repo:     repo,
		issue:    c.Issue,
	}, false, warning
}

// restoreIssueLabels is the symmetric rollback to finalizeIssueLabels.
// Called when a local cleanup step (dirty check, worktree remove,
// branch -D) fails AFTER the labels changed — the platform would
// otherwise say "done" while the local side still has the worktree /
// branch / yml, and the retry would face that divergence.
//
// Best-effort: the returned line names whatever could not be put
// back. A restore failure is communicated but does NOT escalate into
// a second top-level error — the user already has the original local
// error to act on.
//
// NOT called for the steps after the branch delete (cwd switch, sync):
// by then the local close is functionally complete and the labels
// correctly describe it.
func restoreIssueLabels(ctx context.Context, l labelFinalize) string {
	var failed []string
	for _, label := range l.removed {
		if err := l.provider.AddIssueLabel(ctx, l.owner, l.repo, l.issue, label); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", label, err))
		}
	}
	if l.added != "" {
		if err := l.provider.RemoveIssueLabel(ctx, l.owner, l.repo, l.issue, l.added); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", l.added, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf(
			"(could not restore %s on %s/%s#%d — the issue may be in an inconsistent state; "+
				"fix the labels by hand if a retry is needed)",
			strings.Join(failed, ", "), l.owner, l.repo, l.issue)
	}
	return fmt.Sprintf(
		"(issue labels restored on %s/%s#%d — fix the local state above and retry /gtw close)",
		l.owner, l.repo, l.issue)
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
	stdout, stderr, err := deps.Git.Run(ctx, dir,
		"--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return gitErr(fmt.Sprintf("❌ git status in %s failed", dir), err, stderr)
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
