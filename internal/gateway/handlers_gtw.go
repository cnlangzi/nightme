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

// runGTWTest is the manual reaction-flow exerciser. It lets a
// developer drive the F-45 §3.5 reaction pipeline from a chat
// without needing a real Feishu setup. The subcommands:
//
//	/gtw test action <msg_id> <emoji>
//	    Synthesise a reaction event and run it through
//	    gateway.DispatchInbound against the user's real
//	    ChatSession. The runtime's gtw action handler fires
//	    (installed via gateway.RegisterGTWAction); any cards
//	    the executor emits land in the chat normally.
//	    Reply: one-line summary (consumed/dropped + outcome).
//
//	/gtw test seed <user_msg_id> <kind>
//	    Manually store a gtwDraft in the chat. <kind> is one
//	    of branch-exists | worktree-fail | label-taken. Uses
//	    hardcoded test values (issue=42, branch=fix/42-test,
//	    repo=cnlangzi/nightme). Lets you seed a draft and then
//	    drive it with a /gtw test action click.
//
//	/gtw test drafts
//	    Print every gtwDraft in the chat. Useful for verifying
//	    what was seeded or what a /gtw fix left behind.
//
//	/gtw test drain
//	    Clear the gtwContext for the chat (and best-effort drop
//    pending drafts). Useful between test runs.
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
		return sendGTWReply(ctx, channel, msg.ChatID,
			"Usage: /gtw test {action <msg_id> <emoji> | seed <user_msg_id> <kind> | drafts | drain}"), nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "action":
		return runGTWTestAction(ctx, mgr, channel, msg, rest, gw)
	case "seed":
		return runGTWTestSeed(ctx, mgr, channel, msg, rest)
	case "drafts":
		return runGTWTestDrafts(ctx, mgr, channel, msg)
	case "drain":
		return runGTWTestDrain(ctx, mgr, channel, msg)
	default:
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ Unknown /gtw test subcommand: "+sub+" (want action | seed | drafts | drain)"), nil
	}
}

// runGTWTestAction synthesises a reaction and dispatches it via
// the real gateway. The runtime's gtw action handler is already
// installed (gateway.RegisterGTWAction), so any draft in the
// chat is consumed; any card the executor emits is sent to the
// user normally.
func runGTWTestAction(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
	gw Gateway,
) (*CommandResult, error) {
	_ = mgr // not used directly; the gateway looks up the chat
	if len(args) < 2 {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"Usage: /gtw test action <msg_id> <emoji>"), nil
	}
	msgID, emoji := args[0], args[1]
	if msgID == "" || emoji == "" {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test action: msg_id and emoji must be non-empty"), nil
	}

	inbound := &InboundMessage{
		ChatID:     msg.ChatID,
		UserID:     msg.UserID,
		Text:       "", // reactions have no text
		HasMention: true,
		Reaction: &chatsession.ReactionEvent{
			TargetMsgID: msgID,
			Emoji:       emoji,
			UserID:      msg.UserID,
			ChatID:      msg.ChatID,
		},
	}

	res, err := gw.DispatchInbound(ctx, inbound)
	if err != nil {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test action: dispatch error: "+err.Error()), nil
	}
	if res == nil {
		return sendGTWReply(ctx, channel, msg.ChatID,
			fmt.Sprintf("⚠️ /gtw test action %s %s: no result (unexpected nil)", msgID, emoji)), nil
	}
	switch {
	case res.Consumed && !res.Dropped:
		return sendGTWReply(ctx, channel, msg.ChatID,
			fmt.Sprintf("✅ /gtw test action %s %s — consumed=true dropped=false. Any follow-up card is in the chat thread.", msgID, emoji)), nil
	case res.Consumed && res.Dropped:
		return sendGTWReply(ctx, channel, msg.ChatID,
			fmt.Sprintf("⚠️ /gtw test action %s %s — consumed=true dropped=true (handler ran but did not consume; e.g. no matching gtwDraft, or the emoji was not in the documented set).", msgID, emoji)), nil
	default:
		return sendGTWReply(ctx, channel, msg.ChatID,
			fmt.Sprintf("❌ /gtw test action %s %s — consumed=false dropped=%v (dispatcher rejected; check the runtime actionHandler).", msgID, emoji, res.Dropped)), nil
	}
}

// runGTWTestSeed manually stores a gtwDraft so subsequent
// /gtw test action clicks have a target. The test values are
// hardcoded — for full control, use `nightme debug action
// --seed=...` from the CLI instead.
//
// <kind> is one of: branch-exists, worktree-fail, label-taken.
func runGTWTestSeed(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
	args []string,
) (*CommandResult, error) {
	if len(args) < 2 {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"Usage: /gtw test seed <user_msg_id> <kind>  (kind: branch-exists | worktree-fail | label-taken)"), nil
	}
	userMsgID, kind := args[0], args[1]
	if userMsgID == "" {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test seed: user_msg_id is required"), nil
	}

	var draftKind chatsession.GTWDraftKind
	switch kind {
	case "branch-exists":
		draftKind = chatsession.GTWDraftFixBranchExists
	case "worktree-fail":
		draftKind = chatsession.GTWDraftFixWorktreeFail
	case "label-taken":
		draftKind = chatsession.GTWDraftFixLabelTaken
	default:
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test seed: unknown kind "+kind+" (want branch-exists | worktree-fail | label-taken)"), nil
	}

	cs := mgr.GetOrCreate(msg.ChatID, "primary")
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
			ChatID:     msg.ChatID,
		},
		CreatedAt: time.Now(),
	})
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("✅ /gtw test seed — stored %s draft under userMsgID=%q. React with: /gtw test action %s <emoji>", kind, userMsgID, userMsgID)), nil
}

// runGTWTestDrafts lists every gtwDraft in the chat.
func runGTWTestDrafts(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	drafts := cs.ListGTWDrafts()
	if len(drafts) == 0 {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"/gtw test drafts: (none in this chat)"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "/gtw test drafts — %d in this chat:\n", len(drafts))
	for i, d := range drafts {
		fmt.Fprintf(&b, "  [%d] kind=%s  issueID=%d  branch=%q  repo=%q  createdAt=%s\n",
			i, d.Kind, d.Payload.IssueID, d.Payload.Branch, d.Payload.Repo,
			d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	return sendGTWReply(ctx, channel, msg.ChatID, b.String()), nil
}

// runGTWTestDrain clears the gtwContext and best-effort drops
// pending drafts.
func runGTWTestDrain(
	ctx context.Context,
	mgr *chatsession.Manager,
	channel Channel,
	msg *InboundMessage,
) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, "primary")
	before := len(cs.ListGTWDrafts())
	// chatsession has no public "drain all drafts" method; we
	// iterate by stable key (kind-as-key is best-effort) and
	// always end with ClearGTWContext which is the semantically
	// important reset.
	for _, d := range cs.ListGTWDrafts() {
		_ = cs.TakeGTWDraft(string(d.Kind))
	}
	cs.ClearGTWContext()
	after := len(cs.ListGTWDrafts())
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("✅ /gtw test drain — cleared gtwContext (drafts before=%d after=%d)", before, after)), nil
}
