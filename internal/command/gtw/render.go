package gtw

import (
	"fmt"
	"strings"
)

// renderFixSuccessCard builds the §5.2.⑥ success card (plain text;
// success has no interactive buttons in v1).
//
// baseSHA is the HEAD sha of the upstream default branch
// RefreshDefaultBranch pulled before WorktreeAdd. When empty
// (e.g. daemon-recovery re-entry where we skipped the refresh)
// the "based on" line is omitted.
func renderFixSuccessCard(issue *Issue, branch, worktree, repo, baseSHA string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Fix #%d ready\n", issue.ID)
	fmt.Fprintf(&b, "→ branch:   `%s`\n", branch)
	fmt.Fprintf(&b, "→ worktree: %s\n", worktree)
	fmt.Fprintf(&b, "→ issue:    %s#%d [%s]\n", repo, issue.ID, LabelWIP)
	if baseSHA != "" {
		fmt.Fprintf(&b, "→ base:     %s\n", shortSHA(baseSHA))
	}
	b.WriteString("↳ `/gtw push` to ship · `/gtw close` to drop the worktree · or keep developing\n")
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

// BranchExistsCard builds the §5.3.1 interactive decision card.
// This is the single source of truth for production `/gtw fix`
// (emitBranchExistsDraft) and for debug `/gtw test` scenarios that
// exercise the same card shape — debug must not re-hardcode Choices.
//
// F-XX: handles both ID-mode (IssueID > 0) and local-mode
// (IssueID == -1) drafts. Local-mode drafts have no issue
// title / repo to display; the body shows the branch slug
// directly.
func BranchExistsCard(p FixDraftPayload, existingPath string) Card {
	var body string
	if p.IssueID == -1 {
		// Local-mode draft (no remote issue).
		body = fmt.Sprintf("branch: `%s` (local)\n", p.Branch)
	} else {
		body = fmt.Sprintf("issue: #%d  %s\n", p.IssueID, p.Title)
	}
	if existingPath != "" {
		body += fmt.Sprintf("已有 worktree: %s\n", existingPath)
	}
	body += "\n选择操作(反应对应 emoji):"
	return Card{
		Title: fmt.Sprintf("⚠️ 分支 `%s` 已存在", p.Branch),
		Body:  body,
		Choices: []CardChoice{
			{Emoji: "🆕", Label: "用 -v2 新分支", Action: "act:/gtw/branch-newv2"},
			{Emoji: "🔗", Label: "加入现有协作", Action: "act:/gtw/branch-join"},
			{Emoji: "❌", Label: "取消", Action: "act:/gtw/cancel"},
		},
	}
}

// WorktreeFailCard builds the §5.3.3 interactive decision card.
// Same ownership rule as BranchExistsCard: business layer owns the
// shape; debug UAT reuses it.
//
// F-XX: local-mode drafts (IssueID == -1) have no remote issue
// to label; the title omits the issue-id and the cancel
// option does NOT mention the nightme/wip label rollback
// (local-mode never added a label).
func WorktreeFailCard(p FixDraftPayload) Card {
	body := fmt.Sprintf("branch: %s\n", p.Branch)
	if p.GitError != "" {
		body += "[git stderr tail]\n" + p.GitError + "\n"
	}
	body += "\n选择操作(反应对应 emoji):"
	cancelLabel := "取消"
	// LabelAdded is true iff ID-mode added nightme/wip before
	// the worktree failed. Local-mode never adds a label, so
	// this only fires for ID-mode drafts (IssueID > 0).
	if p.LabelAdded && p.IssueID != -1 {
		cancelLabel += " (已撤销 nightme/wip label)"
	}
	title := "❌ 创建 worktree 失败"
	if p.IssueID != -1 {
		title = fmt.Sprintf("❌ 创建 worktree 失败(#%d)", p.IssueID)
	}
	return Card{
		Title: title,
		Body:  body,
		Choices: []CardChoice{
			{Emoji: "🔄", Label: "重试", Action: "act:/gtw/worktree-retry"},
			{Emoji: "❌", Label: cancelLabel, Action: "act:/gtw/cancel"},
		},
	}
}
