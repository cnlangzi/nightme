package gtw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
)

// HandleDraftReaction is the per-draft action router. It is called
// by Manager.HandleReaction (which is the registered reaction
// handler at services.ReactionRouter startup). The ChatSession
// reference is supplied by the runtime-layer wrapper — gtw does
// NOT do cs lookup itself.
//
// Returns (true, nil) when the reaction was consumed (a draft was
// found AND the emoji was one of the documented set for that draft
// kind — the executor actually performed an action). Returns
// (false, nil) when no draft matches OR the emoji is unrecognised;
// the caller falls through and the draft stays in place for the
// user to re-react.
//
// F-XX: the per-chat Sender interface is gone; executors take
// *chatsession.ChatSession directly so they can call
// SetSelectedCwd without going through an interface adapter.
func HandleDraftReaction(
	ctx context.Context,
	m *Manager,
	deps HandlerDeps,
	cs *chatsession.ChatSession,
	ev services.ReactionEvent,
) (bool, error) {
	slog.Default().Warn("F-46 debug: HandleDraftReaction entry",
		"request_id", ev.RequestID,
		"target_msg_id", ev.TargetMsgID,
		"emoji", ev.Emoji,
		"chat_id", ev.ChatID)
	if ev.RequestID == "" || ev.Emoji == "" {
		return false, nil
	}
	draft := m.GetDraft(ev.ChatID, ev.RequestID)
	if draft == nil {
		slog.Default().Warn("F-46 debug: HandleDraftReaction draft not found",
			"request_id", ev.RequestID,
			"chat_id", ev.ChatID)
		return false, nil
	}
	slog.Default().Warn("F-46 debug: HandleDraftReaction draft found",
		"request_id", ev.RequestID,
		"draft_kind", string(draft.Kind),
		"choice_posted", draft.ChoicePosted)
	switch draft.Kind {
	case DraftFixWorktreeFail:
		return executeWorktreeFailAction(ctx, m, deps, cs, ev, draft), nil
	}
	slog.Default().Warn("F-46 debug: HandleDraftReaction draft kind not matched",
		"draft_kind", string(draft.Kind))
	return false, nil
}

// executeWorktreeFailAction handles 🔄 / ❌ on the §5.3.3 card.
func executeWorktreeFailAction(
	ctx context.Context,
	m *Manager,
	deps HandlerDeps,
	cs *chatsession.ChatSession,
	ev services.ReactionEvent,
	draft *Draft,
) bool {
	p := draft.Payload
	rk := messages.ReactionKind(ev.Emoji)
	slog.Default().Warn("F-46 debug: executeWorktreeFailAction entry",
		"emoji", ev.Emoji,
		"reaction_kind", rk)

	switch rk {
	case messages.ReactionCancel:
		slog.Default().Warn("F-46 debug: executeWorktreeFailAction → ReactionCancel")
		m.TakeDraft(ev.ChatID, ev.RequestID)
		resultText := cancelResultText(p)
		// Label rollback only applies to ID-mode drafts (local
		// mode never added a label). resultText is set to a
		// "manual cleanup" hint when the provider is unreachable
		// so the user knows the label was NOT removed.
		if p.LabelAdded && p.Repo != "" {
			owner, repo, _ := splitOwnerRepo(p.Repo)
			provider, providerErr := NewProvider(ProviderKind(p.Provider), "", p.Worktree)
			switch {
			case providerErr != nil || provider == nil:
				resultText = fmt.Sprintf(
					"⚠️ Cancelled fix #%d locally, but could not reach the provider to remove `nightme/wip` label: %v\n  Manual cleanup: `gh issue edit %d --remove-label nightme/wip` (or `glab issue update %d --unlabel nightme/wip`).",
					p.IssueID, providerErr, p.IssueID, p.IssueID)
			default:
				_ = provider.RemoveIssueLabel(ctx, owner, repo, p.IssueID, LabelWIP)
			}
		}
		emitFollowUp(ctx, cs, draft, ev, string(ev.Emoji), resultText)
		return true

	case messages.ReactionRetry:
		slog.Default().Warn("F-46 debug: executeWorktreeFailAction → ReactionRetry")
		m.TakeDraft(ev.ChatID, ev.RequestID)
		// p.Worktree is the main repo root that emitWorktreeFailDraft
		// stored at draft-emission time — the cancel branch already
		// trusts it for `gh issue edit` cwd, so it's authoritative
		// here too. Deriving from cs.SelectedCwd() via
		// repoRootFromChatSession is wrong when the user is in the
		// main repo (the typical fix-failure case): the helper
		// assumes cwd is inside a <repo>.nightme/<slug> worktree
		// and returns the parent dir otherwise — not the repo root.
		repoRoot := p.Worktree
		worktree := WorktreePath(repoRoot, p.Slug)
		err := WorktreeAdd(ctx, repoRoot, p.Branch, worktree, "HEAD", deps.Git)
		resultText := ""
		if err != nil {
			resultText = fmt.Sprintf("❌ Retry failed: %v", err)
		} else if err := cs.SetSelectedCwd(worktree); err != nil {
			resultText = fmt.Sprintf("❌ SetSelectedCwd: %v", err)
		} else {
			// The retry succeeded — worktree exists and cwd is
			// pointing at it. The retry path bypasses
			// completeFixAndDispatch (no issue dispatch here), so
			// we have to run the standard post-WorktreeAdd cleanup
			// ourselves: ensure .gitignore lists .nightme/, commit
			// it if dirty (so /gtw close's dirty check passes),
			// then write the yml snapshot. Without the yml, /gtw
			// close would say "no active fix to close"; without
			// the gitignore commit, it would say "dirty worktree"
			// and the user would be stuck needing --force.
			//
			// v1.5: the yml is the cwd-scoped source of truth;
			// the in-memory slot layer that used to be set here
			// is gone.
			ymlCtx := Context{
				Mode:     ModeFromDraftPayload(p),
				Issue:    p.IssueID,
				Branch:   p.Branch,
				Worktree: worktree,
				RepoRoot: repoRoot,
				Repo:     p.Repo,
				Provider: p.Provider,
			}
			if err := EnsureGitignore(worktree); err != nil {
				slog.Default().Warn("gtw retry: EnsureGitignore failed",
					"worktree", worktree, "err", err)
				resultText = variantReadyResultText(p, p.Branch)
			} else if err := CommitGitignoreIfDirty(ctx, worktree, deps.Git); err != nil {
				slog.Default().Warn("gtw retry: CommitGitignore failed",
					"worktree", worktree, "err", err)
				// Roll back: worktree is unusable without the
				// commit (close will reject as dirty). force=true
				// because the untracked .gitignore is in git's way.
				if rmErr := WorktreeRemove(ctx, repoRoot, worktree, true /* force */, deps.Git); rmErr != nil {
					resultText = fmt.Sprintf(
						"❌ Retry: CommitGitignore failed (%v); rollback also failed (%v).\n"+
							"the worktree at %s is in a stuck state — please `git worktree remove --force %s` manually.",
						err, rmErr, worktree, worktree)
				} else {
					resultText = fmt.Sprintf(
						"❌ Retry: CommitGitignore failed (%v).\n"+
							"rolled back worktree at %s; retry after fixing (e.g. set `git config user.email`).",
						err, worktree)
				}
			} else if err := WriteGTWYml(worktree, ymlCtx, deps.Now); err != nil && !errors.Is(err, ErrGtwYmlExists) {
				slog.Default().Warn("gtw retry: WriteGTWYml failed",
					"worktree", worktree, "err", err)
				// Worktree is usable; just warn that close may not
				// recognise it. User can finish manually.
				resultText = variantReadyResultText(p, p.Branch) +
					fmt.Sprintf("\n⚠️ could not write .nightme/gtw.yml: %v", err)
			} else {
				resultText = variantReadyResultText(p, p.Branch)
			}
		}
		emitFollowUp(ctx, cs, draft, ev, string(ev.Emoji), resultText)
		return true
	}
	return false
}

// emitFollowUp is the single-sink for action-handler outcomes.
// When the dispatcher posted a choice prompt, it emits
// OutChoicePatch keyed by Choice.RequestID so Channel can PATCH in
// place. When the choice path failed (plain-text fallback), it
// emits a plain text reply.
func emitFollowUp(
	ctx context.Context,
	cs *chatsession.ChatSession,
	draft *Draft,
	ev services.ReactionEvent,
	chosenEmoji string,
	resultText string,
) {
	if cs == nil {
		return
	}
	em := cs.Emitter()
	if em == nil {
		return
	}
	if draft.ChoicePosted {
		selectedID := ""
		for _, opt := range draft.ChoiceOptions {
			if opt.Emoji == chosenEmoji {
				selectedID = opt.ID
				break
			}
		}
		_ = em.Send(ctx, messages.OutboundMessage{
			ChatID: ev.ChatID,
			Kind:   messages.OutChoicePatch,
			Text:   resultText,
			Choice: &messages.Choice{
				Kind:       messages.ChoiceKindDecision,
				Title:      draft.ChoiceTitle,
				Body:       draft.ChoiceBody,
				Options:    draft.ChoiceOptions,
				RequestID:  draft.ChoiceRequestID,
				Settled:    true,
				SelectedID: selectedID,
			},
		})
		return
	}
	_ = em.Send(ctx, messages.OutboundMessage{
		ChatID:  ev.ChatID,
		Kind:    messages.OutReply,
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

// repoRootFromChatSession is removed in v1.5. The retry path
// used to call this helper but the implementation only worked
// when cs.SelectedCwd was inside a <repo>.nightme/<slug>
// worktree; for the more common case of cwd == main repo root
// it returned the parent directory, which is not the repo
// root. The retry now uses p.Worktree directly — the value
// emitWorktreeFailDraft stored at draft-emission time.

// ModeFromDraftPayload infers the Mode for the new Context
// being written by an action handler. Local-mode drafts (the
// local branch flow) have IssueID == -1; everything else is
// Remote.
func ModeFromDraftPayload(p FixDraftPayload) Mode {
	if p.IssueID == -1 {
		return ModeLocal
	}
	return ModeRemote
}

// cancelResultText formats the user-visible "cancelled"
// reply. Local-mode drafts omit the "#<id>" prefix; ID-mode
// drafts include it.
func cancelResultText(p FixDraftPayload) string {
	if p.IssueID == -1 {
		return "❌ Cancelled local worktree."
	}
	return fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)
}

// variantReadyResultText formats the "you now have a fresh
// worktree / variant branch" reply. Local mode omits the
// "#<id>" reference; ID mode keeps it.
func variantReadyResultText(p FixDraftPayload, branch string) string {
	if p.IssueID == -1 {
		return fmt.Sprintf("✅ Local worktree ready (using `%s`).", branch)
	}
	return fmt.Sprintf("✅ Fix #%d ready (using `%s`).", p.IssueID, branch)
}
