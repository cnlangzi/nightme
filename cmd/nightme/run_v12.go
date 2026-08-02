// Package main — `nightme run` v1.2 daemon (commit 8b).
//
// The v1.2 daemon wires together:
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
// Existing v1.1 runtime in run.go is preserved (for tests + backwards
// compatibility). The Cobra-level `nightme run` routes here by default
// in commit 8b (configurable via the legacy runDeps path).

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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

// runDeps_v12 holds the construction seams for the v1.2 daemon.
// Same shape as the legacy runDeps but uses chatsession types.
type runDeps_v12 struct {
	loadConfig     func() (*config.Config, error)
	openChatSessions func(*config.Config) (*registry.ChatSessionFile, error)
	openAgentSessions func(*config.Config) (*registry.AgentSessionFile, error)
	buildAgents    func(*config.Config) *agent.Registry
	newChannel     func(*config.Config) (channel.Channel, error)
	signals        <-chan os.Signal
	cleanup        bool
	skipFeishuAuth bool
}

func defaultRunDeps_v12() runDeps_v12 {
	return runDeps_v12{
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

func chatSessionsPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "chat_sessions.json"), nil
}

func agentSessionsPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agent_sessions.json"), nil
}

// newRunCmd builds the long-running Feishu daemon command (v1.2).
//
// v1.2 only: there is no `--v12` flag anymore. The v1.1
// MemoryManager-based daemon (cmd/nightme/run.go, deleted in commit
// 13) was retained as an escape hatch during the 8b/8c transition;
// with v1.2 locked and runtime integration verified, it has been
// removed entirely.
func newRunCmd() *cobra.Command {
	var cleanup bool
	var channelName string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the Feishu daemon (v1.2 ChatSession-based runtime)",
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
			return runRunV12(cmd, cleanup, channelName)
		},
	}
	cmd.Flags().BoolVar(&cleanup, "cleanup", false,
		"Kill every session CLI on shutdown instead of detaching them")
	cmd.Flags().StringVar(&channelName, "channel", "feishu",
		"Channel implementation: feishu (default) or echo (smoke test)")
	return cmd
}

// runRunV12 dispatches to the v1.2 daemon. No flag needed; v1.2 is
// the only runtime (commit 13 deleted the v1.1 fallback).
func runRunV12(cmd *cobra.Command, cleanup bool, channelName string) error {
	if channelName != "" && channelName != "feishu" && channelName != "echo" {
		return fmt.Errorf("run: unknown channel %q (want feishu or echo)", channelName)
	}
	deps := withChannel_v12(defaultRunDeps_v12(), channelName)
	deps.cleanup = cleanup
	return runRunWith_v12(cmd, deps)
}

// withChannel_v12 mirrors the legacy withChannel but for v1.2 deps.
func withChannel_v12(deps runDeps_v12, channelName string) runDeps_v12 {
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

// runRunWith_v12 is the v1.2 daemon entrypoint. Mirrors runRunWith
// (v1.1) in structure: install signal handling, fill in nil deps,
// delegate to runDaemon_v12.
func runRunWith_v12(cmd *cobra.Command, deps runDeps_v12) error {
	if cmd == nil {
		return errors.New("run v1.2: command is required")
	}
	defaults := defaultRunDeps_v12()
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
	return runDaemon_v12(cmd.Context(), out, deps, sigCh)
}

// runDaemon_v12 is the v1.2 daemon core. Wires chatsession.Manager
// + Spawner + FlushHook + EventCallback; runs the gateway until
// signal / context cancel.
func runDaemon_v12(ctx context.Context, out io.Writer, deps runDeps_v12, sigCh <-chan os.Signal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	logger := loggerFromContext(ctx)

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("run v1.2: load config: %w", err)
	}
	if cfg == nil {
		return errors.New("run v1.2: load config: returned nil config")
	}
	if !deps.skipFeishuAuth && (cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "") {
		return errors.New("run v1.2: Feishu credentials are not configured; run `nightme auth login feishu`")
	}

	csFile, err := deps.openChatSessions(cfg)
	if err != nil {
		return fmt.Errorf("run v1.2: open chat_sessions: %w", err)
	}
	asFile, err := deps.openAgentSessions(cfg)
	if err != nil {
		return fmt.Errorf("run v1.2: open agent_sessions: %w", err)
	}

	agents := deps.buildAgents(cfg)
	if agents == nil {
		return errors.New("run v1.2: agent registry is nil")
	}

	ch, err := deps.newChannel(cfg)
	if err != nil {
		return fmt.Errorf("run v1.2: create channel: %w", err)
	}
	if ch == nil {
		return errors.New("run v1.2: channel is nil")
	}

	// Build the ChatSession manager.
	spawner := chatsession.NewRegistrySpawner(agents)
	mgr := chatsession.NewManager().
		WithSpawner(spawner).
		WithPersistence(csFile, asFile)

	// commit 8c: migrate v1.1 registry.json → backup (v1.bak).
	// Per PLAN §4.6.7: v1.x → v1.2 is NOT a transparent migration
	// (v1.1 didn't persist chat_id). We archive the old file
	// and start fresh.
	v1Legacy := filepath.Join(filepath.Dir(cfg.Paths.DataDir), "registry.json")
	if count, err := registry.MigrateV1ToV2(v1Legacy); err != nil {
		logger.Warn("v1.x migration failed", "err", err)
	} else if count > 0 {
		logger.Info("archived v1.x registry", "entries", count, "backup", v1Legacy+".v1.bak")
	}

	// Restore from disk (per-chat ChatSession + per-AgentSession
	// metadata; processes not running).
	if err := mgr.RestoreFromRegistry(); err != nil {
		return fmt.Errorf("run v1.2: restore: %w", err)
	}

	// startChannel wires the channel and starts its connection.
	if err := ch.Start(ctx); err != nil {
		logger.Error("channel disconnected", "reason", err)
		return fmt.Errorf("run v1.2: start channel: %w", err)
	}
	logger.Info("channel connected")
	fmt.Fprintln(out, "Channel connected")

	if fa, ok := ch.(*feishu.Adapter); ok {
		fa.SetLogger(logger)
	}

	// Build the v1.2 router wiring.
	messageDispatcher := v12MessageDispatcher(mgr, ch, cfg.Primary, logger)

	// commit 8c: install EventHandler on every ChatSession. The
	// handler translates AgentEvent → OutboundMessage + sends via
	// the channel. Pre-install on existing chats (restored from
	// disk); new chats get it via /use or first message dispatch.
	eventHandler := v12EventHandler(ch, logger)
	for _, cs := range mgr.List() {
		cs.SetEventHandler(eventHandler)
	}

	gw := gateway.New(messageDispatcher, nil)
	gateway.RegisterChatSessionCommands(gw, mgr, ch, cfg.Primary)

	// Attach channels + start the gateway.
	gwImpl := gw.(*gateway.Router)
	gwImpl.AttachChannels(ch)

	// Start readPumps for already-running AgentSessions that
	// were restored from disk (Detached → running on next
	// LookupActiveAgentSession). For the v1.2 first cut, we
	// don't auto-spawn at startup; users must send a message
	// (which triggers LookupActiveAgentSession → Spawner).
	v12EnsureReadPumps(mgr, ch, cfg.Primary, logger)

	if err := gwImpl.Start(ctx); err != nil {
		return fmt.Errorf("run v1.2: start gateway: %w", err)
	}
	defer func() { _ = gwImpl.Stop(context.Background()) }()

	logger.Info("v1.2 daemon running",
		"chat_sessions", len(mgr.List()),
		"primary", cfg.Primary)

	// Block on signal or context cancellation.
	select {
	case <-ctx.Done():
	case sig, ok := <-sigCh:
		if ok && sig != nil {
			fmt.Fprintf(out, "[nightme v1.2] received %s\n", sig)
		}
	}
	return shutdownRun_v12(out, ch, mgr, csFile, asFile, deps.cleanup, logger)
}

// v12MessageDispatcher is the runtime-injected messageDispatcher
// for the v1.2 inboundDispatcher. It is invoked when no slash
// command matches; it routes the inbound message to the chat's
// active AgentSession via the InputBuffer.
//
// Flow (mirrors v1.1 fallback with ChatSession primitives):
//
//  1. cs = mgr.GetOrCreate(chatID, chatType, cfg.Primary)
//  2. cs.LookupActiveAgentSession() (lazy spawn)
//  3. cs.QueueUserMessage(blocks, userMsgID) (Idle → flush now;
//     Busy → queue)
//  4. SetBusy on first event (drive FSM)
func v12MessageDispatcher(mgr *chatsession.Manager, ch channel.Channel, primary string, logger *slog.Logger) func(context.Context, *gateway.InboundMessage) error {
	return func(ctx context.Context, msg *gateway.InboundMessage) error {
		if msg == nil {
			return nil
		}
		userMsgID := msg.MessageID
		if userMsgID == "" {
			userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}

		cs := mgr.GetOrCreate(msg.ChatID, string(msg.ChatType), primary)

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
		// (before any /use) only goes through v12MessageDispatcher, so we
		// need to start the pump here too. StartReadPump is
		// idempotent — it stops any existing pump first, so calling
		// it again from handleUse is a no-op.
		_ = cs.StartReadPump()

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

// v12EnsureReadPumps walks every ChatSession and ensures a readPump
// is running for its current active AgentSession. Called at
// startup after RestoreFromRegistry; the AgentSessions are
// Detached (no process), so this is a no-op for restored
// sessions — readPumps are started lazily on first /use + send.
//
// commit 8c: this is a placeholder. The runtime's actual readPump
// start happens in handleUse (gateway package) and on first
// message dispatch in v12MessageDispatcher.
func v12EnsureReadPumps(mgr *chatsession.Manager, ch channel.Channel, primary string, logger *slog.Logger) {
	// no-op for commit 8c.
	_ = mgr
	_ = ch
	_ = primary
	_ = logger
}

// v12EventHandler returns the per-event callback installed on
// every ChatSession by the runtime. The callback translates
// AgentEvent → OutboundMessage and dispatches via the channel.
//
// commit 8c: chatID is passed by ChatSession's readPump directly
// (the ChatSession knows its own ChatID); the handler doesn't
// need to look it up.
func v12EventHandler(ch channel.Channel, logger *slog.Logger) chatsession.EventHandler {
	return func(chatID string, s *chatsession.AgentSession, ev agent.AgentEvent) {
		// Translate the AgentEvent to an OutboundMessage.
		out, ok := gateway.Translate(chatID, ev)
		if !ok {
			return
		}
		out.ReplyTo = "" // commit 8c: ReplyTo is set by Channel layer (Receipt FSM)
		if err := ch.Send(context.Background(), out); err != nil && logger != nil {
			logger.Warn("channel send failed",
				"chat_id", chatID,
				"agent_session_id", s.ID,
				"err", err)
		}
	}
}

// v12Responder adapts a channel.Channel for v1.2 outbound
// messages. Keeps the v1.1 channelResponder simple Send path; the
// v1.2 readPump writes directly here.
type v12Responder struct {
	ch     channel.Channel
	mgr    *chatsession.Manager
	logger *slog.Logger
}

// Send translates and dispatches an AgentEvent to the channel for
// the chat owning the active AgentSession.
func (r *v12Responder) Send(ctx context.Context, chatID, userMsgID, text string) error {
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

// shutdownRun_v12 stops the channel, then either detaches or kills
// every ChatSession's AgentSessions depending on cleanup.
//
// Persistence: chat_sessions.json + agent_sessions.json are left in
// place. The Manager has been writing through to them throughout
// the run via WithPersistence.
func shutdownRun_v12(out io.Writer, ch channel.Channel, mgr *chatsession.Manager, csFile *registry.ChatSessionFile, asFile *registry.AgentSessionFile, cleanup bool, logger *slog.Logger) error {
	_ = out // future shutdown status line
	if logger == nil {
		logger = slog.Default()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var firstErr error
	if ch != nil {
		if err := ch.Stop(shutdownCtx); err != nil {
			firstErr = fmt.Errorf("run v1.2: stop channel: %w", err)
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