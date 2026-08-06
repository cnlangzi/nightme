// Package main — `nightme run` long-running Feishu daemon.
//
// The daemon wires together:
//
//   - chatsession.Manager (per-chat ChatSession table)
//   - chatsession.NewRegistrySpawner (lazy fork via agent.Registry)
//   - chatsession.InputBuffer FSM (commit 9; ownership moved to ChatSession)
//   - gateway.RegisterChatSessionCommands (/cwd /use /kill slash commands)
//   - EventCallback: each AgentSession.Events() is consumed by a
//     per-active-AS readPump goroutine that translates AgentEvent →
//     OutboundMessage → channel.Send, AND drives the InputBuffer FSM
//     (non-terminal events → SetBusy; EventDone / Error → SetIdle +
//     OnTurnEnded → flush via the runtime-installed FlushHook).
//

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/registry"
)

// runDeps holds the construction seams for the daemon: every
// dependency is injectable for deterministic tests.
type runDeps struct {
	loadConfig     func() (*config.Config, error)
	openChatSessions func(*config.Config) (*registry.ChatSessionFile, error)
	openAgentSessions func(*config.Config) (*registry.AgentSessionFile, error)
	buildAgents    func(*config.Config) *agent.Registry
	newChannel     func(*config.Config) (channel.Channel, error)
	signals        <-chan os.Signal
	cleanup        bool
	skipFeishuAuth bool
	onReady        func()

	// registerHealth, if non-nil, is called after the channel is
	// constructed and started; the closure wires a closure returning
	// the channel's live WS lifecycle snapshot into the daemoncontrol
	// server's "health" RPC. See daemon_lifecycle.go's daemon child.
	registerHealth func(ch channel.Channel, register func() (string, json.RawMessage, error))
}

func defaultRunDeps() runDeps {
	return runDeps{
		loadConfig:        config.LoadDefault,
		openChatSessions:  defaultOpenChatSessions,
		openAgentSessions: defaultOpenAgentSessions,
		buildAgents:       buildRunAgentRegistry,
		newChannel: func(cfg *config.Config) (channel.Channel, error) {
			return feishu.NewAdapter(cfg)
		},
	}
}

// defaultOpenChatSessions opens chat_sessions.json relative to
// cfg.Paths.DataDir.
func defaultOpenChatSessions(cfg *config.Config) (*registry.ChatSessionFile, error) {
	path, err := chatSessionsPath(cfg)
	if err != nil {
		return nil, err
	}
	return registry.OpenChatSessionFile(path)
}

// defaultOpenAgentSessions opens agent_sessions.json relative to
// cfg.Paths.DataDir.
func defaultOpenAgentSessions(cfg *config.Config) (*registry.AgentSessionFile, error) {
	path, err := agentSessionsPath(cfg)
	if err != nil {
		return nil, err
	}
	return registry.OpenAgentSessionFile(path)
}

// newRunCmd builds the long-running Feishu daemon command.
func newRunCmd() *cobra.Command {
	var cleanup bool
	var channelName string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the Feishu daemon (ChatSession-based runtime)",
		Long: "run starts the Feishu WebSocket channel and serves a Gateway " +
			"router on top of it. Slash commands (/cwd, /use, /kill, /help) " +
			"drive session lifecycle; plain text is forwarded to the live " +
			"agent behind the chat's active AgentSession.\n\n" +
			"By default the daemon detaches session CLIs on shutdown so a " +
			"later `nightme run` (or /use) can resume them. Pass --cleanup " +
			"to instead Kill() every session on SIGINT/SIGTERM — useful for " +
			"CI or one-shot runs.\n\n" +
			"Pass --channel=echo to run the daemon with the echo channel " +
			"(a no-network stub that prints outbound messages to stdout). " +
			"Useful for smoke tests.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd, cleanup, channelName)
		},
	}
	cmd.Flags().BoolVar(&cleanup, "cleanup", false,
		"Kill every session CLI on shutdown instead of detaching them")
	cmd.Flags().StringVar(&channelName, "channel", "feishu",
		"Channel implementation: feishu (default) or echo (smoke test)")
	return cmd
}

// runRun dispatches to the daemon. Channel selection via
// the --channel flag (feishu | echo).
func runRun(cmd *cobra.Command, cleanup bool, channelName string) error {
	if channelName != "" && channelName != "feishu" && channelName != "echo" {
		return fmt.Errorf("run: unknown channel %q (want feishu or echo)", channelName)
	}
	deps := withChannel(defaultRunDeps(), channelName)
	deps.cleanup = cleanup
	return runRunWith(cmd, deps)
}

// withCleanup configures whether the daemon kills every session
// CLI on shutdown (true) or detaches them so a later restart can
// resume them (false). Exposed for tests; production wires this
// from the --cleanup flag via runRun.
func withCleanup(deps runDeps, cleanup bool) runDeps {
	deps.cleanup = cleanup
	return deps
}

// withChannel configures the runtime channel implementation
// (feishu | echo).
func withChannel(deps runDeps, channelName string) runDeps {
	switch channelName {
	case "feishu", "":
		// default — feishu.NewAdapter
	case "echo":
		deps.skipFeishuAuth = true
		deps.newChannel = func(*config.Config) (channel.Channel, error) {
			return echo.New("echo", os.Stdout), nil
		}
	}
	return deps
}

// runRunWith is the daemon entrypoint. Installs signal
// handling, fills in nil deps, delegates to runDaemon.
func runRunWith(cmd *cobra.Command, deps runDeps) error {
	if cmd == nil {
		return errors.New("run: command is required")
	}
	defaults := defaultRunDeps()
	if deps.loadConfig == nil {
		deps.loadConfig = defaults.loadConfig
	}
	if deps.openChatSessions == nil {
		deps.openChatSessions = defaults.openChatSessions
	}
	if deps.openAgentSessions == nil {
		deps.openAgentSessions = defaults.openAgentSessions
	}
	if deps.buildAgents == nil {
		deps.buildAgents = defaults.buildAgents
	}
	if deps.newChannel == nil {
		deps.newChannel = defaults.newChannel
	}

	sigCh := deps.signals
	if sigCh == nil {
		owned := make(chan os.Signal, 2)
		signal.Notify(owned, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(owned)
		sigCh = owned
	}
	out := io.Discard
	if cmd != nil {
		out = cmd.OutOrStdout()
	}
	return runDaemon(cmd.Context(), out, deps, sigCh)
}

// runDaemon is the daemon core. Wires chatsession.Manager +
// Spawner + EventCallback; runs the gateway until signal /
// context cancel.
func runDaemon(ctx context.Context, out io.Writer, deps runDeps, sigCh <-chan os.Signal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	logger := loggerFromContext(ctx)

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("run: load config: %w", err)
	}
	if cfg == nil {
		return errors.New("run: load config: returned nil config")
	}
	if !deps.skipFeishuAuth && (cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "") {
		return errors.New("run: Feishu credentials are not configured; run `nightme auth login feishu`")
	}

	csFile, err := deps.openChatSessions(cfg)
	if err != nil {
		return fmt.Errorf("run: open chat_sessions: %w", err)
	}
	asFile, err := deps.openAgentSessions(cfg)
	if err != nil {
		return fmt.Errorf("run: open agent_sessions: %w", err)
	}

	// Tidy up the obsolete v0.1 registry.json (the v1.2 daemon
	// no longer reads it). Best-effort.
	if err := removeLegacyRegistryFile(cfg); err != nil {
		logger.Warn("remove legacy registry.json", "err", err)
	}

	agents := deps.buildAgents(cfg)
	if agents == nil {
		return errors.New("run: agent registry is nil")
	}

	ch, err := deps.newChannel(cfg)
	if err != nil {
		return fmt.Errorf("run: create channel: %w", err)
	}
	if ch == nil {
		return errors.New("run: channel is nil")
	}

	// Build the ChatSession manager.
	spawner := chatsession.NewRegistrySpawner(agents)
	mgr := chatsession.NewManager().
		WithSpawner(spawner).
		WithPersistence(csFile, asFile)

	// startChannel wires the channel and starts its connection.
	if err := ch.Start(ctx); err != nil {
		logger.Error("channel disconnected", "reason", err)
		return fmt.Errorf("run: start channel: %w", err)
	}
	logger.Info("channel connected")
	fmt.Fprintln(out, "Channel connected")

	var fa *feishu.Adapter
	if f, ok := ch.(*feishu.Adapter); ok {
		fa = f
		fa.SetLogger(logger)
	}

	// F-40: register the WS lifecycle snapshot with the daemoncontrol
	// server so `nightme health` can answer. The closure captures `fa`
	// and is invoked on every "health" RPC; the closure itself is
	// safe to call concurrently because WSHealth.Snapshot takes the
	// read lock. (fa is captured by reference in the closure — the
	// local `fa` in this scope is never reassigned after this point,
	// so no `fa := fa` shadow is needed.)
	if deps.registerHealth != nil && fa != nil {
		deps.registerHealth(fa, func() (string, json.RawMessage, error) {
			snap := fa.Health()
			data, err := json.Marshal(snap)
			if err != nil {
				return "", nil, fmt.Errorf("encode health snapshot: %w", err)
			}
			return ch.Name(), data, nil
		})
	}

	// Build the router wiring (slashCommandDispatcher +
	// messageDispatcher).
	messageDispatcher := newMessageDispatcher(mgr, ch, cfg.Primary, logger)

	// F-31 + F-think + F-38: install gw.OnMessageState AND the
	// runtime's EventHandler into every ChatSession via the
	// Manager.onCreate hook. Both callbacks MUST be installed in
	// this single closure — separating them (one here, one in
	// /use or newMessageDispatcher) is a silent-failure landmine
	// because readpump fires only when eventHandler is non-nil.
	//
	// ORDER MATTERS: WithOnCreate MUST be called BEFORE
	// RestoreFromRegistry. RestoreFromRegistry fires onCreate for
	// every restored ChatSession so handlers are installed
	// uniformly across restored + future chats. Calling
	// WithOnCreate after RestoreFromRegistry silently leaves the
	// restored chats without handlers — no MessageState reactions
	// (⏳/🔄), no OutReply / OutResult / OutTool* forwarding to
	// the channel. The user sees incoming messages arrive but no
	// outgoing follow-up. Bug surfaced when the user restarted
	// the daemon between F-38 implementation and first
	// interaction — fresh in-memory state from chat_sessions.json
	// had no handlers installed because WithOnCreate was set
	// after RestoreFromRegistry. This is a pre-existing bug from
	// the /think refactor (commit 5725a90) that F-38 surfaced
	// because F-38 added more dependency on the handlers actually
	// being installed. The fix is in
	// wireRuntimeCallbacksAndRestore (see below) which bundles
	// WithOnCreate + RestoreFromRegistry so the order can't be
	// inverted at the call site.
	gwImpl := gateway.New(messageDispatcher).(*gateway.Router)
	gateway.RegisterChatSessionCommands(gwImpl, mgr, ch, cfg.Primary)

	// F-51: gtw moved to internal/command/gtw. Wiring is now
	// (a) gtw.Manager owns the state, (b) services.ReactionRouter
	// dispatches reactions, (c) command.Commander routes slash
	// commands, (d) the runtime shim (defined below) translates
	// *gateway.InboundMessage ↔ command.SlashInput /
	// *CommandResult. The old gateway.RegisterGTW /
	// RegisterGTWAction helpers were deleted in F-51 (their
	// implementations moved to the new gtw.Factory + Manager).
	gtwDeps := gtw.HandlerDeps{
		Git:    gtw.ExecGitRunner{},
		Prober: &gtw.ExecHTTPProber{},
		// Send / SendCard are populated lazily by the chatSessionSender
		// (each chat gets its own Sender via gtwMgr.senderFactory).
		// gtw.RunFix / HandleDraftReaction use these per-chat Senders,
		// not the HandlerDeps-level Send. Leaving them nil here would
		// not crash the per-chat paths, but keeping the field explicit
		// makes the F-51 wiring contract easier to audit.
	}
	gtwMgr := gtw.NewManager()
	gtwMgr.SetHandlerDeps(gtwDeps)

	// F-51 P0 fix: wire the per-chat Sender factory. Without
	// this, /gtw fix and reaction paths would nil-deref on
	// GetSender. The factory lazily creates a Sender on the
	// first /gtw call (or reaction) per chat, then caches it.
	gtwMgr.SetSenderFactory(func(chatID string) gtw.Sender {
		return newChatSessionSender(
			chatID,
			newSessionAdapter(mgr, cfg.Primary).GetOrCreate(chatID, cfg.Primary),
			newChannelAdapter(ch),
		)
	})

	// Reaction router (services) — gtw registers its
	// HandleReaction at startup; other commands can do the
	// same in their own init() or factory construction.
	router := commandServices.NewReactionRouter()
	router.Register("*", gtwMgr.HandleReaction)

	// Slash command registry + commander.
	gtwFactory := gtw.NewFactoryWithDeps(gtwMgr, gtwDeps)
	reg := command.NewRegistry()
	reg.Register(gtwFactory)
	commander := command.NewCommander(reg)

	// RuntimeServices — fill all three slots via adapters
	// (B3). Session wraps *chatsession.Manager (the B5+
	// /cwd /use /kill handlers will use this); Channel
	// wraps *gateway.Channel (any command that needs to
	// send replies uses this).
	rt := command.RuntimeServices{
		Session:        newSessionAdapter(mgr, cfg.Primary),
		ReactionRouter: router,
		Channel:        newChannelAdapter(ch),
	}

	// Shim: translate *gateway.InboundMessage → command.SlashInput,
	// dispatch, translate back. The gateway.CommandResult shape only
	// carries Reply/Consumed/Dropped, so out.Outbound (multi-message
	// atomic batches such as card + reply) is intentionally dropped
	// here for now. When a future command needs the multi-message
	// path, extend gateway.CommandResult with an Outbound []OutboundMessage
	// slice and translate from command.Outbound here.
	gwImpl.WithCommander(func(ctx context.Context, msg *gateway.InboundMessage) (*gateway.CommandResult, error) {
		if msg == nil {
			return nil, nil
		}
		input := command.SlashInput{
			ChatID:     msg.ChatID,
			UserID:     msg.UserID,
			Text:       msg.Text,
			MessageID:  msg.MessageID,
			HasMention: msg.HasMention,
		}
		out, err := commander.Dispatch(ctx, rt, input)
		if err != nil {
			return &gateway.CommandResult{Consumed: true, Reply: "❌ " + err.Error()}, nil
		}
		if out == nil {
			return &gateway.CommandResult{Consumed: false}, nil
		}
		return &gateway.CommandResult{
			Consumed: out.Consumed,
			Dropped:  out.Dropped,
			Reply:    out.Reply,
		}, nil
	})

	gwImpl.AttachChannels(ch)

	// F-watch §3.1.1: install the per-chat WatchMode resolver so
	// the dispatcher can drop non-mention group messages when the
	// chat's mode is WatchModeMention (default). The resolver
	// consults the manager directly — no extra state, the registry
	// (chat_sessions.json) is the single source of truth.
	gwImpl.WithWatchModeResolver(func(chatID string) (chatsession.WatchMode, bool) {
		cs := mgr.Get(chatID)
		if cs == nil {
			return 0, false
		}
		return cs.WatchMode(), true
	})

	// F-51: the action handler now routes through
	// services.ReactionRouter instead of calling
	// chatsession.ChatSession.HandleAction (chatsession no
	// longer dispatches reactions). The shim translates
	// *commandServices.ReactionEvent → services.ReactionEvent.
	slog.Default().Warn("F-51 debug: production WithActionHandler installed (router-based)")
	gwImpl.WithActionHandler(func(ctx context.Context, msg *gateway.InboundMessage) bool {
		if msg == nil || msg.Reaction == nil {
			return false
		}
		return router.Handle(ctx, msg.ChatID, commandServices.ReactionEvent{
			TargetMsgID: msg.Reaction.TargetMsgID,
			Emoji:       msg.Reaction.Emoji,
			UserID:      msg.Reaction.UserID,
			ChatID:      msg.Reaction.ChatID,
		})
	})

	// WithOnCreate fires for both restored (RestoreFromRegistry)
	// and future (GetOrCreate) ChatSessions. Place BEFORE
	// RestoreFromRegistry so restored chats get their handlers.
	if err := wireRuntimeCallbacksAndRestore(mgr, ch, gwImpl, logger); err != nil {
		return fmt.Errorf("run: wire+restore: %w", err)
	}

	// Start readPumps for already-running AgentSessions that
	// were restored from disk (Detached → running on next
	// LookupActiveAgentSession). The daemon does NOT auto-spawn
	// at startup; users must send a message (which triggers
	// LookupActiveAgentSession → Spawner).
	ensureReadPumps(mgr, ch, cfg.Primary, logger)

	if err := gwImpl.Start(ctx); err != nil {
		return fmt.Errorf("run: start gateway: %w", err)
	}
	defer func() { _ = gwImpl.Stop(context.Background()) }()

	if deps.onReady != nil {
		deps.onReady()
	}

	logger.Info("daemon running",
		"chat_sessions", len(mgr.List()),
		"primary", cfg.Primary)

	// Block on signal or context cancellation.
	select {
	case <-ctx.Done():
	case sig, ok := <-sigCh:
		if ok && sig != nil {
			fmt.Fprintf(out, "[nightme] received %s\n", sig)
		}
	}
	return shutdownRun(out, ch, mgr, csFile, asFile, deps.cleanup, logger)
}

// newMessageDispatcher builds the runtime-injected
// messageDispatcher (the default branch of the inboundDispatcher).
// It is invoked when no slash command matches; it routes the
// inbound message to the chat's active AgentSession via the
// InputBuffer.
//
// Flow:
//
//  1. cs = mgr.GetOrCreate(chatID, cfg.Primary)   // F-33: chatType removed
//  2. cs.LookupActiveAgentSession() (lazy spawn)
//  3. cs.QueueUserMessage(blocks, userMsgID) (Idle → flush now;
//     Busy → queue)
//  4. SetBusy on first event (drive FSM)
func newMessageDispatcher(mgr *chatsession.Manager, ch channel.Channel, primary string, logger *slog.Logger) func(context.Context, *gateway.InboundMessage) error {
	return func(ctx context.Context, msg *gateway.InboundMessage) error {
		if msg == nil {
			return nil
		}
		userMsgID := msg.MessageID
		if userMsgID == "" {
			userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}

		cs := mgr.GetOrCreate(msg.ChatID, primary)

		// F-31: ChatSession has accepted the message. Emit
		// StateReceived synchronously so the channel can render
		// ⏳ even before spawn resolves (FastAck UX).
		cs.EmitMessageState(userMsgID, agent.MessageReceived)

		// Resolve active AgentSession (lazy spawn on miss).
		_, err := cs.LookupActiveAgentSession()
		if err != nil {
			if errors.Is(err, chatsession.ErrNoActiveCwd) {
				return ch.Send(ctx, gateway.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   gateway.OutReply,
					Text:   "No workspace set. Send /cwd <path> first.",
				})
			}
			// Spawn failed (binary missing, etc.); let the user know.
			return ch.Send(ctx, gateway.OutboundMessage{
				ChatID: msg.ChatID,
				Kind:   gateway.OutReply,
				Text:   fmt.Sprintf("Failed to spawn agent: %v", err),
			})
		}

		// commit fix-5: start a readPump for the freshly-active
		// AgentSession. Without this, the spawned claude process
		// emits events on Events() but no one consumes them — the
		// user sees "hi" go in but no reply ever comes back.
		// handleUse also calls StartReadPump, but the FIRST message
		// (before any /use) only goes through newMessageDispatcher, so we
		// need to start the pump here too. StartReadPump is
		// idempotent — it stops any existing pump first, so calling
		// it again from handleUse is a no-op.
		_ = cs.StartReadPump()

		// F-31: dispatch successful — message has reached the
		// AgentSession. Emit StateForwarded so the channel flips
		// ⏳ → 🔄. (Emitted before QueueUserMessage so the visual
		// transition is visible even if queueing is slow.)
		cs.EmitMessageState(userMsgID, agent.MessageForwarded)

		// Build structured blocks and queue to InputBuffer.
		// F-14 v1.4b: post rich-text messages arrive with
		// msg.Blocks already populated (ordered by Feishu paragraph)
		// and LocalPath back-filled. Prefer msg.Blocks when non-nil;
		// otherwise fall back to the legacy BuildBlocks(msg.Text,
		// msg.Attachments) shape (single-resource msg_types).
		var blocks []agent.ContentBlock
		var blocksPath string
		if len(msg.Blocks) > 0 {
			blocks = msg.Blocks
			blocksPath = "ordered_blocks"
		} else {
			blocks = feishu.BuildBlocks(msg.Text, msg.Attachments)
			blocksPath = "legacy_build_blocks"
		}
		// F-14 visibility: before queuing, trace what the agent will
		// actually receive. Specifically: if blocks only contains
		// ContentText (no ContentImage/File), the build layer dropped
		// the attachments — most likely DownloadAttachments was not
		// called upstream. With logging.level=debug this line shows the
		// block types + total length so we can pinpoint the loss layer.
		if logger != nil {
			types := make([]string, 0, len(blocks))
			for _, b := range blocks {
				types = append(types, string(b.Type))
			}
			logger.Debug("dispatcher: blocks built for queue",
				"chat_id", msg.ChatID,
				"user_msg_id", userMsgID,
				"path", blocksPath,
				"inbound_attachments", len(msg.Attachments),
				"block_count", len(blocks),
				"block_types", types,
			)
		}
		if err := cs.QueueUserMessage(blocks, userMsgID); err != nil {
			if errors.Is(err, chatsession.ErrBufferFull) {
				return ch.Send(ctx, gateway.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   gateway.OutReply,
					Text:   "Input buffer full. Send /flush or /clear.",
				})
			}
			return err
		}
		return nil
	}
}

// ensureReadPumps walks every ChatSession and ensures a readPump
// is running for its current active AgentSession. Called at
// startup after RestoreFromRegistry; the AgentSessions are
// Detached (no process), so this is a no-op for restored
// wireRuntimeCallbacksAndRestore installs the per-ChatSession
// outbound handlers (EventHandler for AgentEvent → OutboundMessage
// translation; MessageStateHandler for F-31 lifecycle reactions)
// via Manager.WithOnCreate, then restores persisted ChatSessions
// from disk. The two calls MUST happen in this order —
// RestoreFromRegistry fires onCreate for every restored
// ChatSession, so any callback registered after RestoreFromRegistry
// silently misses every restored chat (outgoing events become
// invisible: no logs, no channel.Send, no reactions).
//
// Bundling both calls in one helper makes the order impossible
// to get wrong at the call site. See cmd/nightme/run_test.go
// for the regression coverage that pins this contract.
//
// Bug history: F-think (commit 5725a90) introduced the
// WithOnCreate wiring but called it AFTER RestoreFromRegistry —
// the silent failure went unnoticed because MessageState
// reactions are subtle (⏳/🔄/✅/❌ only). F-38 (which flipped
// the default ToolsMode to Hide, deepening the handler's
// runtime dependency on being actually installed) surfaced the
// bug when the user restarted between F-38 implementation and
// first interaction. Manager-level contract is covered in
// chatsession/manager_test.go; this helper's test covers the
// cmd/nightme/run.go wiring specifically.
func wireRuntimeCallbacksAndRestore(
	mgr *chatsession.Manager,
	ch channel.Channel,
	gwImpl *gateway.Router,
	logger *slog.Logger,
) error {
	factory := func(cs *chatsession.ChatSession) chatsession.EventHandler {
		return newEventHandler(ch, cs, mgr, logger)
	}
	mgr.WithOnCreate(func(cs *chatsession.ChatSession) {
		cs.SetEventHandler(factory(cs))
		// F-48: wrap the gateway's OnMessageState so the runtime
		// can stamp SessionContext on MessageForwarded (the
		// Feishu placeholder card needs the footer). The gateway's
		// bare OnMessageState doesn't accept SessionContext — the
		// runtime is the right owner of the stamp because it has
		// access to the AgentSession via cs.ActiveAgentSession().
		// We bypass gwImpl.OnMessageState entirely here: the
		// runtime wrapper does the same thing (validate, build
		// OutboundMessage, send) plus the stamp on MessageForwarded.
		// Other states (Received / Done / Error) don't need the
		// stamp — they don't create UI.
		cs.SetMessageStateHandler(func(chatID, userMsgID string, state agent.MessageState) {
			// Review fix: replicate the gateway's identifier
			// validation. Empty chatID / userMsgID would produce
			// an OutboundMessage with empty routing fields, which
			// the Feishu adapter rejects ("feishu: OutMessageState
			// missing MessageID") and which previously was a
			// silent no-op via gwImpl.OnMessageState's early return.
			if chatID == "" || userMsgID == "" {
				return
			}
			out := gateway.OutboundMessage{
				Kind:    gateway.OutMessageState,
				ChatID:  chatID,
				ReplyTo: userMsgID,
				MessageState: &gateway.MessageStatePayload{
					State:     state,
					MessageID: userMsgID,
				},
			}
			if state == agent.MessageForwarded {
				if as := cs.ActiveAgentSession(); as != nil {
					sessionContextInto(&out, as)
				}
			}
			if err := ch.Send(context.Background(), out); err != nil {
				logger.Warn("runtime: MessageState send failed",
					"chat_id", chatID,
					"state", state,
					"err", err)
			}
		})
	})
	return mgr.RestoreFromRegistry()
}

// sessions — readPumps are started lazily on first /use + send.
//
// The runtime's actual readPump start happens in handleUse
// (gateway package) and on first message dispatch in
// newMessageDispatcher.
func ensureReadPumps(mgr *chatsession.Manager, ch channel.Channel, primary string, logger *slog.Logger) {
	// no-op for now; reserved for future startup-time readPump wiring.
	_ = mgr
	_ = ch
	_ = primary
	_ = logger
}

// newEventHandler returns the per-event callback installed on
// every ChatSession by the runtime. The callback translates
// AgentEvent → OutboundMessage and dispatches via the channel.
//
// chatID is passed by ChatSession's readPump directly (the
// ChatSession knows its own ChatID); the handler doesn't need
// to look it up.
//
// userMsgID is the current turn's single anchor (passed by
// readPump from cs.currentTurnUserMsgID). The handler stamps it
// onto OutboundMessage.ReplyTo so each Channel can route the
// event to its own per-userMsgID receipt (card / thread / DOM
// node). Empty when the event has no anchor (startup EventInit
// etc.) — Channel falls back to plain text in that case.
//
// v1.3 (SPEC §2.2): 1 turn : 1 anchor. Receipt rendering and
// FSM are Channel-internal; Gateway only knows about userMsgID.
//
// Per-cs construction (not per-Mgr): the F-think gate reads
// cs.ThinkMode() on every OutThinking event, and the readPump
// fires only for ChatSessions that already exist, so the
// ChatSession is statically known at install time. Capturing it
// in the closure eliminates the per-event mgr.Get round-trip
// (RLock + map lookup). mgr is still passed because EventInit
// persistence needs mgr.PersistAgentSession, which is the cold
// path (once per AgentSession lifetime, not per event).
func newEventHandler(ch channel.Channel, cs *chatsession.ChatSession, mgr *chatsession.Manager, logger *slog.Logger) chatsession.EventHandler {
	// Per-cs closure. No per-handler mutable state needed anymore:
	// the bridge layer now attaches per-turn Usage to the SAME
	// ResultEvent that delivers the final text (claudecode
	// result.usage + result.modelUsage; Pi message_end.usage), so
	// the runtime no longer needs to buffer OutResult until a
	// following EventUsage lands. The previous EventResult-then-
	// EventUsage two-event split was a bridge-layer artifact — the
	// data was always co-located on the wire.
	return func(chatID string, s *chatsession.AgentSession, ev agent.AgentEvent, userMsgID string) {
		// Capture the agent's own session id from EventInit so the
		// next respawn can replay `--resume <id>`. We persist
		// immediately (rather than waiting for the next status
		// transition) so a daemon crash after this event still
		// remembers the id. The capture is idempotent.
		//
		// Guard: only overwrite an existing (non-empty) ResumeID when
		// the new id is non-empty. Some bridges re-emit EventInit
		// after a child restart with a blank SessionID; we don't
		// want to wipe a previously-captured id in that case.
		if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.SessionID != "" {
			s.SetResumeID(ev.Init.SessionID)
			if mgr != nil {
				if err := mgr.PersistAgentSession(s); err != nil && logger != nil {
					logger.Warn("persist agent session (init) failed",
						"chat_id", chatID,
						"agent_session_id", s.ID,
						"err", err)
				}
			}
		}
		// F-45 §1.4: capture model on first EventInit. Independent
		// of the SessionID capture above (some bridges may emit
		// one without the other). SetModel is idempotent — empty
		// incoming values don't overwrite.
		if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.Model != "" {
			s.SetModel(ev.Init.Model)
		}
		// Per-turn usage accumulation moved out of an EventUsage
		// branch (the kind was removed) — the bridge now attaches
		// Usage to the same AgentEvent that delivers Result (see
		// agent.ResultEvent.Usage). Translate copies it onto
		// OutboundMessage.Usage; we fold it into CumulativeUsage
		// right after Translate, BEFORE stamping SessionContext.
		// OutboundMessage.Usage may be nil (zero-usage turn,
		// synthetic assistant message) — that's fine, we just
		// skip AccumulateUsage for that invocation.
		//
		// F-45 §2.5 改动 D: persist cumulative stats on EventDone
		// (turn end) so each turn costs at most one file write.
		// PersistIfDirty is a no-op when cumulative is unchanged
		// (pure chat without agent invocation doesn't write).
		if ev.Kind == agent.EventDone {
			if err := s.PersistIfDirty(func(e *registry.AgentSessionEntry) error {
				if mgr == nil {
					return nil
				}
				return mgr.PersistAgentSession(s)
			}); err != nil && logger != nil {
				logger.Warn("persist agent session (usage) failed",
					"chat_id", chatID,
					"agent_session_id", s.ID,
					"err", err)
			}
		}
		// F-49: bump the AgentSession's compaction counter on every
		// EventCompaction. Bridges have already digested their
		// protocol differences (Pi suppresses its transient
		// `compaction_start`; Claude Code emits one event per
		// cycle) so every EventCompaction here represents exactly
		// one completed compaction cycle. No OutboundMessage is
		// produced — the runtime is the single owner of compaction
		// bookkeeping, and the count surfaces to the user later
		// via SessionContext.CompactionCount → Footer Line 1
		// "🗜 N". We `return` early so gateway.Translate is never
		// consulted for this kind (it would return
		// `(zero, false)` anyway after F-49 commit 4).
		// See docs/feat/F-49-compaction-counter.md §1.3 / §1.8.
		if ev.Kind == agent.EventCompaction {
			s.RecordCompaction()
			if logger != nil {
				logger.Debug("runtime: compaction observed",
					"chat_id", chatID,
					"agent", s.Agent,
					"agent_session_id", s.ID,
					"count", s.CompactionCount())
			}
			return
		}
		// Translate the AgentEvent to an OutboundMessage.
		out, ok := gateway.Translate(chatID, ev)
		if !ok {
			return
		}
		// Fold the per-turn Usage into CumulativeUsage NOW, before
		// we stamp SessionContext below — single-event design
		// (ResultEvent.Usage rides with Result). nil is fine
		// (zero-usage turn / synthetic message).
		if out.Usage != nil {
			s.AccumulateUsage(out.Usage)
		}
		// Stamp the current turn's anchor so the Channel can
		// route to the right per-userMsgID receipt. ReplyTo
		// stays empty for orphan events (EventInit at startup,
		// internal logs) — Channel renders those as plain text.
		out.ReplyTo = userMsgID
		// F-45 §2.5 改动 C: stamp SessionContext snapshot on the
		// four main-chat Kinds. Other Kinds (thread-only,
		// lifecycle, init/usage payloads themselves) skip the
		// footer — they don't surface in the main chat timeline
		// and would only inflate payload size.
		//
		// F-48 follow-up: MessageState events are stamped at
		// the wireRuntimeCallbacksAndRestore wrapper (not here)
		// — they don't flow through gateway.Translate + this
		// handler, so the case below would never fire.
		switch out.Kind {
		case gateway.OutReply, gateway.OutResult,
			gateway.OutTaskCreate, gateway.OutTaskUpdate:
			sessionContextInto(&out, s)
		}
		// F-think §3.1.2: per-chat OutThinking gate. When the
		// chat has /think off, drop OutThinking events here
		// (after Translate + ReplyTo stamping, before ch.Send)
		// so the Feishu adapter never sees them. Other
		// OutboundKinds — OutReply / OutResult / OutToolStart /
		// OutToolEnd / OutInit / OutUsage — are unaffected.
		// (F-49: OutCompaction deleted — the runtime consumes
		// EventCompaction directly via s.RecordCompaction() and
		// produces no OutboundMessage, so the Channel never sees
		// this kind at all.)
		//
		// cs is captured in the closure (per-cs handler
		// factory), so this lookup is a direct field read —
		// no mgr.Get round-trip, no map probe, no second RLock.
		if out.Kind == gateway.OutThinking && cs != nil && cs.ThinkMode() == chatsession.ThinkModeHide {
			if logger != nil {
				// Info level (not Debug): operators running
				// with default log level must see drops, or
				// /think off silently swallows events. Matches
				// the F-watch drop convention at
				// gateway.go:362 (log.Printf).
				logger.Info("think dropped",
					"chat_id", chatID,
					"user_msg_id", userMsgID,
					"agent_session_id", s.ID)
			}
			return
		}
		// F-38 §3.1.3: per-chat tool-event gate. When the chat
		// has /tools off (default), drop OutToolStart and
		// OutToolEnd events here (after Translate + ReplyTo
		// stamping, before ch.Send) so the Feishu adapter never
		// sees them. Other OutboundKinds — OutReply / OutResult
		// / OutThinking / OutInit / OutUsage — are unaffected.
		// (F-49: OutCompaction deleted — see note above in the
		// /think-off block.) The merge rendering (PATCH on start
		// message_id when /tools on) is a Feishu adapter
		// concern; this gate just decides whether the event
		// reaches the Channel at all.
		//
		// cs is captured in the closure (per-cs handler
		// factory), so this lookup is a direct field read —
		// same pattern as the ThinkMode gate above.
		if (out.Kind == gateway.OutToolStart || out.Kind == gateway.OutToolEnd) &&
			cs != nil && cs.ToolsMode() == agent.ToolsModeHide {
			if logger != nil {
				logger.Info("tools dropped",
					"chat_id", chatID,
					"user_msg_id", userMsgID,
					"agent_session_id", s.ID,
					"kind", out.Kind.String())
			}
			return
		}
		if err := ch.Send(context.Background(), out); err != nil && logger != nil {
			logger.Warn("channel send failed",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"agent_session_id", s.ID,
				"err", err)
		}
	}
}

// sessionContextInto populates the F-45/F-48 SessionContext
// snapshot on the OutboundMessage when there's at least one
// meaningful field to render. Reused by the 4 main-chat kind
// stamp site AND the OutMessageState+MessageForwarded site (the
// Feishu placeholder card needs the same snapshot so its footer
// line 3 — git tracking — renders from the very first "⌨️
// Working..." emit).
//
// F-48: capture per-stamp git status for footer line 3. No
// caching — every stamp recomputes so the footer always reflects
// the latest worktree state (uncommitted edits / unpushed
// commits). Returns (nil, nil) for non-repo / git-failure; we
// treat that as "no git segment" in the footer render path.
//
// Git invocation has a 3s deadline (review fix): a hung git
// (stalled NFS, broken .git/index, ... ) would otherwise block
// the entire outbound-message pipeline — MessageForwarded
// placeholders AND every stamped reply/result wait for git to
// return. 3s is plenty for normal repos (10-50ms typical; up to
// ~1s on very large monorepos) and far below the user's
// "chat is not realtime" tolerance. On timeout, CollectStatus
// returns (nil, nil) and the footer omits the git segment
// silently — chat keeps moving.
//
// Stamp SessionContext when ANY token field or cost is non-zero
// OR when Model has been captured OR when git status is
// available. The CacheCreationInputTokens field must be included
// — formatSessionFooter renders it as part of the '↓ in' segment,
// so a turn that only primed a cache entry (Input=0, Output=0,
// CacheRead=0, Cost=0, but CacheCreation > 0) is renderable and
// must reach the Channel. Without it, a transient cache-rewrite
// turn with no Model yet gets silently dropped.
//
// AgentSession.Agent is immutable (direct field read, no lock);
// Model() and CumulativeUsage() take RLock internally.
func sessionContextInto(out *gateway.OutboundMessage, s *chatsession.AgentSession) {
	snap := s.CumulativeUsage()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	gitSnap, _ := gtw.CollectStatus(ctx, s.Cwd, gtw.ExecGitRunner{})
	cancel()
	hasGit := gitSnap != nil && s.Cwd != ""
	// F-49: also stamp CompactionCount so the footer Line 1 "🗜 N"
	// segment can render. The compaction counter is captured under
	// the same cumulativeUsageMu that guards CumulativeUsage, so
	// the snapshot is internally consistent (count + token stats
	// refer to the same point in time). See
	// docs/feat/F-49-compaction-counter.md §1.5 / §1.8.
	if snap.InputTokens != 0 || snap.OutputTokens != 0 ||
		snap.CacheCreationInputTokens != 0 ||
		snap.CacheReadInputTokens != 0 || snap.CostUSD != 0 ||
		s.Model() != "" || hasGit || s.CompactionCount() > 0 {
		out.SessionContext = &gateway.SessionContext{
			Agent:           s.Agent,
			Model:           s.Model(),
			CumulativeUsage: snap,
			Workspace:       s.Cwd,
			GitStatus:       gitSnap,
			CompactionCount: s.CompactionCount(),
		}
	}
}

// responder adapts a channel.Channel for outbound messages.
// The readPump writes directly here.
type responder struct {
	ch     channel.Channel
	mgr    *chatsession.Manager
	logger *slog.Logger
}

// Send translates and dispatches an AgentEvent to the channel for
// the chat owning the active AgentSession.
func (r *responder) Send(ctx context.Context, chatID, userMsgID, text string) error {
	if r.ch == nil {
		return nil
	}
	return r.ch.Send(ctx, gateway.OutboundMessage{
		ChatID:  chatID,
		Kind:    gateway.OutReply,
		Text:    text,
		ReplyTo: userMsgID,
	})
}

// shutdownRun stops the channel, then either detaches or kills
// every ChatSession's AgentSessions depending on cleanup.
//
// Persistence: chat_sessions.json + agent_sessions.json are left
// in place. The Manager has been writing through to them
// throughout the run via WithPersistence.
func shutdownRun(out io.Writer, ch channel.Channel, mgr *chatsession.Manager, csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile, cleanup bool, logger *slog.Logger) error {
	_ = out // future shutdown status line
	if logger == nil {
		logger = slog.Default()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var firstErr error
	if ch != nil {
		if err := ch.Stop(shutdownCtx); err != nil {
			firstErr = fmt.Errorf("run: stop channel: %w", err)
		}
	}

	if mgr != nil {
		// Persist final state.
		for _, cs := range mgr.List() {
			// Touch lastInteractionAt so the entry is fresh on disk.
			cs.SetActiveAgent(cs.ActiveAgent()) // no-op write trigger via the locked path
		}

		if cleanup {
			for _, cs := range mgr.List() {
				if _, err := cs.KillAll(); err != nil && logger != nil {
					logger.Warn("kill all failed for chat", "chat", cs.ChatID, "err", err)
				}
			}
		}
		// (Detach is the default; AgentSessions that were Running
		// remain in registry as Detached for next start.)
	}

	// Best-effort: flush registry stores.
	if csFile != nil {
		// Upsert each ChatSession so the file reflects current state.
		for _, cs := range mgr.List() {
			_ = csFile.Upsert(cs.Entry())
		}
	}
	_ = asFile

	return firstErr
}