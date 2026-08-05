// F-45: `/gtw fix <id>` slash command + action routing.
//
// `/gtw` is a regular Command registered on the Gateway, just like
// `/cwd` / `/use` / `/kill`. The gtw-specific bits that hang off
// ChatSession (gtwContext / gtwDrafts) are accessed via the
// accessors in internal/chatsession/gtw_accessors.go. Reaction
// routing is the single extra branch in ChatSession.HandleReaction
// referenced by the F-45 §3.5 design.
//
// v1 only implements `/gtw fix <id>`. Other /gtw subcommands are
// F-46+ (push / pr / review / unfix / wt / hook).
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

	// Keep deps by value in the closure so the registered command
	// is self-contained. gtw package is stateless; this is the
	// only place we materialize the binding.
	storedDeps := deps
	gw.Register(Command{
		Name:        "gtw",
		Description: "Team workflow: /gtw fix <issue-id> (claim, worktree, label). F-45.",
		Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
			return handleGTW(ctx, mgr, channel, msg, args, globalPrimary, storedDeps)
		},
	})
}

// gtwSendAdapter wraps gateway.Channel.Send into a gtw.SendFunc.
// Kept as a separate named func so the closure is alloc-free on
// each /gtw invocation. The ctx comes from the caller (the
// gtw.RunFix / HandleReaction invocation), not a fresh
// context.Background() — adapters use the ctx for rate limiting
// and cancellation.
func gtwSendAdapter(channel Channel) gtw.SendFunc {
	return func(ctx context.Context, m gtw.OutMsg) error {
		return channel.Send(ctx, OutboundMessage{
			ChatID:  m.ChatID,
			Kind:    OutReply,
			Text:    m.Text,
			ReplyTo: m.ReplyTo,
		})
	}
}

// handleGTW is the single entry point for all /gtw subcommands.
// v1 dispatches only `fix`; the rest return a "not implemented"
// card. New subcommands are added in F-46+ and dispatched here.
func handleGTW(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
	globalPrimary string,
	deps gtw.HandlerDeps,
) (*CommandResult, error) {
	if len(args) < 1 {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"Usage: /gtw fix <issue-id>"), nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "fix":
		return runGTWFix(ctx, mgr, channel, msg, rest, globalPrimary, deps)
	default:
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ Unknown /gtw subcommand: "+sub+" (v1 only supports `fix`)"), nil
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
// RegisterGTWReaction installs the gtw-draft action executor
// on every ChatSession the manager creates. The runtime calls
// this once at startup (alongside the existing
// `SetMessageStateHandler` wiring) so a /gtw decision card can
// be acted on by user emoji clicks within the same chat.
//
// The handler looks up gtwDrafts by TargetMsgID. If a draft
// exists, it is taken (one-shot per emoji) and dispatched to
// gtw.HandleReaction. If no draft matches, the handler returns
// false so the runtime can fall through to future handlers
// (none today; placeholder for F-31+ reaction-driven FSM).
func RegisterGTWReaction(mgr *chatsession.Manager, deps gtw.HandlerDeps) {
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
		// itself (the gtw.HandleReaction path uses the same
		// Send func that RunFix uses). The default channel-
		// backed adapter lives on the runtime; we don't have
		// a Channel reference here so we leave deps.Send nil
		// and let gtw.HandleReaction fall back to its
		// in-package reply helper. The runtime is expected
		// to pre-populate deps.Send via RegisterGTW before
		// calling RegisterGTWReaction.
		_ = deps.Send
	}
	storedDeps := deps
	mgr.WithOnCreate(func(cs *chatsession.ChatSession) {
		cs.SetReactionHandler(func(ctx context.Context, ev chatsession.ReactionEvent) bool {
			if cs.GTWDraft(ev.TargetMsgID) == nil {
				return false
			}
			csAdapter := &csSender{cs: cs}
			slot := &gtwContextSlot{cs: cs}
			drafts := &gtwDraftsMap{cs: cs}
			consumed, _ := gtw.HandleReaction(ctx, storedDeps, csAdapter, slot, drafts, gtw.ReactionEvent{
				TargetMsgID: ev.TargetMsgID,
				Emoji:       ev.Emoji,
				UserID:      ev.UserID,
				ChatID:      ev.ChatID,
			})
			return consumed
		})
	})
}
