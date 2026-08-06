package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/command/services"
)

// HandleDraftReaction is the per-draft action router. It is called
// by Manager.HandleReaction (which is the registered reaction
// handler at services.ReactionRouter startup).
//
// Replaces the pre-F-51 `HandleAction(ctx, deps, cs, slot,
// drafts, ev)` signature with `(ctx, m, deps, ev)` — the Manager
// owns context + drafts + per-chat Sender lookup, so the slot /
// drafts / cs parameters collapse into one *Manager.
//
// Returns (true, nil) when the reaction was consumed (a draft was
// found AND the emoji was one of the documented set for that draft
// kind — the executor actually performed an action). Returns
// (false, nil) when no draft matches OR the emoji is unrecognised;
// the caller falls through and the draft stays in place for the
// user to re-react.
func HandleDraftReaction(
	ctx context.Context,
	m *Manager,
	deps HandlerDeps,
	ev services.ReactionEvent,
) (bool, error) {
	slog.Default().Warn("F-46 debug: HandleDraftReaction entry",
		"target_msg_id", ev.TargetMsgID,
		"emoji", ev.Emoji,
		"chat_id", ev.ChatID)
	if ev.TargetMsgID == "" || ev.Emoji == "" {
		return false, nil
	}
	draft := m.GetDraft(ev.ChatID, ev.TargetMsgID)
	if draft == nil {
		slog.Default().Warn("F-46 debug: HandleDraftReaction draft not found",
			"target_msg_id", ev.TargetMsgID,
			"chat_id", ev.ChatID)
		return false, nil
	}
	slog.Default().Warn("F-46 debug: HandleDraftReaction draft found",
		"target_msg_id", ev.TargetMsgID,
		"draft_kind", string(draft.Kind),
		"bot_msg_id", draft.BotMessageID)
	sender := m.GetSender(ev.ChatID)
	switch draft.Kind {
	case DraftFixBranchExists:
		return executeBranchExistsAction(ctx, m, deps, sender, ev, draft), nil
	case DraftFixWorktreeFail:
		return executeWorktreeFailAction(ctx, m, deps, sender, ev, draft), nil
	case DraftFixLabelTaken:
		// Reserved for §5.3.2; not emitted by v1.
		return false, nil
	}
	slog.Default().Warn("F-46 debug: HandleDraftReaction draft kind not matched",
		"draft_kind", string(draft.Kind))
	return false, nil
}

// executeBranchExistsAction handles 🆕 / 🔗 / ❌ on the §5.3.1
// card. Returns true when the emoji was recognised and the action
// ran (and the draft was taken); false when the emoji was unknown
// and the draft is left in place for the user to re-react.
func executeBranchExistsAction(
	ctx context.Context,
	m *Manager,
	deps HandlerDeps,
	sender Sender,
	ev services.ReactionEvent,
	draft *Draft,
) bool {
	p := draft.Payload
	if p.Repo == "" {
		// Defensive: shouldn't happen (the runtime writes the
		// repo into the payload at emit time), but if it does
		// the draft is broken — take it and surface a clear
		// error so the user can re-run /gtw fix.
		m.TakeDraft(ev.ChatID, ev.TargetMsgID)
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), "❌ Internal error: draft missing repo.")
		return true
	}

	switch ReactionKind(ev.Emoji) {
	case ReactionCancel:
		m.TakeDraft(ev.ChatID, ev.TargetMsgID)
		rollbackOK := true
		resultText := fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)
		if p.LabelAdded {
			owner, repo, _ := splitOwnerRepo(p.Repo)
			provider, providerErr := NewProvider(ProviderKind(p.Provider), "")
			switch {
			case providerErr != nil || provider == nil:
				resultText = fmt.Sprintf(
					"⚠️ Cancelled fix #%d locally, but could not reach the provider to remove `nightme/wip` label: %v\n  Manual cleanup: `gh issue edit %d --remove-label nightme/wip` (or `glab issue update %d --unlabel nightme/wip`).",
					p.IssueID, providerErr, p.IssueID, p.IssueID)
				rollbackOK = false
			default:
				_ = provider.RemoveLabel(ctx, owner, repo, p.IssueID, LabelWIP)
			}
		}
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), resultText)
		_ = rollbackOK
		return true

	case ReactionNewV2:
		m.TakeDraft(ev.ChatID, ev.TargetMsgID)
		repoRoot := repoRootFromSender(sender)
		resultText := ""
		for n := range 9 {
			n += 2 // 2..10 inclusive
			variant := BranchVariant(p.Branch, n)
			exists, err := BranchExists(ctx, repoRoot, variant, deps.Git)
			if err != nil {
				resultText = fmt.Sprintf("❌ git show-ref failed: %v", err)
				break
			}
			if exists {
				continue
			}
			worktree := WorktreePath(repoRoot, BranchVariant(p.Slug, n))
			if err := WorktreeAdd(ctx, repoRoot, variant, worktree, "HEAD", deps.Git); err != nil {
				resultText = fmt.Sprintf("❌ git worktree add: %v", err)
				break
			}
			if err := sender.SetActiveCwd(worktree); err != nil {
				resultText = fmt.Sprintf("❌ SetActiveCwd: %v", err)
				break
			}
			m.SetContext(ev.ChatID, Context{
				Issue:     p.IssueID,
				Branch:    variant,
				Worktree:  worktree,
				State:     StateFixing,
				UpdatedAt: deps.Now(),
			})
			resultText = fmt.Sprintf("✅ Fix #%d 就绪(使用 %s)。", p.IssueID, variant)
			break
		}
		if resultText == "" {
			resultText = "❌ Too many branch variants; please clean up locally."
		}
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), resultText)
		return true

	case ReactionJoin:
		m.TakeDraft(ev.ChatID, ev.TargetMsgID)
		repoRoot := repoRootFromSender(sender)
		existingPath, err := WorktreeListPath(ctx, repoRoot, p.Branch, deps.Git)
		resultText := ""
		if err != nil {
			resultText = fmt.Sprintf("❌ git worktree list: %v", err)
		} else if existingPath == "" {
			resultText = fmt.Sprintf("❌ Branch %s exists but no worktree holds it; run `git worktree add` manually.", p.Branch)
		} else if err := sender.SetActiveCwd(existingPath); err != nil {
			resultText = fmt.Sprintf("❌ SetActiveCwd: %v", err)
		} else {
			m.SetContext(ev.ChatID, Context{
				Issue:     p.IssueID,
				Branch:    p.Branch,
				Worktree:  existingPath,
				State:     StateFixing,
				UpdatedAt: deps.Now(),
			})
			resultText = fmt.Sprintf("✅ Joined existing worktree at %s.", existingPath)
		}
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), resultText)
		return true
	}
	// Unrecognised emoji on a known draft: leave the draft in
	// place for the user to react correctly.
	return false
}

// executeWorktreeFailAction handles 🔄 / ❌ on the §5.3.3 card.
func executeWorktreeFailAction(
	ctx context.Context,
	m *Manager,
	deps HandlerDeps,
	sender Sender,
	ev services.ReactionEvent,
	draft *Draft,
) bool {
	p := draft.Payload
	rk := ReactionKind(ev.Emoji)
	slog.Default().Warn("F-46 debug: executeWorktreeFailAction entry",
		"emoji", ev.Emoji,
		"reaction_kind", rk)

	switch rk {
	case ReactionCancel:
		slog.Default().Warn("F-46 debug: executeWorktreeFailAction → ReactionCancel")
		m.TakeDraft(ev.ChatID, ev.TargetMsgID)
		rollbackOK := true
		resultText := fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)
		if p.LabelAdded {
			owner, repo, _ := splitOwnerRepo(p.Repo)
			provider, providerErr := NewProvider(ProviderKind(p.Provider), "")
			switch {
			case providerErr != nil || provider == nil:
				resultText = fmt.Sprintf(
					"⚠️ Cancelled fix #%d locally, but could not reach the provider to remove `nightme/wip` label: %v\n  Manual cleanup: `gh issue edit %d --remove-label nightme/wip` (or `glab issue update %d --unlabel nightme/wip`).",
					p.IssueID, providerErr, p.IssueID, p.IssueID)
				rollbackOK = false
			default:
				_ = provider.RemoveLabel(ctx, owner, repo, p.IssueID, LabelWIP)
			}
		}
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), resultText)
		_ = rollbackOK
		return true

	case ReactionRetry:
		slog.Default().Warn("F-46 debug: executeWorktreeFailAction → ReactionRetry")
		m.TakeDraft(ev.ChatID, ev.TargetMsgID)
		repoRoot := repoRootFromSender(sender)
		worktree := WorktreePath(repoRoot, p.Slug)
		err := WorktreeAdd(ctx, repoRoot, p.Branch, worktree, "HEAD", deps.Git)
		resultText := ""
		if err != nil {
			resultText = fmt.Sprintf("❌ Retry failed: %v", err)
		} else if err := sender.SetActiveCwd(worktree); err != nil {
			resultText = fmt.Sprintf("❌ SetActiveCwd: %v", err)
		} else {
			m.SetContext(ev.ChatID, Context{
				Issue:     p.IssueID,
				Branch:    p.Branch,
				Worktree:  worktree,
				State:     StateFixing,
				UpdatedAt: deps.Now(),
			})
			resultText = fmt.Sprintf("✅ Fix #%d 就绪(重试成功)。", p.IssueID)
		}
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), resultText)
		return true
	}
	return false
}

// emitFollowUp is the F-46 single-sink for action-handler
// outcomes. When the dispatched draft has a bot-side message id
// (i.e. the dispatcher sent an interactive card), it emits a
// PATCH that disables the original card and appends a result
// line. When the dispatcher never sent a card (legacy text or
// fallback), it emits a plain text reply preserved from F-45.
func emitFollowUp(
	ctx context.Context,
	deps HandlerDeps,
	draft *Draft,
	ev services.ReactionEvent,
	chosenEmoji string,
	resultText string,
) {
	if draft.BotMessageID != "" {
		_ = deps.Send(ctx, OutMsg{
			ChatID:            ev.ChatID,
			PatchBotMsgID:     draft.BotMessageID,
			PatchChosenEmoji:  chosenEmoji,
			PatchResult:       resultText,
			CardTitle:         draft.CardTitle,
			CardBody:          draft.CardBody,
			CardChoices:       draft.CardChoices,
			CardRequestID:     draft.CardRequestID,
			ChosenChoiceEmoji: chosenEmoji,
		})
		return
	}
	_ = deps.Send(ctx, OutMsg{
		ChatID:  ev.ChatID,
		ReplyTo: ev.TargetMsgID,
		Text:    resultText,
	})
}

// splitOwnerRepo splits "owner/repo" into its two parts.
func splitOwnerRepo(s string) (string, string, error) {
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("gtw: invalid owner/repo %q", s)
	}
	return s[:idx], s[idx+1:], nil
}

// repoRootFromSender returns the repo root given the active cwd.
//
// gtw's worktree layout is sibling/<repo>.nightme/<slug>/, so the
// active cwd (a specific worktree) is 2 levels below the worktree
// parent + 1 level above the repo. The repo is the worktree
// parent with the `.nightme` suffix stripped.
func repoRootFromSender(sender Sender) string {
	if sender == nil {
		return ""
	}
	cwd := sender.ActiveCwd()
	if cwd == "" {
		return ""
	}
	worktreeParent := filepath.Dir(cwd)
	if strings.HasSuffix(filepath.Base(worktreeParent), ".nightme") {
		return strings.TrimSuffix(worktreeParent, ".nightme")
	}
	return worktreeParent
}
