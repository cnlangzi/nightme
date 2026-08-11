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
// call (commit+push, PR title+body generation). 5 minutes
// covers realistic agent commits (lint fixes, conflict
// resolution, multi-tool flows) without wedging /gtw push if
// an agent hangs (e.g. PTY fallback with no idle signal — see
// pty.RunOnce's ptyIdleTimeout for the per-call short-window
// heuristic).
//
// Exported because both /gtw push and /gtw pr use it.
const RunOnceTimeout = 5 * time.Minute

// dispatchPush is the three-state dispatcher for /gtw push.
//
// Flow:
//
//  1. loadDispatchContext → c (Context). Reads cs.SelectedCwd()
//     and either loads `.nightme/gtw.yml` (worktree mode) or
//     derives Worktree/Branch/RepoRoot from git (non-worktree
//     mode).
//  2. git status --porcelain (single call, drives both the
//     conflict check and the clean/dirty dispatch).
//  3. If status has unmerged paths → refuse (rebase/merge
//     mid-state guard via detectConflicts).
//  4. Otherwise:
//     empty → pushClean  (clean branch: nothing OR programmatic push)
//     dirty → pushDirty  (delegate commit + push to one-shot agent)
//
// Reply is sent inline via cs.Channel(); return value is the
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

	statusOut, _, err := deps.Git.Run(ctx, c.Worktree, "status", "--porcelain")
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ git status: %v", err)), nil
	}
	if detectConflicts(statusOut) {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf(
				"❌ worktree %s is in a conflicted state (unmerged paths present)\n"+
					"hint: resolve conflicts and `git add` the resolutions, OR `git rebase --abort` / `git merge --abort`",
				c.Worktree)), nil
	}
	if strings.TrimSpace(statusOut) == "" {
		return pushClean(ctx, cs, deps, chatID, messageID, c)
	}
	return pushDirty(ctx, cs, deps, chatID, messageID, c, args, ymlAgent)
}

// pushClean handles the clean-worktree branch:
//
//   - 0 unpushed commits → "✅ nothing to push" (terminal reply).
//   - N unpushed commits → we push programmatically via
//     programmaticPush (git push -u origin <branch>).
//
// "Nothing to push" is a hard terminal: no agent call, no git
// push. We do this so the user gets fast feedback on the common
// "I'm just checking" case. The user can still run `git push`
// manually if upstream tracking is broken.
//
// After programmaticPush returns nil (claimed success), we
// re-count unpushed. If it's still > 0 we retry once — git push
// can silently no-op on transient network errors that the
// command exit code doesn't surface (e.g. partial TCP close,
// remote returns 200 but the refs aren't updated). One retry
// catches the common race; if the retry also leaves unpushed
// commits behind, we surface the actual commit list so the
// user can investigate (network/auth/protection rule on the
// remote).
func pushClean(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	c Context,
) (*Result, error) {
	unpushed, err := countUnpushed(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ check unpushed commits: %v", err)), nil
	}
	if unpushed == 0 {
		return reply(ctx, cs.Channel(), chatID, messageID,
			"✅ nothing to push — worktree clean and no unpushed commits"), nil
	}

	out, err := programmaticPush(ctx, deps, c)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ git push failed: %v\n%s", err, out)), nil
	}

	// Post-push verification: programmaticPush returning nil is
	// necessary but not sufficient (network race, remote silently
	// rejected, etc.). Re-count and retry once if any commit
	// didn't actually land upstream.
	if verifyMsg := verifyPushedAndRetry(ctx, deps, c, out); verifyMsg != "" {
		return reply(ctx, cs.Channel(), chatID, messageID, verifyMsg), nil
	}

	// Format 3 (see gtw/README.md §2.3): opaque content block.
	// Title names the action, `> <branch>` line names the entity,
	// raw git push output follows verbatim. The previous
	// `[Push]\n━━━━━━━━━━━━━━\n🌿 branch: ...\n📁 worktree: ...`
	// form was a Format 1/2 hybrid that didn't fit any rule; the
	// branch is now redundant with the `>` line and the worktree
	// path is recoverable from git config if the user needs it.
	body := fmt.Sprintf(
		"✅ pushed\n"+
			"> %s\n%s\n",
		c.Branch, strings.TrimRight(out, "\n"),
	)
	return reply(ctx, cs.Channel(), chatID, messageID, body), nil
}

// verifyPushedAndRetry re-counts unpushed after a claimed-successful
// push. If anything is still missing, it retries programmaticPush
// once; if the retry also leaves commits behind, it returns the
// formatted error message naming the unpushed commits. Empty
// return = push verified successful, caller should report success.
//
// Critical: countUnpushed errors MUST be checked, not discarded.
// If we ignored the error and reported "✅ pushed" based on the
// zero value of unpushed (which is 0), a real verification
// failure (NFS unmount, worktree removed, permission denied) would
// surface as a green checkmark that lies to the user.
func verifyPushedAndRetry(ctx context.Context, deps HandlerDeps, c Context, firstOut string) string {
	unpushed, err := countUnpushed(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return fmt.Sprintf("⚠️ pushed (claimed) but verification failed: %v", err)
	}
	if unpushed == 0 {
		return ""
	}

	// Retry once.
	_, err = programmaticPush(ctx, deps, c)
	if err != nil {
		return fmt.Sprintf(
			"❌ push reported success but %d commits on %s still don't appear on origin/%s\n"+
				"first attempt stderr: %s\n"+
				"retry error: %v\n"+
				"hint: check `git push -v %s` — likely network or remote protection rule.\n\n"+
				"unpushed commits:\n%s",
			unpushed, c.Branch, c.Branch,
			strings.TrimSpace(firstOut), err, c.Branch,
			unpushedCommitsForDisplay(ctx, c.Worktree, c.Branch, deps),
		)
	}

	unpushed, err = countUnpushed(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return fmt.Sprintf(
			"❌ %d commits on %s couldn't be re-counted after retry: %v\n"+
				"hint: check `git push -v %s` and `git status` in the worktree.",
			unpushed, c.Branch, err, c.Branch,
		)
	}
	if unpushed == 0 {
		return "" // retry succeeded
	}

	return fmt.Sprintf(
		"❌ %d commits on %s still don't appear on origin/%s after retry\n"+
			"hint: check `git push -v %s` — likely network or remote protection rule.\n\n"+
			"unpushed commits:\n%s",
		unpushed, c.Branch, c.Branch, c.Branch,
		unpushedCommitsForDisplay(ctx, c.Worktree, c.Branch, deps),
	)
}

// verifyAgentPushedAndRecover is the post-agent sanity check used
// by pushDirty. After the agent claims it committed and pushed,
// we independently verify all of:
//
//  1. HEAD branch: agent must still be on c.Branch. If the agent
//     `git checkout`'d to a side branch, committed there, and
//     "pushed" — the worktree from c.Branch's perspective looks
//     clean, countUnpushed is 0, and the user gets a green
//     checkmark for a commit that will never land on c.Branch.
//     Verified by reading the branch HEAD is currently on.
//
//  2. uncommitted files: if any remain, the agent skipped them
//     (intentionally or not). Surface the file list and tell
//     the user to either commit them manually or re-run
//     /gtw push. We do NOT re-invoke the agent — the same
//     agent already had a chance and chose to leave these
//     behind.
//
//  3. unpushed commits: if any remain, the agent's commit
//     succeeded but the push didn't (network race, non-FF,
//     etc.). Try programmaticPush once — same retry semantics
//     as pushClean — and surface the commit list if the retry
//     also leaves things behind.
//
//  4. HEAD advanced: if the worktree was dirty before the agent
//     (pushDirty only runs when status --porcelain has output)
//     and is clean now, but HEAD didn't move, the agent must
//     have done something OUTSIDE git (stash, file restore,
//     etc.) that doesn't constitute a "push". We surface a
//     warning rather than reporting success.
//
// Returns "" when all four checks pass; the caller should report
// success in that case.
func verifyAgentPushedAndRecover(ctx context.Context, deps HandlerDeps, c Context, headBefore string) string {
	// (1) Branch check: agent must still be on c.Branch.
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

	// (2) uncommitted check.
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
			strings.Join(uncommitted, "\n  "),
		)
	}

	// (4) HEAD advance check: if we have a headBefore snapshot
	// and HEAD didn't move, the agent didn't commit anything.
	// countUnpushed would be 0 (HEAD already at upstream), but
	// the dirty → clean transition can't have happened via a
	// commit, so something's off. headBefore == "" is the
	// "unknown" sentinel — caller didn't capture it (older
	// call sites) and we skip this check.
	if headBefore != "" {
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
	}

	// uncommitted is clean → check push.
	unpushed, err := countUnpushed(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return fmt.Sprintf("⚠️ agent finished but failed to verify push state: %v", err)
	}
	if unpushed == 0 {
		return ""
	}

	// Agent committed but the push didn't land — let
	// verifyPushedAndRetry own the retry+verify loop. Pass ""
	// for firstOut because the agent's own push is the
	// "attempt 0" and we don't have its output captured here.
	if verifyMsg := verifyPushedAndRetry(ctx, deps, c, ""); verifyMsg != "" {
		return verifyMsg
	}
	return ""
}

// pushDirty handles the dirty-worktree branch — delegates the
// entire commit + push to a one-shot agent call wrapped in a
// 5-minute timeout.
//
// Agent selection:
//   - args.Agent (from -a / --agent) wins when non-empty.
//   - Otherwise: chat's currently Selected Agent.
//   - If both are empty: refuse (the user must /use first).
//
// Agent validation:
//   - agent.Builtins.Get(name) returns ErrUnknownAgent → refuse.
//   - a.Detect() fails (binary missing) → refuse with a clear
//     "X not found" message before we even attempt the call.
//
// Agent reply:
//   - We do NOT parse the text. We just check err vs nil. The
//     text goes verbatim into the success card for the user to
//     read (commit hash / push status are surfaced from agent
//     prose; nightme does not re-query git to verify).
func pushDirty(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
	c Context,
	args pushArgs,
	ymlAgent string,
) (*Result, error) {
	// pushDirty is the only push path that spawns an agent
	// (pushClean is a plain git push). The yml-configured agent
	// name comes from the caller (cmd.go runPush already loaded
	// the user-level config); pushDirty itself does NOT Load() —
	// that responsibility lives in the cmd.go wrapper which
	// surfaces loadNotes to the user. Loading here would
	// duplicate the warning block in the chat (see wip/gtw-hooks.md
	// "B1: 双重警告").
	agentName, agentNotes := ResolveAgent(args.Agent, ymlAgent, cs)
	if agentName == "" {
		// B2: surface agentNotes here too. Without this, a yml
		// pointing at an unknown agent (e.g. "pi") would
		// silently degrade to "❌ no agent selected" — hiding
		// the user's misconfiguration behind a generic error.
		msg := "❌ no agent selected. Send `/use <name>` first or pass `-a <name>`."
		for _, n := range agentNotes {
			msg += "\n" + n
		}
		return reply(ctx, cs.Channel(), chatID, messageID, msg), nil
	}

	a, err := agent.Builtins.Get(agentName)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ unknown agent %q (check `nightme agents` or your config)", agentName)), nil
	}
	if err := a.Detect(); err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ agent %s not available: %v", agentName, err)), nil
	}

	ctx, cancel := context.WithTimeout(ctx, RunOnceTimeout)
	defer cancel()

	blocks := []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: buildAgentPrompt(c),
	}}
	text, err := a.RunOnce(ctx,
		agent.StartConfig{Workspace: c.Worktree},
		blocks,
	)
	if err != nil {
		return reply(ctx, cs.Channel(), chatID, messageID,
			fmt.Sprintf("❌ agent %s failed: %v", agentName, err)), nil
	}

	// Snapshot HEAD before verification so we can detect "agent
	// claimed success but didn't actually commit" — a class of
	// failure where the worktree's status --porcelain is empty
	// (e.g. agent did `git stash` instead of commit) and
	// countUnpushed is 0 (HEAD was already at origin), but
	// nothing actually landed on the remote. Without this
	// snapshot we'd happily report "✅ pushed" for a no-op.
	headBefore, headSnapErr := headSHA(ctx, c.Worktree, deps)
	if headSnapErr != nil {
		// Non-fatal — the verification will still catch
		// uncommitted + unpushed. Pass "" to skip the
		// HEAD-advance check (caller treats "" as
		// "snapshot unavailable").
		headBefore = ""
	}

	// Post-agent verification: the agent may have committed and
	// pushed (the happy path), but it may also have left files
	// uncommitted or the push may have silently failed (network
	// race, non-fast-forward, …). We check both and take
	// corrective action.
	//
	//   - uncommitted > 0 → agent didn't commit everything.
	//     Surface the file list so the user can commit manually
	//     or run /gtw push again to retry. We do NOT re-invoke
	//     the agent automatically — the same agent already had
	//     a chance and chose to leave these behind, possibly
	//     deliberately (e.g. secrets, generated files). User
	//     judgment is required.
	//
	//   - unpushed > 0 → agent committed but the push didn't
	//     reach origin (or HEAD@{u} is on a different branch).
	//     Try programmaticPush once more before giving up — git
	//     push can no-op on transient errors that exit 0.
	postMsg := verifyAgentPushedAndRecover(ctx, deps, c, headBefore)
	if postMsg != "" {
		body := postMsg
		if text != "" {
			body += "\n\nagent reply (for context):\n" + indentLines(text, "  ")
		}
		return reply(ctx, cs.Channel(), chatID, messageID, body), nil
	}

	// Format: gtw-standard three-line reply — emoji title on
	// line 1, `> <branch>` prompt on line 2, agent's prose on
	// line 3+. feishu auto-prepends `❯ ` to the first line of
	// every OutCommandReply (adapter.go:1588), so the rendered
	// card is:
	//
	//	❯ 🤖 <agent> pushed
	//	> <branch>
	//	<agent text>
	//
	// Note on the header: we deliberately use "🤖 X pushed"
	// instead of "✅ Pushed (via X)". The agent's RunOnce exits
	// 0 even when the underlying git push failed (e.g.
	// non-fast-forward, auth expired) and reports the failure in
	// its prose reply. By design we do NOT parse that text,
	// so we cannot honestly claim ✅. The neutral header lets
	// the agent's text be the source of truth — if it says
	// "pushed abc1234", great; if it says "push failed: ...",
	// the user reads that without a green checkmark contradicting
	// it.
	body := fmt.Sprintf(
		"🤖 %s pushed\n"+
			"> %s\n%s\n",
		agentName, c.Branch, text,
	)
	// Surface agent-resolve notes (e.g. yml referenced an unknown
	// agent but a session default was found) at the bottom of the
	// reply so the user can see why their config didn't fully apply.
	// These are diagnostics, not blockers — main flow has succeeded.
	if len(agentNotes) > 0 {
		body += "\n" + strings.Join(agentNotes, "\n") + "\n"
	}
	return reply(ctx, cs.Channel(), chatID, messageID, body), nil
}

// buildAgentPrompt renders the single text block the agent
// receives. Format:
//   - Working directory + branch always present (anchors the agent).
//   - Issue (#ID) only when ModeRemote (c.Issue > 0).
//   - Conventional Commits format block is mandatory — without
//     it the agent sometimes writes non-conforming messages
//     (free-form subjects, missing type).
//   - Step list: status → diff → commit → push → reply with hash.
//   - "Reference issue with #ID in body" only when c.Issue > 0.
func buildAgentPrompt(c Context) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Working directory: %s\n", c.Worktree)
	fmt.Fprintf(&sb, "Branch: %s\n", c.Branch)
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "Issue: #%d\n", c.Issue)
	}
	sb.WriteString("\nUncommitted changes detected.\n\n")
	sb.WriteString("Task:\n")
	sb.WriteString("1. Run `git status` and `git diff` to inspect changes.\n")
	sb.WriteString("2. Write a Conventional Commits commit message.\n")
	sb.WriteString("   Format: <type>(<optional-scope>): <subject>\n")
	sb.WriteString("   Types: feat, fix, chore, refactor, docs, test, build, ci, perf, style, revert\n")
	sb.WriteString("   Subject: ≤72 chars, imperative mood, no trailing period\n")
	sb.WriteString("   Body: explain WHY, wrap at 72\n")
	if c.Issue > 0 {
		fmt.Fprintf(&sb, "   Reference issue with #%d in body.\n", c.Issue)
	}
	sb.WriteString("3. `git add -A && git commit -m \"<msg>\"` (heredoc if multi-line).\n")
	fmt.Fprintf(&sb, "4. `git push -u origin %s`.\n", c.Branch)
	sb.WriteString("5. Reply with: <commit_hash> <one-line summary>\n")
	return sb.String()
}
