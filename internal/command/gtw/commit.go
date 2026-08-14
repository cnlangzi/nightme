package gtw

import (
	"context"
	"fmt"
	"strings"

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
//  1. CollectReadiness (single source of truth, F-57).
//  2. Hard-refuse conflicts (a commit on an unresolved
//     state still lands broken history locally — even though
//     push would catch it, refuse early so the user fixes
//     intent).
//  3. Refuse detached HEAD.
//  4. No-op if worktree is already clean.
//  5. Capture headBefore.
//  6. Run the configured one-shot agent; verify HEAD advanced,
//     worktree is clean, branch matches c.Branch.
//  7. Re-snapshot and refuse if the agent introduced conflicts
//     (rare but documented).
//  8. Render the commit success card from git log.
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

	// 4. Run agent. runAgentFor returns the full RunResult so the
	// success-path reply can stamp Model / SessionID / Usage onto
	// the OutboundMessage — see replyAgent's doc for the footer
	// stamping rationale (F-CLAUDE-PRINT-002 follow-up: agentbar /
	// usagebar must reach the channel even when GTW bypasses the
	// runtime event pipeline).
	ctx, cancel := context.WithTimeout(ctx, RunOnceTimeout)
	defer cancel()
	runRes, agentName, err := runAgentFor(ctx, cs, c.Worktree,
		buildAgentPrompt(c), args.Agent, ymlAgent)
	if err != nil {
		return replyAgent(ctx, cs.Emitter(), chatID, messageID,
			err.Error(), agentName, runRes), nil
	}

	// 4b. F-CLAUDE-PRINT-002: refresh chatsession.GitStatus
	// post-RunOnce. The agent just landed a commit; the cached
	// snapshot is now stale. Without this, the success card
	// footer would show the pre-commit HEAD/snapshot.
	// RefreshGitStatus is a no-op when the chatsession has no
	// GitStatusDeps wired (test fixtures, unit tests).
	_ = cs.RefreshGitStatus(ctx, deps.GitStatusDeps)

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
	// Success path: forward runRes so the footer (agentbar +
	// usagebar) renders. Failure paths above stay on the no-stamp
	// reply — the user just got an error message, not an agent
	// result, so footer metadata isn't applicable.
	return replyAgent(ctx, cs.Emitter(), chatID, messageID,
		card, agentName, runRes), nil
}

// runAgentFor (in agent_reply.go) is the shared one-shot agent
// invoker used by both /gtw commit and /gtw pr. Commit passes
// buildAgentPrompt(c) as the prompt and c.Worktree as the workspace.
// The returned RunResult is forwarded to replyAgent on the
// success path so the footer (agentbar / usagebar) renders.

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
				"\n"+
				"💡 hint: commit them manually, or re-run /gtw commit to retry.",
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
// receives for /gtw commit. Per F-56 §3, the prompt is the only
// lever between the agent's writing instinct and the commit log:
// nightme derives everything downstream (push status, IM card,
// PR body synthesis) from git state, so what the agent writes
// here IS what users see in `git log`.
//
// Design rationale (v2, prompt-engineering lens — mirrors the
// v2 buildPRPrompt rewrite in pr.go):
//
//   - Tool floor: v1 said "group by relevance" but never told the
//     agent to RUN `git status` + `git diff` first. The agent
//     would scan file names, guess relevance, and commit without
//     looking at the actual changes — same failure mode as the
//     v1 PR prompt (regression pattern: skip tools → write from
//     memory → modal pattern wins). v2 makes the inspection
//     mandatory and lists the four commands whose output the
//     LLM must read before staging.
//
//   - Split anchors: "different concerns go in different commits"
//     is an empty anchor; v1 produced mostly single-commit output
//     because the LLM had no rubric for what counts as separate
//     concerns. v2 names five concrete patterns (code+test,
//     impl+refactor, code+doc, style+logic, chore+real change)
//     so the LLM has a decision rubric, not an adjective. The
//     "one coherent commit beats two forced splits" counter-
//     anchor prevents the opposite regression.
//
//   - Two-tier body rule: PR body and commit body have different
//     optima. PR body is review-time context (long, structured,
//     covers all four dimensions). Commit body is per-change
//     intent for `git log` readers months later (1-3 short
//     paragraphs, focused on WHY this specific commit exists).
//     v2 distinguishes them explicitly so the LLM doesn't pad
//     commit bodies to PR length or strip them to nothing.
//
//   - Type-by-body rule: chore: (typos, dep bumps, comment
//     cleanup) gets an OPTIONAL body; fix: and feat: get a
//     REQUIRED body. Without this anchor the LLM either pads
//     every chore commit with motivation prose or strips every
//     fix commit to a one-liner.
//
//   - Issue trailer enforcement: when c.Issue > 0 the LLM must
//     add `Issue: #N` as the last line. v1's prose hint
//     ("[<Issue: #N>] if applicable") was easy to miss; v2
//     raises it into a Do NOT bullet.
//
//   - Anti-modal-pattern: the modal training-data commit is one
//     subject-only commit, no body. PR #135's
//     `fix(gtw): restore missing space in the commit-agent prompt`
//     is literal empirical evidence that v1 produced this
//     regression in production. v2 disallows it with an explicit
//     Do NOT and a self-check anchored to commit-appropriate
//     depth (different from the PR prompt's `body vs git log`
//     rule — commit body should be SHORTER than the diff, not
//     longer than it).
//
//   - Preserved invariants: the 3 hard rules (don't push, don't
//     revert/restore/stash, no `git add -A`) work as-is. Do NOT
//     touch them in v2 — PR #135 also touched one and that was
//     the same regression we're fixing, not a separate issue.
//
// F-56 §3.3 enumerated the original 3 rules. v2 keeps them
// verbatim under a "Hard rules (non-negotiable)" header and
// adds the new content guidance above.
func buildAgentPrompt(c Context) string {
	var sb strings.Builder

	// --- Role + scope -------------------------------------------
	sb.WriteString("You are a release engineer. Your job: turn uncommitted work into one or more well-formed local commits. Push and PR creation are handled by separate steps; you ONLY commit.\n\n")

	fmt.Fprintf(&sb, "Branch: %s\nWorktree: %s\n", c.Branch, c.Worktree)
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "Issue: #%d\n", c.Issue)
	}
	sb.WriteString("\n")

	// --- Tool floor (mandatory git inspection) -------------------
	sb.WriteString("## Before staging — tool floor\n")
	sb.WriteString("You MUST run and read the output of these commands BEFORE staging anything:\n")
	sb.WriteString("- `git status` — see what's dirty.\n")
	sb.WriteString("- `git diff` (no args) — the unstaged changes you're about to stage.\n")
	sb.WriteString("- `git diff --staged` if anything is already staged — don't lose work.\n")
	sb.WriteString("- `git log --oneline -5` — recent commit subjects for style continuity.\n\n")
	sb.WriteString("Do NOT stage files without reading their diff. The decision of \"what goes in which commit\" requires seeing the actual change, not just file names.\n\n")

	// --- Split anchors (concrete patterns) ----------------------
	sb.WriteString("## Splitting into multiple commits — when and how\n")
	sb.WriteString("ONE commit is correct when all the changes serve the same intent. MULTIPLE commits are correct when the changes serve different intents. Look for these patterns:\n\n")
	sb.WriteString("- **Code change + its test** → usually ONE commit (the test is part of the change). Split only if the test alone is reusable infrastructure.\n")
	sb.WriteString("- **Implementation + refactor** → SPLIT. The refactor is a separate concern even if the implementation depends on it. Order: refactor first, implementation second.\n")
	sb.WriteString("- **Code change + doc/comment update that explains the change** → ONE commit. The doc is part of the change.\n")
	sb.WriteString("- **Style/formatting churn mixed with logic change** → SPLIT. The style churn should be its own `style:` commit so `git blame` on logic lines stays clean.\n")
	sb.WriteString("- **Unrelated chore (typo / dep bump / comment-only edit) mixed with a real change** → SPLIT. Don't bury a real fix in a chore sweep.\n\n")
	sb.WriteString("When in doubt, ONE commit with a clear subject is better than a forced split. Multi-commit is for genuinely separate intents, not for showing off.\n\n")

	// --- Conventional Commits rules -----------------------------
	sb.WriteString("## Conventional Commits\n")
	sb.WriteString("- Format: <type>(<optional-scope>): <subject>\n")
	sb.WriteString("- Types: feat, fix, chore, refactor, docs, test, build, ci, perf, style, revert\n")
	sb.WriteString("- Subject ≤72 chars, imperative mood, no trailing period.\n")
	sb.WriteString("- Scope names the layer (e.g. cmd, command, gtw, feishu, login), not the file path.\n")
	sb.WriteString("- Breaking change: `!` after type/scope + `BREAKING CHANGE:` footer.\n\n")

	// --- Body rules (different from PR body) --------------------
	sb.WriteString("## Body rules — different from PR body\n")
	sb.WriteString("A commit body is NOT a PR body. PR bodies are review-time context for the whole branch. Commit bodies are per-change intent for `git log` readers months later. Different audience, different optimum.\n\n")
	sb.WriteString("- 1-3 short paragraphs (5-15 lines total). Longer than that is a sign the commit is doing too much.\n")
	sb.WriteString("- Lead with the immediate WHY: what bug, what use case, what previous behavior this replaces.\n")
	sb.WriteString("- Mention blast radius only if it's not obvious from the diff (e.g. \"this changes the wire format\" warrants a callout; \"rename a local var\" doesn't).\n")
	sb.WriteString("- For `chore:` (typos, dep bumps, comment cleanup) a body is OPTIONAL. Skip it unless an Issue ref is required.\n")
	sb.WriteString("- For `fix:` and `feat:` a body is REQUIRED. The `Issue: #N` trailer, if applicable, goes on the last line.\n\n")
	sb.WriteString("Self-check: if your commit body is longer than the `git diff` output for that commit, you've written too much. If it's empty for a `fix:` / `feat:`, you've written too little.\n\n")

	// --- Anti-pattern block -------------------------------------
	sb.WriteString("## Do NOT\n")
	sb.WriteString("- Do NOT produce a single subject-only commit for non-trivial work. `fix(agent): SignalProcessGroup helper` with no body is exactly the modal training-data pattern and tells future-you nothing.\n")
	sb.WriteString("- Do NOT pad commit bodies with prose that paraphrases the diff. The diff already shows what changed; the body explains why.\n")
	sb.WriteString("- Do NOT invent split points. One coherent commit beats two commits stitched together with a forced split.\n")
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "- Do NOT skip the `Issue: #%d` trailer — without it, the commit cannot be linked back to the bug tracker later.\n", c.Issue)
	}
	sb.WriteString("\n")

	// --- Example -------------------------------------------------
	// Anchors the LLM onto a real depth target. The example is a
	// lightly rewritten version of PR #139's first commit — the
	// gold-standard series where every commit had a multi-paragraph
	// WHY body. The business content (SignalProcessGroup) is kept
	// so the LLM can see the shape on real prose, not abstract
	// scaffolding. A future reviewer might want to swap this for
	// a synthetic example if the F-54 specifics leak into other
	// commits.
	sb.WriteString("## Example — the depth target\n\n")
	sb.WriteString("```\n")
	sb.WriteString("fix(agent): SignalProcessGroup helper for /stop PG-broadcast\n\n")
	sb.WriteString("/stop sent SIGINT to the cli's single pid, leaving any spawned `Bash`\n")
	sb.WriteString("tool subprocess running. Fix: a new agent.SignalProcessGroup helper\n")
	sb.WriteString("that delivers the signal to the entire OS process group via\n")
	sb.WriteString("`kill(-pid, SIGINT)`, with an `ESRCH` fallback to single-pid signaling.\n")
	sb.WriteString("The post-Setsid cli is the pg leader, so the broadcast reaches every\n")
	sb.WriteString("descendant — the same way Ctrl-C in a TTY hits the foreground\n")
	sb.WriteString("process group.\n\n")
	sb.WriteString("Wired into claudecode.Stop / Close, codex.Stop, opencode.Close,\n")
	sb.WriteString("pi.Close. pi.Stop is unchanged because it uses the rpc `abort` endpoint\n")
	sb.WriteString("rather than Process.Signal.\n\n")
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "Issue: #%d\n", c.Issue)
	}
	sb.WriteString("```\n\n")

	// --- Hard rules (DO NOT TOUCH — work as-is) -----------------
	sb.WriteString("## Hard rules (non-negotiable)\n")
	sb.WriteString("- Do not push. Push is the user's decision, not yours; never run `git push`.\n")
	sb.WriteString("- Do not revert, restore, or stash the user's work.\n")
	sb.WriteString("- `git add <specific files>`, not `git add -A`.\n")

	return sb.String()
}
