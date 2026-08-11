package gtw

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// dispatchCommit runs /gtw commit — single-purpose: take the
// worktree's dirty state, dispatch the configured one-shot agent
// to commit, verify the agent actually committed. Does NOT push.
//
// F-XX (commit/push split): this dispatcher owns the Branch-2
// half of the legacy `/gtw push` flow. Push's Branch-2 used to
// re-snapshot after the agent ran so the same dispatch could
// fall through into Branch 3's programmaticPush; that
// re-snapshot + fall-through is gone — commit ends here, the
// user runs /gtw push separately to actually push.
//
// The flow:
//
//	1. CollectReadiness (single source of truth, F-57).
//	2. Hard-refuse conflicts (a commit on an unresolved
//	   state still lands broken history locally — even though
//	   push would catch it, refuse early so the user fixes
//	   intent).
//	3. Refuse detached HEAD.
//	4. No-op if worktree is already clean.
//	5. Capture headBefore.
//	6. Run the configured one-shot agent; verify HEAD advanced,
//	   worktree is clean, branch matches c.Branch.
//	7. Re-snapshot and refuse if the agent introduced conflicts
//	   (rare but documented).
//	8. Render the commit success card from git log.
//
// Agent selection (CLI -a > yml cfg.Commit.Agent >
// cs.SelectedAgent()) is handled inside runAgentToCommit's
// ResolveAgent call. The yml-loader lives in cmd.go's runCommit
// wrapper so loadNotes (ymalformed yml / read errors) ride along
// in the consolidated reply per wip/gtw-hooks.md.
func dispatchCommit(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	args commitArgs,
	ymlAgent string,
) (*Result, error) {
	c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
	if res != nil {
		return res, nil
	}

	// F-57: same single readiness snapshot push uses. Sharing the
	// snapshot across /gtw commit and /gtw push keeps the two
	// gates reading the same truth.
	snap, err := CollectReadiness(ctx, c.Worktree, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ read worktree status: %v", err)), nil
	}
	if snap == nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ cannot read worktree git status — refusing to commit\n"+
				"hint: ensure the worktree is inside a git repo with at least one commit"), nil
	}

	// 1. Hard-refuse conflicts. Even if commit doesn't push, an
	// unmerged state means the worktree isn't coherent; the user
	// should resolve (or `git rebase --abort`) first.
	if reason := snap.PushBlockReason(); reason != "" {
		return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
	}

	// 1b. Refuse detached HEAD. Mirrors dispatchPush PRBlockReason
	// case 1 — keep the two gates' policy symmetrical.
	if snap.Branch == "" {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ detached HEAD — checkout a named branch first"), nil
	}

	// 2. Nothing to do. Distinct message from push's
	// "nothing to push" so the user sees which axis they're
	// short on.
	if snap.WorkingTreeIsClean() {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"ℹ️ nothing to commit\n"+
				"  no uncommitted changes on "+snap.Branch), nil
	}

	// 3. headBefore is required by verifyAgentCommitted's
	// HEAD-advance check. Silent swallow would skip the very
	// check that catches today's "agent claimed success but
	// didn't commit" class. Surface the read error.
	headBefore, err := headSHA(ctx, c.Worktree, deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ read HEAD: %v", err)), nil
	}

	// 4. Run agent.
	agentName, errMsg := runAgentToCommit(ctx, cs, c, args.Agent, ymlAgent)
	if errMsg != "" {
		return reply(ctx, cs.Emitter(), chatID, messageID, errMsg), nil
	}

	// 5. Verify the agent actually committed (HEAD advance +
	// worktree clean + branch still on c.Branch).
	if msg := verifyAgentCommitted(ctx, deps, c, headBefore); msg != "" {
		return reply(ctx, cs.Emitter(), chatID, messageID, msg), nil
	}

	// 6. Re-snapshot. Rare but documented: an agent can produce
	// a working-tree clean + HEAD-advanced state via stash/restore
	// (the false-success class verify caught), or — much rarer —
	// a state where the commit landed but the agent's tooling
	// added a conflict entry. We cannot trust the original snap.
	snap, err = CollectReadiness(ctx, c.Worktree, deps.Git)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ re-read worktree status after agent: %v", err)), nil
	}
	if snap == nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			"❌ cannot read worktree git status — refusing to commit\n"+
				"hint: ensure the worktree is inside a git repo with at least one commit"), nil
	}
	if reason := snap.PushBlockReason(); reason != "" {
		return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
	}

	// 7. Build the success card from git log — NOT from agent
	// prose. revRange is `headBefore..HEAD` because we're
	// describing the local commit(s), not anything that landed
	// on origin. A failure here is a hard error: the commit
	// landed but we can't render the result. Surface the
	// failure rather than fudging a "committed 0 change(s)" card.
	card, err := replyCommitSuccessCard(ctx, c, agentName,
		headBefore+"..HEAD", deps)
	if err != nil {
		return reply(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ commit landed but couldn't render card: %v", err)), nil
	}
	return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
}

// runAgentToCommit spawns the configured one-shot agent and
// runs the slim buildAgentPrompt against it. The agent's prose
// text is intentionally ignored — verification reads git state
// (verifyAgentCommitted) before deciding anything succeeded.
//
// Returns (name, "") on success, ("", errMsg) on any failure
// that should short-circuit dispatchCommit (no agent selected,
// unknown agent, binary missing, RunOnce errored).
//
// Agent selection (per F-104 / wip/gtw-hooks.md): CLI -a flag
// wins, then yml <commit>.agent, then chat's currently Selected
// Agent. The yml-loader lives in cmd.go's runCommit wrapper (so
// loadNotes can be surfaced to the user); runAgentToCommit
// receives the already-resolved ymlAgent string.
//
// The signature accepts the per-invocation `cliAgent` separately
// (rather than passing a fully-parsed `commitArgs`) so the
// dispatcher can also accept agents resolved from a future
// non-CLI source (e.g. /gtw commit --interactive) without
// reshaping the helper.
func runAgentToCommit(
	ctx context.Context,
	cs *chatsession.ChatSession,
	c Context,
	cliAgent, ymlAgent string,
) (string, string) {
	agentName, agentNotes := ResolveAgent(cliAgent, ymlAgent, cs)
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
// in dispatchCommit. Per F-56 §4.1, it ONLY validates the
// agent's commit work — never the push (push, if the user runs
// /gtw push next, is programmaticPushWithRetry's job).
//
// Three checks, in order:
//
//  1. branch: agent must still be on c.Branch. If the agent
//     `git checkout`'d to a side branch and committed there, the
//     worktree is clean and HEAD is different from headBefore,
//     but the commits will never reach c.Branch — we MUST
//     refuse here.
//  2. worktree clean: status --porcelain must be empty after
//     the agent. If files remain uncommitted, the agent skipped
//     them (deliberately or not). We surface the file list and
//     refuse.
//  3. HEAD advanced: headAfter must differ from headBefore.
//     Catches the false-success class (today's bug) where the
//     worktree became clean via stash/restore (not via commit).
//
// Returns "" when all three pass; a complete IM-friendly
// diagnostic string otherwise (the caller pastes it straight
// into the reply).
//
// headBefore is REQUIRED. The dispatcher (dispatchCommit) always
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
				"Re-run /gtw commit from %s, or merge the side branch back into %s manually.",
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
				"hint: commit them manually, or re-run /gtw commit to retry.\n"+
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
				"Re-run /gtw commit to retry, or inspect the worktree manually.",
			shortSHA(headBefore))
	}

	return ""
}

// buildAgentPrompt renders the single text block the agent
// receives. Per F-56 §3, the prompt is intentionally minimal —
// only role + task + 3 hard rules. No reference to nightme's
// verification / push / IM card generation. The agent is a
// "release engineer" who only commits; push and the success
// card are all owned by nightme's code, derived from git state.
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
