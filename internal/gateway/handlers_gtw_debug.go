package gateway

// Debug / Feishu UAT helpers for `/gtw test`.
//
// Ownership boundary — keep this file free of business card shapes:
//
//   - Card Title / Body / Choices come ONLY from internal/gtw
//     (BranchExistsCard / WorktreeFailCard), the same builders
//     `/gtw fix` uses.
//   - This file seeds synthetic drafts, sends those cards, and waits
//     for a real Feishu click. It must not invent act:/gtw/... Actions
//     or parallel markdown layouts.
//
// CI asserts setup only (card + draft + click instructions). Full
// reaction/PATCH needs a real Feishu session.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gtw"
)

// runGTWTest is the `/gtw test` entry point (debug / Feishu UAT only).
func runGTWTest(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
	deps gtw.HandlerDeps,
	gw Gateway,
) (*CommandResult, error) {
	if len(args) < 1 {
		return runGTWTestHelp(ctx, channel, msg), nil
	}
	switch args[0] {
	case "list":
		return runGTWTestList(ctx, mgr, channel, msg)
	case "reset":
		return runGTWTestReset(ctx, mgr, channel, msg)
	default:
		return runGTWTestScenario(ctx, mgr, channel, msg, args, deps, gw)
	}
}

// gtwTestScenario maps a `/gtw test <name>` preset to the draft kind
// it exercises. Card chrome is NOT stored here — it comes from gtw.*.
type gtwTestScenario struct {
	Name           string
	Description    string
	SetupUserMsgID string
	SetupDraft     string // branch-exists | worktree-fail | "" (orphan/preview)
	SetupEmoji     string // suggested click; empty = preview-only
}

var gtwTestScenarios = []gtwTestScenario{
	{
		Name:           "card",
		Description:    "preview — render the branch-exists interactive card so the user can see its shape",
		SetupUserMsgID: "om_test_card",
	},
	{
		Name:           "ok",
		Description:    "single-choice 🔄 — worktree-fail retry, draft is taken and consumed",
		SetupUserMsgID: "om_test_ok",
		SetupDraft:     "worktree-fail",
		SetupEmoji:     "🔄",
	},
	{
		Name:           "yes-no",
		Description:    "two-choice ❌/🔄 — one recognised emoji acts, one unknown leaves draft",
		SetupUserMsgID: "om_test_yes_no",
		SetupDraft:     "worktree-fail",
		SetupEmoji:     "❌",
	},
	{
		Name:           "three",
		Description:    "three-choice 🆕/🔗/❌ — middle option acted on recognised draft",
		SetupUserMsgID: "om_test_three",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "🔗",
	},
	{
		Name:           "unknown",
		Description:    "unknown emoji 👍 — draft LEFT in place, no follow-up card",
		SetupUserMsgID: "om_test_unknown",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "👍",
	},
	{
		Name:           "orphan",
		Description:    "no draft — expect consumed=true dropped=true, no message",
		SetupUserMsgID: "om_orphan_no_draft",
		SetupEmoji:     "✅",
	},
}

func runGTWTestScenario(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
	deps gtw.HandlerDeps,
	gw Gateway,
) (*CommandResult, error) {
	if len(args) < 1 {
		return runGTWTestHelp(ctx, channel, msg), nil
	}
	name := args[0]
	var sc *gtwTestScenario
	for i := range gtwTestScenarios {
		if gtwTestScenarios[i].Name == name {
			sc = &gtwTestScenarios[i]
			break
		}
	}
	if sc == nil {
		return runGTWTestHelp(ctx, channel, msg), nil
	}

	if sc.SetupEmoji == "" && sc.SetupDraft == "" {
		return runGTWTestPreview(ctx, channel, msg, name, sc)
	}

	if sc.SetupDraft != "" {
		if err := gtwTestSeedDraft(mgr, msg.ChatID, sc.SetupUserMsgID, sc.SetupDraft); err != nil {
			return sendGTWReply(ctx, channel, msg.ChatID,
				"❌ /gtw test "+name+": seed failed: "+err.Error()), nil
		}
	}

	if deps.Send == nil || deps.Git == nil || deps.NewPlatform == nil || deps.Now == nil {
		fallback := gtwTestFallbackDeps(channel)
		if deps.Send == nil {
			deps.Send = fallback.Send
		}
		if deps.Git == nil {
			deps.Git = fallback.Git
		}
		if deps.NewPlatform == nil {
			deps.NewPlatform = fallback.NewPlatform
		}
		if deps.Now == nil {
			deps.Now = fallback.Now
		}
	}

	slog.Default().Warn("F-46 debug: runGTWTestScenario: about to wireGTWActionOnSession",
		"chat_id", msg.ChatID)
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	wireGTWActionOnSession(cs, deps)

	// Send the BUSINESS card (not a hardcoded debug twin), then re-key
	// the draft under the bot message id so a real click finds it.
	var cardMsgID string
	if sc.SetupDraft != "" {
		card := gtwTestBusinessCard(sc.SetupDraft, msg.ChatID)
		if card != nil {
			card.RequestID = "gtw-test-" + sc.SetupUserMsgID
			var err error
			cardMsgID, err = sendDebugDecisionCard(ctx, channel, msg.ChatID, *card)
			if err != nil {
				return sendGTWReply(ctx, channel, msg.ChatID,
					"❌ /gtw test "+name+": send card failed: "+err.Error()), nil
			}
			gtwTestRekeyDraft(mgr, msg.ChatID, sc.SetupUserMsgID, cardMsgID)
			gtwTestStampCardOnDraft(mgr, msg.ChatID, cardMsgID, *card, cardMsgID)
		}
	}

	_ = gw
	slog.Default().Warn("F-46 debug: runGTWTestScenario: card sent, waiting for user click",
		"chat_id", msg.ChatID,
		"card_msg_id", cardMsgID,
		"setup_user_msg_id", sc.SetupUserMsgID,
		"setup_emoji", sc.SetupEmoji)
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("🧪 /gtw test %s  [debug / Feishu UAT]\n  setup: %s\n  react:  click %s on the card above\n  expected: production HandleAction PATCHes the card on your click.\n\n  (no auto-dispatch — unit tests only assert this setup; full E2E needs a real Feishu session.)",
			name, sc.Description, sc.SetupEmoji)), nil
}

// gtwTestBusinessCard builds the card via production gtw helpers so
// debug UAT cannot drift from `/gtw fix` decision-card chrome.
func gtwTestBusinessCard(kind, chatID string) *gtw.Card {
	p := gtwTestPayload(chatID, kind)
	switch kind {
	case "branch-exists":
		c := gtw.BranchExistsCard(p, "")
		return &c
	case "worktree-fail":
		c := gtw.WorktreeFailCard(p)
		return &c
	default:
		return nil
	}
}

func gtwTestPayload(chatID, kind string) gtw.FixDraftPayload {
	return gtw.FixDraftPayload{
		IssueID:    42,
		Title:      "synthetic /gtw test seed",
		Branch:     "fix/42-test",
		Slug:       "42-test",
		Repo:       "cnlangzi/nightme",
		Platform:   "github",
		LabelAdded: kind != "worktree-fail",
		ChatID:     chatID,
	}
}

// sendDebugDecisionCard posts a gtw.Card through OutCard. RequestID
// must already be set by the caller (gtw-test-…).
func sendDebugDecisionCard(ctx context.Context, channel Channel, chatID string, card gtw.Card) (string, error) {
	msgID, err := channel.SendCard(ctx, OutboundMessage{
		ChatID: chatID,
		Kind:   OutCard,
		Card: &Card{
			Kind:      CardKindDecision,
			Title:     card.Title,
			Body:      card.Body,
			Choices:   toGatewayCardChoices(card.Choices),
			RequestID: card.RequestID,
		},
	})
	if err != nil {
		return "", err
	}
	if msgID == "" {
		msgID = "echo-card-" + card.RequestID
	}
	return msgID, nil
}

func runGTWTestPreview(
	ctx context.Context,
	channel Channel,
	msg *InboundMessage,
	name string,
	sc *gtwTestScenario,
) (*CommandResult, error) {
	if name != "card" {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test "+name+": no preview for this scenario"), nil
	}
	card := gtw.BranchExistsCard(gtwTestPayload(msg.ChatID, "branch-exists"), "(none — test env)")
	card.RequestID = "gtw-test-preview-" + sc.SetupUserMsgID
	if _, err := sendDebugDecisionCard(ctx, channel, msg.ChatID, card); err != nil {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test card: send failed: "+err.Error()), nil
	}
	return sendGTWReply(ctx, channel, msg.ChatID,
		`🧪 /gtw test card — interactive decision-card preview

Card chrome comes from gtw.BranchExistsCard (same as /gtw fix).
Click a button on Feishu to exercise production HandleAction + PATCH.
`), nil
}

func runGTWTestHelp(ctx context.Context, channel Channel, msg *InboundMessage) *CommandResult {
	var b strings.Builder
	b.WriteString("🧪 /gtw test <scenario>  — debug / Feishu UAT only\n\n")
	b.WriteString("Seeds a decision card via gtw business renderers, then\n")
	b.WriteString("waits for YOUR Feishu click. No auto-dispatch.\n\n")
	b.WriteString("Scenarios:\n")
	for _, sc := range gtwTestScenarios {
		fmt.Fprintf(&b, "  %-18s  %s\n", sc.Name, sc.Description)
	}
	b.WriteString("\nUtility:\n")
	b.WriteString("  list    print every gtwDraft currently in the chat\n")
	b.WriteString("  reset   drop every gtwDraft and clear gtwContext\n")
	return sendGTWReply(ctx, channel, msg.ChatID, b.String())
}

func runGTWTestList(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	drafts := cs.ListGTWDrafts()
	if len(drafts) == 0 {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"/gtw test list: (none in this chat)"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "/gtw test list — %d in this chat:\n", len(drafts))
	for i, d := range drafts {
		fmt.Fprintf(&b, "  [%d] kind=%s  issueID=%d  branch=%q  repo=%q  createdAt=%s\n",
			i, d.Kind, d.Payload.IssueID, d.Payload.Branch, d.Payload.Repo,
			d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	return sendGTWReply(ctx, channel, msg.ChatID, b.String()), nil
}

func runGTWTestReset(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	before := cs.GTWDraftCount()
	cs.ClearGTWDrafts()
	cs.ClearGTWContext()
	after := cs.GTWDraftCount()
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("✅ /gtw test reset — cleared gtwContext (drafts before=%d after=%d)", before, after)), nil
}

func gtwTestFallbackDeps(channel Channel) gtw.HandlerDeps {
	return gtw.HandlerDeps{
		Send: func(ctx context.Context, m gtw.OutMsg) error {
			return channel.Send(ctx, OutboundMessage{
				ChatID: m.ChatID,
				Kind:   OutReply,
				Text:   m.Text,
			})
		},
		Git:         gtw.ExecGitRunner{},
		NewPlatform: gtw.NewPlatformClient,
		Now:         time.Now,
	}
}

// wireGTWActionOnSession installs the production gtw action handler on
// one ChatSession. Needed because RegisterGTWAction's WithOnCreate only
// fires for newly created sessions; restored sessions need a backfill
// before `/gtw test` clicks can land.
func wireGTWActionOnSession(cs *chatsession.ChatSession, deps gtw.HandlerDeps) {
	if cs == nil {
		return
	}
	slog.Default().Warn("F-46 debug: wireGTWActionOnSession entry",
		"chat_id", cs.ChatID,
		"draft_count_before", cs.GTWDraftCount())
	csAdapter := &csSender{cs: cs}
	slot := &gtwContextSlot{cs: cs}
	drafts := &gtwDraftsMap{cs: cs}
	stored := deps
	cs.SetActionHandler(func(ctx context.Context, ev chatsession.ReactionEvent) bool {
		if cs.GTWDraft(ev.TargetMsgID) == nil {
			slog.Default().Warn("F-46 debug: wireGTWActionOnSession: no draft",
				"target_msg_id", ev.TargetMsgID,
				"draft_count", cs.GTWDraftCount())
			return false
		}
		consumed, err := gtw.HandleAction(ctx, stored, csAdapter, slot, drafts, gtw.ReactionEvent{
			TargetMsgID: ev.TargetMsgID,
			Emoji:       ev.Emoji,
			UserID:      ev.UserID,
			ChatID:      ev.ChatID,
		})
		if err != nil {
			slog.Default().Warn("F-46 debug: HandleAction error",
				"err", err, "target_msg_id", ev.TargetMsgID)
		}
		return consumed
	})
}

func gtwTestStampCardOnDraft(mgr *chatsession.Manager, chatID, draftKey string, card gtw.Card, botMsgID string) {
	if mgr == nil || chatID == "" || draftKey == "" {
		return
	}
	cs := mgr.Get(chatID)
	if cs == nil {
		return
	}
	draft := cs.GTWDraft(draftKey)
	if draft == nil {
		return
	}
	draft.BotMessageID = botMsgID
	draft.CardTitle = card.Title
	draft.CardBody = card.Body
	draft.CardRequestID = card.RequestID
	draft.CardChoices = toChatsessionChoices(card.Choices)
	cs.StoreGTWDraft(draftKey, draft)
}

func toChatsessionChoices(in []gtw.CardChoice) []chatsession.CardChoice {
	if len(in) == 0 {
		return nil
	}
	out := make([]chatsession.CardChoice, len(in))
	for i, c := range in {
		out[i] = chatsession.CardChoice{
			Emoji:  c.Emoji,
			Label:  c.Label,
			Action: c.Action,
		}
	}
	return out
}

func gtwTestRekeyDraft(mgr *chatsession.Manager, chatID, oldKey, newKey string) {
	if mgr == nil || chatID == "" || oldKey == "" || newKey == "" || oldKey == newKey {
		return
	}
	cs := mgr.Get(chatID)
	if cs == nil {
		return
	}
	draft := cs.GTWDraft(oldKey)
	if draft == nil {
		return
	}
	cs.StoreGTWDraft(newKey, draft)
	cs.TakeGTWDraft(oldKey)
}

// gtwTestSeedDraft stores a synthetic draft. Card chrome is stamped
// later from gtw.BranchExistsCard / WorktreeFailCard after SendCard.
//
// kind ∈ {branch-exists, worktree-fail, label-taken}.
func gtwTestSeedDraft(mgr *chatsession.Manager, chatID, userMsgID, kind string) error {
	var draftKind chatsession.GTWDraftKind
	switch kind {
	case "branch-exists":
		draftKind = chatsession.GTWDraftFixBranchExists
	case "worktree-fail":
		draftKind = chatsession.GTWDraftFixWorktreeFail
	case "label-taken":
		draftKind = chatsession.GTWDraftFixLabelTaken
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	p := gtwTestPayload(chatID, kind)
	cs := mgr.GetOrCreate(chatID, "primary")
	cs.StoreGTWDraft(userMsgID, &chatsession.GTWDraft{
		Kind: draftKind,
		Payload: chatsession.GTWFixDraftPayload{
			IssueID:    p.IssueID,
			Title:      p.Title,
			Branch:     p.Branch,
			Slug:       p.Slug,
			Repo:       p.Repo,
			Platform:   p.Platform,
			LabelAdded: p.LabelAdded,
			ChatID:     p.ChatID,
		},
		CardRequestID: "gtw-test-" + userMsgID,
		CreatedAt:     time.Now(),
	})
	return nil
}
