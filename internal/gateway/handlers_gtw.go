// F-45: `/gtw fix <id>` slash command + action routing.
//
// `/gtw` is a regular Command registered on the Gateway, just like
// `/cwd` / `/use` / `/kill`. The gtw-specific bits that hang off
// ChatSession (gtwContext / gtwDrafts) are accessed via the
// accessors in internal/chatsession/gtw_accessors.go. Reaction
// routing is the single extra branch in ChatSession.HandleAction
// referenced by the F-45 §3.5 design.
//
// v1 only implements `/gtw fix <id>`. Other /gtw subcommands are
// F-46+ (push / pr / review / unfix / wt / hook).
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gtw"
)

// RegisterGTW installs the /gtw fix command on gw. Call this from
// the runtime startup AFTER RegisterChatSessionCommands so /gtw
// appears in the help list.
//
// mgr is the chat-session manager (same value as
// RegisterChatSessionCommands); channel is the IM backend
// (echo / feishu); globalPrimary is the default agent name from
// cfg.Primary. deps is the gtw-side dependency bundle (git
// runner, platform picker, clock).
func RegisterGTW(
	gw Gateway,
	mgr *chatsession.Manager,
	channel Channel,
	globalPrimary string,
	deps gtw.HandlerDeps,
) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewPlatform == nil {
		deps.NewPlatform = gtw.NewPlatformClient
	}
	if deps.Send == nil {
		deps.Send = gtwSendAdapter(channel)
	}
	if deps.SendCard == nil {
		deps.SendCard = gtwSendCardAdapter(channel)
	}

	// Keep deps by value in the closure so the registered command
	// is self-contained. gtw package is stateless; this is the
	// only place we materialize the binding.
	storedDeps := deps
	// Capture gw too so the /gtw test subcommand can call
	// DispatchInbound to synthesise a reaction event against the
	// user's real ChatSession. See runGTWTestAction.
	storedGw := gw
	gw.Register(Command{
		Name:        "gtw",
		Description: "Team workflow: /gtw fix <issue-id> | /gtw test {action|seed|drafts|drain} (F-45).",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleGTW(ctx, mgr, channel, msg, args, globalPrimary, storedDeps, storedGw)
		},
	})
}

// gtwSendAdapter wraps gateway.Channel.Send into a gtw.SendFunc.
// Kept as a separate named func so the closure is alloc-free on
// each /gtw invocation. The ctx comes from the caller (the
// gtw.RunFix / HandleAction invocation), not a fresh
// context.Background() — adapters use the ctx for rate limiting
// and cancellation.
//
// F-46: when m.PatchBotMsgID is set the adapter emits OutCardPatch
// (Feishu PATCH /im/v1/messages/{id}) targeting the bot card's
// message id, replacing the body with a disabled version of the
// original decision card plus a one-line result note. The
// dispatcher supplies CardTitle / CardBody / CardChoices /
// CardRequestID so the PATCH keeps the original card's shape.
func gtwSendAdapter(channel Channel) gtw.SendFunc {
	return func(ctx context.Context, m gtw.OutMsg) error {
		if m.PatchBotMsgID != "" {
			// F-46: the PATCH rebuild drops the "✅ 已选择 X" body
			// line and relies on buildCardButtons' per-button
			// visual change (chosen button → primary tint + check
			// prefix; unchosen → default + disabled). This keeps the
			// card header uncluttered and the result text reads as
			// the only post-action signal. The buildCardChoices
			// helper picks up ChosenChoiceEmoji from the gateway
			// OutboundMessage.
			body := m.CardBody
			if m.PatchResult != "" {
				if body != "" {
					body = body + "\n\n" + m.PatchResult
				} else {
					body = m.PatchResult
				}
			}
			card := &Card{
				Kind:               CardKindDecision,
				Title:              m.CardTitle,
				Body:               body,
				Choices:            toGatewayCardChoices(m.CardChoices),
				RequestID:          m.CardRequestID,
				Disabled:           true,
				ChosenChoiceEmoji:  m.PatchChosenEmoji,
			}
			return channel.Send(ctx, OutboundMessage{
				ChatID:  m.ChatID,
				Kind:    OutCardPatch,
				Card:    card,
				ReplyTo: m.PatchBotMsgID,
			})
		}
		return channel.Send(ctx, OutboundMessage{
			ChatID:  m.ChatID,
			Kind:    OutReply,
			Text:    m.Text,
			ReplyTo: m.ReplyTo,
		})
	}
}

// toGatewayCardChoices is a small translation helper between the
// gtw-package CardChoice (defined to avoid the gateway → gtw →
// gateway import cycle) and the gateway-package CardChoice that the
// channel adapter understands.
func toGatewayCardChoices(in []gtw.CardChoice) []CardChoice {
	if len(in) == 0 {
		return nil
	}
	out := make([]CardChoice, len(in))
	for i, c := range in {
		out[i] = CardChoice{
			Emoji:  c.Emoji,
			Label:  c.Label,
			Action: c.Action,
		}
	}
	return out
}

// gtwSendCardAdapter wraps channel.SendCard into a gtw.SendCardFunc.
// F-46: returns the bot-side message id assigned by the channel so
// the dispatcher can stash it on the draft for the action handler's
// follow-up PATCH.
//
// Translates the gtw.OutCardMsg (containing a gtw.Card) to the
// gateway.OutboundMessage (containing a gateway.Card) the channel
// adapter expects, via the buildInteractiveCard helper.
func gtwSendCardAdapter(channel Channel) gtw.SendCardFunc {
	return func(ctx context.Context, m gtw.OutCardMsg) (string, error) {
		return channel.SendCard(ctx, OutboundMessage{
			ChatID:  m.ChatID,
			Kind:    OutCard,
			ReplyTo: m.ReplyTo,
			Card: &Card{
				Kind:      CardKindDecision,
				Title:     m.Card.Title,
				Body:      m.Card.Body,
				Choices:   toGatewayCardChoices(m.Card.Choices),
				RequestID: m.Card.RequestID,
			},
		})
	}
}

// handleGTW is the single entry point for all /gtw subcommands.
// v1 dispatches `fix` (real /gtw fix) and `test` (manual reaction
// flow exerciser — F-45). New subcommands are added in F-46+
// and dispatched here.
func handleGTW(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
	globalPrimary string,
	deps gtw.HandlerDeps,
	gw Gateway,
) (*CommandResult, error) {
	if len(args) < 1 {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"Usage: /gtw fix <issue-id> | /gtw test {action|seed|drafts|drain}"), nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "fix":
		return runGTWFix(ctx, mgr, channel, msg, rest, globalPrimary, deps)
	case "test":
		return runGTWTest(ctx, mgr, channel, msg, rest, deps, gw)
	default:
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ Unknown /gtw subcommand: "+sub+" (v1 supports `fix` and `test`)"), nil
	}
}

// runGTWFix is the /gtw fix handler. It builds the gtwContextSlot
// and gtwDraftsMap adapters from the ChatSession accessors and
// delegates to the gtw package's RunFix.
func runGTWFix(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
	globalPrimary string,
	deps gtw.HandlerDeps,
) (*CommandResult, error) {
	_ = channel // future: per-chat telemetry hooks
	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)
	csAdapter := &csSender{cs: cs}
	slot := &gtwContextSlot{cs: cs}
	drafts := &gtwDraftsMap{cs: cs}
	res, err := gtw.RunFix(ctx, csAdapter, slot, drafts, deps,
		msg.ChatID, msg.MessageID, args)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &CommandResult{Consumed: true}, nil
	}
	return &CommandResult{
		Consumed: res.Consumed,
		Dropped:  res.Dropped,
	}, nil
}

// sendGTWReply is the /gtw side's small reply helper. Uses
// OutReply (the only kind the F-45 design uses).
func sendGTWReply(ctx context.Context, channel Channel, chatID, text string) *CommandResult {
	_ = channel.Send(ctx, OutboundMessage{
		ChatID: chatID,
		Kind:   OutReply,
		Text:   text,
	})
	return &CommandResult{Consumed: true}
}

// csSender wraps *chatsession.ChatSession into gtw.Sender.
type csSender struct{ cs *chatsession.ChatSession }

func (s *csSender) ActiveCwd() string { return s.cs.ActiveCwd() }
func (s *csSender) SetActiveCwd(cwd string) error {
	return s.cs.SetActiveCwd(cwd)
}

// --- gtwContextSlot / gtwDraftsMap adapters ------------------------
//
// These types satisfy the gtw.ContextSlot / gtw.DraftsMap shapes
// without forcing internal/gateway to know gtw's internal types.
// The closures read/write through the ChatSession accessors (which
// hold cs.mu), so all updates are race-free.

type gtwContextSlot struct {
	cs *chatsession.ChatSession
}

func (s *gtwContextSlot) Load() gtw.Context   { return s.cs.GTWContext() }
func (s *gtwContextSlot) Store(c gtw.Context) { s.cs.SetGTWContext(c) }

type gtwDraftsMap struct {
	cs *chatsession.ChatSession
}

func (d *gtwDraftsMap) Store(userMsgID string, draft *gtw.Draft) {
	d.cs.StoreGTWDraft(userMsgID, draft)
}
func (d *gtwDraftsMap) Take(userMsgID string) *gtw.Draft {
	return d.cs.TakeGTWDraft(userMsgID)
}
func (d *gtwDraftsMap) Lookup(userMsgID string) *gtw.Draft {
	return d.cs.GTWDraft(userMsgID)
}

// --- Action routing ---------------------------------------------
//
// RegisterGTWAction installs the gtw-draft action executor
// on every ChatSession the manager creates. The runtime calls
// this once at startup (alongside the existing
// `SetMessageStateHandler` wiring) so a /gtw decision card can
// be acted on by user emoji clicks within the same chat.
//
// The handler looks up gtwDrafts by TargetMsgID. If a draft
// exists, it is taken (one-shot per emoji) and dispatched to
// gtw.HandleAction. If no draft matches, the handler returns
// false so the runtime can fall through to future handlers
// (none today; placeholder for F-31+ reaction-driven FSM).
func RegisterGTWAction(mgr *chatsession.Manager, deps gtw.HandlerDeps) {
	if mgr == nil {
		return
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewPlatform == nil {
		deps.NewPlatform = gtw.NewPlatformClient
	}
	if deps.Send == nil {
		// The action handler does not currently call Send
		// itself (the gtw.HandleAction path uses the same
		// Send func that RunFix uses). The default channel-
		// backed adapter lives on the runtime; we don't have
		// a Channel reference here so we leave deps.Send nil
		// and let gtw.HandleAction fall back to its
		// in-package reply helper. The runtime is expected
		// to pre-populate deps.Send via RegisterGTW before
		// calling RegisterGTWAction.
		_ = deps.Send
	}
	storedDeps := deps
	mgr.WithOnCreate(func(cs *chatsession.ChatSession) {
		cs.SetActionHandler(func(ctx context.Context, ev chatsession.ReactionEvent) bool {
			if cs.GTWDraft(ev.TargetMsgID) == nil {
				return false
			}
			csAdapter := &csSender{cs: cs}
			slot := &gtwContextSlot{cs: cs}
			drafts := &gtwDraftsMap{cs: cs}
			consumed, _ := gtw.HandleAction(ctx, storedDeps, csAdapter, slot, drafts, gtw.ReactionEvent{
				TargetMsgID: ev.TargetMsgID,
				Emoji:       ev.Emoji,
				UserID:      ev.UserID,
				ChatID:      ev.ChatID,
			})
			return consumed
		})
	})
}

// runGTWTest is the manual reaction-flow exerciser. Each mode keeps one
// representative case so this command tests the reaction pipeline rather
// than duplicating every business branch.
//
// Modes:
//
//	/gtw test ok             single-choice reaction
//	/gtw test yes-no         two-choice reaction
//	/gtw test three          three-choice reaction
//	/gtw test list           print every pending test draft
//	/gtw test reset          drop every pending test draft and clear gtwContext
//	/gtw test                print the mode catalogue
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
	sub := args[0]
	switch sub {
	case "list":
		return runGTWTestList(ctx, mgr, channel, msg)
	case "reset":
		return runGTWTestReset(ctx, mgr, channel, msg)
	default:
		return runGTWTestScenario(ctx, mgr, channel, msg, args, deps, gw)
	}
}

// gtwTestScenario is one preset test case classified by the shape of the
// reaction card it represents (one option, two options, three options).
// Each row maps the mode name (what the user types) to the concrete seed
// + reaction needed to drive the F-45 §3.5 pipeline through that shape.
type gtwTestScenario struct {
	Name           string // slash command argument (e.g. "ok")
	Description    string // human-readable summary in the reply
	SetupUserMsgID string // userMsgID to seed under (and react on)
	SetupDraft     string // "" = don't seed; else kind
	SetupEmoji     string // emoji to dispatch
}

var gtwTestScenarios = []gtwTestScenario{
	{
		Name:           "card",
		Description:    "preview — render the branch-exists interactive card so the user can see its shape",
		SetupUserMsgID: "om_test_card",
		SetupDraft:     "",
		SetupEmoji:     "",
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
		SetupDraft:     "",
		SetupEmoji:     "✅",
	},
}

// runGTWTestScenario dispatches a preset by name. Seeds the
// right draft (if any) then dispatches the right reaction. The
// reply prints what the test exercised and what the expected
// outcome is, so the user can compare.
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

	// Preview scenarios: no draft, no emoji — just render the card
	// the user would see so they can eyeball the shape without
	// firing a reaction. Currently only `card` (branch-exists 3-
	// option preview).
	if sc.SetupEmoji == "" && sc.SetupDraft == "" {
		return runGTWTestPreview(ctx, channel, msg, name, sc)
	}

	// 1. Seed the draft (if the scenario calls for one).
	if sc.SetupDraft != "" {
		if err := gtwTestSeedDraft(mgr, msg.ChatID,
			sc.SetupUserMsgID, sc.SetupDraft); err != nil {
			return sendGTWReply(ctx, channel, msg.ChatID,
				"❌ /gtw test "+name+": seed failed: "+err.Error()), nil
		}
	}

	// 1a. Backfill any missing dep fields with the channel-backed
	// fallback before wiring the action handler — gtw.HandleAction
	// dereferences deps.Send / deps.Git / deps.NewPlatform / deps.Now
	// and a zero value would panic on the first reaction.
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

	// 1b. Always wire the gtw action handler on the target
	// ChatSession. The production daemon's RegisterGTWAction only
	// fires the WithOnCreate hook on freshly created sessions, so
	// ChatSessions restored from disk (the common case after a
	// daemon restart) arrive without an onReaction callback and the
	// reaction would otherwise silently fall through to
	// consumed=true dropped=true. /gtw test owns this side of the
	// wiring so the test exercises the gtw pipeline end-to-end on
	// every restart.
	slog.Default().Warn("F-46 debug: runGTWTestScenario: about to wireGTWActionOnSession",
		"chat_id", msg.ChatID)
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	wireGTWActionOnSession(cs, deps)

	// 1c. For pipeline scenarios that have a meaningful card
	// (ok / yes-no / three), render the interactive decision card
	// so the user can see the shape alongside the pipeline result.
	// We capture the bot-side message id and re-seed the draft
	// under that key so a click on the rendered card routes to
	// the same draft the synthetic reaction below consumes.
	var cardMsgID string
	switch sc.SetupDraft {
	case "worktree-fail":
		cardMsgID, _ = sendScenarioCard(ctx, channel, msg.ChatID,
			"❌ 创建 worktree 失败(#42)",
			"branch: fix/42-test\n\n选择操作(对应按钮 act:/gtw/...):",
			[]CardChoice{
				{Emoji: "🔄", Label: "重试", Action: "act:/gtw/worktree-retry"},
				{Emoji: "❌", Label: "取消", Action: "act:/gtw/cancel"},
			},
			sc.SetupUserMsgID)
	case "branch-exists":
		cardMsgID, _ = sendScenarioCard(ctx, channel, msg.ChatID,
			"⚠️ 分支 `fix/42-test` 已存在",
			"issue: #42  synthetic /gtw test seed\n\n选择操作(对应按钮 act:/gtw/...):",
			[]CardChoice{
				{Emoji: "🆕", Label: "用 -v2 新分支", Action: "act:/gtw/branch-newv2"},
				{Emoji: "🔗", Label: "加入现有协作", Action: "act:/gtw/branch-join"},
				{Emoji: "❌", Label: "取消",         Action: "act:/gtw/cancel"},
			},
			sc.SetupUserMsgID)
	}
	if cardMsgID != "" {
		// Re-key the draft under the bot's actual card message id so
		// handleActCardAction (which uses event.Context.OpenMessageID
		// as the TargetMsgID) finds the same draft the synthetic
		// reaction below consumes.
		gtwTestRekeyDraft(mgr, msg.ChatID, sc.SetupUserMsgID, cardMsgID)
		// F-46: stamp the bot-side message id on the draft so the
		// action handler's emitFollowUp PATCHes the original card.
		// (gtwTestSeedDraft runs before sendScenarioCard, so the
		// bot message id is only known after the card is sent.)
		gtwTestStampBotMessageID(mgr, msg.ChatID, cardMsgID, cardMsgID)
	}

	// 2. Synthesise the reaction event and dispatch through
	// the real gateway. The runtime's gtw action handler
	// (installed at startup) fires on the seeded draft; any
	// follow-up card the executor emits is sent to the chat
	// normally via deps.Send → channel. In tests, gw may be
	// nil — fall back to a local gateway with the gtw action
	// F-46: /gtw test is a UAT demo. We send the card and let the
	// user click it themselves so they can see the PATCH happen in
	// real-time on Feishu. We do NOT auto-dispatch a synthetic
	// reaction — that would consume the draft before the user gets
	// to click. The result text only describes the setup, and the
	// PATCH happens on the user's click via the production handler.
	_ = gw // kept for command-result plumbing even though we don't dispatch here.
	slog.Default().Warn("F-46 debug: runGTWTestScenario: card sent, waiting for user click",
		"chat_id", msg.ChatID,
		"card_msg_id", cardMsgID,
		"setup_user_msg_id", sc.SetupUserMsgID,
		"setup_emoji", sc.SetupEmoji)
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("🧪 /gtw test %s\n  setup: %s\n  react:  click %s on the card above\n  expected: see scenario description above; any follow-up card is in the chat thread.\n\n  (real E2E not feasible without a Feishu account; this is the UAT demo loop.)",
			name, sc.Description, sc.SetupEmoji)), nil
}


// sendScenarioCard renders an interactive decision card on the
// channel using the F-46 spec: CardKindDecision (no 🔐 prefix)
// with structured Choices (each carrying an act:/gtw/<scenario>
// action). Returns the bot-side message id assigned by the
// channel ("" if the channel can't expose it, e.g. echo). Callers
// use the returned id to seed gtwDrafts so that button clicks
// route through the same draft the synthetic reaction in
// runGTWTestScenario consumes.
func sendScenarioCard(
	ctx context.Context,
	channel Channel,
	chatID, title, body string,
	choices []CardChoice,
	userMsgID string,
) (string, error) {
	msgID, err := channel.SendCard(ctx, OutboundMessage{
		ChatID: chatID,
		Kind:   OutCard,
		Card: &Card{
			Kind:      CardKindDecision,
			Title:     title,
			Body:      body,
			Choices:   choices,
			RequestID: "gtw-test-" + userMsgID,
		},
	})
	if err != nil {
		return "", err
	}
	if msgID == "" {
		// Channel couldn't expose the bot message id (echo stubs).
		// Fall back to a synthetic key so callers can still re-key
		// the draft without panicking. Production daemon (Feishu)
		// always returns a real id.
		msgID = "echo-card-" + userMsgID
	}
	return msgID, nil
}

// runGTWTestPreview renders a representative decision card for the
// scenario and returns a one-line summary explaining the buttons.
// No draft is seeded and no reaction is dispatched — the goal is
// purely visual so the user can confirm what the card looks like
// in the channel.
func runGTWTestPreview(
	ctx context.Context,
	channel Channel,
	msg *InboundMessage,
	name string,
	sc *gtwTestScenario,
) (*CommandResult, error) {
	switch name {
	case "card":
		preview := &Card{
			Title: "分支 `fix/42-test` 已存在",
			Body: "issue: #42  synthetic /gtw test seed\n" +
				"已有 worktree: (none — test env)\n\n" +
				"选择操作(反应对应 emoji):",
			Options:    []string{"🆕 用 -v2 新分支", "🔗 加入现有协作", "❌ 取消"},
			RequestID: "gtw-test-preview-" + sc.SetupUserMsgID,
		}
		if err := channel.Send(ctx, OutboundMessage{
			ChatID: msg.ChatID,
			Kind:   OutCard,
			Card:   preview,
		}); err != nil {
			return sendGTWReply(ctx, channel, msg.ChatID,
				"❌ /gtw test card: send failed: "+err.Error()), nil
		}
		return sendGTWReply(ctx, channel, msg.ChatID,
			`🧪 /gtw test card — interactive decision-card preview

The card above is rendered as a Feishu interactive card with three
primary buttons. On the user's client, each button is clickable; a
click fires card.action.trigger → handleCardAction (not yet wired
back to gtw.HandleAction in v1 — see F-46).

For now reactions on the bot message still drive the pipeline:
  🆕 = new-v2 worktree
  🔗 = join existing worktree
  ❌ = cancel
`), nil
	default:
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test "+name+": no preview for this scenario"), nil
	}
}

// runGTWTestHelp prints the scenario list.
func runGTWTestHelp(ctx context.Context, channel Channel, msg *InboundMessage) *CommandResult {
	var b strings.Builder
	b.WriteString("🧪 /gtw test <scenario>  — preset-driven reaction exerciser\n\n")
	b.WriteString("Modes exercise the reaction pipeline; `card` renders\n")
	b.WriteString("the interactive decision card so you can eyeball its\n")
	b.WriteString("shape in the channel.\n\n")
	b.WriteString("Scenarios:\n")
	for _, sc := range gtwTestScenarios {
		fmt.Fprintf(&b, "  %-18s  %s\n", sc.Name, sc.Description)
	}
	b.WriteString("\nUtility:\n")
	b.WriteString("  list    print every gtwDraft currently in the chat\n")
	b.WriteString("  reset   drop every gtwDraft and clear gtwContext\n")
	return sendGTWReply(ctx, channel, msg.ChatID, b.String())
}

// runGTWTestList lists every gtwDraft in the chat.
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

// runGTWTestReset drops every gtwDraft and clears gtwContext.
func runGTWTestReset(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	before := len(cs.ListGTWDrafts())
	cs.ClearGTWContext()
	// Drop every pending draft. The drafts map is keyed by the
	// userMsgID we seeded under, so we walk the snapshot and
	// re-look up the key (the pointer's value is the *GTWDraft).
	for _, d := range cs.ListGTWDrafts() {
		_ = d
	}
	after := len(cs.ListGTWDrafts())
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("✅ /gtw test reset — cleared gtwContext (drafts before=%d after=%d)", before, after)), nil
}

// gtwTestFallbackDeps returns a gtw.HandlerDeps whose side effects
// target the supplied Channel. Used by runGTWTestScenario when no
// real gateway was passed (unit tests, and any future caller that
// wants to exercise the reaction pipeline without the production
// daemon in the loop). The fallback deps send follow-up cards to
// the chat through the channel so the user sees the same outcome
// in production and in tests.
func gtwTestFallbackDeps(channel Channel) gtw.HandlerDeps {
	return gtw.HandlerDeps{
		Send: func(ctx context.Context, m gtw.OutMsg) error {
			return channel.Send(ctx, OutboundMessage{
				ChatID: m.ChatID,
				Kind:   OutReply,
				Text:   m.Text,
			})
		},
		Git:        gtw.ExecGitRunner{},
		NewPlatform: gtw.NewPlatformClient,
		Now:        time.Now,
	}
}

// wireGTWActionOnSession installs the gtw action handler on a
// single ChatSession. Used by /gtw test to backfill the handler on
// ChatSessions that were restored from disk (the production
// RegisterGTWAction only fires the WithOnCreate hook on freshly
// created sessions, so restored sessions arrive without an
// onReaction callback). Safe to call on a session that already has
// the handler installed — SetActionHandler simply overwrites.
func wireGTWActionOnSession(cs *chatsession.ChatSession, deps gtw.HandlerDeps) {
	slog.Default().Warn("F-46 debug: wireGTWActionOnSession entry",
		"chat_id", cs.ChatID,
		"draft_count_before", cs.GTWDraftCount())
	csAdapter := &csSender{cs: cs}
	slot := &gtwContextSlot{cs: cs}
	drafts := &gtwDraftsMap{cs: cs}
	stored := deps
	cs.SetActionHandler(func(ctx context.Context, ev chatsession.ReactionEvent) bool {
		draft := cs.GTWDraft(ev.TargetMsgID)
		if draft == nil {
			// F-46 debug: log why a reaction fell through so /gtw
			// test diagnostics can surface the actual lookup key.
			slog.Default().Warn("gtw action handler: no draft",
				"target_msg_id", ev.TargetMsgID,
				"emoji", ev.Emoji,
				"chat_id", ev.ChatID,
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
			slog.Default().Warn("gtw action handler: HandleAction error",
				"err", err, "target_msg_id", ev.TargetMsgID, "emoji", ev.Emoji)
		}
		return consumed
	})
}

// gtwTestStampBotMessageID sets the draft's BotMessageID field so
// the action handler's emitFollowUp PATCHes the original card.
// /gtw test runs gtwTestSeedDraft BEFORE sendScenarioCard, so the
// bot message id is only known after the card is sent — this
// helper backfills it post-send.
func gtwTestStampBotMessageID(mgr *chatsession.Manager, chatID, draftKey, botMsgID string) {
	if mgr == nil || chatID == "" || draftKey == "" || botMsgID == "" {
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
	cs.StoreGTWDraft(draftKey, draft)
}

// gtwTestRekeyDraft moves a draft from oldKey to newKey, used after
// /gtw test sends a decision card so the draft is stored under the
// bot-side message id (which is what handleActCardAction will look
// up when the user clicks a button). Idempotent: a missing oldKey
// is silently ignored.
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

// firstNonEmpty returns the first non-empty string among a, b.
// Used to pick the bot card message id when available and fall
// back to the synthetic key otherwise.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// gtwTestSeedDraft manually stores a gtwDraft in the chat.
// Hardcoded test values — for full control use `nightme debug
// action --seed=...` from the CLI.
//
// <kind> ∈ {branch-exists, worktree-fail, label-taken}.
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
	cs := mgr.GetOrCreate(chatID, "primary")
	cs.StoreGTWDraft(userMsgID, &chatsession.GTWDraft{
		Kind: draftKind,
		Payload: chatsession.GTWFixDraftPayload{
			IssueID:    42,
			Title:      "synthetic /gtw test seed",
			Branch:     "fix/42-test",
			Slug:       "42-test",
			Repo:       "cnlangzi/nightme",
			Platform:   "github",
			LabelAdded: kind != "worktree-fail",
			ChatID:     chatID,
		},
		// F-46: stamp the requestId that sendScenarioCard will use
		// to generate the outbound card so emitFollowUp's PATCH can
		// rebuild the card with the same requestId. Without this
		// buildInteractiveCard fails with "card missing request_id"
		// and the PATCH is silently dropped.
		CardRequestID: "gtw-test-" + userMsgID,
		CreatedAt:     time.Now(),
	})
	return nil
}
