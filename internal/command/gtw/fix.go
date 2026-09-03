package gtw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/pathutil"
	"github.com/cnlangzi/nightme/internal/prcache"
)

// /gtw {pr, close} success paths apply a known PR result to
// every AgentSession in the chat's pool by calling
// `deps.PRCache.WritePR(as.ID, pr)` inline:
//
//   - /gtw pr:  pr = the new PR from `gh pr create`
//               (no refresh; the next stamp's lazy MaybeRefresh
//               corrects any branch mismatch within 60 s).
//   - /gtw close: pr = nil (branch is being deleted; no
//                 refresh; the next stamp's lazy MaybeRefresh
//                 fetches fresh from scratch).
//
// /gtw fix and /gtw push don't touch the cache — the
// per-stamp MaybeRefresh covers them. Inline pool walk lives
// directly at each callsite so the intent reads in context.

// All fields are required; pass an instance constructed in the
// runtime's startup code (cmd/nightme/run.go).
//
// Replies go through cs.Emitter().Send. Interactive choices are
// Kind=OutChoice; follow-up disable is Kind=OutChoicePatch keyed by
// Choice.RequestID.
type HandlerDeps struct {
	// Git wraps the local git binary. Tests inject a fake.
	Git GitRunner
	// Prober is the HTTPProber for Detect's Stage B API probe
	// (used only when the URL hint is ambiguous). nil → production
	// uses ExecHTTPProber{} with 3s default timeout. Tests inject
	// a fake that returns canned JSON for fixture-driven cases.
	Prober HTTPProber
	// Detect is the provider-detection function. nil → production
	// uses package-level Detect (URL hint + API probe). Tests
	// override to inject a fakeProvider without running real
	// Detect logic (see F-50 §1.4 for the injection pattern).
	Detect func(ctx context.Context, remoteURL string, prober HTTPProber, worktree string) (GitProvider, error)
	// Now is the clock. Tests override for deterministic drafts.
	Now func() time.Time
	// PRCache is the runtime-owned *prcache.Registry that
	// /gtw {pr, close} success paths write into directly
	// (we already know the answer; no refresh round-trip).
	// nil → /gtw dispatchers no-op (unit tests that don't
	// wire the runtime registry). The runtime injects the
	// same instance that powers the per-stamp MaybeRefresh
	// trigger in runtime.go, so writes here land on the live
	// cache.
	PRCache *prcache.Registry

	// SkipRefreshDefaultBranch, when true, causes RunFix to
	// skip the RefreshDefaultBranch step (no `git checkout
	// <default>` / `git pull --ff-only`). Production leaves
	// this false so the upstream default branch is always
	// synced before the worktree branches off it; tests set
	// it to true when their setup can't satisfy the pull (no
	// upstream, fake URL, sandbox without network).
	//
	// The unit tests in worktree_test.go and
	// refresh_default_branch_test.go exercise the real flow;
	// this flag exists only for end-to-end integration tests
	// that want to focus on the /gtw fix business logic without
	// needing a usable origin remote.
	SkipRefreshDefaultBranch bool
}

// Result is the gtw-package view of a command's outcome. Mirrors
// inbound.CommandResult without taking a dependency on inbound.
// The runtime layer converts to *inbound.CommandResult before
// returning from the slash-command handler.
type Result struct {
	Consumed bool
	Dropped  bool
}

// RunFix is the exported entry point for the /gtw fix command.
// Called from cmd/nightme/run.go via gtw.Factory.runFix.
//
// F-XX: takes *chatsession.ChatSession directly (no longer through
// the Sender interface) and accepts a Mode parameter to route
// between the issue (remote) flow and the local branch flow.
//
//	args is the user-supplied argv tail AFTER the "fix"
//	subcommand. For ModeRemote: args[0] is the issue id.
//	For ModeLocal: args[0] is the literal branch name.
//	Pass args[1:] (not args[0]) into ModeLocal/Remote since
//	the "fix" subcommand token has already been consumed by
//	Factory.runFix.
func RunFix(
	ctx context.Context,
	mode Mode,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	args []string,
	yes bool,
) (*Result, error) {
	if deps.Now == nil {
		deps.Now = timeNow
	}
	if len(args) < 1 {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"Usage: /gtw fix <issue-id>  |  /gtw fix --name <branch>"), nil
	}

	// --- preflight: SelectedCwd must be set for both modes --------
	if cs == nil || cs.SelectedCwd() == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ "+command.NoActiveCwdReply), nil
	}
	// --- preflight: this directory must not already be a fix worktree ---
	// gtw is per-directory: every directory is its own island.
	// A yml at <cwd>/.nightme/gtw.yml means this directory IS
	// already the worktree of an in-flight fix (started in a
	// previous session or by the user forgetting /gtw close).
	// v1.x deliberately does NOT scan sibling worktrees for ymls
	// — parallel /gtw fix across separate worktrees is the
	// explicit design (the chat's reaction cards are cwd-scoped,
	// each worktree's yml is its own recovery point).
	//
	// F-XX: starting a new fix on top of an active one is
	// always a logic error regardless of intent. The previous
	// --force bypass path is gone (see F-gtw-fix.md §1.2);
	// users with stale worktree paths run `git worktree
	// remove --force <path>` or `/gtw close` manually.
	if _, err := os.Stat(pathutil.Join(cs.SelectedCwd(), nightmeDirName, gtwYmlName)); err == nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"⚠️ Already inside a /gtw fix. Finish or cancel it first."), nil
	}

	switch mode {
	case ModeLocal:
		// F-XX: local mode has no yes parameter; Factory.runFix
		// drops args.Yes when args.Mode == ModeLocal.
		return runFixLocal(ctx, cs, deps, chatID, messageID, args[0])
	default:
		return runFixRemote(ctx, cs, deps, chatID, messageID, args[0], yes)
	}
}

// runFixRemote implements the F-45 / F-XX ID-mode flow:
//
//	/gtw fix <issue-id>
//
// Steps:
//
//  1. RepoRoot → RemoteOriginURL → Detect → GetIssue.
//  2. Branch name = DeriveBranchFromTitle(issue.Title, id).
//  3. PreflightWorktreeCreate → catches path / branch / parent
//     errors before WorktreeAdd.
//  4. BranchExists? → hard-fail reply (F-XX §3.1; no card).
//  5. WorktreeAdd (creates the durable worktree first; failure
//     here surfaces the git error and bails without touching
//     the label).
//  6. AddIssueLabel(LabelWIP). Failure rolls back the worktree
//     and branch via rollbackLabelStep.
//  7. SetSelectedCwd → WriteGTWYml (the yml is the cwd-scoped
//     source of truth for hooks, /gtw close, and recovery).
//  8. Render success card.
//  9. Dispatch issue body to ChatSession.QueueUserMessage so
//     the agent picks it up. Failure here does NOT roll back
//     the worktree — the user can re-trigger manually.
func runFixRemote(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	rawID string,
	yes bool,
) (*Result, error) {
	issueID, err := parseIssueID(rawID)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, fmt.Sprintf("❌ %v", err)), nil
	}

	// F-XX: translate the boolean yes flag into the dispatch
	// mode constant. We do this once at the top of runFixRemote
	// so the value threads through to the success card and the
	// agent prompt without further branching downstream.
	dispMode := DispatchPlan
	if yes {
		dispMode = DispatchExecute
	}

	// F-XX §3.1: daemon-recovery via the worktree's git
	// branch is removed. The previous re-entry path (branch
	// exists at exactly the target worktree path → skip
	// creation, re-dispatch) was deleted; branch collision
	// is now an unconditional hard-fail. Users recovering
	// from a daemon crash must run `/gtw close` first
	// (clears the worktree + branch + label) before
	// retrying /gtw fix.

	prober := deps.Prober
	if prober == nil {
		prober = &ExecHTTPProber{}
	}

	// --- locate repo + remote (§5.2.② prep) -----------------------
	repoRoot, err := RepoRoot(ctx, cs.SelectedCwd(), deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	remoteURL, err := RemoteOriginURL(ctx, repoRoot, deps.Git)
	if err != nil || remoteURL == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ No `origin` remote. Add one with `git remote add origin <url>`."), nil
	}
	detect := deps.Detect
	if detect == nil {
		detect = Detect
	}
	// The worktree doesn't exist yet at this point — runFixRemote
	// runs BEFORE WorktreeAdd so we can use provider hints to name
	// the worktree. But provider.GetIssue / AddIssueLabel calls below
	// still need a valid CWD so `gh` can fork `git`. Pass repoRoot
	// — it's always a real directory (we just computed it from
	// `git rev-parse --show-toplevel`), so the CWD contract holds
	// even when the daemon's own CWD has been stale'd.
	provider, err := detect(ctx, remoteURL, prober, repoRoot)
	if err != nil {
		// D3 split: distinguish "URL is malformed" (user error)
		// from "host not recognised as GitHub/GitLab". Never
		// echo the raw remoteURL — it may carry userinfo.
		redacted := redactForDisplay(remoteURL)
		switch {
		case errors.Is(err, ErrInvalidRemoteURL):
			if redacted == "" {
				return reply(ctx, cs.Emitter(), chatID, messageID,
					"❌ 无效的 remote URL（凭证已脱敏）\n  Expected: https://github.com/<owner>/<repo>.git, git@github.com:<owner>/<repo>.git, ssh://git@<host>/path, git://<host>/path, etc."), nil
			}
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ 无效的 remote URL: %s\n  Expected: https://github.com/<owner>/<repo>.git, git@github.com:<owner>/<repo>.git, ssh://git@<host>/path, git://<host>/path, etc.", redacted)), nil
		default:
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ 暂不支持的 Git 平台 (host: %s — neither github.com/gitlab.com URL hint nor /api/v3/meta or /api/v4/version probe recognised it).", redacted)), nil
		}
	}
	if provider == nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Provider detection returned no result (deps.Detect override bug)."), nil
	}
	providerKind := provider.Kind()
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ Cannot parse owner/repo from remote URL %s.", redactForDisplay(remoteURL))), nil
	}

	// --- fetch issue + derive branch (§5.2.②) --------------------
	issue, err := provider.GetIssue(ctx, owner, repo, issueID)
	if err != nil {
		if errors.Is(err, ErrIssueNotFound) {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ Issue #%d not found in %s/%s.", issueID, owner, repo)), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ Failed to fetch issue: %v", err)), nil
	}
	branch := DeriveBranchFromTitle(issue.Title, issueID)
	worktreePath := WorktreePath(repoRoot, branch)

	// --- branch-exists decision (F-XX §3.1) ---------------------
	// BranchExists == true → hard-fail reply. No worktree add,
	// no label, no dispatch, no decision card. Users with a
	// stale branch must run `/gtw close` first (or `git branch -D`
	// for an orphaned branch).
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		body := fmt.Sprintf("❌ Branch `%s` already exists", branch)
		if existingPath != "" {
			body += fmt.Sprintf("\n→ worktree: %s", existingPath)
		}
		body += "\n↳ finish or drop the active fix with `/gtw close`, then retry"
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// --- preflight (path / branch / parent) ----------------------
	// F-XX: --force is removed. The previous force-cleanup path
	// (forceCleanWorktreePath) is gone — its only use case under
	// the new branch-exists hard-fail is destructive
	// auto-recovery. Users with a stale worktree path now run
	// `git worktree remove --force <path>` or `/gtw close`
	// manually.
	if err := PreflightWorktreeCreate(ctx, repoRoot, branch, worktreePath, deps.Git); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	// --- sync default branch BEFORE creating worktree ---------
	// We want the worktree to branch off the latest upstream
	// code, not whatever stale state the user's local main is
	// in. RefreshDefaultBranch errors on (a) no upstream
	// configured, (b) dirty main repo, (c) non-fast-forward
	// pull. All three are surfaced as a user-friendly reply;
	// the worktree is NOT created in any of those cases.
	//
	// The HEAD SHA returned is threaded through to the
	// success card so the user sees "based on origin/main@abc1234".
	// Empty string when RefreshDefaultBranch skipped (future
	// "no upstream" tolerance) — renderFixSuccessCard handles
	// both.
	//
	// SkipRefreshDefaultBranch (deps flag) is honoured for
	// tests that can't satisfy the pull — see HandlerDeps.
	var baseSHA string
	if !deps.SkipRefreshDefaultBranch {
		var err error
		baseSHA, _, err = RefreshDefaultBranch(ctx, repoRoot, deps)
		if err != nil {
			return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
		}
	}

	// --- create the worktree FIRST (§5.2.③) ---------------------
	// The worktree is the durable signal of "this user is
	// working on this fix". The label is best-effort metadata
	// on the remote issue tracker — applied AFTER the worktree
	// exists so a label API failure cannot leave us needing to
	// undo. If WorktreeAdd fails we surface the git error and
	// bail out without touching the label; if AddIssueLabel fails
	// later, the worktree is already real and the user has a
	// usable setup, label or not.
	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		// /gtw fix failure paths reply with the error text and
		// stop. v1.5 retired the §5.3.3 retry card — users get a
		// single, immediate reply, no draft to click. No cleanup
		// is needed: WorktreeAdd failed before any worktree or
		// label was created, and the chat's SelectedCwd is
		// unchanged (we haven't moved into a worktree yet).
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ /gtw fix: git worktree add failed: %v\n"+
				"[git stderr tail]\n%s",
				err, tailLines(stderrFromWorktreeErr(err), 10))), nil
	}

	// --- label the issue (post-WorktreeAdd; atomic with worktree) ---
	// The label is the external coordination signal on the issue
	// tracker: it tells other viewers (and concurrent /gtw fix
	// attempts on other machines) that this issue is claimed. A
	// worktree without a label means the issue still looks
	// unclaimed — a parallel /gtw fix can race in, leaving two
	// fixes contending for the same issue with no way for either
	// side to detect the conflict.
	//
	// v1.x: the fix is atomic with the label. AddIssueLabel failure
	// rolls back the worktree AND the branch so the user can fix
	// the underlying cause (token scope, network, missing label,
	// rate limit, etc.) and re-run /gtw fix from a clean state.
	// The "label is decoration, worktree is durable" framing from
	// v1 is gone — see commit history for the rationale.
	//
	// Rollback has two independent steps (worktree remove + branch
	// delete) and either can fail in turn. We report each step's
	// outcome honestly: only ask the user to clean up manually
	// the things we couldn't do ourselves. The provider's error
	// is echoed verbatim — no paraphrase, no speculation about
	// cause. No manual `gh`/`glab` retry command is suggested:
	// the whole point of atomic semantics is that the user just
	// re-runs /gtw fix once the cause is fixed.
	// --- ensure gtw labels exist on the repo (§5.2.④) ---------------
	// v1.x: /gtw fix bootstraps the `nightme/*` label set on the
	// first run against any repo. /gtw fix 235 failed outright
	// because `nightme/wip` was missing — `gh issue edit
	// --add-label` errors with "'nightme/wip' not found" rather
	// than creating the label. ensureGtwLabels calls `gh label
	// create --force` / `glab label create` (both idempotent)
	// for every entry in AllLabels BEFORE the AddIssueLabel step, so
	// AddIssueLabel always succeeds on a freshly-cloned repo.
	//
	// Failure semantics: any CreateLabel error rolls back the
	// worktree + branch via the same atomic path as an AddIssueLabel
	// failure. The error is echoed verbatim so the user sees
	// whether the root cause is a token-scope issue, a network
	// blip, or a missing repo permission (gh/glab's own message
	// is the most actionable hint available).
	//
	// Implementation note: AllLabels is short (6 entries); the
	// calls are serial, not parallel, because (a) /gtw fix is
	// one-shot per invocation and a few hundred ms of latency
	// is acceptable, (b) serial calls keep error attribution
	// simple when one of the 6 fails, and (c) gh/glab's own
	// rate-limit handling gets a clean per-call retry surface.
	if err := ensureGtwLabels(ctx, provider, owner, repo); err != nil {
		body := rollbackLabelStep(ctx, deps, repoRoot, worktreePath, branch, issueID,
			fmt.Sprintf("❌ Could not ensure gtw labels on %s/%s: %v\n", owner, repo, err))
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	if err := provider.AddIssueLabel(ctx, owner, repo, issueID, LabelWIP); err != nil {
		body := rollbackLabelStep(ctx, deps, repoRoot, worktreePath, branch, issueID,
			fmt.Sprintf("❌ Could not add label %q to issue #%d: %v\n", LabelWIP, issueID, err))
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// --- switch cwd + write context + render + dispatch ----------
	return completeFixAndDispatch(ctx, cs, deps, chatID, messageID,
		branch, worktreePath, owner+"/"+repo, repoRoot, string(providerKind), ModeRemote, issueID, issue, baseSHA, dispMode)
}

// runFixLocal implements the F-XX local-mode flow:
//
//	/gtw fix --name <branch>
//
// Steps:
//
//  1. DeriveBranchFromName(rawName) — slugifies user input; errors
//     out if the result is empty (no remote-issue id to fall
//     back to).
//  2. RepoRoot → no origin required.
//  3. PreflightWorktreeCreate.
//  4. BranchExists? → hard-fail reply (F-XX §3.1; no card).
//  5. WorktreeAdd; on failure → plain-text reply (v1.5 retired
//     the §5.3.3 retry card; failure stops the flow).
//  6. SetSelectedCwd → WriteGTWYml (cwd-scoped source of truth).
//  7. Render the simplified local success card.
//
// Local mode does NOT call provider.GetIssue / AddIssueLabel /
// QueueUserMessage — the user is opting into a no-remote flow
// that should work even in repos without an `origin`.
//
// F-XX: local mode has no yes/dispMode parameter — the
// Plan / Execute split has no meaning without an agent
// dispatch. Factory.runFix already drops args.Yes when
// args.Mode == ModeLocal before calling RunFix.
func runFixLocal(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	rawName string,
) (*Result, error) {
	branch, err := DeriveBranchFromName(rawName)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, "❌ "+err.Error()), nil
	}

	repoRoot, err := RepoRoot(ctx, cs.SelectedCwd(), deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first."), nil
	}
	worktreePath := WorktreePath(repoRoot, branch)

	// --- branch-exists decision (F-XX §3.1) ---------------------
	// Local mode: same hard-fail as runFixRemote. No decision
	// card — users must clean up the stale branch manually
	// before retrying.
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		body := fmt.Sprintf("❌ Branch `%s` already exists", branch)
		if existingPath != "" {
			body += fmt.Sprintf("\n→ worktree: %s", existingPath)
		}
		body += "\n↳ run `git worktree remove --force <path>` or `git branch -D <branch>` to clean up, then retry"
		return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
	}

	// F-XX: --force removed for local mode too. See runFixRemote
	// for the rationale.
	if err := PreflightWorktreeCreate(ctx, repoRoot, branch, worktreePath, deps.Git); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		// /gtw fix failure paths reply with the error text and
		// stop. v1.5 retired the §5.3.3 retry card — users get a
		// single, immediate reply, no draft to click. No cleanup
		// is needed: WorktreeAdd failed before any worktree was
		// created, and the chat's SelectedCwd is unchanged.
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ /gtw fix: git worktree add failed: %v\n"+
				"[git stderr tail]\n%s",
				err, tailLines(stderrFromWorktreeErr(err), 10))), nil
	}

	// F-XX: dispMode is unused for ModeLocal (local never
	// dispatches and renders its own success card); pass
	// DispatchPlan as a zero-equivalent placeholder.
	return completeFixAndDispatch(ctx, cs, deps, chatID, messageID,
		branch, worktreePath, "", repoRoot, "", ModeLocal, -1, nil, "" /* baseSHA: local mode doesn't refresh */, DispatchPlan)
}

// completeFixAndDispatch handles the common tail of both modes:
// switch cwd → ensure worktree .gitignore → write yml snapshot
// → store Context → render success card → dispatch the issue
// body to the agent (ID mode only). Centralising this means
// both modes share the same "after worktree is created" logic;
// the mode-specific bits stay in runFixRemote / runFixLocal.
//
// issue is non-nil only in ID mode; local mode passes nil (the
// dispatcher check at the bottom skips it).
//
// repoRoot is the main repo path (needed for /gtw close to run
// `git worktree remove` from there later) and is also written
// into the on-disk snapshot. provider is the platform kind
// ("github" / "gitlab"); empty for ModeLocal. repoRoot must be
// absolute — callers in runFixRemote / runFixLocal get it from
// RepoRoot(ctx, ...) which always returns an absolute path.
func completeFixAndDispatch(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, branch, worktreePath, repo, repoRoot, provider string,
	mode Mode,
	issueID int,
	issue *Issue,
	baseSHA string,
	dispMode IssueDispatchMode,
) (*Result, error) {
	// --- switch cwd (§5.2.④) -------------------------------------
	if err := cs.SetSelectedCwd(worktreePath); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ SetSelectedCwd failed: %v", err)), nil
	}

	// --- ensure worktree .gitignore (§14.4 step 5) ---------------
	// We touch the worktree's gitignore AFTER SetSelectedCwd so the
	// file lives where the user's eye will land. Then we commit
	// the change so the worktree ends up genuinely clean — that
	// way `git worktree remove` later succeeds without
	// --force. The commit author is forced to the gtw tool
	// identity (see CommitGitignoreIfDirty) so the user can
	// visually identify and squash tool commits if desired.
	//
	// EnsureGitignore failures are warn-only — the user can
	// fix a missing .gitignore manually. CommitGitignore failures
	// are HARD errors: without the commit, .gitignore stays
	// untracked and /gtw close will reject the worktree as
	// dirty, leaving the user stuck. Better to surface the
	// problem now and roll back to the pre-WorktreeAdd state
	// via a friendly reply.
	if err := EnsureGitignore(worktreePath); err != nil {
		slog.Default().Warn("gtw: EnsureGitignore failed",
			"worktree", worktreePath,
			"err", err)
	} else if err := CommitGitignoreIfDirty(ctx, worktreePath, deps.Git); err != nil {
		slog.Default().Warn("gtw: CommitGitignore failed",
			"worktree", worktreePath,
			"err", err)
		// Roll back: the worktree was created but is unusable
		// (untracked .gitignore → /gtw close will refuse). Best
		// to remove it now so the user can retry. We pass
		// force=true because .gitignore IS in the way of the
		// remove — git would refuse otherwise.
		if rmErr := WorktreeRemove(ctx, repoRoot, worktreePath, true /* force */, deps.Git); rmErr != nil {
			slog.Default().Warn("gtw: rollback WorktreeRemove after CommitGitignore failure",
				"worktree", worktreePath,
				"commit_err", err,
				"remove_err", rmErr)
			// Even the rollback failed. Surface the original
			// error + the rollback note so the user knows to
			// clean up by hand.
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ /gtw fix: CommitGitignore failed (%v); rollback also failed (%v).\n"+
					"the worktree at %s is in a stuck state — please `git worktree remove --force %s` manually.",
					err, rmErr, worktreePath, worktreePath)), nil
		}
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ /gtw fix: CommitGitignore failed (%v).\n"+
				"rolled back worktree at %s. fix and retry:\n"+
				"  - ensure `git config user.email` is set, OR\n"+
				"  - manually commit %s/.gitignore before retrying /gtw fix",
				err, worktreePath, worktreePath)), nil
	}

	// --- write on-disk snapshot (§14.4 step 6) -------------------
	// Persist the immutable fix snapshot so /gtw close can rebuild
	// state even after a daemon restart. The yml is the cwd-scoped
	// source of truth for hooks, /gtw close, and reaction handlers;
	// there is no parallel in-memory copy (the slot is gone).
	// ErrGtwYmlExists is silently skipped (the on-disk snapshot
	// already exists; re-running /gtw fix on the same worktree
	// would re-write it, but we keep the old one to avoid clobbering
	// any user edits). Any other error is warn-only: the worktree
	// is the durable side effect and the user can manually finish
	// or recover via /gtw close.
	if err := WriteGTWYml(worktreePath, Context{
		Mode:     mode,
		Issue:    issueID,
		Branch:   branch,
		Worktree: worktreePath,
		RepoRoot: repoRoot,
		Repo:     repo,
		Provider: provider,
	}, deps.Now); err != nil && !errors.Is(err, ErrGtwYmlExists) {
		slog.Default().Warn("gtw: WriteGTWYml failed",
			"worktree", worktreePath,
			"err", err)
	}

	// /gtw fix doesn't touch the PR cache: the new worktree's
	// AS has no cache yet (fresh allocation on the next
	// stamp's GetOrCreate → MaybeRefresh), and any OLD
	// worktree's AS still correctly points at its own branch.
	// The per-stamp lazy MaybeRefresh in runtime.go's
	// LookupPR closure picks up the new workspace on the
	// next outbound stamp.

	// --- render the success card (§5.2.⑥) ------------------------
	var card string
	if mode == ModeLocal {
		card = renderFixLocalSuccessCard(branch, worktreePath)
	} else {
		// ID mode. Callers (runFixRemote) guarantee issue is
		// non-nil; a nil here would be a programming error.
		card = renderFixSuccessCard(issue, branch, worktreePath, repo, baseSHA, dispMode)
	}
	result := reply(ctx, cs.Emitter(), chatID, messageID, card)

	// --- dispatch issue to agent (ID mode only) -------------------
	// We do this AFTER the reply so the user sees the success
	// card immediately even if dispatch stalls / fails. Failures
	// here are warn-only — the worktree is the durable side
	// effect; a failed dispatch can be retried by the user
	// re-running /gtw fix or by manually sending the issue
	// reference to the agent.
	if mode == ModeRemote && issue != nil {
		// Download attachments (best-effort) and assemble the
		// dispatch blocks. Download failures log a warning
		// and continue with text-only — the agent still gets
		// the issue body, just no files.
		var attachmentBlocks []agent.ContentBlock
		if len(issue.Attachments) > 0 {
			dir := attachmentsDir(worktreePath, issueID)
			attachmentBlocks = downloadAttachmentsBestEffort(ctx, issue.Attachments, dir)
		}
		blocks := buildIssueDispatchBlocks(issue, attachmentBlocks, branch, repo, dispMode)
		if err := dispatchIssueToChatSession(ctx, cs, deps.Now, chatID, messageID, blocks); err != nil {
			slog.Default().Warn("gtw: dispatch issue to agent failed",
				"issue_id", issue.ID,
				"chat_id", chatID,
				"err", err)
			// Append a single-line warning so the user knows
			// the agent didn't receive the dispatch. We do not
			// roll back the worktree — see comment above.
			_ = cs.Emitter().Send(ctx, messages.OutboundMessage{
				ChatID:  chatID,
				Kind:    messages.OutCommandReply, // one-shot plain text, not a rolling-log receipt card
				ReplyTo: messageID,
				Text:    fmt.Sprintf("⚠️ Could not dispatch issue #%d to agent: %v\nThe worktree is ready; you can /cwd into %s and tell the agent to fix #%d.", issue.ID, err, worktreePath, issue.ID),
			})
		}
	}
	return result, nil
}

// dispatchIssueToChatSession packages the issue + downloaded
// attachments into a slice of ContentBlocks and pushes them
// into the chat session's input queue. The queue's TryFlush()
// will spawn (or reuse) an AgentSession and submit the prompt.
//
// blocks is the full ContentBlock slice — usually one text
// block (issue dispatch template) followed by zero or more
// ContentFile blocks for downloaded attachments. completeFixAndDispatch
// builds this slice by running buildIssueDispatchBlocks +
// downloadAttachmentsBestEffort.
//
// The synthesised Message reuses the GTW command's inbound
// messageID so the agent's reply is anchored as a thread reply
// under the user's original /gtw fix command — IM UX continuity.
//
// Kind is MessageKindQueue so the message forms its own Prompt
// batch and isn't merged with subsequent user messages (a user
// typing "thanks" right after the /gtw fix shouldn't be batched
// into the same prompt as "fix issue #42").
//
// now is the clock dependency; callers pass deps.Now (which is
// already used by completeFixAndDispatch for the Context
// timestamp) so tests can pin time deterministically.
func dispatchIssueToChatSession(
	_ context.Context,
	cs *chatsession.ChatSession,
	now func() time.Time,
	chatID, messageID string,
	blocks []agent.ContentBlock,
) error {
	// fix-placehold-card: resolve the active AgentSession BEFORE
	// emitting MessageQueued so the runtime eventbus subscriber
	// can stamp AgentBar onto the placeholder card (it reads
	// cs.SelectedAgentSession() at publish time — see
	// internal/runtime/eventbus.go). Mirrors the swap applied
	// to internal/runtime/dispatcher.go and
	// internal/chatsession/manager.go::HandleInbound.
	//
	// On error (no workspace / spawn failed) we deliberately do
	// NOT emit MessageQueued — the caller already surfaces the
	// failure via a warning log + OutCommandReply warning
	// (see completeFixAndDispatch below), and emitting ⏳ first
	// would leave an orphan reaction on the user message with
	// no follow-up MessageSubmitted / MessageDone to clear it.
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		return err
	}

	// F-54 timing: emit MessageQueued AFTER the AS is resolved
	// so the subscriber sees a non-nil selectedAS. Channels can
	// render ⏳ (and AgentBar) immediately; QueueUserMessage
	// follows so the message is delivered.
	cs.EmitMessageState(messageID, agent.MessageQueued)
	return cs.QueueUserMessage(chatsession.Message{
		ID:         messageID,
		ChatID:     chatID,
		Blocks:     blocks,
		ReceivedAt: now(),
		Kind:       chatsession.MessageKindQueue,
	})
}

// buildIssueDispatchBlocks assembles the full
// []agent.ContentBlock slice for the dispatch: a single text
// block (the issue template) followed by attachmentBlocks
// (zero or more ContentImage / ContentFile). The caller
// downloads attachments before passing them in.
//
// The text block is built LAST, after the attachment blocks are
// known, so the Attachments section can report accurate counts
// ("N images, M files") drawn from what actually downloaded —
// not from issue.Attachments, which may include attachments that
// failed to download (network / 4xx / oversize) and would
// otherwise be advertised to the agent but missing from the
// blocks slice.
func buildIssueDispatchBlocks(issue *Issue, attachmentBlocks []agent.ContentBlock, branch, repo string, mode IssueDispatchMode) []agent.ContentBlock {
	imageCount, fileCount := countAttachmentBlocks(attachmentBlocks)
	text := buildIssueDispatchText(issue, branch, repo, mode, imageCount, fileCount)
	blocks := make([]agent.ContentBlock, 0, 1+len(attachmentBlocks))
	blocks = append(blocks, agent.ContentBlock{Type: agent.ContentText, Text: text})
	blocks = append(blocks, attachmentBlocks...)
	return blocks
}

// countAttachmentBlocks splits a slice of downloaded attachment
// blocks into (imageCount, fileCount) by Type. Used by
// buildIssueDispatchBlocks so the dispatch text's Attachments
// section reports what the agent will actually see rather than
// what the issue nominally carried. Non-image/non-file blocks
// (e.g. a stray ContentText) are ignored — only image + file
// blocks count toward the attachment tally.
func countAttachmentBlocks(blocks []agent.ContentBlock) (images, files int) {
	for _, b := range blocks {
		switch b.Type {
		case agent.ContentImage:
			images++
		case agent.ContentFile:
			files++
		}
	}
	return images, files
}

// buildIssueDispatchText formats the issue as a fixed-template
// markdown block. The template lives here (rather than in the
// renderer) because it's the "agent prompt" not the "user-facing
// success card". The user never sees this body directly — they
// see the success card; the agent sees this block.
//
// Section order is stable so agent prompts can rely on it:
// header / metadata / body / attachments / task. The Task
// section's verb varies by mode (Plan = "analyze only, do not
// modify"; Execute = "implement the fix"). Metadata /
// description / attachments sections are shared verbatim.
//
// imageCount / fileCount are the counts actually downloaded
// (from countAttachmentBlocks over the real attachmentBlocks),
// NOT len(issue.Attachments): an attachment that failed to
// download (network / 4xx / oversize) must NOT be advertised
// to the agent — it would tell the model "see the screenshot"
// for a screenshot that isn't in the blocks slice. Zero of
// both suppresses the whole Attachments section.
//
// The Attachments section speaks the agent's language, not ours:
// it says "images shown inline" / "files downloaded, read on
// demand" — never "ContentImage" / "ContentFile", which are our
// internal block-type names the runtime agent has no concept of.
// The bridges translate the blocks themselves (claudecode inlines
// image pixels as base64, codex passes image paths via -i, file
// blocks become "File: <path>" annotations); the prompt text only
// needs to prime the agent that attachments exist and how they'll
// arrive, so it knows to look for pixels / read files.
//
// See F-gtw-fix.md §4 for the rationale and the full prompt
// templates. gtw always dispatches exactly one prompt per
// /gtw fix; subsequent agent↔user confirmation flows through
// the chat, never back through gtw.
func buildIssueDispatchText(issue *Issue, branch, repo string, mode IssueDispatchMode, imageCount, fileCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📥 GitHub issue #%d — %s\n\n", issue.ID, issue.Title)
	fmt.Fprintf(&b, "## Metadata\n")
	fmt.Fprintf(&b, "- repo: %s\n", repo)
	fmt.Fprintf(&b, "- branch: %s\n", branch)
	fmt.Fprintf(&b, "- url: %s\n\n", issue.URL)
	if strings.TrimSpace(issue.Body) != "" {
		b.WriteString("## Description\n")
		b.WriteString(issue.Body)
		b.WriteString("\n\n")
	}
	if imageCount > 0 || fileCount > 0 {
		b.WriteString("## Attachments\n")
		if imageCount > 0 {
			fmt.Fprintf(&b, "- %d image(s): shown inline in this message (you can see them directly)\n", imageCount)
		}
		if fileCount > 0 {
			fmt.Fprintf(&b, "- %d file(s): downloaded to the worktree; read on demand with your file tools\n", fileCount)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Task\n")
	switch mode {
	case DispatchPlan:
		// F-gtw-fix.md §4.1 — Plan is a *due-diligence* pass,
		// not an implementation pass. Its output is a list of
		// questions and decisions for the user to review, NOT a
		// set of code edits. This prompt must NOT act on code.
		//
		// Runtime self-containment: this string runs in a
		// standalone agent on the user's own worktree — it
		// cannot see this repo's docs (F-gtw-fix.md,
		// REVIEWER_INSTRUCTIONS.md) and does not need to know
		// that Execute is a separate dispatch mode. So the
		// runtime text must NOT reference section numbers, doc
		// filenames, or "the Execute pass" — every instruction
		// the agent needs must be self-contained below.
		//
		// Methodology: every claim grounded in code; the request
		// text is a *problem statement to verify*, not a *spec to
		// implement*. Agents must read the code, trace the call
		// path, and cite file:line (or a grep / runtime trace)
		// for every claim. If a claim cannot be grounded, the
		// agent must say so explicitly rather than invent a
		// citation.
		b.WriteString("This is a due-diligence pass, not an implementation pass. Your deliverable is a *plan*: a list of questions about the request and the decisions that answer them, grounded in the worktree's current source. You will NOT modify, create, or delete any files — produce the plan and stop; the user decides what happens next.\n\n")
		b.WriteString("Baseline rule: the worktree's current source is ground truth. The request text is a problem statement to *verify* against the code, not a spec to *implement*. If the code contradicts the request, say so — the user needs to know the request is wrong, not a confirmation that pretends otherwise.\n\n")
		b.WriteString("Step 1 — Decompose the request into verifiable claims. List each concrete statement the request makes (e.g. 'sessions expire after 7 days', 'the label is set via gh issue edit', 'the bug reproduces on Linux only'). Number them. You will verify each one in step 2.\n\n")
		b.WriteString("Step 2 — Verify every claim against the code. For each numbered claim from step 1, run the search / read that would confirm or refute it. Cite either:\n")
		b.WriteString("  • file:line + the relevant code snippet, OR\n")
		b.WriteString("  • a grep / test command and its output, OR\n")
		b.WriteString("  • 'unverifiable — the code does not address this claim' (this is a legitimate answer; surface it as an open question, don't paper over it)\n")
		b.WriteString("If the code does not match what the request claims, say so explicitly. Do NOT stretch the narrative to fit.\n\n")
		b.WriteString("Step 3 — Classify each claim as one of:\n")
		b.WriteString("  • Confirmed bug: code does X, request says it should do Y, the gap is the bug\n")
		b.WriteString("  • Misunderstanding: code already does what the request asks; the request is based on a wrong read of the code\n")
		b.WriteString("  • Feature gap: code doesn't address this area at all; new capability required\n")
		b.WriteString("  • Unverifiable: cannot tell from the code alone (state what evidence would resolve it)\n\n")
		b.WriteString("Step 4 — Root cause + fix shape (only for confirmed bugs and feature gaps). For confirmed bugs: trace the actual call path that produces the wrong behaviour, name the file:line where the gap lives, and propose a fix that addresses the root cause (not a symptom patch). For feature gaps: locate the closest existing implementation (file:line) that the new code should integrate with, and name the seams (file:line) where the new code would touch existing code.\n\n")
		b.WriteString("Step 5 — Test / verification strategy. Which existing tests cover the affected code path? What new test would catch a regression? If no test exists and adding one is non-trivial, say so.\n\n")
		b.WriteString("Step 6 — Questions for the user (this is the most important section). Any claim that came back as 'Misunderstanding', 'Feature gap', or 'Unverifiable' is a question you cannot answer from the code alone — it requires the user. List these questions explicitly so the user knows what to confirm before authorising implementation with -y. Examples:\n")
		b.WriteString("  • 'The request says X but the code does Y. Did you mean Y, or does the code have a bug?'\n")
		b.WriteString("  • 'Step 4 assumes the bug lives at file:line X. If it's actually at file:line Y, the fix shape changes. Confirm.'\n")
		b.WriteString("  • 'The bug reproduces on Linux only' — there's no Linux-only branch in the code. Where is this assumption coming from? (external state?)\n")
		b.WriteString("Each question should be one sentence the user can answer yes/no or with a short clarification. Don't bundle multiple decisions into one question.\n\n")
		b.WriteString("Output format (the user reviews this in chat to decide whether to authorise implementation with -y):\n")
		b.WriteString("  ## Plan for: <request title>\n")
		b.WriteString("  ### Request decomposition\n  1. <claim 1>  2. <claim 2>  ...\n")
		b.WriteString("  ### Verification\n  1. <file:line + code snippet, OR grep output, OR 'unverifiable'>\n     2. <same>\n     ...\n")
		b.WriteString("  ### Classification\n  1. <Confirmed bug | Misunderstanding | Feature gap | Unverifiable>\n     2. <same>\n     ...\n")
		b.WriteString("  ### Root cause / fix shape\n  (only for confirmed bugs + feature gaps; cite file:line)\n")
		b.WriteString("  ### Test strategy\n  (which existing tests cover this; what new test if any)\n")
		b.WriteString("  ### Questions for the user\n  1. <one sentence the user can answer yes/no or with short clarification>\n     2. ...\n")
		b.WriteString("\n")
		b.WriteString("If there are NO questions (every claim verified cleanly), state so explicitly: 'No questions for the user; the plan is complete and the user can authorise -y without further input.'\n\n")
		b.WriteString("Do NOT modify, create, or delete any files. Present the plan and STOP — wait for the user to reply in this chat before making any code changes.\n")
	case DispatchExecute:
		// F-gtw-fix.md §4.2 — Execute is the *fulfilment* of a
		// plan, run in GOBL mode (Goals / Obstacles / Boundaries
		// / Learn): the agent is autonomous on the path (which
		// files to open, which tests to run, how to sequence
		// the work), but every *decision* still needs to be
		// code-grounded. The plan is a starting contract;
		// deviations are allowed but must be announced in chat
		// before being acted on, so the user has a chance to
		// interrupt.
		//
		// Runtime self-containment (same caveat as Plan above):
		// this string runs in a standalone agent on the user's
		// worktree and cannot see this repo's docs. It also
		// cannot assume a prior Plan turn exists in the chat —
		// `-y` dispatches Execute directly with no Plan round.
		// So the runtime text must NOT say "the plan above"
		// (there may be none) and must NOT cite §4.2; it takes
		// the request + worktree as given and acts.
		b.WriteString("Implement the fix for the request above against the worktree, in GOBL mode (Goals / Obstacles / Boundaries / Learn). The worktree is prepared; a plan may or may not have been produced in an earlier turn — if one was, treat it as a starting contract; if not, derive the plan yourself from the code first, then act.\n\n")
		b.WriteString("Goal: the verified-change summary the user can review.\n")
		b.WriteString("Boundaries:\n")
		b.WriteString("- Do not invent functionality the request didn't ask for.\n")
		b.WriteString("- Do not refactor unrelated code.\n")
		b.WriteString("- Do not skip, suppress, or mark-expected failing tests.\n")
		b.WriteString("- Do not report 'complete' if any test is failing. **All tests must pass before completion.** A failing test is not a deliverable.\n\n")
		b.WriteString("Operating principles:\n")
		b.WriteString("- Treat any prior plan as a starting contract, not a straitjacket. If during execution you discover a plan is incomplete or wrong (root cause is different, an additional file needs changing, a fix in a file the plan didn't list), you may revise — but declare the revision in chat FIRST with file:line evidence, then act. The user has one round-trip to interrupt before you proceed with the deviation. If there was no prior plan, every non-trivial decision counts as a deviation you should announce before acting.\n")
		b.WriteString("- Decisions must be code-grounded: every file you touch or test you run cites the file:line or the test command + exit code that justified it. If you find yourself about to do something the request text suggests but the code contradicts, surface the contradiction — don't silently follow the text.\n")
		b.WriteString("- When tests fail, diagnose against the baseline (was this failure pre-existing? Did your change introduce it?). Pre-existing failures are not yours to silently fix; report and let the user decide. Failures you introduced must be fixed before completion.\n\n")
		b.WriteString("Workflow:\n")
		b.WriteString("1. Re-read the files you intend to change. Confirm each planned change still applies to today's baseline (the worktree may have drifted since any prior plan turn).\n")
		b.WriteString("2. Apply the minimal change that satisfies the request (and the plan, if one exists). If a deviation is needed, announce it before acting.\n")
		b.WriteString("3. Run the project's test command (infer from go.mod / Makefile / CI config). Report the full command, the exit code, and which tests ran.\n")
		b.WriteString("4. If a test fails: do NOT silently suppress, skip, or mark expected. Diagnose (pre-existing vs introduced), fix introduced failures against the baseline code, re-run until green. Pre-existing failures — report and ask the user.\n")
		b.WriteString("5. Summarise:\n")
		b.WriteString("  - Files changed, with file:line ranges and one-line justification per range (which planned step does it fulfil, or which in-flight deviation)\n")
		b.WriteString("  - Decisions made: any deviations from the plan (or from the request, if no plan existed), with file:line evidence and the user-facing question (if any) you asked along the way\n")
		b.WriteString("  - Test command(s) run, with exit code\n")
		b.WriteString("  - One sentence: 'this change is correct against the baseline because <file:line evidence>'\n")
	}
	return b.String()
}

// parseIssueID accepts a string like "42" or "#42" and returns the
// int. Anything else is an error.
func parseIssueID(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "#")
	if raw == "" {
		return 0, errors.New("empty issue id")
	}
	n := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid issue id %q (digits only)", raw)
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, errors.New("issue id cannot be 0")
	}
	return n, nil
}

// tailLines returns the last n non-empty lines of s, joined with \n.
func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// stderrFromWorktreeErr extracts the captured stderr from a
// *WorktreeError. Returns "" for other error kinds.
func stderrFromWorktreeErr(err error) string {
	if werr, ok := errors.AsType[*WorktreeError](err); ok {
		return werr.Stderr
	}
	return ""
}

// toChatsessionChoiceOptions was removed in F-51: the gtw package
// now owns ChoiceOption directly (no chatsession alias needed).
// The renderer stores card.Options verbatim on the draft.

// renderChoiceMarkdown + gtwChoiceToGateway + the gtw.Choice /
// gtw.ChoiceOption types were all removed in v1.5 along with
// the §5.3.3 worktree-fail retry card (WorktreeFailChoice +
// emitWorktreeFailDraft + the DraftFixWorktreeFail kind). The
// gtw package no longer emits interactive cards of its own;
// the messages-level Choice / ChoiceOption types are still in
// use by the channels package (feishu/slack/telegram).

// ensureGtwLabels bootstraps the full AllLabels set on the
// remote repo via provider.CreateLabel. The order matches
// AllLabels (display order) so that any error message naming
// the failing label matches what the user sees in /gtw push
// or /gtw pr's UI. The first failure short-circuits the loop
// and is returned verbatim — subsequent labels stay
// un-bootstrapped until the user fixes the cause and re-runs
// /gtw fix, at which point the loop picks up where it left
// off because CreateLabel is idempotent.
//
// Why serial, not parallel: AllLabels has 6 entries; gh/glab's
// own retry / rate-limit handling gets a cleaner surface when
// each call is its own round-trip. The /gtw fix user is happy
// to wait a few hundred ms in exchange for a clear error
// attribution when one label fails.
//
// Returns nil when every label is bootstrapped (created or
// already present). The caller (runFixRemote's AddIssueLabel path)
// treats nil as success and proceeds to the AddIssueLabel step.
func ensureGtwLabels(ctx context.Context, provider GitProvider, owner, repo string) error {
	for _, name := range AllLabels {
		meta := LabelMetaFor(name)
		if err := provider.CreateLabel(ctx, owner, repo, name, meta.Color, meta.Description); err != nil {
			return fmt.Errorf("ensure %q: %w", name, err)
		}
	}
	return nil
}

// rollbackLabelStep rolls back the worktree + branch created by
// the /gtw fix flow when the post-WorktreeAdd label step
// (CreateLabel or AddIssueLabel) fails. Returns the user-facing
// reply body, which the caller hands to reply(). The shape of
// the three error branches (clean / partial / fully stuck) is
// preserved verbatim from the v1.x atomic semantics — only the
// error-message prefix changes.
//
// head is the leading "❌ Could not ... : <err>" line that
// distinguishes CreateLabel failures from AddIssueLabel failures.
// The trailing "fix the cause and re-run /gtw fix <id>" hint
// is identical in both cases because both share the same
// root-cause class: label-state coordination with the remote
// issue tracker.
//
// The helper centralises the rollback so CreateLabel and
// AddIssueLabel failure paths stay in lockstep; adding a new
// post-worktree label step in the future only requires one
// rollbackLabelStep call instead of duplicating the
// three-branch switch.
func rollbackLabelStep(
	ctx context.Context,
	deps HandlerDeps,
	repoRoot, worktreePath, branch string,
	issueID int,
	head string,
) string {
	wtErr := WorktreeRemove(ctx, repoRoot, worktreePath, true /* force */, deps.Git)
	// Branch delete runs only if the worktree came out cleanly.
	// Otherwise we leave the branch for the user to handle
	// alongside the worktree so they keep a consistent mental
	// model of "the worktree is gone, the branch references it".
	var branchErr error
	if wtErr == nil {
		_, stderr, brErr := deps.Git.Run(ctx, repoRoot, "branch", "-D", branch)
		if brErr != nil {
			branchErr = fmt.Errorf("%w: %s", brErr, strings.TrimSpace(stderr))
		}
	}

	switch {
	case wtErr == nil && branchErr == nil:
		return head +
			"worktree and branch have been rolled back.\n" +
			"fix the cause and re-run /gtw fix " + fmt.Sprintf("%d", issueID)
	case wtErr == nil && branchErr != nil:
		return head +
			fmt.Sprintf("rolled back worktree at %s, but branch %q still exists.\n", worktreePath, branch) +
			fmt.Sprintf("  [rollback warning] %v\n", branchErr) +
			fmt.Sprintf("  please clean up manually: `git branch -D %s`\n", branch) +
			"fix the cause and re-run /gtw fix " + fmt.Sprintf("%d", issueID)
	default:
		// wtErr != nil: worktree remove failed. Branch is
		// untouched (and likely still attached to the stuck
		// worktree). User has to clean up BOTH.
		wtMsg := "unknown error"
		if wtErr != nil {
			wtMsg = wtErr.Error()
		}
		return head +
			"could not roll back automatically:\n" +
			fmt.Sprintf("  [worktree] %s\n", wtMsg) +
			fmt.Sprintf("    clean up with: `git worktree remove --force %s`\n", worktreePath) +
			fmt.Sprintf("  [branch] %q likely still attached — clean up with: `git branch -D %s`\n", branch, branch) +
			"fix the cause and re-run /gtw fix " + fmt.Sprintf("%d", issueID)
	}
}

// reply posts a plain-text OutReply via the chat session's
// outbound chokepoint. nil-safe on the emitter: if the emitter is
// nil (test setup without a manager emitter, or runtime
// misconfiguration), the reply is dropped silently and
// Consumed=true is returned. The daemon must never crash on a
// missing outbound surface — see wip/commander.md §2.7 for the
// nil-skip invariant.
//
// reply is the GTW-package no-agent-stamp thin wrapper over
// replyAgent (agent_reply.go). Dispatchers that did NOT invoke
// an agent (push / sync / close / fix / ctx-error paths) keep
// using reply — they have no agent metadata to stamp on the
// OutboundMessage. Dispatchers that DID invoke an agent (commit /
// pr) use replyAgent directly with the captured agentName +
// agent.RunResult so the channel footer renders the agentbar and
// usagebar lines (F-CLAUDE-PRINT-002 follow-up: GTW bypasses the
// runtime event pipeline, so the stamp has to happen here).
//
// reply sends a single OutReply through the shared Emitter. The
// Emitter stamps GitStatus at the chokepoint (outbound.Options
// .GitStatusLookup); callers don't need a ChatSession reference
// here.
func reply(ctx context.Context, em messages.Emitter, chatID, messageID, text string) *Result {
	return replyAgent(ctx, em, chatID, messageID, text, "", agent.RunResult{})
}
