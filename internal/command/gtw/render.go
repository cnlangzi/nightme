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

// BranchExistsCard builds the §5.3.1 interactive decision card.
// This is the single source of truth for production `/gtw fix`
// (emitBranchExistsDraft) and for debug `/gtw test` scenarios that
// exercise the same card shape — debug must not re-hardcode Choices.
func BranchExistsCard(p FixDraftPayload, existingPath string) Card {
	body := fmt.Sprintf("issue: #%d  %s\n", p.IssueID, p.Title)
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
func WorktreeFailCard(p FixDraftPayload) Card {
	body := fmt.Sprintf("branch: %s\n", p.Branch)
	if p.GitError != "" {
		body += "[git stderr tail]\n" + p.GitError + "\n"
	}
	body += "\n选择操作(反应对应 emoji):"
	cancelLabel := "取消"
	if p.LabelAdded {
		cancelLabel += " (已撤销 nightme/wip label)"
	}
	return Card{
		Title: fmt.Sprintf("❌ 创建 worktree 失败(#%d)", p.IssueID),
		Body:  body,
		Choices: []CardChoice{
			{Emoji: "🔄", Label: "重试", Action: "act:/gtw/worktree-retry"},
			{Emoji: "❌", Label: cancelLabel, Action: "act:/gtw/cancel"},
		},
	}
}
