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
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
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

	// Restore from disk (per-chat ChatSession + per-AgentSession
	// metadata; processes not running).
	if err := mgr.RestoreFromRegistry(); err != nil {
		return fmt.Errorf("run: restore: %w", err)
	}

	// startChannel wires the channel and starts its connection.
	if err := ch.Start(ctx); err != nil {
		logger.Error("channel disconnected", "reason", err)
		return fmt.Errorf("run: start channel: %w", err)
	}
	logger.Info("channel connected")
	fmt.Fprintln(out, "Channel connected")

	if fa, ok := ch.(*feishu.Adapter); ok {
		fa.SetLogger(logger)
	}

	// Build the router wiring (slashCommandDispatcher +
	// messageDispatcher).
	messageDispatcher := newMessageDispatcher(mgr, ch, cfg.Primary, logger)

	// Install EventHandler on every ChatSession — both restored
	// and post-startup. The handler translates AgentEvent →
	// OutboundMessage + sends via the channel. It's also where
	// F-think's /think off gate lives, so missing this install
	// would silently make /think a no-op for any new chat.
	//
	// Per-cs factory: each ChatSession gets its own closure that
	// captures `cs` directly, eliminating the per-event
	// mgr.Get(chatID) round-trip the gate used to do. The factory
	// is constructed once; the per-cs closures are cheap (just a
	// captured pointer).
	//
	// F-31 message-state handler is installed in the same
	// WithOnCreate block so restored + future chats share one
	// installation site (no separate for-loop).
	eventHandlerFactory := func(cs *chatsession.ChatSession) chatsession.EventHandler {
		return newEventHandler(ch, cs, mgr, logger)
	}
	gw := gateway.New(messageDispatcher)
	gateway.RegisterChatSessionCommands(gw, mgr, ch, cfg.Primary)

	// Attach channels + start the gateway.
	gwImpl := gw.(*gateway.Router)
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

	// F-31 + F-think: wire gw.OnMessageState AND the runtime's
	// EventHandler into every ChatSession via the Manager.onCreate
	// hook. This covers both restored chats (Manager fires
	// onCreate for restored entries too, see RestoreFromRegistry)
	// and chats created later via mgr.GetOrCreate(). Both callbacks
	// MUST be installed in this single closure — separating them
	// (one here, one in /use or newMessageDispatcher) is a
	// silent-failure landmine because readpump fires only when
	// eventHandler is non-nil.
	mgr.WithOnCreate(func(cs *chatsession.ChatSession) {
		cs.SetEventHandler(eventHandlerFactory(cs))
		cs.SetMessageStateHandler(gwImpl.OnMessageState)
	})

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
		cs.EmitMessageState(userMsgID, agent.StateReceived)

		// Resolve active AgentSession (lazy spawn on miss).
		_, err := cs.LookupActiveAgentSession()
		if err != nil {
			if errors.Is(err, chatsession.ErrNoActiveCwd) {
				return ch.Send(ctx, gateway.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   gateway.OutText,
					Text:   "No workspace set. Send /cwd <path> first.",
				})
			}
			// Spawn failed (binary missing, etc.); let the user know.
			return ch.Send(ctx, gateway.OutboundMessage{
				ChatID: msg.ChatID,
				Kind:   gateway.OutText,
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
		cs.EmitMessageState(userMsgID, agent.StateForwarded)

		// Build structured blocks and queue to InputBuffer.
		blocks := feishu.BuildBlocks(msg.Text, msg.Attachments)
		if err := cs.QueueUserMessage(blocks, userMsgID); err != nil {
			if errors.Is(err, chatsession.ErrBufferFull) {
				return ch.Send(ctx, gateway.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   gateway.OutText,
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
		// Translate the AgentEvent to an OutboundMessage.
		out, ok := gateway.Translate(chatID, ev)
		if !ok {
			return
		}
		// Stamp the current turn's anchor so the Channel can
		// route to the right per-userMsgID receipt. ReplyTo
		// stays empty for orphan events (EventInit at startup,
		// internal logs) — Channel renders those as plain text.
		out.ReplyTo = userMsgID
		// F-think §3.1.2: per-chat OutThinking gate. When the
		// chat has /think off, drop OutThinking events here
		// (after Translate + ReplyTo stamping, before ch.Send)
		// so the Feishu adapter never sees them. Other
		// OutboundKinds — OutText / OutResult / OutToolStart /
		// OutToolEnd / OutCompaction / OutInit / OutUsage —
		// are unaffected.
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
		if err := ch.Send(context.Background(), out); err != nil && logger != nil {
			logger.Warn("channel send failed",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"agent_session_id", s.ID,
				"err", err)
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
		Kind:    gateway.OutText,
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
				if err := cs.KillAll(); err != nil && logger != nil {
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