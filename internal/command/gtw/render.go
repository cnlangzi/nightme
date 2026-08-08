package gtw

import (
	"fmt"
	"strings"
)

// renderFixSuccessCard builds the §5.2.⑥ success card (plain text;
// success has no interactive buttons in v1).
func renderFixSuccessCard(issue *Issue, branch, worktree, repo string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Fix #%d 就绪\n", issue.ID)
	b.WriteString("━━━━━━━━━━━━━━\n")
	b.WriteString("[Context]\n")
	fmt.Fprintf(&b, "🌿 branch:   %s\n", branch)
	fmt.Fprintf(&b, "📁 worktree: %s\n", worktree)
	fmt.Fprintf(&b, "🏷 平台:%s#%d [%s]\n", repo, issue.ID, LabelWIP)
	if issue.URL != "" {
		fmt.Fprintf(&b, "🔗 %s\n", issue.URL)
	}
	b.WriteString("━━━━━━━━━━━━━━\n")
	b.WriteString("[Command result]\n")
	b.WriteString("💡 下一步:`/gtw push` (F-46) 或自由对话开发\n")
	return b.String()
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
	b.WriteString("✅ Local worktree 就绪\n")
	b.WriteString("━━━━━━━━━━━━━━\n")
	b.WriteString("[Context]\n")
	fmt.Fprintf(&b, "🌿 branch:   %s\n", branch)
	fmt.Fprintf(&b, "📁 worktree: %s\n", worktree)
	b.WriteString("━━━━━━━━━━━━━━\n")
	b.WriteString("[Command result]\n")
	b.WriteString("💡 下一步:在 worktree 中自由开发,准备好后 /cwd 切回主仓即可\n")
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
