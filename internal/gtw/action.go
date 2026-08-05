package gtw

import (
	"context"
	"fmt"
	"strings"
)

// HandleAction is the gtw-internal reaction router. The runtime
// calls it from ChatSession.HandleAction (the single extra branch
// added in F-45 §3.5). It looks up the draft by Reaction.TargetMsgID
// and dispatches to the kind-specific executor.
//
// Returns (true, nil) when the reaction was consumed (a draft was
// found and executed). Returns (false, nil) when no draft matches —
// the caller should fall through to the F-31 MessageState FSM.
func HandleAction(
	ctx context.Context,
	deps HandlerDeps,
	cs Sender,
	slot ContextSlot,
	drafts DraftsMap,
	ev ReactionEvent,
) (bool, error) {
	if ev.TargetMsgID == "" || ev.Emoji == "" {
		return false, nil
	}
	draft := drafts.Lookup(ev.TargetMsgID)
	if draft == nil {
		return false, nil
	}
	switch draft.Kind {
	case DraftFixBranchExists:
		executeBranchExistsReaction(ctx, deps, cs, slot, drafts, ev, draft)
	case DraftFixLabelTaken:
		// Reserved for §5.3.2; not emitted by v1.
	case DraftFixWorktreeFail:
		executeWorktreeFailReaction(ctx, deps, cs, slot, drafts, ev, draft)
	}
	return true, nil
}

// executeBranchExistsReaction handles 🆕 / 🔗 / ❌ on the §5.3.1 card.
func executeBranchExistsReaction(
	ctx context.Context,
	deps HandlerDeps,
	cs Sender,
	slot ContextSlot,
	drafts DraftsMap,
	ev ReactionEvent,
	draft *Draft,
) {
	drafts.Take(ev.TargetMsgID)
	p := draft.Payload
	if p.Repo == "" {
		_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID, Text: "❌ Internal error: draft missing repo."})
		return
	}

	switch ReactionKind(ev.Emoji) {
	case ReactionCancel:
		if p.LabelAdded {
			owner, repo, _ := splitOwnerRepo(p.Repo)
			plat, _ := deps.NewPlatform(PlatformKind(p.Platform))
			if plat != nil {
				_ = plat.RemoveLabel(ctx, owner, repo, p.IssueID, LabelWIP)
			}
		}
		_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
			Text: fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)})
		return

	case ReactionNewV2:
		repoRoot := repoRootFromCS(cs)
		for n := range 9 {
			n += 2 // 2..10 inclusive
			variant := BranchVariant(p.Branch, n)
			exists, err := BranchExists(ctx, repoRoot, variant, deps.Git)
			if err != nil {
				_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
					Text: fmt.Sprintf("❌ git show-ref failed: %v", err)})
				return
			}
			if exists {
				continue
			}
			worktree := WorktreePath(repoRoot, BranchVariant(p.Slug, n))
			if err := WorktreeAdd(ctx, repoRoot, variant, worktree, "HEAD", deps.Git); err != nil {
				_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
					Text: fmt.Sprintf("❌ git worktree add: %v", err)})
				return
			}
			if err := cs.SetActiveCwd(worktree); err != nil {
				_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
					Text: fmt.Sprintf("❌ SetActiveCwd: %v", err)})
				return
			}
			slot.Store(Context{
				Issue:     p.IssueID,
				Branch:    variant,
				Worktree:  worktree,
				State:     StateFixing,
				UpdatedAt: deps.Now(),
			})
			_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
				Text: fmt.Sprintf("✅ Fix #%d 就绪(使用 %s)。", p.IssueID, variant)})
			return
		}
		_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
			Text: "❌ Too many branch variants; please clean up locally."})
		return

	case ReactionJoin:
		repoRoot := repoRootFromCS(cs)
		existingPath, err := WorktreeListPath(ctx, repoRoot, p.Branch, deps.Git)
		if err != nil {
			_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
				Text: fmt.Sprintf("❌ git worktree list: %v", err)})
			return
		}
		if existingPath == "" {
			_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
				Text: fmt.Sprintf("❌ Branch %s exists but no worktree holds it; run `git worktree add` manually.", p.Branch)})
			return
		}
		if err := cs.SetActiveCwd(existingPath); err != nil {
			_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
				Text: fmt.Sprintf("❌ SetActiveCwd: %v", err)})
			return
		}
		slot.Store(Context{
			Issue:     p.IssueID,
			Branch:    p.Branch,
			Worktree:  existingPath,
			State:     StateFixing,
			UpdatedAt: deps.Now(),
		})
		_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
			Text: fmt.Sprintf("✅ Joined existing worktree at %s.", existingPath)})
		return
	}
	// Unknown emoji on a known draft: silently consume.
}

// executeWorktreeFailReaction handles 🔄 / ❌ on the §5.3.3 card.
func executeWorktreeFailReaction(
	ctx context.Context,
	deps HandlerDeps,
	cs Sender,
	slot ContextSlot,
	drafts DraftsMap,
	ev ReactionEvent,
	draft *Draft,
) {
	drafts.Take(ev.TargetMsgID)
	p := draft.Payload

	switch ReactionKind(ev.Emoji) {
	case ReactionCancel:
		if p.LabelAdded {
			owner, repo, _ := splitOwnerRepo(p.Repo)
			plat, _ := deps.NewPlatform(PlatformKind(p.Platform))
			if plat != nil {
				_ = plat.RemoveLabel(ctx, owner, repo, p.IssueID, LabelWIP)
			}
		}
		_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
			Text: fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)})
		return

	case ReactionRetry:
		repoRoot := repoRootFromCS(cs)
		worktree := WorktreePath(repoRoot, p.Slug)
		err := WorktreeAdd(ctx, repoRoot, p.Branch, worktree, "HEAD", deps.Git)
		if err != nil {
			_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
				Text: fmt.Sprintf("❌ Retry failed: %v", err)})
			return
		}
		if err := cs.SetActiveCwd(worktree); err != nil {
			_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
				Text: fmt.Sprintf("❌ SetActiveCwd: %v", err)})
			return
		}
		slot.Store(Context{
			Issue:     p.IssueID,
			Branch:    p.Branch,
			Worktree:  worktree,
			State:     StateFixing,
			UpdatedAt: deps.Now(),
		})
		_ = deps.Send(ctx, OutMsg{ChatID: ev.ChatID,
			Text: fmt.Sprintf("✅ Fix #%d 就绪(重试成功)。", p.IssueID)})
		return
	}
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
func repoRootFromCS(cs Sender) string {
	cwd := cs.ActiveCwd()
	if cwd == "" {
		return ""
	}
	from := cwd
	for range 2 {
		idx := strings.LastIndex(from, "/")
		if idx < 0 {
			return from
		}
		from = from[:idx]
	}
	return from
}
