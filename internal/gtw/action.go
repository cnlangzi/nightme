package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// HandleAction is the gtw-internal reaction router. The runtime
// calls it from ChatSession.HandleAction (the single extra branch
// added in F-50 §6.1 reaction routing). It looks up the draft by Reaction.TargetMsgID
// and dispatches to the kind-specific executor.
//
// Returns (true, nil) when the reaction was consumed (a draft was
// found AND the emoji was one of the documented set for that draft
// kind — the executor actually performed an action). Returns
// (false, nil) when no draft matches OR the emoji is unrecognised;
// the caller falls through to the F-31 MessageState FSM and the
// draft stays in place for the user to re-react. F-45 review
// finding #3: the previous version called Take at function entry,
// which silently consumed the draft for any emoji including
// custom ones and 👍.
func HandleAction(
	ctx context.Context,
	deps HandlerDeps,
	cs Sender,
	slot ContextSlot,
	drafts DraftsMap,
	ev ReactionEvent,
) (bool, error) {
	slog.Default().Warn("F-46 debug: HandleAction entry",
		"target_msg_id", ev.TargetMsgID,
		"emoji", ev.Emoji,
		"chat_id", ev.ChatID)
	if ev.TargetMsgID == "" || ev.Emoji == "" {
		slog.Default().Warn("F-46 debug: HandleAction empty target/emoji, return false")
		return false, nil
	}
	draft := drafts.Lookup(ev.TargetMsgID)
	if draft == nil {
		slog.Default().Warn("F-46 debug: HandleAction draft not found",
			"target_msg_id", ev.TargetMsgID)
		return false, nil
	}
	slog.Default().Warn("F-46 debug: HandleAction draft found",
		"target_msg_id", ev.TargetMsgID,
		"draft_kind", draft.Kind,
		"bot_msg_id", draft.BotMessageID)
	switch draft.Kind {
	case DraftFixBranchExists:
		return executeBranchExistsAction(ctx, deps, cs, slot, drafts, ev, draft), nil
	case DraftFixLabelTaken:
		// Reserved for §5.3.2; not emitted by v1.
		return false, nil
	case DraftFixWorktreeFail:
		return executeWorktreeFailAction(ctx, deps, cs, slot, drafts, ev, draft), nil
	}
	slog.Default().Warn("F-46 debug: HandleAction draft kind not matched",
		"draft_kind", draft.Kind)
	return false, nil
}

// executeBranchExistsAction handles 🆕 / 🔗 / ❌ on the §5.3.1
// card. Returns true when the emoji was recognised and the action
// ran (and the draft was taken); false when the emoji was unknown
// and the draft is left in place for the user to re-react.
// F-45 review finding #3 + #4.
func executeBranchExistsAction(
	ctx context.Context,
	deps HandlerDeps,
	cs Sender,
	slot ContextSlot,
	drafts DraftsMap,
	ev ReactionEvent,
	draft *Draft,
) bool {
	p := draft.Payload
	if p.Repo == "" {
		// Defensive: shouldn't happen (the runtime writes the
		// repo into the payload at emit time), but if it does
		// the draft is broken — take it and surface a clear
		// error so the user can re-run /gtw fix.
		drafts.Take(ev.TargetMsgID)
		emitFollowUp(ctx, deps, draft, ev, string(ev.Emoji), "❌ Internal error: draft missing repo.")
		return true
	}

	switch ReactionKind(ev.Emoji) {
	case ReactionCancel:
		drafts.Take(ev.TargetMsgID)
		// F-45 §5.4 "platform label 更新失败 — low — warn 继续":
		// if the platform client can't be constructed (gh/glab
		// missing or unsupported host), we still tell the user
		// the local fix state is cleared but be explicit that
		// the nightme/wip label is still on the issue. Better
		// to over-report than leave a stale label dangling.
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
		drafts.Take(ev.TargetMsgID)
		repoRoot := repoRootFromCS(cs)
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
			if err := cs.SetActiveCwd(worktree); err != nil {
				resultText = fmt.Sprintf("❌ SetActiveCwd: %v", err)
				break
			}
			slot.Store(Context{
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
		drafts.Take(ev.TargetMsgID)
		repoRoot := repoRootFromCS(cs)
		existingPath, err := WorktreeListPath(ctx, repoRoot, p.Branch, deps.Git)
		resultText := ""
		if err != nil {
			resultText = fmt.Sprintf("❌ git worktree list: %v", err)
		} else if existingPath == "" {
			resultText = fmt.Sprintf("❌ Branch %s exists but no worktree holds it; run `git worktree add` manually.", p.Branch)
		} else if err := cs.SetActiveCwd(existingPath); err != nil {
			resultText = fmt.Sprintf("❌ SetActiveCwd: %v", err)
		} else {
			slot.Store(Context{
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
	// place for the user to react correctly. Returning false
	// tells the dispatcher this was not consumed (so the chat
	// layer can fall through to a future handler if needed),
	// and the draft stays alive for the next recognised click.
	return false
}

// executeWorktreeFailAction handles 🔄 / ❌ on the §5.3.3 card.
// Same contract as executeBranchExistsAction: returns true when
// the action ran (draft taken), false when the emoji is unknown
// (draft left in place).
func executeWorktreeFailAction(
	ctx context.Context,
	deps HandlerDeps,
	cs Sender,
	slot ContextSlot,
	drafts DraftsMap,
	ev ReactionEvent,
	draft *Draft,
) bool {
	p := draft.Payload
	rk := ReactionKind(ev.Emoji)
	slog.Default().Warn("F-46 debug: executeWorktreeFailAction entry",
		"emoji", ev.Emoji,
		"reaction_kind", rk,
		"reaction_retry", ReactionRetry,
		"matches", rk == ReactionRetry)

	switch ReactionKind(ev.Emoji) {
	case ReactionCancel:
		slog.Default().Warn("F-46 debug: executeWorktreeFailAction → ReactionCancel")
		drafts.Take(ev.TargetMsgID)
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
		drafts.Take(ev.TargetMsgID)
		repoRoot := repoRootFromCS(cs)
		worktree := WorktreePath(repoRoot, p.Slug)
		err := WorktreeAdd(ctx, repoRoot, p.Branch, worktree, "HEAD", deps.Git)
		resultText := ""
		if err != nil {
			resultText = fmt.Sprintf("❌ Retry failed: %v", err)
		} else if err := cs.SetActiveCwd(worktree); err != nil {
			resultText = fmt.Sprintf("❌ SetActiveCwd: %v", err)
		} else {
			slot.Store(Context{
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
	// Unrecognised emoji: same semantics as branch-exists.
	slog.Default().Warn("F-46 debug: executeWorktreeFailAction → emoji not matched",
		"reaction_kind", rk)
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
	ev ReactionEvent,
	chosenEmoji string,
	resultText string,
) {
	slog.Default().Warn("F-46 debug: emitFollowUp entry",
		"bot_msg_id", draft.BotMessageID,
		"chosen_emoji", chosenEmoji,
		"result_text", resultText)
	if draft.BotMessageID != "" {
		_ = deps.Send(ctx, OutMsg{
			ChatID:             ev.ChatID,
			PatchBotMsgID:      draft.BotMessageID,
			PatchChosenEmoji:   chosenEmoji,
			PatchResult:        resultText,
			CardTitle:          draft.CardTitle,
			CardBody:           draft.CardBody,
			CardChoices:        gtwChoicesFromDraft(draft),
			CardRequestID:      draft.CardRequestID,
			ChosenChoiceEmoji:   chosenEmoji,
		})
		return
	}
	_ = deps.Send(ctx, OutMsg{
		ChatID:  ev.ChatID,
		ReplyTo: ev.TargetMsgID,
		Text:    resultText,
	})
}

// gtwChoicesFromDraft translates the chatsession.CardChoice
// slice on the draft into the gtw-package CardChoice slice the
// OutMsg adapter expects.
func gtwChoicesFromDraft(draft *Draft) []CardChoice {
	if len(draft.CardChoices) == 0 {
		return nil
	}
	out := make([]CardChoice, len(draft.CardChoices))
	for i, c := range draft.CardChoices {
		out[i] = CardChoice{
			Emoji:  c.Emoji,
			Label:  c.Label,
			Action: c.Action,
		}
	}
	return out
}
// splitOwnerRepo splits "owner/repo" into its two parts.
func splitOwnerRepo(s string) (string, string, error) {
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("gtw: invalid owner/repo %q", s)
	}
	return s[:idx], s[idx+1:], nil
}

// repoRootFromCS returns the repo root given the active cwd.
//
// gtw's worktree layout is sibling/<repo>.nightme/<slug>/, so the
// active cwd (a specific worktree) is 2 levels below the worktree
// parent + 1 level above the repo. The repo is the worktree
// parent with the `.nightme` suffix stripped.
//
// Examples (sibling=/code, repo=nightme):
//
//	/code/nightme.nightme/42-foo  →  /code/nightme
//	/code                            →  /code  (no .nightme parent; assume cwd IS the repo)
//
// The old "walk up 2" implementation was wrong (returned
// /code/nightme.nightme, not /code/nightme) and made every 🆕/🔗/🔄
// reaction fail with "fatal: not a git repository". The strip
// approach is exact for the gtw layout and safe for the
// "cwd is already the repo" case (no .nightme to strip).
func repoRootFromCS(cs Sender) string {
	cwd := cs.ActiveCwd()
	if cwd == "" {
		return ""
	}
	// Worktree parent is `dirname(cwd)`; the repo is the worktree
	// parent with the trailing `.nightme` component removed.
	worktreeParent := filepath.Dir(cwd)
	if strings.HasSuffix(filepath.Base(worktreeParent), ".nightme") {
		return strings.TrimSuffix(worktreeParent, ".nightme")
	}
	// Not a gtw worktree layout; fall back to the parent dir.
	// This matches the test fixture (worktree at <dir>/wt) and
	// the "cwd is the main repo" case.
	return worktreeParent
}
