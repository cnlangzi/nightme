package gtw

import (
	"context"
	"fmt"
	"strings"
)

// buildSyncReply runs RefreshDefaultBranch and turns the result
// into an IM-friendly summary suitable for either /gtw sync
// (slash dispatcher path) or /gtw close (which calls it inline
// after tearing down the worktree, so the same card format
// reaches the user). On any error from RefreshDefaultBranch —
// dirty main, no origin, rebase conflict — the error is
// returned to the caller verbatim (its message already includes
// the git stderr tail + user-facing hint); the caller is
// responsible for any ❌ prefix / wrapping.
//
// Honours deps.SkipRefreshDefaultBranch by returning (empty, nil)
// so the caller can treat it as "sync not requested" without
// special-casing the flag. /gtw sync ignores this flag (it's a
// user-initiated refresh); /gtw close respects it as a test seam.
//
// When DefaultBranch can't be read after a successful pull, falls
// back to the raw git pull output rather than fabricating a card.
func buildSyncReply(ctx context.Context, repoRoot string, deps HandlerDeps) (string, error) {
	if deps.SkipRefreshDefaultBranch {
		return "", nil
	}
	newHead, pullOut, err := RefreshDefaultBranch(ctx, repoRoot, deps)
	if err != nil {
		return "", err
	}
	branch, derr := DefaultBranch(ctx, repoRoot, deps.Git)
	if derr != nil || branch == "" {
		return pullOut, nil
	}
	return renderSyncReply(branch, newHead, pullOut, repoRoot, ctx, deps.Git), nil
}

// renderFixSuccessCard builds the §5.2.⑥ success card (plain text;
// success has no interactive buttons in v1).
//
// baseSHA is the HEAD sha of the upstream default branch
// RefreshDefaultBranch pulled before WorktreeAdd. When empty
// the "based on" line is omitted.
//
// F-XX: the trailing "↳ ..." hint line differs by IssueDispatchMode:
//   - DispatchPlan     → "agent is analyzing — review the plan in
//     chat, then tell the agent when to proceed"
//   - DispatchExecute  → "agent is fixing now — follow progress in
//     chat · `/gtw commit` + `/gtw push` when done"
//
// The header line adds "(direct execute)" suffix in Execute
// mode. See F-gtw-fix.md §5.
func renderFixSuccessCard(issue *Issue, branch, worktree, repo, baseSHA string, mode IssueDispatchMode) string {
	var b strings.Builder
	if mode == DispatchExecute {
		fmt.Fprintf(&b, "✅ Fix #%d ready (direct execute)\n", issue.ID)
	} else {
		fmt.Fprintf(&b, "✅ Fix #%d ready\n", issue.ID)
	}
	fmt.Fprintf(&b, "→ branch:   `%s`\n", branch)
	fmt.Fprintf(&b, "→ worktree: %s\n", worktree)
	fmt.Fprintf(&b, "→ issue:    %s#%d [%s]\n", repo, issue.ID, LabelWIP)
	if baseSHA != "" {
		fmt.Fprintf(&b, "→ base:     %s\n", shortSHA(baseSHA))
	}
	switch mode {
	case DispatchExecute:
		b.WriteString("↳ agent is fixing now — follow progress in chat · `/gtw commit` + `/gtw push` when done\n")
	case DispatchPlan:
		b.WriteString("↳ agent is analyzing — review the plan in chat, then tell the agent when to proceed\n")
	default:
		// Defensive: any future IssueDispatchMode without an
		// explicit case lands here. Silent fallback is fine —
		// the next round of tests will catch accidental enum
		// drift via the missing-mode-wording assertions.
		b.WriteString("↳ agent is working — follow progress in chat\n")
	}
	return b.String()
}

// shortSHA trims a full 40-char git SHA to the conventional
// 12-char abbreviation. Used in success cards to keep the
// card readable — the full SHA is recoverable by the user
// via `git log` if they need it.
func shortSHA(sha string) string {
	if len(sha) < 12 {
		return sha
	}
	return sha[:12]
}

// renderSyncReply turns git pull --rebase stdout into a compact
// IM-friendly summary. Shared by /gtw sync (cmd.go runSync) and
// /gtw close (close.go RunClose) so both surface the same card.
//
//   - "Already up to date." → "✨ origin/<branch> already up to date"
//   - "Updating <old>..<new>" → header line + commit subject list
//     capped at 10 entries ("📥 pulled N commits: • …")
//   - Anything else (rare config variants) → fall back to the
//     raw pull output so the user still sees what happened.
//
// On any parsing/log failure (missing SHA, git log error), we
// degrade gracefully to the raw pull output rather than surfacing
// a formatting-internal error to the user.
func renderSyncReply(branch, newHead, pullOut, repoRoot string, ctx context.Context, git GitRunner) string {
	if strings.Contains(pullOut, "Already up to date.") {
		return fmt.Sprintf("✨ origin/%s already up to date", branch)
	}

	// Find the "Updating <old>..<new>" line.
	var oldSHA string
	for line := range strings.SplitSeq(pullOut, "\n") {
		rest, ok := strings.CutPrefix(line, "Updating ")
		if !ok {
			continue
		}
		parts := strings.SplitN(rest, "..", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			oldSHA = parts[0]
			break
		}
	}
	if oldSHA == "" {
		// No "Updating" line we recognise — fall back to raw.
		return pullOut
	}

	// Get commit subjects for the pulled range.
	subjectsRaw, _, err := git.Run(ctx, repoRoot, "log", oldSHA+"..HEAD", "--pretty=%s", "-n", "10")
	if err != nil || strings.TrimSpace(subjectsRaw) == "" {
		// Fall back to a plain header so the user still sees the
		// sync happened.
		return fmt.Sprintf("✅ origin/%s @ %s", branch, shortSHA(newHead))
	}
	subjects := strings.Split(strings.TrimSpace(subjectsRaw), "\n")

	var b strings.Builder
	fmt.Fprintf(&b, "✅ origin/%s @ %s\n", branch, shortSHA(newHead))
	fmt.Fprintf(&b, "📥 pulled %d commits:\n", len(subjects))
	for _, s := range subjects {
		fmt.Fprintf(&b, " • %s\n", s)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderFixLocalSuccessCard builds the simplified success card
// for the F-XX local-mode flow. There's no remote issue, no
// platform label, and no agent dispatch — just a confirmation
// that the worktree + branch are ready.
//
// F-XX: replaces (in the local-mode path) the cluttered
// success card that mentions issue id / platform / wip label —
// those fields don't apply to local branches.
func renderFixLocalSuccessCard(branch, worktree string) string {
	var b strings.Builder
	b.WriteString("✅ Local worktree ready\n")
	fmt.Fprintf(&b, "→ branch:   `%s`\n", branch)
	fmt.Fprintf(&b, "→ worktree: %s\n", worktree)
	b.WriteString("↳ work freely here · or `/gtw close` to drop the worktree\n")
	return b.String()
}

// WorktreeFailChoice + gtw.Choice / gtw.ChoiceOption were all
// removed in v1.5 along with the §5.3.3 worktree-fail retry
// card. The gtw package no longer emits interactive cards of
// its own.

// replyCommitSuccessCard builds the post-/gtw commit success IM
// card from git state — never from agent prose. Per F-56 §5, the
// agent's text is intentionally NOT used: nightme reads `git log`
// itself and renders the card so the format is consistent across
// agents (pi / claude / codex).
//
// `agentName` is the agent that just ran the one-shot commit
// (e.g. "pi"); empty when no agent was involved (reserved for
// future "raw git commit" mode).
//
// `revRange` is the git log range that captures the commits the
// commit just produced (typically `headBefore..HEAD`).
//
// Returns the rendered card on success, or an error if the
// underlying `git log` failed. A successful commit with a broken
// log query is a bug we want to surface, not a card we want to
// fudge with `committed 0 change(s)`.
func replyCommitSuccessCard(ctx context.Context, c Context, agentName, revRange string, deps HandlerDeps) (string, error) {
	logOut, err := gitLogRange(ctx, c.Worktree, revRange, deps)
	if err != nil {
		return "", fmt.Errorf("read committed commits: %w", err)
	}
	commitCount := 0
	if logOut != "" {
		commitCount = strings.Count(logOut, "\n") + 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🤖 %s committed %d change(s) on %s:\n",
		agentName, commitCount, c.Branch)
	if logOut != "" {
		b.WriteString(logOut)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "> %s\n", c.Branch)
	return b.String(), nil
}

// replyPushSuccessCard builds the post-/gtw push success IM card
// from git state — never from agent prose. Same source-of-truth
// rule as replyCommitSuccessCard.
//
// `revRange` is the git log range that captures the commits the
// push just landed (typically `headBefore..origin/<branch>`).
//
// Returns the rendered card on success, or an error if the
// underlying `git log` failed. A successful push with a broken
// log query is a bug we want to surface, not a card we want to
// fudge with `pushed 0 commit(s)`.
func replyPushSuccessCard(ctx context.Context, c Context, revRange string, deps HandlerDeps) (string, error) {
	logOut, err := gitLogRange(ctx, c.Worktree, revRange, deps)
	if err != nil {
		return "", fmt.Errorf("read pushed commits: %w", err)
	}
	commitCount := 0
	if logOut != "" {
		commitCount = strings.Count(logOut, "\n") + 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ pushed %d commit(s) to %s:\n",
		commitCount, c.Branch)
	if logOut != "" {
		b.WriteString(logOut)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "> %s\n", c.Branch)
	return b.String(), nil
}
