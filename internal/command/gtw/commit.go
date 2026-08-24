package gtw

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/timeouts"
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
	snap, err := CollectReadinessForDispatch(ctx, c.Worktree, deps.Git)
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

	// 1b. Refuse detached HEAD. Mirrors dispatchPush's
	// "snap.Branch == \"\"" early-return — keep the two
	// gates' policy symmetrical.
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
	ctx, cancel := context.WithTimeout(ctx, timeouts.Agent)
	defer cancel()
	runRes, agentName, err := runAgentFor(ctx, cs, c.Worktree,
		buildAgentPrompt(c), chatID, messageID, args.Agent, ymlAgent)
	if err != nil {
		return replyAgent(ctx, cs.Emitter(), chatID, messageID,
			err.Error(), agentName, runRes), nil
	}

	// 4b. (no cs-level GitStatus refresh needed post-RunOnce:
	// ChatSession.GitStatus now rebuilds a fresh snapshot on
	// every call, so the success card stamp naturally sees the
	// post-commit HEAD/snapshot.)

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
	snap, err = CollectReadinessForDispatch(ctx, c.Worktree, deps.Git)
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
// receives for /gtw commit.
//
// Design goal (post-fix-gtw-commit): the LLM's only job is to
// stage the current local branch's uncommitted changes into one
// or more local commits following Conventional Commits and stop.
// It must NOT modify files, run tests, run builds, verify its
// own work, push, rebase, or otherwise overstep.
// overstep. nightme does readiness + post-commit verification
// (verifyAgentCommitted); the LLM only writes commits.
//
// Prompt rationale (replaces the earlier ~92-line v2 prompt,
// which taught the LLM to do patch / hunk surgery and self-
// verification — see PR-feedback discussion 2026-08-24):
//
//   - Allowlist + denylist (defense in depth). The allowlist
//     names the four commands the LLM may run (status /
//     diff-family / log / add / commit). The denylist (the
//     `You MUST NOT` block) names the actions that are known
//     regression sources — file edits, `git apply` hunk
//     surgery, test/build runs, self-verification, push,
//     history mutation, branch hopping, revert/restore/stash,
//     `git add -A`. Both layers matter: the allowlist alone
//     leaves gaps (e.g. "don't run tests" via the Bash tool
//     — the LLM could still try), and the denylist alone
//     leaves the LLM room to invent creative workarounds for
//     actions that aren't explicitly named. Together they
//     cover both directions.
//
//   - Whole-file staging only: explicit "NOT `git add -A`, NOT
//     hunk-level staging" rule + the `git apply` prohibition.
//     This is the regression that motivated the fix — the
//     earlier prompt never forbade `git apply --cached`, so the
//     LLM extracted individual hunks into hand-crafted patches
//     when one file mixed concerns. Whole-file staging may put
//     extra files into a "wrong" commit, but that's a cleanup
//     the user does later — never the LLM's call to make.
//
//   - Wrapper verifies; you commit: explicit "Do not verify
//     your own work" rule. verifyAgentCommitted already does
//     branch / clean / HEAD-advance checks; having the LLM
//     redo them is a task-drift problem — the LLM loses focus
//     on the commit task while it re-runs checks the wrapper
//     owns. The user's request was unambiguous:
//     "不需要它执行验证".
//
//   - No file modifications: "MUST NOT edit, create, or delete
//     any file" closes the door on the LLM deciding to "fix a
//     typo first" or "update CHANGELOG.md before committing".
//     The user wants the LLM to commit the work AS-IS.
//
//   - History immutability: amend / rebase / reset / checkout
//     -all change existing commits or move HEAD off-branch.
//     The LLM must not touch history; if a commit is wrong the
//     user fixes it manually.
//
//   - Terse final reply: success card is rendered from
//     `git log headBefore..HEAD`; the LLM's text reply is just
//     a brief confirmation. Telling the LLM "do not narrate
//     your work" is a task-focus rule — the LLM should commit,
//     not write prose about what it did. (The streamed tool
//     events during the run stay untouched — they're the
//     process visibility channel by design, and not in scope
//     for this fix.)
//
// What survives from the v2 prompt:
//
//   - Conventional Commits subject shape.
//   - Subject length / mood / no-period rules.
//   - fix:/feat: REQUIRED body, chore: OPTIONAL.
//   - Anti-modal-pattern guard (PR #135 regression).
//   - Issue trailer enforcement when c.Issue > 0.
//   - "One coherent commit beats two forced splits" counter-
//     anchor on splitting.
//
// What is dropped:
//
//   - Tool floor (4 commands "you MUST run"). Replaced with
//     a single "run `git status` + `git diff` ONCE" — the
//     4-command mandatory list caused the LLM to re-run
//     inspection commands repeatedly across multi-commit
//     runs, drifting away from the commit task into a
//     self-verification loop. ("Repeated inspection" here
//     is a task-focus problem, not a token-cost problem —
//     detailed logs are wanted; the issue is the LLM
//     forgetting to commit while it inspects.)
//
//     Tradeoff acknowledged: the old tool floor had a
//     dedicated `git diff --staged` bullet ("if anything is
//     already staged — don't lose work"). The new Workflow
//     step 1 keeps that nudge inline ("If anything is already
//     staged, also run `git diff --staged` — don't lose
//     pre-staged work"), but demotes it from "mandatory
//     inspection" to "conditional extra step". `git status`
//     porcelain still surfaces staged files, so a careful
//     agent can act on it; the explicit nudge survives as
//     a workflow anchor, not a tool floor.
//   - 5-row split rubric. Replaced with a one-line "ONE
//     coherent commit beats two forced splits" anchor — the
//     LLM already knows what an "intent" is.
//   - Two-tier body rule (commit vs PR body differentiation).
//     The LLM was not confusing the two; the rule was defensive
//     against a regression that did not occur.
//   - Type-by-body rule (chore OPTIONAL, fix+feat REQUIRED).
//     Folded into the new compact body-rules section.
//   - Gold example (the SignalProcessGroup depth target).
//     Replaced by the explicit anti-modal guard; the example
//     was pattern-matching context that the body rules +
//     anti-modal line already encode, and it had drifted the
//     LLM into reproducing the example's depth and shape even
//     for commits that did not need it (a task-focus problem:
//     the LLM was matching the example rather than writing
//     for the actual change).
func buildAgentPrompt(c Context) string {
	var sb strings.Builder

	// --- Role + scope -------------------------------------------
	sb.WriteString("You are a release engineer. Stage the current local branch's uncommitted changes into one or more local commits following Conventional Commits on that branch. Push and PR creation are handled by separate steps; you ONLY commit.\n\n")

	fmt.Fprintf(&sb, "Branch: %s\nWorktree: %s\n", c.Branch, c.Worktree)
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "Issue: #%d\n", c.Issue)
	}
	sb.WriteString("\n")

	// --- Allowed operations (strict allowlist) ------------------
	sb.WriteString("## Allowed operations (strict allowlist)\n")
	sb.WriteString("You may only run these git commands — nothing else:\n")
	sb.WriteString("- `git status` — read porcelain.\n")
	sb.WriteString("- `git diff` / `git diff --staged` / `git log` — read content.\n")
	sb.WriteString("- `git add <specific files>` — stage WHOLE FILES (NOT `git add -A`, NOT hunk-level staging).\n")
	sb.WriteString("- `git commit -m \"<subject>\" [-m \"<body>\"]` — create a commit.\n\n")
	sb.WriteString("You MUST NOT:\n")
	sb.WriteString("- Edit, create, or delete any file in the worktree.\n")
	sb.WriteString("- Run `git apply`, `git apply --cached`, or any patch / hunk surgery. Stage whole files only.\n")
	sb.WriteString("- Run tests, builds, linters, formatters, or any other tool.\n")
	sb.WriteString("- Verify your own work. The wrapper runs HEAD-advance / clean / branch checks after you exit.\n")
	sb.WriteString("- Push. Never run `git push`.\n")
	sb.WriteString("- Amend, rebase, reset, or otherwise modify existing commits or history.\n")
	sb.WriteString("- Checkout a different branch. Stay on the current branch.\n")
	sb.WriteString("- Revert, restore, or stash the user's work.\n")
	sb.WriteString("- `git add -A` or `git add .`.\n\n")

	// --- Workflow ------------------------------------------------
	sb.WriteString("## Workflow\n")
	sb.WriteString("1. Run `git status` + `git diff` ONCE to see what's dirty. If anything is already staged, also run `git diff --staged` — don't lose pre-staged work.\n")
	sb.WriteString("2. Decide commit boundaries — one per logical intent. Use your judgment: ONE coherent commit beats two forced splits.\n")
	sb.WriteString("3. For each commit: `git add <files>` then `git commit -m ...`. Stop.\n\n")

	// --- Commit message rules -----------------------------------
	sb.WriteString("## Commit message rules\n")
	sb.WriteString("- Conventional Commits: <type>(<optional-scope>): <subject>\n")
	sb.WriteString("- Subject ≤72 chars, imperative mood, no trailing period.\n")
	sb.WriteString("- For `fix:` / `feat:` a 1-3 paragraph body explaining WHY this commit exists is REQUIRED.\n")
	sb.WriteString("- For `chore:` a body is OPTIONAL.\n")
	sb.WriteString("- No subject-only commit for non-trivial work.\n")
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "- End the body with `Issue: #%d` on its own line.\n", c.Issue)
	}
	sb.WriteString("\n")

	// --- Final reply contract -----------------------------------
	// The LLM's text reply at the end of the run is overlaid on
	// the success card (built from `git log headBefore..HEAD`).
	// The reply contract keeps it brief — one line per commit
	// — so the chat stays focused on the commit list. ("Do not
	// narrate your work" is a task-focus rule: the LLM should
	// commit, not write prose about what it did. The streamed
	// tool events during the run are the process visibility
	// channel and are NOT touched here — they're by design.)
	sb.WriteString("## Final reply\n")
	sb.WriteString("The wrapper renders the success card from `git log`; your text reply is seen as a brief confirmation only. Reply with one line per commit: `<hash> <subject>`. If you produced no commits, reply `(no commits)`. Do not narrate your work.\n")

	return sb.String()
}
