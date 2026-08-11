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
//     (non-terminal events → SetBusy; EventAgentDone / Error → SetIdle +
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
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/cwd"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/command/close"
	newcmd "github.com/cnlangzi/nightme/internal/command/newcmd"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/command/stop"
	"github.com/cnlangzi/nightme/internal/command/think"
	"github.com/cnlangzi/nightme/internal/command/tools"
	"github.com/cnlangzi/nightme/internal/command/use"
	"github.com/cnlangzi/nightme/internal/command/watch"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/prcache"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/shell"
	"github.com/cnlangzi/nightme/internal/statusbar"
)

// runDeps holds the construction seams for the daemon: every
// dependency is injectable for deterministic tests.
type runDeps struct {
	loadConfig        func() (*config.Config, error)
	openChatSessions  func(*config.Config) (*registry.ChatSessionFile, error)
	openAgentSessions func(*config.Config) (*registry.AgentSessionFile, error)
	buildAgents       func(*config.Config) *agent.Registry
	newChannel        func(*config.Config) (channel.Channel, error)
	signals           <-chan os.Signal
	skipFeishuLogin   bool
	onReady           func()

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
	var channelName string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the Feishu daemon (ChatSession-based runtime)",
		Long: "run starts the Feishu WebSocket channel and serves a Gateway " +
			"router on top of it. Slash commands (/cwd, /use, /kill, /help) " +
			"drive session lifecycle; plain text is forwarded to the live " +
			"agent behind the chat's active AgentSession.\n\n" +
			"On shutdown the daemon stops the channel and persists final " +
			"state. Agent processes are LONG-LIVED and intentionally NOT " +
			"killed by nightme — they survive nightme restart via the " +
			"Detached registry state, and `nightme run` (or /use) re-attaches " +
			"to them on next start. Use `/kill` from the relevant chat to " +
			"terminate agent processes.\n\n" +
			"Pass --channel=echo to run the daemon with the echo channel " +
			"(a no-network stub that prints outbound messages to stdout). " +
			"Useful for smoke tests.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd, channelName)
		},
	}
	cmd.Flags().StringVar(&channelName, "channel", "feishu",
		"Channel implementation: feishu (default) or echo (smoke test)")
	return cmd
}

// runRun dispatches to the daemon. Channel selection via
// the --channel flag (feishu | echo).
func runRun(cmd *cobra.Command, channelName string) error {
	if channelName != "" && channelName != "feishu" && channelName != "echo" {
		return fmt.Errorf("run: unknown channel %q (want feishu or echo)", channelName)
	}
	deps := withChannel(defaultRunDeps(), channelName)
	return runRunWith(cmd, deps)
}

// withChannel configures the runtime channel implementation
// (feishu | echo).
func withChannel(deps runDeps, channelName string) runDeps {
	switch channelName {
	case "feishu", "":
		// default — feishu.NewAdapter
	case "echo":
		deps.skipFeishuLogin = true
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
//
// Wiring order (top to bottom — read in order, each step
// depends on the previous ones):
//
//  1. Load config (cfg) and registry stores (csFile, asFile)
//  2. Build agent registry (agents) + IM channel (ch); ch.Start
//  3. Build chatsession.Manager (mgr) with spawner + persistence
//  4. Build shared outbound infra:
//       - prcache.Registry (per-AS PR cache)
//       - gtw.HandlerDeps (git runner, HTTP prober)
//       - outbound.Emitter (the single outbound chokepoint;
//         holds ch and the statusbar.Source that reads prCacheReg +
//         gtwDeps + mgr)
//  5. Build gtw.Manager, ReactionRouter, command.Commander,
//     shell.Dispatcher (the command-adapter layer)
//  6. Build gateway.Router (messageDispatcher + em); wire
//     gwImpl.WithCommander / WithShellDispatch / WithActionHandler
//  7. mgr.WithEmitter(em) + wireRuntimeCallbacksAndRestore (must
//     precede gwImpl.Start; the latter depends on chat sessions
//     having their per-bus subscribers installed)
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
	if !deps.skipFeishuLogin && (cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "") {
		return errors.New("run: Feishu credentials are not configured; run `nightme login feishu`")
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
	// messageDispatcher is constructed later (after `em` is
	// built) so its error-reply paths — queue full / no
	// workspace / spawn failed — go through the same Emitter as
	// the runtime pump and pick up the StatusBar footer. The
	// old channel_wrap prepended its own Emitter wrap, so the
	// bug class "error replies miss the footer" is now caught at
	// compile time: the dispatcher is typed as outbound.Emitter
	// from the start.

	// F-31 + F-think + F-38: install gw.OnMessageState AND the
	// runtime's EventHandler into every ChatSession via the
	// Manager.onCreate hook. Both callbacks MUST be installed
	// together — separating them is a silent-failure landmine
	// because AgentEventBus / MessageStateBus fire only when
	// they have subscribers. The actual wiring + ordering
	// (WithOnCreate BEFORE RestoreFromRegistry) is enforced
	// inside wireRuntimeCallbacksAndRestore — see its doc.
	//
	// Per-AgentSession PR / MR cache. Built before Emitter
	// construction (the statusbar.Source reads it) and before
	// gateway.New (the Emitter flows into Gateway's outbound
	// chokepoint). See prcache.Registry comment for why this
	// is owned at runtime scope, not on AgentSession itself.
	prCacheReg := &prcache.Registry{}

	gtwDeps := gtw.HandlerDeps{
		Git:           gtw.ExecGitRunner{},
		Prober:        &gtw.ExecHTTPProber{},
		PRInvalidator: prCacheReg,
	}

	// StatusBar deps — shared by every stamp site (runtime
	// pump, MessageStateBus, Emitter's Source). The closures
	// capture gtwDeps + prCacheReg; statusbar itself stays
	// decoupled from gtw (which would create an import cycle:
	// statusbar → gtw → chatsession → outbound → statusbar).
	statusbarDeps := statusbar.Deps{
		CollectGit: func(ctx context.Context, cwd string) (*messages.GitStatusSnapshot, error) {
			return gtw.CollectReadiness(ctx, cwd, gtw.ExecGitRunner{})
		},
		RefreshPR: func(asID, cwd string) {
			if prCache := prCacheReg.GetOrCreate(asID); prCache != nil {
				prCache.MaybeRefresh(cwd, gtwDeps)
			}
		},
		LookupPR: func(asID string) *messages.PR {
			if prCache := prCacheReg.GetOrCreate(asID); prCache != nil {
				return prCache.PR()
			}
			return nil
		},
	}

	// emitter is the single daemon-wide outbound chokepoint.
	// Constructed here (before gateway.New) so the Gateway can
	// hold the reference at construction time — every
	// downstream send (runtime pump / slash command /
	// MessageState / PATCH) flows through the same Emitter
	// instance. Manager.WithEmitter (further down) binds the
	// same Emitter to every ChatSession.
	em := outbound.New(ch, outbound.Options{Source: statusbar.NewRuntimeSource(
		func(chatID string) statusbar.ChatInfo {
			cs := mgr.Get(chatID)
			if cs == nil {
				return statusbar.ChatInfo{}
			}
			return statusbar.ChatInfo{
				Cwd: cs.SelectedCwd(),
				AS:  cs.SelectedAgentSession(),
			}
		},
		statusbarDeps,
	)})

	// Bind the same Emitter to chatsession.Manager so its
	// HandleInbound error-reply paths inherit the StatusBar
	// footer (no-workspace / spawn-failed / queue-full).
	mgr.WithEmitter(em).WithPrimaryAgent(cfg.Primary)

	// F-58: gateway is now a thin pump + binding table. The
	// dispatch chain lives in *inbound.Router (constructed
	// below, after commander / shellDispatcher / router are
	// built). The four direct dependencies here match the
	// priority chain in inbound.Dispatch:
	//   1. command.Commander  (/-prefixed text)
	//   2. shell.Dispatcher   (!-prefixed text)
	//   3. chatsession.Manager (default — agent loop)
	//   4. services.ReactionRouter (msg.Reaction / msg.Action)
	// Note: the ReactionRouter is consulted FIRST in the
	// chain (action events carry empty Text and must not fall
	// through to a slash command). Order of the four
	// constructor arguments does NOT match the priority
	// order; see internal/gateway/inbound/inbound.go for the
	// actual chain slice.
	var gwImpl *gateway.Router
	_ = em // em is consumed by inbound.Router's runtime wire-up below

	// All chat-session commands (/cwd /use /kill /new /watch /think
	// /tools) and /gtw are SlashCommandFactory implementations
	// implementations registered with reg.Register below. The legacy
	// gateway.RegisterChatSessionCommands helper is deleted;
	// gateway only sees the slash-command path via WithCommander
	// (the shim below) and the reaction path via WithActionHandler.

	// F-XX: gtw directly uses *chatsession.ChatSession (the
	// Sender interface is gone). Wiring is now:
	//   (a) gtw.Manager owns the state,
	//   (b) services.ReactionRouter dispatches reactions,
	//   (c) command.Commander routes slash commands,
	//   (d) this runtime shim translates *messages.InboundMessage
	//       ↔ command.SlashInput / *CommandResult,
	//   (e) SetGetChatSession hands gtw a per-chat lookup so
	//       RunFix / HandleDraftReaction can call
	//       cs.SelectedCwd / SetSelectedCwd / QueueUserMessage
	//       directly.

	gtwMgr := gtw.NewManager()
	gtwMgr.SetHandlerDeps(gtwDeps)

	// chatID → ChatSession lookup. The runtime owns this closure
	// so gtw's reaction / fix handlers can call into the
	// chatsession API without re-implementing GetOrCreate.
	// (Replaces the per-channel-resolver pattern that was used
	// when chatsession carried a per-chat Channel. Now that
	// chatsession holds a shared Emitter, only a shared CS
	// lookup is needed here.)
	mgr.WithEmitter(em)

	// F-XX: wire the per-chat ChatSession lookup. Without this,
	// /gtw fix and reaction paths would nil-deref on
	// GetChatSession. The lookup lazily creates a ChatSession
	// via mgr.GetOrCreate on first /gtw call (or reaction) per
	// chat, then caches it.
	gtwMgr.SetGetChatSession(func(chatID string) *chatsession.ChatSession {
		cs, _ := mgr.GetOrCreate(chatID, cfg.Primary)
		return cs
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
	reg.Register(watch.NewFactory(mgr))
	reg.Register(think.NewFactory(mgr))
	reg.Register(tools.NewFactory(mgr))
	reg.Register(cwd.NewFactory(mgr))
	reg.Register(use.NewFactory(mgr))
	reg.Register(close.NewFactory(mgr))
	reg.Register(stop.NewFactory(mgr))
	reg.Register(newcmd.NewFactory(mgr))
	commander := command.NewCommander(reg)

	// shellDispatcher owns the full shell-dispatch flow:
	// prefix detection, placeholder reply, async exec, result
	// posting. The shim in cmd/nightme/run.go only does type
	// adaptation — see shell.Dispatcher.Handle for the actual
	// logic.
	shellDispatcher := shell.NewDispatcher(shell.NewChatSessionSender(mgr))

	// RuntimeServices is built inside *inbound.Router —
	// the v0.x runtime shim that wrapped commander.Dispatch
	// is gone (F-58). The inbound package builds the
	// RuntimeServices closure internally (Logger, Config)
	// when calling commander.Dispatch.


	gwImpl.AttachChannels(ch)

	// F-watch §3.1.1: the per-chat WatchMode gate used to be wired
	// here via gwImpl.WithWatchModeResolver. It moved into
	// chatsession.Manager.AcceptInbound (called from
	// newMessageDispatcher below) so the policy sits next to its
	// state — no more callback injection across the import
	// boundary. See internal/chatsession/manager.go AcceptInbound.

	// F-51: the action handler now routes through
	// services.ReactionRouter instead of calling
	// chatsession.ChatSession.HandleAction (chatsession no
	// longer dispatches reactions). The shim translates
	// F-58: build the dispatch chain. The *inbound.Router owns
	// the four direct dependencies (chatsession, command, shell,
	// action) and walks them in priority order. Replaces the v0.x
	// shim closures (WithCommander / WithShellDispatch /
	// WithActionHandler) that used to live here.
	ir := inbound.New(mgr, commander, shellDispatcher, router, cfg.Primary)
	gwImpl = gateway.New(ir, em)

	// WithOnCreate fires for both restored (RestoreFromRegistry)
	// and future (GetOrCreate) ChatSessions. Place BEFORE
	// RestoreFromRegistry so restored chats get their handlers.
if err := wireRuntimeCallbacksAndRestore(mgr, em, logger, statusbarDeps, markPromptDone(ch)); err != nil {
		return fmt.Errorf("run: wire+restore: %w", err)
	}

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
	return shutdownRun(out, ch, mgr, csFile, asFile, prCacheReg, logger)
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
//  2. cs.LookupSelectedAgentSession() (lazy spawn)
//  3. cs.QueueUserMessage(blocks, userMsgID) (Idle → flush now;
//     Busy → queue)
//  4. SetBusy on first event (drive FSM)
func newMessageDispatcher(mgr *chatsession.Manager, em outbound.Emitter, primary string, logger *slog.Logger) func(context.Context, *messages.InboundMessage) error {
	return func(ctx context.Context, msg *messages.InboundMessage) error {
		if msg == nil {
			return nil
		}
		// F-watch §3.1.1: per-chat WatchMode gate (formerly in
		// gateway.applyWatchModeGate). Lives here now so the
		// policy sits next to its state — chatsession owns both
		// the WatchMode field and the AcceptInbound decision.
		// Drop early, before any GetOrCreate / spawn work, so
		// filtered messages don't allocate state or wake pumps.
		// Slash commands never reach this branch (the commander
		// shim returns first inside DispatchInbound).
		if !mgr.AcceptInbound(msg.ChatID, msg.HasMention) {
			slog.Default().Info("dispatcher: drop non-mention group message (WatchMode != All)",
				"chat_id", msg.ChatID, "message_id", msg.MessageID)
			return nil
		}
		userMsgID := msg.MessageID
		if userMsgID == "" {
			userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}

		cs, _ := mgr.GetOrCreate(msg.ChatID, primary)

		// F-31 / F-53: ChatSession has accepted the message. Emit
		// MessageQueued synchronously so the channel can render
		// ⏳ even before spawn resolves (FastAck UX).
		cs.EmitMessageState(userMsgID, agent.MessageQueued)

		// Resolve active AgentSession (lazy spawn on miss).
		_, err := cs.LookupSelectedAgentSession()
		if err != nil {
			if errors.Is(err, chatsession.ErrNoSelectedCwd) {
				return em.Send(ctx, messages.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   messages.OutReply,
					Text:   "No workspace set. Send /cwd <path> first.",
				})
			}
			// Spawn failed (binary missing, etc.); let the user know.
			return em.Send(ctx, messages.OutboundMessage{
				ChatID: msg.ChatID,
				Kind:   messages.OutReply,
				Text:   fmt.Sprintf("Failed to spawn agent: %v", err),
			})
		}

		// CS-AS 边界重构 Phase 1: readpump is now per-AS (started
		// by Spawn inside AgentSession). The chat layer just consumes
		// the enriched event stream via cs.PumpEvents (launched in
		// wireRuntimeCallbacksAndRestore). No StartReadPump call here
		// — the old per-CS readpump file is gone.

		// F-31 / F-53: dispatch successful — message has reached the
		// AgentSession. Emit MessageSubmitted so the channel flips
		// ⏳ → 🔄. (Emitted before QueueUserMessage so the visual
		// transition is visible even if queueing is slow.)
		cs.EmitMessageState(userMsgID, agent.MessageSubmitted)

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
		// F-53: build the per-message domain object. The
		// `ReceivedAt` is set to the inbound timestamp so log /
		// debug surfaces see the true arrival time (not the
		// dispatcher-pass time, which may be a hair later when
		// the spawn path took a moment).
		userMsg := chatsession.Message{
			ID:         userMsgID,
			ChatID:     msg.ChatID,
			Blocks:     blocks,
			ReceivedAt: msg.Time,
		}
		if err := cs.QueueUserMessage(userMsg); err != nil {
			if errors.Is(err, chatsession.ErrQueueFull) {
				return em.Send(ctx, messages.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   messages.OutReply,
					Text:   "Input queue full — the agent is behind. Wait for it to catch up before sending more.",
				})
			}
			return err
		}
		return nil
	}
}

// wireRuntimeCallbacksAndRestore installs the per-ChatSession
// outbound handlers (EventHandler for AgentEvent → OutboundMessage
// translation; MessageStateBus subscriber for F-31 lifecycle reactions)
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
// markPromptDone returns the Feishu-specific PromptEnd callback
// wired into wireRuntimeCallbacksAndRestore. For non-Feishu
// channels the no-op default is used; Feishu channels transition
// the receipt card to PromptDone and add the ✅ reaction. The
// wrapper exists so the runtime layer doesn't have to type-assert
// the Channel interface back to *feishu.Adapter inside the
// per-ChatSession install closure.
func markPromptDone(ch channel.Channel) func(ctx context.Context, chatID, msgID string) {
	if fa, ok := ch.(*feishu.Adapter); ok {
		return fa.MarkReceiptPromptDone
	}
	return func(context.Context, string, string) {}
}

func wireRuntimeCallbacksAndRestore(
	mgr *chatsession.Manager,
	em outbound.Emitter,
	logger *slog.Logger,
	sbDeps statusbar.Deps,
	// markPromptDone is called when ChatSession.endPrompt fires
	// (EventAgentDone / EventAgentError in the readpump). The
	// runtime injects the Feishu-specific implementation; for
	// non-Feishu channels the callback is a no-op. Passing it
	// in (rather than type-asserting ch to *feishu.Adapter
	// here) keeps wireRuntimeCallbacksAndRestore channel-agnostic.
	markPromptDone func(ctx context.Context, chatID, msgID string),
) error {
	mgr.WithOnCreate(func(cs *chatsession.ChatSession) {
		// Startup audit trail: one line per chat, bounded by the
		// number of persisted chats — confirms the outbound
		// wiring (AgentEventBus / MessageStateBus / PromptEndBus /
		// PumpEvents) is actually installed for every
		// restored-or-new ChatSession. See the bug history note on
		// wireRuntimeCallbacksAndRestore above: a missing/misordered
		// handler here is a silent failure (no logs, no
		// channel.Send, no reactions), so this line is the cheapest
		// signal that wiring succeeded.
		//
		// Debug-level (not Info) so a daemon with hundreds of
		// persisted chats doesn't flood the log at startup; use
		// `Logging.Level: debug` to surface the audit trail when
		// investigating handler-installation regressions.
		if logger != nil {
			logger.Debug("runtime: handlers installed for chat",
				"chat_id", cs.ChatID,
				"cs_id", cs.ID)
		}

		// F-54: subscribe to the per-ChatSession event buses.
		// Each handler is a typed lambda on the corresponding
		// envelope; the Bus fires them in registration order with
		// panic isolation. Multiple subscribers may coexist; we
		// register exactly one per bus here. New subscribers
		// (audit, metrics, HUD) can register alongside.

		// AgentEventBus — translates bridge AgentEvent to
		// OutboundMessage and dispatches via Channel.Send. The
		// per-cs handler closure is built ONCE here (not inside
		// the Subscribe callback) — newEventHandler itself
		// allocates a closure, so calling it on every Publish
		// would burn one allocation per event.
		agentHandler := newEventHandler(em, cs, mgr, logger, sbDeps)
		cs.AgentEventBus.Subscribe(func(env chatsession.AgentEventEnvelope) bool {
			agentHandler(env)
			return false
		})

		// F-48: wrap the gateway's OnMessageState so the runtime
		// can stamp StatusBar on MessageSubmitted (the
		// Feishu placeholder card needs the footer). The gateway's
		// bare OnMessageState doesn't accept StatusBar — the
		// runtime is the right owner of the stamp because it has
		// access to the AgentSession via cs.SelectedAgentSession().
		// We bypass gwImpl.OnMessageState entirely here: the
		// runtime wrapper does the same thing (validate, build
		// OutboundMessage, send) plus the stamp on MessageSubmitted.
		// Other states (MessageQueued / MessageDropped) don't need
		// the stamp — they don't create UI.
		//
		// F-54 review note: this handler routes via `em` (the single
		// Emitter passed into wireRuntimeCallbacksAndRestore). Today
		// there is one Channel in production, so the difference is
		// observable only in tests that wire multiple channels via
		// gateway.Bind/chatToChan. For multi-channel deployments,
		// route via a per-chatID Channel lookup in front of `em`
		// (the wrap currently does the type conversion; a multi-channel
		// variant would resolve the underlying channel.Channel first
		// and wrap-emit it). The AgentEventBus and PromptEndBus
		// handlers above have the same latent gap; the same one-line
		// fix applies to both.
		cs.MessageStateBus.Subscribe(func(e chatsession.MessageStateEvent) bool {
			// Review fix: replicate the gateway's identifier
			// validation. Empty chatID / userMsgID would produce
			// an OutboundMessage with empty routing fields, which
			// the Feishu adapter rejects ("feishu: OutMessageState
			// missing MessageID") and which previously was a
			// silent no-op via gwImpl.OnMessageState's early return.
			if e.ChatID == "" || e.UserMsgID == "" {
				return false
			}
			out := messages.OutboundMessage{
				Kind:    messages.OutMessageState,
				ChatID:  e.ChatID,
				ReplyTo: e.UserMsgID,
				MessageState: &messages.MessageStatePayload{
					State:     e.State,
					MessageID: e.UserMsgID,
				},
			}
			if e.State == agent.MessageSubmitted {
				// multi-as Phase 1: source AS comes from the
				// event itself, not from cs.SelectedAgentSession().
				// We pre-stamp here so the emitter's stamper (which
				// uses cs.SelectedAgentSession()) sees a non-nil
				// StatusBar and skips its lookup — preserving
				// the "source AS, not selected AS" semantics this
				// bus has always had.
				if as := cs.LookupAS(e.AgentSessionID); as != nil {
					statusbar.StampFromAS(&out, as, sbDeps)
				}
			}
			if err := em.Send(context.Background(), out); err != nil {
				logger.Warn("runtime: MessageState send failed",
					"chat_id", e.ChatID,
					"state", e.State,
					"err", err)
			}
			return false
		})

		// F-53 follow-up: when ChatSession.endPrompt fires
		// (EventAgentDone / EventAgentError in the readpump), route the
		// terminal event to the Feishu adapter so the receipt
		// card transitions to PromptDone and the ✅ reaction
		// is added on the card. No user-message reaction is
		// emitted from this path — the user-message surface is
		// now minimal (⏳ only).
		cs.PromptEndBus.Subscribe(func(e agentsession.PromptEndedEvent) bool {
			if e.ChatID == "" || e.UserMsgID == "" {
				return false
			}
			// The adapter call is fire-and-forget: failures are
			// logged inside SetPromptState. We use
			// context.Background() because the readpump-driven
			// endPrompt happens off the inbound message path;
			// there's no inbound ctx to chain. The runtime
			// injects the Feishu-specific implementation; for
			// non-Feishu channels the callback is a no-op.
			markPromptDone(context.Background(), e.ChatID, e.UserMsgID)
			return false
		})

		// CS-AS 边界重构 Phase 1: launch the per-chat pumpEvents
		// goroutine. The pump drains cs.ActiveEvents() and dispatches
		// each EnrichedEvent by Kind:
		//   - KindAgentEvent   → cs.AgentEventBus (subscriber above)
		//   - KindPromptEnded  → writebackMessageState (built into
		//                        cs.PumpEvents — publishes on PromptEndBus)
		//   - KindLifecycle    → log + flip AgentSession.SetExited (in pump_events)
		// Replaces the per-CS StartReadPump / readRunPump that the
		// pre-Phase-1 readpump.go file used to install. The new model
		// has readpump per-AS (started by Spawn), so the chat layer
		// only consumes the enriched event stream.
		//
		// The goroutine ends when cs.ActiveEvents() returns !ok,
		// which happens when the active AS is Shutdown (for /use this
		// happens at daemon exit; for /kill, immediately).
		go cs.PumpEvents(context.Background())
	})
	return mgr.RestoreFromRegistry()
}

// sessions — readPumps are started lazily on first /use + send.
//
// The runtime's actual readPump start happens in handleUse
// (gateway package) and on first message dispatch in
// newEventHandler returns the per-event callback installed on
// every ChatSession by the runtime. The callback translates
// AgentEvent → OutboundMessage and dispatches via the channel.
//
// chatID is passed by ChatSession's readPump directly (the
// ChatSession knows its own ChatID); the handler doesn't need
// to look it up.
//
// userMsgID is the current turn's single anchor (passed by
// readPump from AgentSession.currentPrompt.LastMessageID; F-53
// moved it there from the deleted cs.currentTurnUserMsgID
// scalar). The handler stamps it onto OutboundMessage.ReplyTo
// so each Channel can route the event to its own per-userMsgID
// receipt (card / thread / DOM node). Empty when the event has
// no anchor (startup EventAgentReady, post-/use while no Prompt is
// active, etc.) — Channel falls back to plain text in that case.
//
// v1.3 (SPEC §2.2): 1 turn : 1 anchor. Receipt rendering and
// FSM are Channel-internal; Gateway only knows about userMsgID.
//
// Per-cs construction (not per-Mgr): the F-think gate reads
// cs.ThinkMode() on every OutThinking event, and the readPump
// fires only for ChatSessions that already exist, so the
// ChatSession is statically known at install time. Capturing it
// in the closure eliminates the per-event mgr.Get round-trip
// (RLock + map lookup). mgr is still passed because EventAgentReady
// persistence needs mgr.PersistAgentSession, which is the cold
// path (once per AgentSession lifetime, not per event).
//
// F-54: the handler takes a typed chatsession.AgentEventEnvelope
// (delivered by cs.AgentEventBus). The legacy
// `chatsession.EventHandler` callback signature is gone — see
// docs/feat/F-54-event-bus.md §3.5.
func newEventHandler(
	em outbound.Emitter,
	cs *chatsession.ChatSession,
	mgr *chatsession.Manager,
	logger *slog.Logger,
	sbDeps statusbar.Deps,
) func(env chatsession.AgentEventEnvelope) {
	// Per-cs closure. No per-handler mutable state needed anymore:
	// the bridge layer now attaches per-turn Usage to the SAME
	// AgentResultEvent that delivers the final text (claudecode
	// result.usage + result.modelUsage; Pi message_end.usage), so
	// the runtime does not buffer OutResult waiting for a follow-up
	// usage event. F-52 / AgentEvent-flattening unified the data on
	// a single wire event; this collapsing was the bridge-layer
	// artefact it dissolved.
	return func(env chatsession.AgentEventEnvelope) {
		chatID, s, ev, userMsgID := env.ChatID, env.AgentSession, env.Event, env.UserMsgID
		// Capture the agent's own session id from EventAgentReady so the
		// next respawn can replay `--resume <id>`. We persist
		// immediately (rather than waiting for the next status
		// transition) so a daemon crash after this event still
		// remembers the id. The capture is idempotent.
		//
		// Guard: only overwrite an existing (non-empty) SessionID when
		// the new id is non-empty. Some bridges re-emit EventAgentReady
		// after a child restart with a blank SessionID; we don't
		// want to wipe a previously-captured id in that case.
		if ev.Kind == agent.EventAgentReady && ev.SessionID != "" {
			s.SetSessionID(ev.SessionID)
			if mgr != nil {
				if err := mgr.PersistAgentSession(s); err != nil && logger != nil {
					logger.Warn("persist agent session (init) failed",
						"chat_id", chatID,
						"agent_session_id", s.ID,
						"err", err)
				}
			}
		}
		// F-45 §1.4: capture model on first EventAgentReady. Independent
		// of the SessionID capture above (some bridges may emit
		// one without the other). SetModel is idempotent — empty
		// incoming values don't overwrite.
		if ev.Kind == agent.EventAgentReady && ev.Model != "" {
			s.SetModel(ev.Model)
		}
		// Per-event usage flows from bridge → out.Usage (via
		// gateway.Translate) → StatusBar.Usage (via
		// stampFromAS below) → channel footer. The runtime
		// is a passive pass-through; no accumulation, no dedup,
		// no priority. Usage rides on EventAgentResult (populated by
		// the bridges via AgentResultEvent.Usage) — AgentDoneEvent.Usage is
		// not currently surfaced because gateway.Translate drops
		// EventAgentDone events before the runtime stamps
		// StatusBar. Bridges that only emit EventAgentDone-with-
		// Usage (no EventAgentResult) will not see their Usage in the
		// footer; only pi-style bridges that have an explicit
		// EventAgentResult path will.
		//
		// PersistIfDirty is no longer driven from here (the
		// cumulative-dirty trigger is gone with cross-turn usage
		// aggregation). Future per-AgentSession dirty state can
		// hook in without changing call sites — see
		// AgentSession.PersistIfDirty for the new contract.
		// F-49 compaction tracking removed: bridges no longer
		// emit a dedicated compaction event; runtime is a pure
		// pass-through. The per-cycle counter / Footer 🗜 N
		// segment is dropped across the runtime. The pre-F-49
		// `case agent.EventAgentCompaction: ... return` block was
		// removed entirely; no per-event short-circuit remains.

		// Translate the AgentEvent to an OutboundMessage.
		out, ok := outbound.Translate(chatID, *ev)
		if !ok {
			if logger != nil {
				logger.Debug("runtime: Translate dropped event",
					"chat_id", chatID,
					"kind", ev.Kind.String(),
					"agent_session_id", s.ID)
			}
			// Translate drops events that don't surface to the
			// channel (EventAgentDone, EventAgentCompaction (F-49 deleted), thread-only
			// kinds, etc.). The runtime no longer folds usage
			// anywhere — Agent is a passive pass-through,
			// and the channel-side footer reads ctx.Usage directly
			// from OutboundMessage on the OK path below. Done.
			// Usage, if any, dies with the dropped event (channel
			// never sees it). See docs/feat/F-45-session-footer.md
			// §1.5 / §1.6 for the per-turn snapshot contract.
			return
		}
		// The runtime is a passive pass-through: out.Usage is
		// populated by gateway.Translate from the bridge event
		// (AgentResultEvent.Usage / AgentDoneEvent.Usage) and rendered
		// verbatim by the channel footer. No accumulation, no
		// dedup, no priority — bridges are free to populate
		// either field and the channel reads out.Usage directly.
		//
		// Stamp the current turn's anchor so the Channel can
		// route to the right per-userMsgID receipt. ReplyTo
		// stays empty for orphan events (EventAgentReady at startup,
		// internal logs) — Channel renders those as plain text.
		out.ReplyTo = userMsgID
		// F-45 §2.5 改动 C: stamp StatusBar snapshot on the
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
case messages.OutReply, messages.OutResult,
			messages.OutTaskCreate, messages.OutTaskUpdate:
			statusbar.StampFromAS(&out, s, sbDeps)
		}
		// F-think §3.1.2: per-chat OutThinking gate. When the
		// chat has /think off, drop OutThinking events here
		// (after Translate + ReplyTo stamping, before em.Send)
		// so the Feishu adapter never sees them. Other
		// OutboundKinds — OutReply / OutResult / OutToolStart /
		// OutToolEnd / OutInit / OutUsage — are unaffected.
		// (F-49: OutCompaction deleted — the runtime consumed
		// EventAgentCompaction directly via AgentSession.RecordCompaction()
		// pre-F-49 and produced no OutboundMessage, so the Channel never saw
		// this kind at all.)
		//
		// cs is captured in the closure (per-cs handler
		// factory), so this lookup is a direct field read —
		// no mgr.Get round-trip, no map probe, no second RLock.
		if out.Kind == messages.OutThinking && cs != nil && cs.ThinkMode() == chatsession.ThinkModeHide {
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
		// stamping, before em.Send) so the Feishu adapter never
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
		if (out.Kind == messages.OutToolStart || out.Kind == messages.OutToolEnd) &&
			cs != nil && cs.ToolsMode() == chatsession.ToolsModeHide {
			if logger != nil {
				logger.Info("tools dropped",
					"chat_id", chatID,
					"user_msg_id", userMsgID,
					"agent_session_id", s.ID,
					"kind", out.Kind.String())
			}
			return
		}
		if err := em.Send(context.Background(), out); err != nil && logger != nil {
			logger.Warn("channel send failed",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"agent_session_id", s.ID,
				"err", err)
		}
		if logger != nil {
			logger.Debug("runtime: ch.Send dispatched",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"agent_session_id", s.ID,
				"kind", out.Kind.String(),
				"text_len", len(out.Text))
		}
	}
}

// shutdownRun stops the channel and persists final state.
//
// Agent processes are INTENTIONALLY NOT killed here — they are
//
// Agent processes are INTENTIONALLY NOT killed here — they are
// long-running CLI sessions independent of nightme's lifetime.
// AgentSessions that were Running remain in the registry as
// Detached; the next `nightme run` re-attach path (Manager.RestoreFromRegistry
// + FromAgentSessionEntry) hands them back to nightme, and
// LookupActiveAgentSession reuses them via --resume where the
// bridge supports it. /kill is the only path that terminates
// agent processes; it is cwd-scoped and runs in chatsession.KillAgent /
// chatsession.KillAllAgents (see internal/chatsession/kill.go).
//
// Persistence: chat_sessions.json + agent_sessions.json are left
// in place. The Manager has been writing through to them
// throughout the run via WithPersistence.
func shutdownRun(out io.Writer, ch channel.Channel, mgr *chatsession.Manager, csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile, prReg *prcache.Registry, logger *slog.Logger) error {
	_ = out // future shutdown status line
	_ = asFile
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

	// Cancel every per-AgentSession PR-cache refresh goroutine
	// so the daemon doesn't exit mid-`gh pr list`. Best-effort:
	// the goroutines are stateless (HTTP/git calls), so a missed
	// cancel only wastes a few round-trips, not state corruption.
	// We do this BEFORE persisting chat state so any in-flight
	// refresh that was about to land back into a Cache sees the
	// cancel signal at its next checkpoint and exits silently.
	if prReg != nil {
		prReg.CloseAll()
	}

	if mgr != nil {
		// Persist final state.
		for _, cs := range mgr.List() {
			// Touch lastInteractionAt so the entry is fresh on disk.
			cs.SetSelectedAgent(cs.SelectedAgent()) // no-op write trigger via the locked path
		}
	}

	// Best-effort: flush registry stores.
	if csFile != nil {
		// Upsert each ChatSession so the file reflects current state.
		for _, cs := range mgr.List() {
			_ = csFile.Upsert(cs.Entry())
		}
	}

	return firstErr
}
