package gtw

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// dispatchPush is the dispatcher for /gtw push. After the F-XX
// commit/push split, this dispatcher is push-only: it does NOT
// spawn an agent and does NOT run Branch 2's "agent commits then
// we push" fall-through. The agent path lives in dispatchCommit.
//
// Two branches remain:
//
//   - Branch 1 (no-op): snap.HasNothingToPush() — clean tree,
//     upstream exists, ahead=0.
//   - Branch 3 (push): everything else that reaches push — clean
//     tree + (upstream missing OR ahead > 0). The "upstream
//     missing" case is the first-push path; programmaticPush
//     runs `git push -u origin <branch>`.
//
// `Branch 2` (dirty → agent commits → re-snapshot → push) no
// longer exists here. Users running /gtw push on a dirty
// worktree see a "commit first" guidance message and exit
// without spawning an agent or pushing. The split makes the
// agent's failure modes (HEAD-not-advanced, worktree-still-dirty,
// agent-introduced-conflict) impossible to surface inside push's
// IM reply — they belong to /gtw commit where they actually
// happen.
//
// Reply is sent inline via cs.Emitter(); return value is the
// runtime's *Result carrying Consumed / Dropped only.
func dispatchPush(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	_ pushArgs,
) (*Result, error) {
	c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}

	// F-57: single readiness snapshot. Replaces the old
	// (statusOut → detectConflicts) + (isClean string-compare) +
	// countUnpushed triple-probe with one git status call. The
	// snap is the same one /gtw pr and /gtw commit use, so
	// the gates read the same truth (continuity is structural
	// — see F-57 §5).
	snap, err := CollectReadinessForDispatch(ctx, c.Worktree, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ read worktree status: %v", err)), nil
	}
	if snap == nil {
		// (nil, nil) from CollectReadiness = not a git repo, empty
		// repo, or git error. None of these are states we can push
		// from. Surface a clear refusal rather than silently
		// continuing.
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ cannot read worktree git status — refusing to push\n"+
				"hint: ensure the worktree is inside a git repo with at least one commit"), nil
	}

	// 1. Hard-refuse conflicts. Pushing an unresolved state would
	// land a broken tip on origin — no override.
	if reason := snap.PushBlockReason(); reason != "" {
		return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
	}

	// 1b. Refuse detached HEAD. Pushing from detached HEAD with
	// `git push -u origin HEAD` either fails with "no upstream
	// branch" or — worse — lands an anonymous ref on origin. The
	// user should checkout a named branch first. Mirrors the
	// dispatchPR's pre-refactor "snap.Branch == \"\"" early-return
	// (the two gates share the same readiness snapshot, so the
	// policy stays consistent).
	if snap.Branch == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ detached HEAD — checkout a named branch first"), nil
	}

	// 1c. Refuse dirty worktree. After the F-XX commit/push split,
	// /gtw push no longer commits on the user's behalf. A dirty
	// worktree here means the user either forgot to commit or
	// intentionally wants to commit later; either way, push is
	// the wrong next step. Surface a "commit first" guidance.
	if !snap.WorkingTreeIsClean() {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ worktree is dirty — /gtw push no longer auto-commits\n"+
				"hint: run `/gtw commit` first, then `/gtw push`"), nil
	}

	// 2. Nothing to do. Note: a branch with no upstream at all
	// deliberately returns HasNothingToPush=false here (the
	// branch was never published; programmaticPush handles the
	// first push).
	if snap.HasNothingToPush() {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"ℹ️ nothing to push\n"+
				"  no uncommitted changes\n"+
				"  no unpushed commits on "+snap.Branch), nil
	}

	// 3. Capture originBefore (the previous origin/<branch> tip)
	// BEFORE the push, so the success card can list exactly the
	// commits that just landed. EMPTY when the branch has never
	// been pushed (first-push path); in that case `git push -u`
	// is what establishes origin/<branch>.
	//
	// Captured here (after the readiness gate, so we know the
	// worktree is clean + on a real branch) rather than at entry
	// (where a dirty worktree would have made the captured SHA
	// useless for the post-push log query).
	originBefore, err := originBranchSHA(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ read origin/%s: %v", c.Branch, err)), nil
	}

	// 4. Push with retry + post-push verification
	// (countUnpushed==0).
	if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
		// err.Error() is already a complete IM-friendly message
		// (per F-56 §4.3 design). Paste it straight in.
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	// 5. Build the success card from git log — NOT from agent
	// prose. The rev range MUST be `originBefore..origin/<branch>`
	// so the card lists exactly the commits this push just landed.
	//
	// Historical trap (the bug behind the "pushed 0 commit(s)"
	// behaviour the Feishu card was showing): the previous shape
	// was `headBefore..origin/<branch>`. After a successful push
	// origin/<branch> == local tip == headBefore, so that range
	// collapses to empty and the card lies about having pushed
	// nothing. Using the pre-push origin tip as the left side
	// avoids the collapse.
	//
	// First-push case: originBefore is "" because origin/<branch>
	// didn't exist before. Anchoring to `c.Branch` alone is a trap
	// — if the branch was forked from `main`, that range lists
	// every commit reachable from `<branch>`, i.e. main's entire
	// history plus the new commits. Better: anchor to
	// `origin/<default>..origin/<branch>` so we list ONLY the
	// commits this push landed. Falls back to `c.Branch` when
	// origin/<default> doesn't exist (brand-new repo) or when the
	// branch being pushed IS the default (pushing main itself for
	// the first time uploads everything reachable).
	var revRange string
	if originBefore != "" {
		revRange = originBefore + "..origin/" + c.Branch
	} else {
		revRange = firstPushRevRange(ctx, c, deps)
	}
	card, err := replyPushSuccessCard(ctx, c, revRange, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ push succeeded but couldn't render card: %v", err)), nil
	}
	return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
}
