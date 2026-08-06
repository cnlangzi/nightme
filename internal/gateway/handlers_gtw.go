// F-45: `/gtw fix <id>` slash command + action routing.
//
// `/gtw` is a regular Command registered on the Gateway, just like
// `/cwd` / `/use` / `/kill`. The gtw-specific bits that hang off
// ChatSession (gtwContext / gtwDrafts) are accessed via the
// accessors in internal/chatsession/gtw_accessors.go. Reaction
// routing is the single extra branch in ChatSession.HandleAction
// referenced by the F-45 §3.5 design.
//
// v1 implements `/gtw fix <id>`. Debug/UAT `/gtw test` lives in
// handlers_gtw_debug.go so it cannot mix card shapes or seeds into
// the business path. Other subcommands are F-46+ (push / pr / …).
package gateway

import (
	"context"
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
	// Capture gw for /gtw test plumbing (reply routing). Reaction
	// auto-dispatch was removed — /gtw test is Feishu UAT/debug only
	// and waits for a real card click / emoji reaction.
	storedGw := gw
	gw.Register(Command{
		Name:        "gtw",
		Description: "Team workflow: /gtw fix <issue-id> | /gtw test <scenario> (debug/Feishu UAT).",
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
			"Usage: /gtw fix <issue-id> | /gtw test <scenario> (debug/Feishu UAT)"), nil
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
