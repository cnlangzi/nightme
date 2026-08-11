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
	"github.com/cnlangzi/nightme/internal/command/kill"
	newcmd "github.com/cnlangzi/nightme/internal/command/newcmd"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/command/stop"
	"github.com/cnlangzi/nightme/internal/command/think"
	"github.com/cnlangzi/nightme/internal/command/tools"
	"github.com/cnlangzi/nightme/internal/command/use"
	"github.com/cnlangzi/nightme/internal/command/watch"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/prcache"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/shell"
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
	messageDispatcher := newMessageDispatcher(mgr, ch, cfg.Primary, logger)

	// F-31 + F-think + F-38: install gw.OnMessageState AND the
	// runtime's EventHandler into every ChatSession via the
	// Manager.onCreate hook. Both callbacks MUST be installed in
	// this single closure — separating them (one here, one in
	// /use or newMessageDispatcher) is a silent-failure landmine
	// because readpump fires only when AgentEventBus has subscribers.
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
	// Per-AgentSession PR / MR cache. Built before Emitter
	// construction (the Stamper reads it) and before
	// gateway.New (the Emitter flows into Gateway's outbound
	// chokepoint). See prcache.Registry comment for why this
	// is owned at runtime scope, not on AgentSession itself.
	prCacheReg := &prcache.Registry{}

	gtwDeps := gtw.HandlerDeps{
		Git:           gtw.ExecGitRunner{},
		Prober:        &gtw.ExecHTTPProber{},
		PRInvalidator: prCacheReg,
	}

	// emitter is the single daemon-wide outbound chokepoint.
	// Constructed here (before gateway.New) so the Gateway can
	// hold the reference at construction time — every
	// downstream send (runtime pump / slash command /
	// MessageState / PATCH) flows through the same Emitter
	// instance. Manager.WithEmitter (further down) binds the
	// same Emitter to every ChatSession.
	em := outbound.New(ch, outbound.Options{Stamper: newRuntimeStamper(mgr, prCacheReg, gtwDeps)})

	gwImpl := gateway.New(messageDispatcher, em).(*gateway.Router)
	// All chat-session commands (/cwd /use /kill /new /watch /think
	// /tools) and /gtw are SlashCommandFactory implementations
	// implementations registered with reg.Register below. The legacy
	// gateway.RegisterChatSessionCommands helper is deleted; the
	// gateway only sees the slash-command path via WithCommander
	// (the shim below) and the reaction path via WithActionHandler.

	// F-XX: gtw directly uses *chatsession.ChatSession (the
	// Sender interface is gone). Wiring is now:
	//   (a) gtw.Manager owns the state,
	//   (b) services.ReactionRouter dispatches reactions,
	//   (c) command.Commander routes slash commands,
	//   (d) this runtime shim translates *gateway.InboundMessage
	//       ↔ command.SlashInput / *CommandResult,
	//   (e) SetGetChatSession hands gtw a per-chat lookup so
	//       RunFix / HandleDraftReaction can call
	//       cs.SelectedCwd / SetSelectedCwd / QueueUserMessage
	//       directly.
	// Per-AgentSession PR / MR cache. Owned at runtime scope
	// (NOT on AgentSession itself — the import cycle: gtw →
	// (prCacheReg, gtwDeps, em are all constructed earlier, before
	// gateway.New, so the Gateway can hold the Emitter reference
	// at construction time. The lines below continue the gtwMgr
	// setup that depends on those.)

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
	reg.Register(kill.NewFactory(mgr))
	reg.Register(stop.NewFactory(mgr))
	reg.Register(newcmd.NewFactory(mgr))
	commander := command.NewCommander(reg)

	// shellDispatcher owns the full shell-dispatch flow:
	// prefix detection, placeholder reply, async exec, result
	// posting. The shim in cmd/nightme/run.go only does type
	// adaptation — see shell.Dispatcher.Handle for the actual
	// logic.
	shellDispatcher := shell.NewDispatcher(chatSessionChannelSender{mgr: mgr})

	// RuntimeServices carries the shared command runtime interfaces.
	// Each command package holds *chatsession.Manager directly
	// via its Factory. Channel wraps *channel.Channel (any
	// command that needs to send replies uses this).
	rt := command.RuntimeServices{
		Config: command.Config{Primary: cfg.Primary},
		Logger: logger,
	}

	// Shim: translate *gateway.InboundMessage → command.SlashInput,
	// dispatch, translate back. The gateway.CommandResult shape only
	// carries Reply/Consumed/Dropped, so out.Outbound (multi-message
	// atomic batches such as card + reply) is intentionally dropped
	// here for now. When a future command needs the multi-message
	// path, extend gateway.CommandResult with an Outbound []OutboundMessage
	// slice and translate from command.Outbound here.
	//
	// 2026-08-06: Commander.Dispatch now returns (output, handled,
	// err). When handled=false (plain text, no "/" prefix) the shim
	// returns (nil, nil) so the gateway falls through to its legacy
	// dispatcher + agent loop. When handled=true but output.Consumed=
	// false (slash command attempt that did not match any factory),
	// the shim returns a Consumed=false result — the gateway
	// recognises that as "fall through" and forwards the original
	// text to the agent loop, preserving the pre-F-51 passthrough
	// characteristic for inputs like "/etc/passwd" or "/@everyone".
	gwImpl.WithCommander(func(ctx context.Context, msg *gateway.InboundMessage) (*gateway.CommandResult, error) {
		if msg == nil {
			return nil, nil
		}

		// GetOrCreate 这里调用一次,emitter 已经注入(见
		// 上方 WithEmitter 调用),所以 cs.Emitter() 应当非 nil。
		// 如果 nil(resolver 失败),log warn 并返回 nil 让 gateway
		// 走 fallback 到 agent loop。
		cs, err := mgr.GetOrCreate(msg.ChatID, cfg.Primary)
		if err != nil {
			slog.Default().Warn("runtime shim: GetOrCreate failed",
				"chat_id", msg.ChatID, "err", err)
			return nil, nil
		}

		input := command.SlashInput{
			ChatID:     msg.ChatID,
			UserID:     msg.UserID,
			Text:       msg.Text,
			MessageID:  msg.MessageID,
			HasMention: msg.HasMention,
		}
		out, handled, err := commander.Dispatch(ctx, rt, cs, input)
		if err != nil {
			errText := "❌ " + err.Error()
			ch := cs.Emitter()
			if ch != nil {
				_ = ch.Send(ctx, gateway.OutboundMessage{
					ChatID:  msg.ChatID,
					Text:    errText,
					ReplyTo: msg.MessageID,
				})
			}
			return &gateway.CommandResult{Consumed: true, Reply: errText}, nil
		}
		if !handled {
			return nil, nil
		}
		// handled=true. out 可能 Consumed=true(command replied)
		// 或 Consumed=false(slash 命令没匹配到 factory)。
		ch := cs.Emitter()
		if out.Consumed && out.Reply != "" && ch != nil {
			_ = ch.Send(ctx, gateway.OutboundMessage{
				ChatID:  msg.ChatID,
				Text:    out.Reply,
				ReplyTo: msg.MessageID,
			})
		}
		// 透传 out.Outbound (都是 gateway.OutboundMessage,直接
		// 调 cs.Emitter().Send/SendCard — PATCH 语义由调用方设 Kind=OutCardPatch
		// 后通过 Send 走,见 channel/feishu/adapter.go: case OutCardPatch)
		for _, ob := range out.Outbound {
			if ch == nil {
				continue
			}
			if ob.Card != nil {
				_, _ = ch.SendCard(ctx, ob)
			} else {
				_ = ch.Send(ctx, ob)
			}
		}
		return &gateway.CommandResult{
			Consumed: out.Consumed,
			Dropped:  out.Dropped,
			Reply:    out.Reply,
		}, nil
	})

	// feat-shell: install the `!cmd` shell dispatcher. Symmetric
	// to the commander shim above; runs in priority 3 of the
	// gateway dispatch chain (between tryCommandDispatch and
	// tryMessageDispatch). The shell package owns its own prefix
	// detection (parseShell) — this shim is transport-only.
	//
	// We resolve the chat session once via mgr.GetOrCreate (same
	// as the commander shim) so shell.Dispatch gets the chat's
	// SelectedCwd as its working directory. If GetOrCreate fails
	// (e.g. agent registry not loaded), fall through to message
	// dispatch — the user can still talk to the agent.
	//
	// Reply delivery: the gateway's dispatchLoop only checks
	// result != nil and otherwise discards CommandResult.Reply
	// (see gateway.go's dispatchLoop / Contract section). Each
	// shim is responsible for sending its own reply to the
	// channel. We mirror the commander shim's pattern.
	//
	// ASYNC execution: shell commands run in a detached goroutine,
	// not the dispatch goroutine. The dispatcher returns immediately
	// (Consumed=true, no Reply) so the gateway can continue
	// processing the next message and — crucially — so the daemon
	// can shut down cleanly without deadlocking on its own restart.
	//
	// Why async is REQUIRED (not optional):
	//
	//   The dispatchLoop goroutine is registered with the gateway's
	//   wg. When the daemon shuts down, gateway.Stop calls
	//   wg.Wait() — which blocks until dispatchLoop exits.
	//   dispatchLoop can only exit when it's at the top of its
	//   select. If dispatchLoop is currently blocked inside
	//   shell.Dispatch (synchronously waiting on `sh -c "make
	//   restart"`), it can't return to the select, can't exit, and
	//   wg.Wait() hangs forever.
	//
	//   Meanwhile `bin/nightme restart` (a child of `make`, itself
	//   a child of `sh`) is waiting for the daemon's lockfile to
	//   release so the new daemon can start. The daemon can't
	//   release the lockfile because it's stuck in shutdown waiting
	//   on wg.Wait(). Classic self-deadlock.
	//
	//   By returning Consumed=true synchronously and running the
	//   shell command in a separate goroutine, dispatchLoop is
	//   freed immediately. The daemon can shutdownRun → Stop →
	//   wg.Wait() → all goroutines accounted for → exit cleanly.
	//   The shell goroutine outlives the old daemon (becomes a
	//   short-lived orphan; init reaps it), `make restart` finishes,
	//   the new daemon starts, and the user sees the result on the
	//   new daemon's session via the persisted MessageID.
	//
	// Reply delivery happens INSIDE the goroutine, with a fresh
	// context.Background-derived ctx (the inbound ctx is cancelled
	// during shutdown, and the goroutine may outlive the shutdown).
	// Reply is best-effort: if the channel/CS is gone (daemon
	// restarted, cs unloaded), ch.Send fails silently. The user
	// still sees the result if the new daemon picked up the same
	// chat — the messageID is persisted in the inbound envelope.
	gwImpl.WithShellDispatch(func(ctx context.Context, msg *gateway.InboundMessage) (*gateway.CommandResult, error) {
		if msg == nil {
			return nil, nil
		}
		cs, err := mgr.GetOrCreate(msg.ChatID, cfg.Primary)
		if err != nil {
			slog.Default().Warn("runtime shim (shell): GetOrCreate failed",
				"chat_id", msg.ChatID, "err", err)
			return nil, nil
		}

		// The shell package owns ALL shell-related logic
		// (prefix detection, placeholder, async exec, reply
		// posting). This shim is pure type adaptation:
		// gateway.InboundMessage → shell.InboundRequest.
		// Handler returning Consumed=false means "not a shell
		// command" — the gateway falls through to tryMessageDispatch.
		result := shellDispatcher.Handle(shell.InboundRequest{
			Request: shell.Request{
				Text: msg.Text,
				Cwd:  cs.SelectedCwd(),
			},
			ChatID:    msg.ChatID,
			MessageID: msg.MessageID,
		})
		return &gateway.CommandResult{Consumed: result.Consumed}, nil
	})

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
	// *commandServices.ReactionEvent → services.ReactionEvent.
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
if err := wireRuntimeCallbacksAndRestore(mgr, ch, em, logger, prCacheReg, gtwDeps); err != nil {
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
func newMessageDispatcher(mgr *chatsession.Manager, em outbound.Emitter, primary string, logger *slog.Logger) func(context.Context, *gateway.InboundMessage) error {
	return func(ctx context.Context, msg *gateway.InboundMessage) error {
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
				return em.Send(ctx, gateway.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   gateway.OutReply,
					Text:   "No workspace set. Send /cwd <path> first.",
				})
			}
			// Spawn failed (binary missing, etc.); let the user know.
			return em.Send(ctx, gateway.OutboundMessage{
				ChatID: msg.ChatID,
				Kind:   gateway.OutReply,
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
				return em.Send(ctx, gateway.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   gateway.OutReply,
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
func wireRuntimeCallbacksAndRestore(
	mgr *chatsession.Manager,
	ch channel.Channel,
	em outbound.Emitter,
	logger *slog.Logger,
	prReg *prcache.Registry,
	gtwDeps gtw.HandlerDeps,
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
		agentHandler := newEventHandler(em, cs, mgr, logger, prReg, gtwDeps)
		cs.AgentEventBus.Subscribe(func(env chatsession.AgentEventEnvelope) bool {
			agentHandler(env)
			return false
		})

		// F-48: wrap the gateway's OnMessageState so the runtime
		// can stamp SessionContext on MessageSubmitted (the
		// Feishu placeholder card needs the footer). The gateway's
		// bare OnMessageState doesn't accept SessionContext — the
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
			out := gateway.OutboundMessage{
				Kind:    gateway.OutMessageState,
				ChatID:  e.ChatID,
				ReplyTo: e.UserMsgID,
				MessageState: &gateway.MessageStatePayload{
					State:     e.State,
					MessageID: e.UserMsgID,
				},
			}
			if e.State == agent.MessageSubmitted {
				// multi-as Phase 1: source AS comes from the
				// event itself, not from cs.SelectedAgentSession().
				// We pre-stamp here so the emitter's stamper (which
				// uses cs.SelectedAgentSession()) sees a non-nil
				// SessionContext and skips its lookup — preserving
				// the "source AS, not selected AS" semantics this
				// bus has always had.
				if as := lookupASByID(cs, e.AgentSessionID); as != nil {
					sessionContextInto(&out, as, prReg, gtwDeps)
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
			// there's no inbound ctx to chain.
			if fa, ok := ch.(*feishu.Adapter); ok {
				fa.MarkReceiptPromptDone(context.Background(), e.ChatID, e.UserMsgID)
			}
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
	prReg *prcache.Registry,
	gtwDeps gtw.HandlerDeps,
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
		// gateway.Translate) → SessionContext.Usage (via
		// sessionContextInto below) → channel footer. The runtime
		// is a passive pass-through; no accumulation, no dedup,
		// no priority. Usage rides on EventAgentResult (populated by
		// the bridges via AgentResultEvent.Usage) — AgentDoneEvent.Usage is
		// not currently surfaced because gateway.Translate drops
		// EventAgentDone events before the runtime stamps
		// SessionContext. Bridges that only emit EventAgentDone-with-
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
			sessionContextInto(&out, s, prReg, gtwDeps)
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
		if (out.Kind == gateway.OutToolStart || out.Kind == gateway.OutToolEnd) &&
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

// sessionContextInto populates the F-45/F-48 SessionContext
// snapshot on the OutboundMessage when there's at least one
// meaningful field to render. Reused by the 4 main-chat kind
// stamp site AND the OutMessageState+MessageSubmitted site (the
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
// the entire outbound-message pipeline — MessageSubmitted
// placeholders AND every stamped reply/result wait for git to
// return. 3s is plenty for normal repos (10-50ms typical; up to
// ~1s on very large monorepos) and far below the user's
// "chat is not realtime" tolerance. On timeout, CollectStatus
// returns (nil, nil) and the footer omits the git segment
// silently — chat keeps moving.
//
// Stamp SessionContext when there's something worth showing in
// the footer — either Agent/Model identity, git tracking, a
// non-zero compaction count, or a per-event Usage payload on
// this OutboundMessage.
//
// Usage flows straight from the bridge event onto the outbound
// message (out.Usage is set by gateway.Translate from
// AgentResultEvent.Usage / AgentDoneEvent.Usage). The runtime does NOT
// aggregate across turns; Agent is a passive
// pass-through. The footer renders out.Usage directly.

// lookupASByID finds an AgentSession in the chat's pool by ID.
// Used by event subscribers to recover the source AS from
// AgentSessionID carried on each event (multi-as Phase 1: source
// AS comes from the event, not from cs.selectedAS).
//
// Returns nil if the AS is no longer in the pool (e.g. after a
// concurrent /kill). Subscribers must handle nil.
func lookupASByID(cs *chatsession.ChatSession, id string) *agentsession.AgentSession {
	if cs == nil || id == "" {
		return nil
	}
	for _, as := range cs.Pool() {
		if as.ID == id {
			return as
		}
	}
	return nil
}

// AgentSession.Agent is immutable (direct field read, no lock);
// Model() takes RLock internally; git status is captured fresh
// on each stamp (3s deadline, no caching — see F-48 §1.7).
func sessionContextInto(out *gateway.OutboundMessage, s *agentsession.AgentSession, prReg *prcache.Registry, deps gtw.HandlerDeps) {
	// F-55: out.Usage is set by gateway.Translate from the
	// bridge wire payload (AgentResultEvent.Usage / AgentDoneEvent.Usage).
	// The runtime is a passive pass-through — copy verbatim so the
	// channel footer can render it via ctx.Usage. Pre-F-55 the
	// copy was missing, so footers silently rendered without
	// usage data. buildSessionContext is the shared gate; the
	// stamper and this legacy path both feed it the same AS +
	// usage inputs. PullRequest lookup is plumbed through here too
	// (see buildSessionContext) so the F-49 footer line surfaces
	// the open PR / MR per AgentSession.
	out.SessionContext = buildSessionContext(s, out.Usage, prReg, deps)
}

// buildSessionContext is the pure (no I/O of its own — git
// collection is delegated) stamping primitive shared by the
// runtime pump (sessionContextInto) and the outbound.Stamper
// (newRuntimeStamper). It snapshots the AgentSession's identity
// fields plus a freshly-collected git status, and returns a
// SessionContext when any meaningful field is non-empty.
//
// Returns nil when there's nothing to render — caller treats nil
// as "skip the footer this turn" rather than rendering an empty
// footer segment.
//
// Git invocation has a 3s deadline (review fix): a hung git
// (stalled NFS, broken .git/index, ... ) would otherwise block
// the entire outbound-message pipeline. 3s is plenty for normal
// repos (10-50ms typical; up to ~1s on very large monorepos) and
// far below the user's "chat is not realtime" tolerance. On
// timeout, CollectStatus returns (nil, nil) and the footer omits
// the git segment silently.
//
// PR / MR lookup (F-49): per-AgentSession, cached on prReg. The
// read is strictly synchronous (returns the cached value,
// possibly nil on the first ever call) — no I/O blocks the stamp
// path. MaybeRefresh inspects the cache and spawns the network
// round-trip asynchronously when the entry is stale; the next
// stamp picks up the refreshed value. The same prRef is fed to
// the materialize gate below so a populated PullRequest alone is
// enough to surface a SessionContext.
//
// Nil-safe on the prReg dependency: a hand-wired debug build
// that skips registry construction still gets a fully-functional
// stamp path with PullRequest == nil. GetOrCreate itself never
// returns nil once we have a non-nil registry. Test pinned by
// TestSessionContextInto_NilPRRegistryLeavesEmpty.
func buildSessionContext(s *agentsession.AgentSession, usage *agent.UsageInfo, prReg *prcache.Registry, deps gtw.HandlerDeps) *gateway.SessionContext {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	gitSnap, _ := gtw.CollectStatus(ctx, s.Cwd, gtw.ExecGitRunner{})
	cancel()
	hasGit := gitSnap != nil && s.Cwd != ""

	// Snapshot Model() and SessionID() once each — both take
	// asMu.RLock() internally; calling them twice in the gate
	// + the literal doubles the lock acquisitions per stamp for
	// no functional gain (a racing SetModel / SetSessionID
	// between gate and literal would either be visible or not
	// in both places anyway, and a stale read in the literal is
	// no worse than a stale read in the gate).
	model := s.Model()
	sessionID := s.SessionID()

	var prRef *gtw.PR
	if prReg != nil {
		if prCache := prReg.GetOrCreate(s.ID); prCache != nil {
			prCache.MaybeRefresh(s.Cwd, deps)
			prRef = prCache.PR()
		}
	}
	hasPR := prRef != nil

	if s.Agent == "" && model == "" && sessionID == "" && !hasGit && !hasPR && usage == nil {
		return nil
	}
	return &gateway.SessionContext{
		Agent:       s.Agent,
		Model:       model,
		SessionID:   sessionID,
		Workspace:   s.Cwd,
		GitStatus:   gitSnap,
		PullRequest: prRef,
		Usage:       usage,
	}
}

// newRuntimeStamper returns an outbound.Stamper that injects the
// F-45/F-48 SessionContext footer for every outbound message.
// Wired into outbound.Emitter at construction time so the stamp
// happens uniformly — runtime pump, slash-command replies, and
// MessageState subscribers all converge on the same stamping path.
//
// Returning nil (when the chat has no active AgentSession, or
// every stamp field is empty) signals "skip the footer this turn";
// the channel render path treats nil SessionContext as "omit the
// footer segment" rather than rendering an empty one.
//
// Pre-refactor this logic lived at every call site (cmd/nightme/run.go's
// sessionContextInto calls at the pump + MessageStateBus sites) and
// was skipped entirely by slash-command replies — the whole point
// of moving it onto outbound.Emitter is that nobody has to remember
// to call it any more.
//
// PullRequest lookup (F-49) is plumbed through prReg/deps the same
// way as sessionContextInto — both stamp sites share the per-AS
// prcache.Registry so a /gtw pr Invalidate surfaces on the very
// next outbound (not after the 60s TTL).
func newRuntimeStamper(mgr *chatsession.Manager, prReg *prcache.Registry, deps gtw.HandlerDeps) outbound.Stamper {
	return func(chatID string) *gateway.SessionContext {
		if mgr == nil {
			return nil
		}
		cs := mgr.Get(chatID)
		if cs == nil {
			return nil
		}
		as := cs.SelectedAgentSession()
		if as == nil {
			return nil
		}
		// buildSessionContext takes optional usage; the stamper has
		// no per-event usage here (usage lives on the OutboundMessage
		// when present, which the emitter sees before stamper lookup).
		// Pass nil — usage-bearing events already carry their usage
		// on the msg and don't need this footer path to populate it.
		return buildSessionContext(as, nil, prReg, deps)
	}
}

// (responder removed: was a vestigial adapter from the pre-
// outbound-package readPump era. The runtime pump now constructs
// its own Emitter.Send calls in newEventHandler; this type had no
// remaining callers.)

// shutdownRun stops the channel and persists final state.
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

// toCardChoices translates command.CardChoice (command pkg) to
// the wire-level gateway.CardChoice. Both have the same fields;
// this is a direct copy. Defined here (in run.go) so the
// runtime owns the boundary translation without leaking the
// command-package's mirror types deeper into the runtime.
func toCardChoices(in []command.CardChoice) []gateway.CardChoice {
	if len(in) == 0 {
		return nil
	}
	out := make([]gateway.CardChoice, len(in))
	for i, c := range in {
		out[i] = gateway.CardChoice{
			Emoji:  c.Emoji,
			Label:  c.Label,
			Action: c.Action,
		}
	}
	return out
}

// chatSessionChannelSender is the runtime-side adapter that
// implements shell.Sender on top of outbound.Emitter. Each
// chat session carries its own channel (the Feishu adapter
// wrapping the underlying connection), and the dispatcher
// looks it up by ChatID at Send time — so a single sender
// routes to any chat the manager knows about.
//
// Send is best-effort: if the chat session can't be resolved
// (e.g. unloaded during shutdown) or the channel refuses, we
// silently drop. The shell dispatcher's Handle is the
// fire-and-forget reply path (the result card), not a critical
// control message.
// chatSessionChannelSender implements shell.Sender on top of the
// Manager's shared outbound.Emitter. The shell dispatcher only
// needs Send (no SendCard) so this is a thin one-method shim.
type chatSessionChannelSender struct {
	mgr *chatsession.Manager
}

// Send looks up the ChatSession for the requested chatID and
// posts the reply through its Emitter. nil-safe everywhere: a
// missing chat session, missing emitter, or missing reply
// target all silently no-op (matches the old wrap's behaviour).
func (s chatSessionChannelSender) Send(ctx context.Context, msg shell.Outbound) error {
	if msg.ChatID == "" {
		return nil
	}
	cs := s.mgr.Get(msg.ChatID)
	if cs == nil {
		return nil
	}
	em := cs.Emitter()
	if em == nil {
		return nil
	}
	return em.Send(ctx, gateway.OutboundMessage{
		ChatID:  msg.ChatID,
		Kind:    gateway.OutReply,
		Text:    msg.Text,
		ReplyTo: msg.ReplyTo,
	})
}

// (chatSessionChannelSender.Send[1] was the old implementation
// that called cs.Emitter() into a local variable named 'ch' and
// passed the chatsession.OutboundMessage-typed payload. It has
// been removed: the new Send at line ~1483 is the single source
// of truth.)
