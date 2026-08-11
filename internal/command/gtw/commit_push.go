package gtw

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// RunOnceTimeout is the hard deadline for a one-shot agent
// call (Branch 2 of /gtw push, plus /gtw pr's one-shot). 5
// minutes covers realistic agent commits (lint fixes, conflict
// resolution, multi-tool flows) without wedging /gtw push if
// an agent hangs (e.g. PTY fallback with no idle signal — see
// pty.RunOnce's ptyIdleTimeout for the per-call short-window
// heuristic).
//
// Exported because both /gtw push and /gtw pr use it.
const RunOnceTimeout = 5 * time.Minute

// dispatchPush is the dispatcher for /gtw push, layered on top of
// the F-57 Readiness gate. The structural shape is still three
// branches (no-op / agent+push / push-only), but the entry
// gating moved from a triple git-probe (status + detectConflicts
// + countUnpushed) to a single CollectReadiness snapshot read
// from messages.GitStatusSnapshot predicates:
//
//   - Branch 1 (no-op):  snap.HasNothingToPush() — clean tree,
//     upstream exists, ahead=0.
//   - Branch 2 (agent+push): snap.WorkingTreeIsClean() == false
//     — worktree dirty, agent runs to commit, then we re-snapshot
//     and verify.
//   - Branch 3 (push-only): everything else that reaches push
//     — clean tree + (upstream missing OR ahead > 0). The
//     "upstream missing" case is the first-push path; programmaticPush
//     runs `git push -u origin <branch>`.
//
// Agent is only spawned in Branch 2; the push is owned by nightme
// in Branches 2 and 3; Branch 1 just notifies the user and exits.
// Reply is sent inline via cs.Emitter(); return value is the
// runtime's *Result carrying Consumed / Dropped only.
func dispatchPush(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	args pushArgs,
	ymlAgent string,
) (*Result, error) {
	c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}

	// F-57: single readiness snapshot. Replaces the old
	// (statusOut → detectConflicts) + (isClean string-compare) +
	// countUnpushed triple-probe with one git status call. The
	// snap is the same one /gtw pr uses, so the two gates read
	// the same truth (continuity is structural — see F-57 §5).
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

	// headBefore is required by both Branch 2's verifyAgentCommitted
	// and the success card's revRange. Silently swallowing the error
	// would cause the verify to skip its HEAD-advance check (the
	// very check that catches today's bug) AND the success card
	// to render an invalid `..origin/branch` range. Surface it.
	headBefore, err := headSHA(ctx, c.Worktree, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ read HEAD: %v", err)), nil
	}

	// ── Branch 2: dirty → agent commits → re-snapshot → push ──
	agentName := ""
	if !snap.WorkingTreeIsClean() {
		name, errMsg := runAgentToCommit(ctx, cs, c, args, ymlAgent)
		if errMsg != "" {
			return reply(ctx, cs.Emitter(), chatID, messageID, errMsg), nil
		}
		agentName = name

		// Verify the agent actually committed (HEAD advance +
		// worktree clean + branch still on c.Branch). If any
		// check fails, surface the diagnostic and DO NOT push.
		if msg := verifyAgentCommitted(ctx, deps, c, headBefore); msg != "" {
			return reply(ctx, cs.Emitter(), chatID, messageID, msg), nil
		}

		// F-57: re-snapshot after the agent runs. We cannot trust
		// the original snap — the agent may have introduced a
		// conflict (rare but documented), or the verify may have
		// passed but the tree still report something we need to
		// re-decide on. This is the only correct place to ask
		// "should I push now?".
		snap, err = CollectReadiness(ctx, c.Worktree, deps.Git)
		if err != nil {
			return reply(ctx, cs.Emitter(), chatID, messageID,
				fmt.Sprintf("❌ re-read worktree status after agent: %v", err)), nil
		}
		if snap == nil {
			// Same contract as the entry path (line 68): CollectReadiness
			// returns (nil, nil) on git error / empty repo — neither state
			// is one we can push from. Surface a refusal rather than
			// nil-derefing on the next snap.* call.
			return reply(ctx, cs.Emitter(), chatID, messageID,
				"❌ cannot read worktree git status — refusing to push\n"+
					"hint: ensure the worktree is inside a git repo with at least one commit"), nil
		}
		// Re-check conflict gate. Pushing must never happen if the
		// agent left unmerged entries.
		if reason := snap.PushBlockReason(); reason != "" {
			return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
		}
	}

	// ── Branch 3 (or Branch 2 续): worktree is clean, push ──
	// Defensive: if the agent ran but produced no new commits
	// (e.g. HEAD advance check passed but AheadOfRemote is still
	// 0 because the agent's commit was already on origin), surface
	// a warning rather than calling push with nothing to do.
	if snap.HasNothingToPush() {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"⚠️ worktree is clean but nothing to push.\n"+
				"hint: inspect the worktree's HEAD vs origin/"+snap.Branch+" manually."), nil
	}

	if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
		// err.Error() is already a complete IM-friendly message
		// (per F-56 §4.3 design). Paste it straight in.
		return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
	}

	// Build the success card from git log — NOT from agent prose.
	// revRange is `headBefore..origin/<branch>` so the card lists
	// exactly the commits this push just landed. A failure here
	// is a hard error: the push succeeded but we can't render
	// the result. Surface the failure rather than fudging a
	// "pushed 0 commit(s)" card.
	card, err := replySuccessCard(ctx, c, agentName,
		headBefore+"..origin/"+c.Branch, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ push succeeded but couldn't render card: %v", err)), nil
	}
	return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
}

// runAgentToCommit spawns the configured one-shot agent and
// runs the slim buildAgentPrompt against it. The agent's prose
// text is intentionally ignored — verification reads git state
// (verifyAgentCommitted) before deciding anything succeeded.
//
// Returns (name, "") on success, ("", errMsg) on any failure
// that should short-circuit dispatchPush (no agent selected,
// unknown agent, binary missing, RunOnce errored).
//
// Agent selection (per F-104 / wip/gtw-hooks.md): CLI -a flag
// wins, then yml <cmd>.agent, then chat's currently Selected
// Agent. The yml-loader lives in cmd.go's runPush wrapper (so
// loadNotes can be surfaced to the user); runAgentToCommit
// receives the already-resolved ymlAgent string.
func runAgentToCommit(
	ctx context.Context,
	cs *chatsession.ChatSession,
	c Context,
	args pushArgs,
	ymlAgent string,
) (string, string) {
	agentName, agentNotes := ResolveAgent(args.Agent, ymlAgent, cs)
	if agentName == "" {
		// B2: surface agentNotes so a yml pointing at an unknown
		// agent doesn't silently degrade to a generic "no agent
		// selected" reply.
		var msg strings.Builder
		msg.WriteString("❌ no agent selected. Send `/use <name>` first or pass `-a <name>`.")
		for _, n := range agentNotes {
			msg.WriteByte('\n')
			msg.WriteString(n)
		}
		return "", msg.String()
	}

	a, err := agent.Builtins.Get(agentName)
	if err != nil {
		return "", fmt.Sprintf("❌ unknown agent %q (check `nightme agents` or your config)", agentName)
	}
	if err := a.Detect(); err != nil {
		return "", fmt.Sprintf("❌ agent %s not available: %v", agentName, err)
	}

	ctx, cancel := context.WithTimeout(ctx, RunOnceTimeout)
	defer cancel()

	blocks := []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: buildAgentPrompt(c),
	}}
	// RunOnce's text return value is intentionally discarded.
	// Per F-56 §1.2, git state — not agent prose — is the
	// source of truth.
	_, err = a.RunOnce(ctx,
		agent.StartConfig{Workspace: c.Worktree},
		blocks,
	)
	if err != nil {
		return agentName, fmt.Sprintf("❌ agent %s failed: %v", agentName, err)
	}
	return agentName, ""
}

// verifyAgentCommitted is the post-agent sanity check that runs
// in Branch 2 of dispatchPush. Per F-56 §4.1, it ONLY validates
// the agent's commit work — never the push (that's
// programmaticPushWithRetry's job).
//
// Three checks, in order:
//
//  1. branch: agent must still be on c.Branch. If the agent
//     `git checkout`'d to a side branch and committed there, the
//     worktree is clean and HEAD is different from headBefore,
//     but the commits will never land on c.Branch — we MUST
//     refuse here, not at push time.
//
//  2. worktree clean: status --porcelain must be empty after
//     the agent. If files remain uncommitted, the agent skipped
//     them (deliberately or not). We surface the file list and
//     refuse to push.
//
//  3. HEAD advanced: headAfter must differ from headBefore.
//     Catches the false-success class (today's bug) where the
//     worktree became clean via stash/restore (not via commit).
//
// Returns "" when all three pass; a complete IM-friendly
// diagnostic string otherwise (the caller pastes it straight
// into the reply).
//
// headBefore is REQUIRED. The dispatcher (dispatchPush) always
// captures it before spawning the agent and aborts if the
// capture itself fails. If this function is ever called with
// headBefore == "" it returns an internal-error diagnostic
// rather than skipping the HEAD-advance check — the false-success
// class is the exact thing this check exists to catch, so it
// cannot be silently disabled.
func verifyAgentCommitted(ctx context.Context, deps HandlerDeps, c Context, headBefore string) string {
	if headBefore == "" {
		return "⚠️ internal error: headBefore not captured; cannot verify commits.\n" +
			"hint: this should not happen — please file a bug."
	}
	// The 3 checks below are read-only post-mortem: they assume
	// the agent's RunOnce has already returned and the agent
	// process is no longer mutating the worktree. This is
	// guaranteed by RunOnce's contract (returns when the agent
	// emits its final event and the process is reaped by the
	// caller's defer a.Close() — see agent/runonce.go).
	// (1) Branch check.
	branchAfter, err := currentBranch(ctx, c.Worktree, deps)
	if err != nil {
		return fmt.Sprintf("⚠️ agent finished but failed to read current branch: %v", err)
	}
	if branchAfter != c.Branch {
		return fmt.Sprintf(
			"⚠️ agent finished but HEAD is on %q, expected %q.\n"+
				"hint: the agent may have `git checkout`'d to a side branch and committed there. "+
				"Re-run /gtw push from %s, or merge the side branch back into %s manually.",
			branchAfter, c.Branch, c.Worktree, c.Branch)
	}

	// (2) Worktree clean check.
	uncommitted, err := listUncommittedFiles(ctx, c.Worktree, deps)
	if err != nil {
		return fmt.Sprintf("⚠️ agent finished but failed to verify clean state: %v", err)
	}
	if len(uncommitted) > 0 {
		return fmt.Sprintf(
			"⚠️ agent finished but %d file(s) are still uncommitted in %s:\n"+
				"  %s\n"+
				"hint: commit them manually, or re-run /gtw push to retry.\n"+
				"  (the agent was given a chance to commit them — re-invoking it automatically won't necessarily help)",
			len(uncommitted), c.Worktree,
			strings.Join(uncommitted, "\n  "))
	}

	// (3) HEAD advance check. headBefore is required (checked
	// at function entry); no `if headBefore != ""` skip here.
	headAfter, err := headSHA(ctx, c.Worktree, deps)
	if err != nil {
		return fmt.Sprintf("⚠️ agent finished but failed to read HEAD: %v", err)
	}
	if headAfter == headBefore {
		return fmt.Sprintf(
			"⚠️ agent finished but no new commit was created (HEAD unchanged at %s).\n"+
				"hint: the worktree was dirty before the agent and is clean now, but git has no record of a commit. "+
				"Re-run /gtw push to retry, or inspect the worktree manually.",
			shortSHA(headBefore))
	}

	return ""
}

// buildAgentPrompt renders the single text block the agent
// receives. Per F-56 §3, the prompt is intentionally minimal —
// only role + task + 3 hard rules. No reference to nightme's
// verification / push / IM card generation. The agent is a
// "release engineer" who only commits; push, push retry, and
// the success card are all owned by nightme's code, derived
// from git state.
//
// F-56 §3.3 enumerates the 3 rules and the rationale for each
// (don't push / don't restore-or-stash / no `git add -A`).
// Everything else is left to the agent's judgment.
func buildAgentPrompt(c Context) string {
	var sb strings.Builder
	sb.WriteString("You are a release engineer.\n\n")

	fmt.Fprintf(&sb,
		"The user has uncommitted work on branch %s in %s",
		c.Branch, c.Worktree)
	if c.Issue > 0 {
		fmt.Fprintf(&sb, " for issue #%d", c.Issue)
	}
	sb.WriteString(". They need it committed to local git.\n\n")

	sb.WriteString("Group the changes by relevance — different concerns go in\n")
	sb.WriteString("different commits, related changes go together. Use\n")
	sb.WriteString("Conventional Commits for each:\n\n")
	sb.WriteString("  <type>(<scope>): <subject>\n")
	sb.WriteString("  types: feat, fix, chore, refactor, docs, test, build,\n")
	sb.WriteString("         ci, perf, style, revert\n")
	sb.WriteString("  subject: ≤72 chars, imperative, no trailing period\n")
	sb.WriteString("  body: WHY, wrapped at 72  [<Issue: #N>] if applicable.\n\n")

	sb.WriteString("Rules:\n")
	sb.WriteString("- Do not push. Push is the user's decision, not yours;\n")
	sb.WriteString("  never run `git push`.\n")
	sb.WriteString("- Do not revert, restore, or stash the user's work.\n")
	sb.WriteString("- `git add <specific files>`, not `git add -A`.\n")

	return sb.String()
}
