// Package main — `nightme debug` subcommands.
//
// Debug subcommands let you exercise the F-45 reaction/action
// pipeline in isolation, without spinning up a Feishu sandbox
// or a real running daemon. They construct a minimal gateway +
// chatsession stack in-process, then drive the dispatcher (and
// optionally the gtw executor) the same way the real runtime
// would.
//
// All subcommands share the same in-memory fixtures:
//
//   - a chatsession.Manager with no spawner
//   - a gateway.Gateway with the F-45 actionHandler installed
//     (looks up the ChatSession via mgr and calls HandleAction,
//     which delegates to gtw.HandleAction)
//   - a fake channel that captures every OutboundMessage for
//     post-mortem printing
//
// Subcommands:
//
//	nightme debug action <msg_id> <emoji>
//	    Synthesise a reaction event and run it through
//	    gateway.DispatchInbound. Prints the dispatch result +
//	    every message the runtime would have sent. Use this to
//	    verify the F-45 reaction-routing path end-to-end
//	    without a Feishu setup.
//
//	    With --seed=<kind>, a gtwDraft is stored in the chat
//	    BEFORE the reaction is dispatched, so you can test
//	    specific scenarios without needing a real /gtw fix run:
//
//	        nightme debug action --seed branch-exists om_card_abc ❌
//	            --issue 42 --branch fix/42-test --repo cnlangzi/nightme
//
//	nightme debug seed <user_msg_id>
//	    Manually store a gtwDraft in the chat (keyed by
//	    <user_msg_id>). Required --seed=<kind> flag. Equivalent
//	    to running a /gtw fix that hits a decision-card branch.
//
//	nightme debug drafts
//	    Print every gtwDraft currently stored in the chat
//	    (keyed by user message id, with full payload JSON).
//	    Lets you inspect the decision-card stack after a
//	    reaction has been processed.
//
//	nightme debug drain
//	    Drop every gtwDraft in the chat (reset to clean state).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/chatsession"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/command/gtw"
)

// debugFlags captures the shared flags.
type debugFlags struct {
	chatID  string
	userID  string
	seedKind string
	issueID int
	branch  string
	repo    string
	title   string
}

func newDebugCmd() *cobra.Command {
	var f debugFlags

	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Exercise F-45 reaction/action flow without Feishu",
		Long: "debug subcommands build a minimal in-memory gateway + chatsession\n" +
			"stack and drive the dispatcher the same way the real runtime\n" +
			"would. Useful for manually verifying reaction routing +\n" +
			"draft execution end-to-end without a Feishu sandbox.\n\n" +
			"Subcommands:\n" +
			"  action --chat <id> <msg_id> <emoji>   synthesise a reaction + dispatch\n" +
			"  seed --chat <id> --seed=<kind> <id>  store a gtwDraft for testing\n" +
			"  drafts --chat <id>                  list gtwDrafts in the chat\n" +
			"  drain --chat <id>                   drop every gtwDraft in the chat",
	}
	cmd.PersistentFlags().StringVar(&f.chatID, "chat", "debug", "synthetic chat id")
	cmd.PersistentFlags().StringVar(&f.userID, "user", "ou_debug_user", "synthetic user id (for reactions)")
	cmd.PersistentFlags().StringVar(&f.seedKind, "seed", "", "seed a gtwDraft before dispatching; one of: branch-exists, worktree-fail, label-taken")
	cmd.PersistentFlags().IntVar(&f.issueID, "issue", 42, "issue id used when seeding a gtwDraft")
	cmd.PersistentFlags().StringVar(&f.branch, "branch", "fix/42-test-branch", "branch name used in the seeded gtwDraft payload")
	cmd.PersistentFlags().StringVar(&f.repo, "repo", "cnlangzi/nightme", "owner/repo used in the seeded gtwDraft payload")
	cmd.PersistentFlags().StringVar(&f.title, "title", "synthetic /gtw fix test issue", "issue title used in the seeded gtwDraft payload")

	// action subcommand: <msg_id> <emoji> (positional)
	actionCmd := &cobra.Command{
		Use:   "action <msg_id> <emoji>",
		Short: "Synthesise a reaction and run it through gateway.DispatchInbound",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugAction(cmd, f, args[0], args[1])
		},
	}

	draftsCmd := &cobra.Command{
		Use:   "drafts",
		Short: "Print every gtwDraft in the chat (keyed by user message id)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDebugDrafts(cmd, f)
		},
	}

	seedCmd := &cobra.Command{
		Use:   "seed <user_msg_id>",
		Short: "Manually store a gtwDraft in the chat (so subsequent reactions have a target)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDebugSeed(cmd, f, args[0])
		},
	}

	drainCmd := &cobra.Command{
		Use:   "drain",
		Short: "Drop every gtwDraft in the chat (reset to clean state)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDebugDrain(cmd, f)
		},
	}

	cmd.AddCommand(actionCmd, draftsCmd, seedCmd, drainCmd)
	return cmd
}

// debugFixture is the in-process stack every debug subcommand uses.
type debugFixture struct {
	cs       *chatsession.ChatSession
	mgr      *chatsession.Manager
	gtwMgr   *gtw.Manager
	gw       gateway.Gateway
	captured *capturingChannel
}

func newDebugFixture(f debugFlags) (*debugFixture, error) {
	if f.chatID == "" {
		return nil, fmt.Errorf("--chat is required")
	}

	// 1. chatsession.Manager (no spawner — gtw.RunFix's SetActiveCwd
	// goes through the chat directly, no agent process needed).
	mgr := chatsession.NewManager()

	// 2. Capturing channel — built FIRST so the gtw deps can use
	// it as the Send target. Records every OutboundMessage the
	// gtw executor emits.
	captured := &capturingChannel{
		chatID: f.chatID,
		msgs:   []gateway.OutboundMessage{},
	}

	// 3. gtw deps — minimal: no real gh/glab (the executor's
	// platform call will fail and the cancel/new-v2/retry paths
	// will exercise their failure branches, which is itself
	// useful test coverage). Send pushes to captured so we can
	// print what the executor emitted.
	gtwDeps := gtw.HandlerDeps{
		Git:         gtw.ExecGitRunner{},
		Prober:      &gtw.ExecHTTPProber{},
		Now:         func() time.Time { return timeNow() },
		Send: func(_ context.Context, m gtw.OutMsg) error {
			captured.mu.Lock()
			defer captured.mu.Unlock()
			captured.msgs = append(captured.msgs, gateway.OutboundMessage{
				ChatID: m.ChatID, Kind: gateway.OutReply, Text: m.Text, ReplyTo: m.ReplyTo,
			})
			return nil
		},
	}

	// 4. F-51: build a fresh gtw.Manager + services.ReactionRouter
	// and register gtwMgr.HandleReaction. The gateway's
	// WithActionHandler below calls router.Handle for each
	// reaction event. (Pre-F-51 this was
	// gateway.RegisterGTWAction(mgr, gtwDeps), which installed
	// SetActionHandler on each ChatSession. F-51 removed that
	// path — reactions are now router-based.)
	gtwMgr := gtw.NewManager()
	gtwMgr.SetHandlerDeps(gtwDeps)
	gtwRouter := commandServices.NewReactionRouter()
	gtwRouter.Register("*", gtwMgr.HandleReaction)

	// 5. Create the ChatSession — triggers the WithOnCreate
	// callback, which installs the SetActionHandler.
	cs := mgr.GetOrCreate(f.chatID, "primary")

	// 6. Gateway with actionHandler + capturing channel.
	// WithActionHandler is on the Gateway interface (not just
	// *Router) per the F-45 dispatcher work. WithWatchModeResolver
	// is only on the concrete *Router type, not the public
	// interface; we skip it here because the default WatchMode
	// gate (WatchModeAll-equivalent fall-through for reaction
	// events) is what we want for debug mode.
	gw := gateway.New(captured.MessageDispatcher())
	gw.AttachChannels(captured)
	if router, ok := gw.(*gateway.Router); ok {
		router.WithWatchModeResolver(func(_ string) (chatsession.WatchMode, bool) {
			return chatsession.WatchModeAll, true
		})
	}
	gw.WithActionHandler(func(ctx context.Context, msg *gateway.InboundMessage) bool {
		if msg == nil || msg.Reaction == nil {
			return false
		}
		return gtwRouter.Handle(ctx, msg.ChatID, commandServices.ReactionEvent{
			TargetMsgID: msg.Reaction.TargetMsgID,
			Emoji:       msg.Reaction.Emoji,
			UserID:      msg.Reaction.UserID,
			ChatID:      msg.Reaction.ChatID,
		})
	})

	return &debugFixture{
		cs:       cs,
		mgr:      mgr,
		gtwMgr:   gtwMgr,
		gw:       gw,
		captured: captured,
	}, nil
}

// runDebugAction synthesises a reaction and runs it through
// gateway.DispatchInbound. Prints the dispatch result and every
// captured OutboundMessage.
//
// Example usage:
//
//	nightme debug action om_card_abc ✅
//	  --chat oc_chat_xyz --user ou_alice
//
// With --seed=<kind>, a gtwDraft is stored in the chat BEFORE
// the reaction is dispatched, so you can test specific
// scenarios without needing a real /gtw fix run:
//
//	nightme debug action --seed branch-exists om_card_abc ❌
//	  --issue 42 --branch fix/42-test --repo cnlangzi/nightme
func runDebugAction(cmd *cobra.Command, f debugFlags, msgID, emoji string) error {
	if msgID == "" || emoji == "" {
		return fmt.Errorf("usage: nightme debug action <msg_id> <emoji>")
	}
	fix, err := newDebugFixture(f)
	if err != nil {
		return err
	}

	if f.seedKind != "" {
		if err := seedDraft(fix, f, msgID, f.seedKind); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[seeded] kind=%s userMsgID=%q issue=%d\n",
			f.seedKind, msgID, f.issueID)
	}

	inbound := &gateway.InboundMessage{
		ChatID:     f.chatID,
		UserID:     f.userID,
		Text:       "", // reactions have no text
		HasMention: true,
		Reaction: &commandServices.ReactionEvent{
			TargetMsgID: msgID,
			Emoji:       emoji,
			UserID:      f.userID,
			ChatID:      f.chatID,
		},
	}

	res, err := fix.gw.DispatchInbound(context.Background(), inbound)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "[dispatch error] %v\n", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"[dispatch result] consumed=%v dropped=%v\n",
		res != nil && res.Consumed,
		res != nil && res.Dropped,
	)
	fmt.Fprintf(cmd.OutOrStdout(), "[reaction] targetMsgID=%q emoji=%q userID=%q\n",
		msgID, emoji, f.userID)
	return printCaptured(cmd, fix.captured)
}

// runDebugSeed manually stores a gtwDraft in the chat. Useful when
// you want to test specific scenarios (cancel, new-v2, retry)
// without running a full /gtw fix first.
func runDebugSeed(cmd *cobra.Command, f debugFlags, userMsgID string) error {
	if userMsgID == "" {
		return fmt.Errorf("usage: nightme debug seed <user_msg_id> --seed=<kind>")
	}
	if f.seedKind == "" {
		return fmt.Errorf("--seed is required (one of: branch-exists, worktree-fail, label-taken)")
	}
	fix, err := newDebugFixture(f)
	if err != nil {
		return err
	}
	if err := seedDraft(fix, f, userMsgID, f.seedKind); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "[seeded] kind=%s userMsgID=%q issue=%d\n",
		f.seedKind, userMsgID, f.issueID)
	return nil
}

// runDebugDrain drops every gtwDraft in the chat.
func runDebugDrain(cmd *cobra.Command, f debugFlags) error {
	fix, err := newDebugFixture(f)
	if err != nil {
		return err
	}
	// F-51: state lives on gtw.Manager; chatsession no longer
	// knows about gtw. Reset the manager's per-chat state.
	before := fix.gtwMgr.DraftCount(f.chatID)
	fix.gtwMgr.Reset(f.chatID)
	fmt.Fprintf(cmd.OutOrStdout(),
		"drained gtw for chat %q (cleared %d drafts + context)\n",
		f.chatID, before)
	return nil
}

// runDebugDrafts lists every gtwDraft in the chat.
func runDebugDrafts(cmd *cobra.Command, f debugFlags) error {
	fix, err := newDebugFixture(f)
	if err != nil {
		return err
	}
	drafts := fix.gtwMgr.ListDrafts(f.chatID)
	if len(drafts) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no gtwDrafts in chat %q\n", f.chatID)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "gtwDrafts in chat %q (%d):\n", f.chatID, len(drafts))
	for i, d := range drafts {
		encoded, _ := json.MarshalIndent(d.Payload, "      ", "  ")
		fmt.Fprintf(cmd.OutOrStdout(),
			"  [draft-%d] kind=%s createdAt=%s\n      payload=%s\n",
			i, d.Kind, d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), string(encoded),
		)
	}
	return nil
}

// seedDraft synthesises a gtwDraft and stores it in the chat
// under userMsgID. The payload mirrors what gtw.RunFix would
// emit, so the action handler's downstream behaviour (label
// removal, worktree add, etc.) can be exercised.
func seedDraft(fix *debugFixture, f debugFlags, userMsgID, kind string) error {
	var draftKind gtw.DraftKind
	switch kind {
	case "branch-exists":
		draftKind = gtw.DraftFixBranchExists
	case "worktree-fail":
		draftKind = gtw.DraftFixWorktreeFail
	case "label-taken":
		draftKind = gtw.DraftFixLabelTaken
	default:
		return fmt.Errorf("unknown --seed=%q (want branch-exists | worktree-fail | label-taken)", kind)
	}
	fix.gtwMgr.StoreDraft(f.chatID, userMsgID, &gtw.Draft{
		Kind: draftKind,
		Payload: gtw.FixDraftPayload{
			IssueID:    f.issueID,
			Title:      f.title,
			Branch:     f.branch,
			Slug:       fmt.Sprintf("%d-test-slug", f.issueID),
			Repo:       f.repo,
			Provider:   "github",
			LabelAdded: kind != "worktree-fail", // branch-exists & label-taken added it; worktree-fail did not
			ChatID:     f.chatID,
		},
		CreatedAt: timeNow(),
	})
	return nil
}

// printCaptured prints every OutboundMessage the runtime would
// have sent during the test, in the order they were queued.
func printCaptured(cmd *cobra.Command, captured *capturingChannel) error {
	if len(captured.msgs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "captured messages: (none)")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "captured messages (%d):\n", len(captured.msgs))
	for i, m := range captured.msgs {
		preview := m.Text
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  [%d] kind=%d replyTo=%q\n      preview: %s\n",
			i+1, m.Kind, m.ReplyTo, preview,
		)
	}
	return nil
}

// timeNow is a tiny helper so the seed timestamps are stable
// across runs.
func timeNow() (t time.Time) {
	return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
}

// capturingChannel is a minimal in-memory channel that records
// every OutboundMessage the gateway sends, for the debug
// subcommands to print. It implements the channel.Channel
// interface (just Name + Send + Incoming + Start + Stop).
type capturingChannel struct {
	chatID string
	mu     sync.Mutex
	msgs   []gateway.OutboundMessage
}

func (c *capturingChannel) Name() string { return "capture" }
func (c *capturingChannel) Start(_ context.Context) error { return nil }
func (c *capturingChannel) Stop(_ context.Context) error  { return nil }

func (c *capturingChannel) SendCard(_ context.Context, m gateway.OutboundMessage) (string, error) {
	_ = c.Send(context.Background(), m)
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("capture-card-%d", len(c.msgs)), nil
}

func (c *capturingChannel) Send(_ context.Context, m gateway.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

// MessageDispatcher returns the runtime-side plain-text dispatcher.
// In the debug fixtures, reactions are short-circuited by the
// actionHandler before they get here, so this is a safety net
// that should rarely fire. It returns nil (= the gateway sees
// the dispatch as consumed-success) so the test doesn't error
// out on an unexpected fall-through.
func (c *capturingChannel) MessageDispatcher() gateway.MessageDispatcher {
	return func(_ context.Context, _ *gateway.InboundMessage) error { return nil }
}

// Incoming is required by the channel.Channel interface. The
// debug subcommands never publish to it; the channel is closed
// immediately.
func (c *capturingChannel) Incoming() <-chan gateway.InboundMessage {
	ch := make(chan gateway.InboundMessage, 1)
	close(ch)
	return ch
}
