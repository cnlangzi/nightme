package gtw

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/pathutil"
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
	case DraftFixLabelTaken:
		// Reserved for §5.3.2; not emitted by v1.
		return false, nil
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
		repoRoot := repoRootFromChatSession(cs)
		worktree := WorktreePath(repoRoot, p.Slug)
		err := WorktreeAdd(ctx, repoRoot, p.Branch, worktree, "HEAD", deps.Git)
		resultText := ""
		if err != nil {
			resultText = fmt.Sprintf("❌ Retry failed: %v", err)
		} else if err := cs.SetSelectedCwd(worktree); err != nil {
			resultText = fmt.Sprintf("❌ SetSelectedCwd: %v", err)
		} else {
			m.SetContext(ev.ChatID, Context{
				Mode:      ModeFromDraftPayload(p),
				Issue:     p.IssueID,
				Branch:    p.Branch,
				Worktree:  worktree,
				State:     StateFixing,
				UpdatedAt: deps.Now(),
			})
			resultText = variantReadyResultText(p, p.Branch)
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

// repoRootFromChatSession returns the repo root given the active
// cwd on the ChatSession.
//
// gtw's worktree layout is sibling/<repo>.nightme/<slug>/, so the
// active cwd (a specific worktree) is 2 levels below the worktree
// parent + 1 level above the repo. The repo is the worktree
// parent with the `.nightme` suffix stripped.
//
// F-XX: replaces repoRootFromSender; takes
// *chatsession.ChatSession directly.
func repoRootFromChatSession(cs *chatsession.ChatSession) string {
	if cs == nil {
		return ""
	}
	cwd := cs.SelectedCwd()
	if cwd == "" {
		return ""
	}
	// F-PATHUTIL-001 §13.3.1: pathutil.Dir / pathutil.Base for
	// sibling-worktree detection. cwd comes from SelectedCwd
	// (already-normalized) so the result is equivalent to
	// filepath.*; using pathutil keeps the rule "no caller
	// inlines filepath" honest.
	worktreeParent := pathutil.Dir(cwd)
	if strings.HasSuffix(pathutil.Base(worktreeParent), ".nightme") {
		return strings.TrimSuffix(worktreeParent, ".nightme")
	}
	return worktreeParent
}

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
