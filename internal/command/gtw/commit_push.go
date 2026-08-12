package gtw

import (
	"context"
	"fmt"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// RunOnceTimeout is the hard deadline for a one-shot agent call
// used by both /gtw commit (F-XX split: was Branch 2 of push) and
// /gtw pr. 5 minutes covers realistic agent commits (lint fixes,
// conflict resolution, multi-tool flows) and PR-body generation
// without wedging the dispatcher if an agent hangs (e.g. PTY
// fallback with no idle signal — see pty.RunOnce's ptyIdleTimeout
// for the per-call short-window heuristic).
//
// Exported because both /gtw commit and /gtw pr use it.
const RunOnceTimeout = 5 * time.Minute

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
	snap, err := CollectReadiness(ctx, c.Worktree, deps.Git)
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
	// dispatchPR PRBlockReason case 1 (the two gates share the
	// same readiness snapshot, so the policy stays consistent).
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

	// 3. Capture headBefore for the success card's rev-range. We
	// capture it here (after the readiness gate, so we know the
	// worktree is clean + on a real branch) rather than at entry
	// (where a dirty worktree would have made the captured SHA
	// useless for the post-push log query).
	headBefore, err := headSHA(ctx, c.Worktree, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ read HEAD: %v", err)), nil
	}

	// 4. Push with retry + post-push verification
	// (countUnpushed==0).
	if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
		// err.Error() is already a complete IM-friendly message
		// (per F-56 §4.3 design). Paste it straight in.
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	// 5. Build the success card from git log — NOT from agent
	// prose. revRange is `headBefore..origin/<branch>` so the
	// card lists exactly the commits this push just landed. A
	// failure here is a hard error: the push succeeded but we
	// can't render the result. Surface the failure rather than
	// fudging a "pushed 0 commit(s)" card.
	card, err := replyPushSuccessCard(ctx, c,
		headBefore+"..origin/"+c.Branch, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ push succeeded but couldn't render card: %v", err)), nil
	}
	return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
}
