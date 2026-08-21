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
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/pathutil"
	"github.com/cnlangzi/nightme/internal/prcache"
)

// ContextSlot is the gtw-package view of one Manager's per-chat
// context slot. Production: gtw.Manager.GetContext / SetContext.
// Tests can pass a small adapter that wraps those methods.
//
// The slot is value-typed: Load returns a copy, Store accepts a
// copy. The gtw package never holds a live pointer to the stored
// value, which keeps the reader/writer race surface to zero.
// Pass the zero Context{} to Store to clear.
type ContextSlot interface {
	Load() Context
	Store(c Context)
}

// DraftsMap is the gtw-package view of one ChatSession's
// gtwDrafts map. Production: gtwDraftsMap from
// internal/gateway. Tests: a map[string]*Draft with closures.
type DraftsMap interface {
	Store(requestID string, d *Draft)
	Take(requestID string) *Draft
	Lookup(requestID string) *Draft
}

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
	slot ContextSlot,
	drafts DraftsMap,
	deps HandlerDeps,
	chatID, messageID string,
	args []string,
	force bool,
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
	if cur := slot.Load(); cur != (Context{}) {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"⚠️ Already inside a /gtw fix. Finish or cancel it first."), nil
	}
	// preflightOrphanYml catches the one case the in-memory
	// slot check above cannot: the user is sitting inside a
	// worktree that already holds .nightme/gtw.yml (e.g.
	// /cwd'd into a previous fix's worktree and forgot to
	// /gtw close). v1.x does NOT scan sibling worktrees for
	// ymls — parallel /gtw fix across separate worktrees is
	// supported. See preflightOrphanYml's doc for the full
	// rationale and the history of the removed sibling scan.
	//
	// force=true does NOT bypass this check: starting a new
	// fix on top of an active one is always a logic error
	// regardless of intent. --force is for the worktree-path
	// collision case (forceCleanWorktreePath), not for
	// overriding the slot/preflight layer.
	if err := preflightOrphanYml(cs.SelectedCwd()); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	switch mode {
	case ModeLocal:
		return runFixLocal(ctx, cs, slot, drafts, deps, chatID, messageID, args[0], force)
	default:
		return runFixRemote(ctx, cs, slot, drafts, deps, chatID, messageID, args[0], force)
	}
}

// forceCleanWorktreePath is the --force counterpart to
// PreflightWorktreeCreate's path-occupied check. When the user
// passes /gtw fix <id> --force, they explicitly accept that
// any leftover at the target worktree path will be nuked —
// typically because the previous /gtw close aborted and left
// a stale directory, or because they're re-running after a
// failed /gtw fix.
//
// We only act when the path is occupied. A no-op when the path
// doesn't exist (the normal first-time /gtw fix case). On a
// real git failure (worktree remove --force refused, etc.)
// returns the underlying WorktreeError so the caller can
// surface a friendly reply.
func forceCleanWorktreePath(ctx context.Context, repoRoot, worktreePath string, git GitRunner) error {
	if _, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to clean
		}
		return fmt.Errorf("stat %s: %w", worktreePath, err)
	}
	// Path exists. Force-remove via `git worktree remove
	// --force <path>` from the main repo. force=true tells git
	// to skip the "untracked / modified files" safety net —
	// the user opted into this with --force, so any local
	// edits in the leftover worktree are forfeit.
	return WorktreeRemove(ctx, repoRoot, worktreePath, true /* force */, git)
}

// runFixRemote implements the F-45 / F-XX ID-mode flow:
//
//	/gtw fix <issue-id>
//
// Steps:
//
//  1. RebuildContext (daemon-recovery; §5.7).
//  2. RepoRoot → RemoteOriginURL → Detect → GetIssue.
//  3. Branch name = DeriveBranchFromTitle(issue.Title, id).
//  4. PreflightWorktreeCreate → catches path / branch / parent
//     errors before WorktreeAdd.
//  5. BranchExists? → DraftFixBranchExists card.
//  6. AddIssueLabel(LabelWIP); on WorktreeAdd failure RemoveIssueLabel
//     and emit DraftFixWorktreeFail card.
//  7. SetSelectedCwd → slot.Store(ModeRemote).
//  8. Render success card.
//  9. Dispatch issue body to ChatSession.QueueUserMessage so
//     the agent picks it up. Failure here does NOT roll back
//     the worktree — the user can re-trigger manually.
func runFixRemote(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	drafts DraftsMap,
	deps HandlerDeps,
	chatID, messageID string,
	rawID string,
	force bool,
) (*Result, error) {
	issueID, err := parseIssueID(rawID)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, fmt.Sprintf("❌ %v", err)), nil
	}

	// F-XX: daemon-recovery via the worktree's git branch was
	// removed — the F-45 RebuildContext path relied on a
	// `fix/<id>-*` branch prefix which the new naming
	// convention no longer uses. Recovery now flows through the
	// BranchExists → existingPath == worktreePath check below:
	// when the user re-runs /gtw fix after a daemon restart
	// while the worktree + branch still exist, we hit the same-
	// path branch and skip creation. No separate RebuildContext
	// step is needed.

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

	// --- branch-exists decision (§5.3.1) -------------------------
	// Done BEFORE the preflight path-exists check: when the
	// branch is attached at exactly the target worktree path,
	// this is the daemon-recovery path (user re-runs /gtw fix
	// after a restart) and the worktree directory is expected
	// to exist. Preflight's "path occupied" check would
	// otherwise block the recovery. The branch-exists card
	// handles the "branch occupied elsewhere" case (where
	// preflight would also fail).
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		// F-PATHUTIL-001 §5.2: compare via pathutil.Equal instead
		// of the raw filepath.Clean equality. The Clean equality
		// is case-sensitive on Windows (so "C:\Foo" and
		// "c:\foo" spuriously miss-match) and slash-sensitive
		// (so "C:/foo" from `git rev-parse` does not equal the
		// backslash form WorktreePath produces). pathutil.Equal
		// handles both. See PreflightWorktreeCreate's
		// canonical-path logic for the parallel concern on the
		// preflight side.
		if existingPath != "" && pathutil.Equal(existingPath, worktreePath) {
			return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
				branch, worktreePath, owner+"/"+repo, repoRoot, string(providerKind), ModeRemote, issueID, issue, true /* skipDispatch */, "" /* baseSHA: re-entry skips refresh */)
		}
		return emitBranchExistsDraft(ctx, cs, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  issueID,
			Title:    issue.Title,
			Branch:   branch,
			Slug:     branch,
			Repo:     owner + "/" + repo,
			Provider: string(providerKind),
			ChatID:   chatID,
			Worktree: repoRoot,
		}, existingPath)
	}

	// --- preflight (path / branch / parent) ----------------------
	// --force skips the path-occupied branch of the preflight
	// and force-removes any leftover at the target path
	// instead. The branch-already-attached-elsewhere check is
	// deliberately kept — touching an UNRELATED worktree the
	// user is actively using in some other chat is not the
	// kind of destruction --force is meant to authorise.
	if force {
		if err := forceCleanWorktreePath(ctx, repoRoot, worktreePath, deps.Git); err != nil {
			var we *WorktreeError
			stderr := ""
			if errors.As(err, &we) {
				stderr = tailLines(we.Stderr, 10)
			}
			body := fmt.Sprintf("❌ --force cleanup failed: %v", err)
			if stderr != "" {
				body += "\n[git stderr tail]\n" + stderr
			}
			return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
		}
	} else {
		if err := PreflightWorktreeCreate(ctx, repoRoot, branch, worktreePath, deps.Git); err != nil {
			return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
		}
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
		return emitWorktreeFailDraft(ctx, cs, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  issueID,
			Title:    issue.Title,
			Branch:   branch,
			Slug:     branch,
			Repo:     owner + "/" + repo,
			Provider: string(providerKind),
			Worktree: repoRoot,
			GitError: tailLines(stderrFromWorktreeErr(err), 10),
			// LabelAdded is intentionally false here — the
			// label is applied AFTER WorktreeAdd (post-fix),
			// never before, so a WorktreeAdd failure means
			// the label was never touched. The reaction card
			// for this failure mode therefore never needs
			// to clean up a label.
			LabelAdded: false,
			ChatID:     chatID,
		})
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
	return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
		branch, worktreePath, owner+"/"+repo, repoRoot, string(providerKind), ModeRemote, issueID, issue, false, baseSHA)
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
//  4. BranchExists? → DraftFixBranchExists card (no LabelAdded
//     payload — local mode has no remote state to roll back).
//  5. WorktreeAdd; on failure emit DraftFixWorktreeFail card.
//  6. SetSelectedCwd → slot.Store(ModeLocal, Issue=-1).
//  7. Render the simplified local success card.
//
// Local mode does NOT call provider.GetIssue / AddIssueLabel /
// QueueUserMessage — the user is opting into a no-remote flow
// that should work even in repos without an `origin`.
func runFixLocal(
	ctx context.Context,
	cs *chatsession.ChatSession,
	slot ContextSlot,
	drafts DraftsMap,
	deps HandlerDeps,
	chatID, messageID string,
	rawName string,
	force bool,
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

	// --- branch-exists decision (BEFORE preflight; see ID-mode
	// runFixRemote for the rationale — preflight's "path occupied"
	// check would block the daemon-recovery path where the branch
	// is already attached at the target worktree path).
	exists, err := BranchExists(ctx, repoRoot, branch, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ git show-ref failed: %v", err)), nil
	}
	if exists {
		existingPath, _ := WorktreeListPath(ctx, repoRoot, branch, deps.Git)
		// See runFixRemote: pathutil.Equal handles Windows
		// case- and slash-insensitivity so the recovery check
		// matches regardless of which form git's porcelain output
		// emitted.
		if existingPath != "" && pathutil.Equal(existingPath, worktreePath) {
			return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
				branch, worktreePath, "", repoRoot, "", ModeLocal, -1, nil, true /* skipDispatch */, "" /* baseSHA: re-entry skips refresh */)
		}
		return emitBranchExistsDraft(ctx, cs, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  -1,
			Title:    "(local branch)",
			Branch:   branch,
			Slug:     branch,
			Repo:     "",
			ChatID:   chatID,
			Worktree: repoRoot,
		}, existingPath)
	}

	// --force: same semantics as runFixRemote. Skip the
	// path-occupied preflight, force-clean any leftover.
	if force {
		if err := forceCleanWorktreePath(ctx, repoRoot, worktreePath, deps.Git); err != nil {
			var we *WorktreeError
			stderr := ""
			if errors.As(err, &we) {
				stderr = tailLines(we.Stderr, 10)
			}
			body := fmt.Sprintf("❌ --force cleanup failed: %v", err)
			if stderr != "" {
				body += "\n[git stderr tail]\n" + stderr
			}
			return reply(ctx, cs.Emitter(), chatID, messageID, body), nil
		}
	} else if err := PreflightWorktreeCreate(ctx, repoRoot, branch, worktreePath, deps.Git); err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	if err := WorktreeAdd(ctx, repoRoot, branch, worktreePath, "HEAD", deps.Git); err != nil {
		return emitWorktreeFailDraft(ctx, cs, deps, chatID, messageID, messageID, drafts, FixDraftPayload{
			IssueID:  -1,
			Title:    "(local branch)",
			Branch:   branch,
			Slug:     branch,
			Repo:     "",
			Worktree: repoRoot,
			GitError: tailLines(stderrFromWorktreeErr(err), 10),
			ChatID:   chatID,
		})
	}

	return completeFixAndDispatch(ctx, cs, slot, deps, chatID, messageID,
		branch, worktreePath, "", repoRoot, "", ModeLocal, -1, nil, false, "" /* baseSHA: local mode doesn't refresh */)
}

// completeFixAndDispatch handles the common tail of both modes:
// switch cwd → ensure worktree .gitignore → write yml snapshot
// → store Context → render success card → dispatch the issue
// body to the agent (ID mode only). Centralising this means
// both modes share the same "after worktree is created" logic;
// the mode-specific bits stay in runFixRemote / runFixLocal.
//
// issue is non-nil only in ID mode; local mode passes nil (the
// dispatcher check at the bottom skips it). skipDispatch is true
// when the caller already decided we shouldn't dispatch (e.g.
// the branch was already attached at the target path — a re-entry
// after daemon recovery).
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
	slot ContextSlot,
	deps HandlerDeps,
	chatID, messageID, branch, worktreePath, repo, repoRoot, provider string,
	mode Mode,
	issueID int,
	issue *Issue,
	skipDispatch bool,
	baseSHA string,
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
	// state even after a daemon restart. State/UpdatedAt stay
	// in-memory only. ErrGtwYmlExists is the daemon-recovery path
	// (we just re-entered completeFixAndDispatch for a worktree
	// that already has a yml) — silently skip. Any other error
	// is warn-only: the worktree is the durable side effect and
	// the user can manually finish or recover via /gtw close.
	now := deps.Now()
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

	// --- write gtwContext (§5.2.⑤) -------------------------------
	slot.Store(Context{
		Mode:      mode,
		Issue:     issueID,
		Branch:    branch,
		Worktree:  worktreePath,
		RepoRoot:  repoRoot,
		Repo:      repo,
		Provider:  provider,
		State:     StateFixing,
		UpdatedAt: now,
	})

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
		card = renderFixSuccessCard(issue, branch, worktreePath, repo, baseSHA)
	}
	result := reply(ctx, cs.Emitter(), chatID, messageID, card)

	// --- dispatch issue to agent (ID mode only) -------------------
	// We do this AFTER the reply so the user sees the success
	// card immediately even if dispatch stalls / fails. Failures
	// here are warn-only — the worktree is the durable side
	// effect; a failed dispatch can be retried by the user
	// re-running /gtw fix or by manually sending the issue
	// reference to the agent.
	if mode == ModeRemote && !skipDispatch && issue != nil {
		// Download attachments (best-effort) and assemble the
		// dispatch blocks. Download failures log a warning
		// and continue with text-only — the agent still gets
		// the issue body, just no files.
		var attachmentBlocks []agent.ContentBlock
		if len(issue.Attachments) > 0 {
			dir := attachmentsDir(worktreePath, issueID)
			attachmentBlocks = downloadAttachmentsBestEffort(ctx, issue.Attachments, dir)
		}
		blocks := buildIssueDispatchBlocks(issue, attachmentBlocks, branch, repo)
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
// (zero or more ContentFile). The caller downloads
// attachments before passing them in.
func buildIssueDispatchBlocks(issue *Issue, attachmentBlocks []agent.ContentBlock, branch, repo string) []agent.ContentBlock {
	text := buildIssueDispatchText(issue, branch, repo)
	blocks := make([]agent.ContentBlock, 0, 1+len(attachmentBlocks))
	blocks = append(blocks, agent.ContentBlock{Type: agent.ContentText, Text: text})
	blocks = append(blocks, attachmentBlocks...)
	return blocks
}

// buildIssueDispatchText formats the issue as a fixed-template
// markdown block. The template lives here (rather than in the
// renderer) because it's the "agent prompt" not the "user-facing
// success card". The user never sees this body directly — they
// see the success card; the agent sees this block.
//
// Section order is stable so agent prompts can rely on it:
// header / metadata / body / attachments / closing.
func buildIssueDispatchText(issue *Issue, branch, repo string) string {
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
	if len(issue.Attachments) > 0 {
		fmt.Fprintf(&b, "## Attachments\n%d file(s) attached as ContentFile blocks — paths are absolute on this machine.\n\n",
			len(issue.Attachments))
	}
	b.WriteString("## Task\n")
	b.WriteString("Please investigate the issue above and implement a fix on the branch noted. ")
	b.WriteString("The worktree is already prepared; reply when you have a plan, then proceed.")
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

func emitBranchExistsDraft(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	drafts DraftsMap,
	payload FixDraftPayload,
	existingPath string,
) (*Result, error) {
	card := BranchExistsChoice(payload, existingPath)
	return sendDraft(ctx, cs, deps, chatID, messageID, userMsgID, card, drafts, DraftFixBranchExists, payload)
}

func emitWorktreeFailDraft(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	drafts DraftsMap,
	payload FixDraftPayload,
) (*Result, error) {
	card := WorktreeFailChoice(payload)
	return sendDraft(ctx, cs, deps, chatID, messageID, userMsgID, card, drafts, DraftFixWorktreeFail, payload)
}

func sendDraft(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID, userMsgID string,
	card Choice,
	drafts DraftsMap,
	kind DraftKind,
	payload FixDraftPayload,
) (*Result, error) {
	requestID := "gtw-fix-" + userMsgID
	if userMsgID == "" {
		requestID = "gtw-fix-" + payload.Branch
	}
	card.RequestID = requestID

	em := cs.Emitter()
	cardPosted := false
	if em != nil {
		if err := em.Send(ctx, messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutChoice,
			Choice: gtwChoiceToGateway(card),
		}); err == nil {
			cardPosted = true
		} else {
			// Card path failed: fall through to markdown so the
			// user still sees the decision content. Follow-up
			// after click will be plain text (ChoicePosted=false).
			_ = replyAgent(ctx, em, chatID, messageID,
				renderChoiceMarkdown(card), "", agent.RunResult{})
		}
	}

	drafts.Store(requestID, &Draft{
		Kind:            kind,
		Payload:         payload,
		CreatedAt:       deps.Now(),
		ChoicePosted:    cardPosted,
		ChoiceTitle:     card.Title,
		ChoiceBody:      card.Body,
		ChoiceOptions:   card.Options,
		ChoiceRequestID: requestID,
	})
	return &Result{Consumed: true}, nil
}

// toChatsessionChoiceOptions was removed in F-51: the gtw package
// now owns ChoiceOption directly (no chatsession alias needed).
// The renderer stores card.Options verbatim on the draft.

// renderChoiceMarkdown flattens a Choice back to plain markdown for
// legacy channels that don't support interactive choice prompts (Feishu
// Web in some configs, Slack, etc.). The shape mirrors the F-45
// plain-text decision cards so the user's view is unchanged.
func renderChoiceMarkdown(c Choice) string {
	var b strings.Builder
	if c.Title != "" {
		b.WriteString(c.Title)
		b.WriteString("\n")
	}
	if c.Body != "" {
		b.WriteString(c.Body)
		b.WriteString("\n")
	}
	if len(c.Options) > 0 {
		b.WriteString("\n选择操作(反应对应 emoji):\n")
		for _, ch := range c.Options {
			label := ch.Label
			if ch.Emoji != "" {
				label = ch.Emoji + " " + label
			}
			b.WriteString("  ")
			b.WriteString(label)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// gtwChoiceToGateway translates gtw.Choice (business view) to the
// wire-level messages.Choice. Kind is always ChoiceKindDecision.
func gtwChoiceToGateway(in Choice) *messages.Choice {
	opts := in.Options
	if opts != nil {
		opts = append([]messages.ChoiceOption(nil), opts...)
	}
	return &messages.Choice{
		Kind:      messages.ChoiceKindDecision,
		Title:     in.Title,
		Body:      in.Body,
		Options:   opts,
		RequestID: in.RequestID,
	}
}

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
func reply(ctx context.Context, em outbound.Emitter, chatID, messageID, text string) *Result {
	return replyAgent(ctx, em, chatID, messageID, text, "", agent.RunResult{})
}
