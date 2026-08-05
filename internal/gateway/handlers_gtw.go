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

// runGTWTest is the manual reaction-flow exerciser. Each
// subcommand is a preset: it bakes the full setup (seed draft +
// dispatch reaction) and prints what the test exercised plus
// what was expected, so the user can compare.
//
// Scenarios are pre-defined; typing /gtw test <scenario> runs
// the full seed+action cycle for that scenario. No parameters
// to figure out — the scenario name implies the full setup.
//
// Subcommands:
//
//	/gtw test <scenario>      preset-driven; e.g. branch-cancel
//	/gtw test list           print every gtwDraft currently in the chat
//	/gtw test reset          drop every gtwDraft and clear gtwContext
//	/gtw test                print the scenario catalogue
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
	rest := args[1:]
	switch sub {
	case "list":
		return runGTWTestList(ctx, mgr, channel, msg)
	case "reset":
		return runGTWTestReset(ctx, mgr, channel, msg)
	default:
		return runGTWTestScenario(ctx, mgr, channel, msg, rest, deps, gw)
	}
}

// gtwTestScenario is one preset test case. The user types
// /gtw test <name> and the subcommand seeds the right draft
// (if any) then dispatches the right reaction. Each scenario
// name implies the full setup — no need to specify msg_id,
// emoji, or draft kind separately.
type gtwTestScenario struct {
	Name           string // slash command argument (e.g. "branch-cancel")
	Description    string // human-readable summary in the reply
	SetupUserMsgID string // userMsgID to seed under (and react on)
	SetupDraft     string // "" = don't seed; else kind
	SetupEmoji     string // emoji to dispatch
}

var gtwTestScenarios = []gtwTestScenario{
	{
		Name:           "branch-cancel",
		Description:    "§5.3.1 ❌ cancel — expect '❌ Cancelled fix #N.'",
		SetupUserMsgID: "om_test_branch_cancel",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "❌",
	},
	{
		Name:           "branch-newv2",
		Description:    "§5.3.1 🆕 new-v2 — expect git worktree add attempt (fails in test env)",
		SetupUserMsgID: "om_test_branch_newv2",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "🆕",
	},
	{
		Name:           "branch-join",
		Description:    "§5.3.1 🔗 join — expect '❌ Branch exists but no worktree' (test env has no worktree)",
		SetupUserMsgID: "om_test_branch_join",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "🔗",
	},
	{
		Name:           "branch-unknown",
		Description:    "F-45 review #3: 👍 unrecognised emoji — expect draft LEFT in place, no message",
		SetupUserMsgID: "om_test_branch_unknown",
		SetupDraft:     "branch-exists",
		SetupEmoji:     "👍",
	},
	{
		Name:           "worktree-retry",
		Description:    "§5.3.3 🔄 retry — expect git worktree add attempt (success on second try in repo cwd)",
		SetupUserMsgID: "om_test_worktree_retry",
		SetupDraft:     "worktree-fail",
		SetupEmoji:     "🔄",
	},
	{
		Name:           "worktree-cancel",
		Description:    "§5.3.3 ❌ cancel — expect '❌ Cancelled fix #N.' (LabelAdded=false, no platform call)",
		SetupUserMsgID: "om_test_worktree_cancel",
		SetupDraft:     "worktree-fail",
		SetupEmoji:     "❌",
	},
	{
		Name:           "label-take",
		Description:    "§5.3.2 🤝 force-take — F-49 not yet implemented; expect '❌ Unrecognised emoji'",
		SetupUserMsgID: "om_test_label_take",
		SetupDraft:     "label-taken",
		SetupEmoji:     "🤝",
	},
	{
		Name:           "label-skip",
		Description:    "§5.3.2 ❌ skip — expect '❌ Cancelled fix #N.' (no platform call since not LabelAdded)",
		SetupUserMsgID: "om_test_label_skip",
		SetupDraft:     "label-taken",
		SetupEmoji:     "❌",
	},
	{
		Name:           "orphan",
		Description:    "Reaction on a non-existent msg_id — expect consumed=true dropped=true, no message",
		SetupUserMsgID: "om_orphan_no_draft",
		SetupDraft:     "", // no seed → tests the no-draft path
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

	// 1. Seed the draft (if the scenario calls for one).
	if sc.SetupDraft != "" {
		if err := gtwTestSeedDraft(mgr, msg.ChatID,
			sc.SetupUserMsgID, sc.SetupDraft); err != nil {
			return sendGTWReply(ctx, channel, msg.ChatID,
				"❌ /gtw test "+name+": seed failed: "+err.Error()), nil
		}
	}

	// 2. Synthesise the reaction event and dispatch through
	// the real gateway. The runtime's gtw action handler
	// (installed at startup) fires on the seeded draft; any
	// follow-up card the executor emits is sent to the chat
	// normally via deps.Send → channel. In tests, gw may be
	// nil — fall back to a minimal local gateway.
	if gw == nil {
		gw = New(func(_ context.Context, _ *InboundMessage) error { return nil })
	}
	res, err := gw.DispatchInbound(ctx, &InboundMessage{
		ChatID:     msg.ChatID,
		UserID:     msg.UserID,
		Text:       "",
		HasMention: true,
		Reaction: &chatsession.ReactionEvent{
			TargetMsgID: sc.SetupUserMsgID,
			Emoji:       sc.SetupEmoji,
			UserID:      msg.UserID,
			ChatID:      msg.ChatID,
		},
	})
	if err != nil {
		return sendGTWReply(ctx, channel, msg.ChatID,
			"❌ /gtw test "+name+": dispatch error: "+err.Error()), nil
	}

	// 3. Report. The reply is the scenario description (so
	// the user can match against expected behaviour) plus the
	// actual dispatch result.
	result := "?"
	if res != nil {
		if res.Consumed && !res.Dropped {
			result = "consumed=true dropped=false (handler acted)"
		} else if res.Consumed && res.Dropped {
			result = "consumed=true dropped=true (handler ran but declined; e.g. unrecognised emoji, no draft, or gtw action handler not wired)"
		} else {
			result = fmt.Sprintf("consumed=false dropped=%v (dispatcher rejected)", res.Dropped)
		}
	}
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("🧪 /gtw test %s\n  setup: %s\n  react:  %s %s\n  result: %s\n  expected: see scenario description above; any follow-up card is in the chat thread.",
			name, sc.Description, sc.SetupEmoji, sc.SetupUserMsgID, result)), nil
}

// runGTWTestHelp prints the scenario list.
func runGTWTestHelp(ctx context.Context, channel Channel, msg *InboundMessage) *CommandResult {
	var b strings.Builder
	b.WriteString("🧪 /gtw test <scenario>  — preset-driven reaction exerciser\n\n")
	b.WriteString("Each scenario bakes the full setup: it seeds the right\n")
	b.WriteString("gtwDraft (if needed) and dispatches the right reaction.\n")
	b.WriteString("No parameters to figure out — just type the scenario name.\n\n")
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
	for _, d := range cs.ListGTWDrafts() {
		_ = cs.TakeGTWDraft(string(d.Kind))
	}
	cs.ClearGTWContext()
	after := len(cs.ListGTWDrafts())
	return sendGTWReply(ctx, channel, msg.ChatID,
		fmt.Sprintf("✅ /gtw test reset — cleared gtwContext (drafts before=%d after=%d)", before, after)), nil
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
		CreatedAt: time.Now(),
	})
	return nil
}
