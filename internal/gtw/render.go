package gtw

import (
	"fmt"
	"strings"
)

// renderFixSuccessCard builds the §5.2.⑥ success card. Layout is
//
//	[Bot] ✅ Fix #42 就绪
//	      ━━━━━━━━━━━━━━━━
//	      [Context]
//	      🌿 branch:   fix/42-login-state-expiration
//	      📁 worktree: ~/code/nightme.nightme/42-login-state-expiration
//	      🏷 平台:cnlangzi/nightme#42 [nightme/wip]
//	      ━━━━━━━━━━━━━━━━
//	      [Command result]
//	      💡 下一步:/gtw push 或 /gtw pr
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

// renderBranchExistsCard builds the §5.3.1 decision card. The
// existingPath is empty when the branch is in a non-default
// worktree (rare in v1; we still render the card with the path
// blank).
func renderBranchExistsCard(p FixDraftPayload, existingPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ 分支 `%s` 已存在\n", p.Branch)
	b.WriteString("━━━━━━━━━━━━━━\n")
	fmt.Fprintf(&b, "issue: #%d  %s\n", p.IssueID, p.Title)
	if existingPath != "" {
		fmt.Fprintf(&b, "已有 worktree: %s\n", existingPath)
	}
	b.WriteString("━━━━━━━━━━━━━━\n")
	b.WriteString("选择操作(反应对应 emoji):\n")
	b.WriteString("  🆕 用 -v2 新分支\n")
	b.WriteString("  🔗 加入现有协作(切到已有 worktree)\n")
	b.WriteString("  ❌ 取消\n")
	return b.String()
}

// renderWorktreeFailCard builds the §5.3.3 decision card.
func renderWorktreeFailCard(p FixDraftPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "❌ 创建 worktree 失败(#%d)\n", p.IssueID)
	b.WriteString("━━━━━━━━━━━━━━\n")
	fmt.Fprintf(&b, "branch: %s\n", p.Branch)
	if p.GitError != "" {
		b.WriteString("[git stderr tail]\n")
		b.WriteString(p.GitError)
		b.WriteString("\n")
	}
	b.WriteString("━━━━━━━━━━━━━━\n")
	b.WriteString("选择操作(反应对应 emoji):\n")
	b.WriteString("  🔄 重试\n")
	b.WriteString("  ❌ 取消")
	if p.LabelAdded {
		b.WriteString(" (已撤销 nightme/wip label)")
	}
	b.WriteString("\n")
	return b.String()
}
