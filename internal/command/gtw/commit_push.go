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
	return pushDirty(ctx, cs, chatID, messageID, c, args)
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
	body := fmt.Sprintf(
		"✅ Pushed %q\n"+
			"━━━━━━━━━━━━━━\n"+
			"[Push]\n%s\n"+
			"━━━━━━━━━━━━━━\n"+
			"🌿 branch:   %s\n"+
			"📁 worktree: %s\n",
		c.Branch, indentLines(out, "  "), c.Branch, c.Worktree,
	)
	return reply(ctx, cs.Channel(), chatID, messageID, body), nil
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
	chatID, messageID string,
	c Context,
	args pushArgs,
) (*Result, error) {
	agentName := args.Agent
	if agentName == "" {
		agentName = cs.SelectedAgent()
	}
	if agentName == "" {
		return reply(ctx, cs.Channel(), chatID, messageID,
			"❌ no agent selected. Send `/use <name>` first or pass `-a <name>`."), nil
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

	// Note on the header: we deliberately use a neutral "🤖 Agent X
	// completed" framing instead of "✅ Pushed (via X)". The agent's
	// RunOnce exits 0 even when the underlying git push failed (e.g.
	// non-fast-forward, auth expired) and reports the failure in its
	// prose reply. By design we do NOT parse that text (per the
	// plan's "agent reply is opaque; we just check err vs nil"
	// decision), so we cannot honestly claim ✅. The neutral header
	// lets the agent's text be the source of truth — if it says
	// "pushed abc1234", great; if it says "push failed: ...", the
	// user reads that without a green checkmark contradicting it.
	body := fmt.Sprintf(
		"🤖 Agent %s completed\n"+
			"━━━━━━━━━━━━━━\n%s\n"+
			"━━━━━━━━━━━━━━\n"+
			"🌿 branch:   %s\n"+
			"📁 worktree: %s\n",
		agentName, indentLines(text, "  "), c.Branch, c.Worktree,
	)
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
