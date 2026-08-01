// Package main — `nightme run` daemon command.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/session"
)

// runDeps contains the construction seams for the daemon. The production
// command uses the defaults below; tests replace them with in-process fakes.
type runDeps struct {
	loadConfig   func() (*config.Config, error)
	openRegistry func(*config.Config) (*registry.File, error)
	buildAgents  func(*config.Config) *agent.Registry
	newChannel   func(*config.Config) (channel.Channel, error)
	newManager   func(*agent.Registry, *registry.File, session.EventCallback) session.Manager
	newGateway   func(session.Manager, *agent.Registry, gateway.Responder) gateway.Gateway
	signals      <-chan os.Signal
	cleanup      bool
}

func defaultRunDeps() runDeps {
	return runDeps{
		loadConfig:   config.LoadDefault,
		openRegistry: openRegistry,
		buildAgents:  buildRunAgentRegistry,
		newChannel: func(cfg *config.Config) (channel.Channel, error) {
			return feishu.NewAdapter(cfg)
		},
		newManager: func(agents *agent.Registry, reg *registry.File, cb session.EventCallback) session.Manager {
			return session.NewMemoryManager(agents, reg, cb)
		},
		newGateway: func(mgr session.Manager, agents *agent.Registry, resp gateway.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gateway.RegisterDefaultCommands(gw, mgr, agents, resp)
			return gw
		},
	}
}

// newRunCmd builds the long-running Feishu daemon command.
func newRunCmd() *cobra.Command {
	var cleanup bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the Feishu daemon",
		Long: "run starts the Feishu WebSocket channel and serves a Gateway " +
			"router on top of it. Slash commands (/cwd, /run, /kill, /help) " +
			"drive session lifecycle; plain text is forwarded to the live " +
			"agent behind the chat's session.\n\n" +
			"By default the daemon detaches session CLIs on shutdown so a " +
			"later `nightme run` (or /run) can resume them. Pass --cleanup " +
			"to instead Kill() every session on SIGINT/SIGTERM — useful for " +
			"CI or one-shot runs.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd, cleanup)
		},
	}
	cmd.Flags().BoolVar(&cleanup, "cleanup", false,
		"Kill every session CLI on shutdown instead of detaching them")
	return cmd
}

// runRun is the cobra entrypoint. The split makes the daemon easy to drive
// with fake channels and managers in unit tests.
func runRun(cmd *cobra.Command, cleanup bool) error {
	return runRunWith(cmd, withCleanup(defaultRunDeps(), cleanup))
}

// withCleanup returns deps with the cleanup flag applied. Exported
// to tests so they can drive both shutdown paths without parsing
// cobra flags.
func withCleanup(deps runDeps, cleanup bool) runDeps {
	deps.cleanup = cleanup
	return deps
}

// runRunWith installs signal handling when the caller did not provide a
// signal stream, then runs the testable daemon core.
func runRunWith(cmd *cobra.Command, deps runDeps) error {
	if cmd == nil {
		return errors.New("run: command is required")
	}
	defaults := defaultRunDeps()
	if deps.loadConfig == nil {
		deps.loadConfig = defaults.loadConfig
	}
	if deps.openRegistry == nil {
		deps.openRegistry = defaults.openRegistry
	}
	if deps.buildAgents == nil {
		deps.buildAgents = defaults.buildAgents
	}
	if deps.newChannel == nil {
		deps.newChannel = defaults.newChannel
	}
	if deps.newManager == nil {
		deps.newManager = defaults.newManager
	}
	if deps.newGateway == nil {
		deps.newGateway = defaults.newGateway
	}

	sigCh := deps.signals
	if sigCh == nil {
		owned := make(chan os.Signal, 2)
		signal.Notify(owned, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(owned)
		sigCh = owned
	}
	return runDaemon(cmd.Context(), cmd.OutOrStdout(), deps, sigCh)
}

// runDaemon wires the channel, session manager, and gateway, then
// loops: route incoming messages through the Gateway, render agent
// events back to the channel, and shut down cleanly on signal.
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
	if strings.TrimSpace(cfg.Feishu.AppID) == "" || strings.TrimSpace(cfg.Feishu.AppSecret) == "" {
		return errors.New("run: Feishu credentials are not configured; run `nightme auth login feishu`")
	}

	reg, err := deps.openRegistry(cfg)
	if err != nil {
		return fmt.Errorf("run: open registry: %w", err)
	}
	agents := deps.buildAgents(cfg)
	if agents == nil {
		return errors.New("run: agent registry is nil")
	}

	ch, err := deps.newChannel(cfg)
	if err != nil {
		return fmt.Errorf("run: create Feishu channel: %w", err)
	}
	if ch == nil {
		return errors.New("run: Feishu channel is nil")
	}

	// The session manager's EventCallback must be installed after
	// the channel exists so per-event rendering can reach the user.
	mgr := deps.newManager(agents, reg, nil)
	if mgr == nil {
		return errors.New("run: session manager is nil")
	}

	// Responder pushes chat-side replies through the channel.
	// If the channel is a Feishu adapter, we also wire the
	// F-25 Renderer (receipts + reactions) and route user messages
	// through the session's InputBuffer so concurrent messages are
	// queued while a turn is busy.
	var renderer *feishu.Renderer
	if feishuCh, ok := ch.(*feishu.Adapter); ok {
		renderer = feishu.NewRenderer(feishuCh)
	}
	responder := channelResponder{ch: ch, renderer: renderer}
	if renderer != nil {
		responder.userMessageFn = func(chatID, userMsgID string, blocks []agent.ContentBlock) error {
			sess, err := mgr.GetByChat(chatID)
			if err != nil {
				return fmt.Errorf("no session for chat: %w", err)
			}
			return sess.QueueUserMessage(blocks, userMsgID)
		}
	}

	// Build the gateway now that the manager exists. The fallback
	// forwards non-slash text to the chat's session via SendText.
	gw := deps.newGateway(mgr, agents, responder)
	if gw == nil {
		return errors.New("run: gateway is nil")
	}

	// Wrap the gateway's Handle() so the fallback we pass in
	// (closing over mgr) sees the same logic. We replace the
	// default fallback with one that forwards to the live session.
	//
	// F-25 integration: the fallback routes through the
	// channelResponder.SendUserMessage path, which (when the renderer
	// is wired) creates a MessageReceipt, queues the message via the
	// session's InputBuffer, and flips the receipt to Executing on
	// dispatch. This is what makes concurrent user messages buffer
	// correctly while Claude is busy.
	fallback := func(ctx context.Context, msg *gateway.Message) error {
		// Forwarding is best-effort. If the chat has no /cwd or
		// the agent is not running, we still send a hint so the
		// user is not left wondering.
		sess, err := mgr.GetByChat(msg.ChatID)
		if err != nil {
			return responder.Reply(ctx, msg.ChatID, "no workspace set, send /cwd <path> first")
		}
		if sess.Status() != session.StatusRunning {
			return responder.Reply(ctx, msg.ChatID, "CLI not running, send /run <agent> to start")
		}
		// Compose the structured user turn: caption text + the
		// successful attachment paths. Failed attachments are
		// already filtered out by BuildBlocks. Empty when the
		// message carried no text and all downloads failed (the
		// dispatcher branch above drops the message entirely when
		// AllFailed, so we never reach here in that case).
		blocks := feishu.BuildBlocks(msg.Text, msg.Attachments)
		// Stable, unique per Feishu message so the receipt/buffer
		// correlation keys never collide. Feishu CreateTime is
		// millisecond precision and two events from the same
		// sender within 1ms would otherwise share the same
		// composite ID and merge into a single ⏳→🔄→✅ cycle.
		userMsgID := msg.MessageID
		if userMsgID == "" {
			userMsgID = msg.SenderID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}
		// Renderer path: creates receipt + queues via InputBuffer.
		// Falls back to legacy SendText path when no renderer.
		if responder.renderer != nil {
			return responder.SendUserMessage(ctx, msg.ChatID, userMsgID, blocks)
		}
		return sess.SendBlocks(ctx, blocks)
	}
	gw = gateway.New(fallback)
	gateway.RegisterDefaultCommands(gw, mgr, agents, responder)

	// Reinstall the EventCallback now that the channel is alive.
	// The MemoryManager's callback is fixed at construction time
	// (deps.newManager); we instead drive rendering in a small
	// goroutine per session via the manager.List() loop below.
	_ = mgr

	if err := mgr.Restore(ctx); err != nil {
		return fmt.Errorf("run: restore sessions: %w", err)
	}

	if err := ch.Start(ctx); err != nil {
		logger.Error("channel disconnected", "reason", err)
		return fmt.Errorf("run: start Feishu channel: %w", err)
	}
	logger.Info("channel connected", "channel", "feishu")
	fmt.Fprintln(out, "Feishu WebSocket connected")

	// Spawn per-session pumps so live agent events are rendered
	// back to the chat. We (re)attach every time the session
	// table changes by polling on each incoming message.
	attachments := newSessionAttachments(mgr, ch, renderer)
	attachments.logger = logger

	incoming := ch.Incoming()
	if incoming == nil {
		return shutdownRun(out, ch, mgr, deps.cleanup, logger)
	}
	for {
		attachments.Sweep(ctx)

		select {
		case <-ctx.Done():
			return shutdownRun(out, ch, mgr, deps.cleanup, logger)
		case sig, ok := <-sigCh:
			if ok && sig != nil {
				fmt.Fprintf(out, "[nightme] received %s\n", sig)
			}
			return shutdownRun(out, ch, mgr, deps.cleanup, logger)
		case msg, ok := <-incoming:
			if !ok {
				return shutdownRun(out, ch, mgr, deps.cleanup, logger)
			}
			fmt.Fprintf(out, "received: %s (attachments=%d)\n", msg.Text, len(msg.Attachments))

			// F-14 inbound attachment handling. If the channel
			// message carries any non-text attachments, download
			// them synchronously (with retries) before letting the
			// gateway see the message. The Feishu adapter is the
			// only one that produces attachments today; other
			// channels fall through unchanged.
			//
			// Skip the download path entirely when there is no
			// session bound to this chat — without a session the
			// inbox directory cannot be created, every download
			// would "fail" with the misleading ❌ message, and the
			// user's caption would be silently dropped. Letting the
			// gateway's fallback handle the un-bound case produces a
			// correct "no workspace set" / "CLI not running" hint
			// instead.
			if len(msg.Attachments) > 0 {
				if feishuCh, ok := ch.(*feishu.Adapter); ok {
					sess, _ := mgr.GetByChat(msg.ChatID)
					if sess != nil {
						result := feishuCh.DownloadAttachments(ctx, msg.MessageID, msg.Attachments, sess.ID)
						msg.Attachments = result.Atts

						// All downloads failed: the inbound message
						// would mislead the agent (caption without
						// the image it referred to). Drop the
						// forwarding and tell the user to resend.
						if result.AllFailed {
							_ = responder.Reply(ctx, msg.ChatID,
								"❌ 文件下载失败 (重试 3 次后)\n请重新发送该消息。")
							continue
						}
						// Partial failure: forward what's available
						// and tell the user about the rest.
						if len(result.FailureKeys) > 0 {
							_ = responder.Reply(ctx, msg.ChatID,
								fmt.Sprintf("⚠️ 部分文件下载失败: %s\n请重新发送未下载成功的文件。",
									strings.Join(result.FailureKeys, ", ")))
						}
					}
				}
			}

			handleCtx := gateway.WithGateway(ctx, gw)
			if err := gw.Handle(handleCtx, &gateway.Message{
				ChatID:      msg.ChatID,
				Text:        msg.Text,
				SenderID:    msg.SenderID,
				Time:        msg.Time,
				ChatType:    msg.ChatType,
				MessageID:   msg.MessageID,
				Attachments: msg.Attachments,
			}); err != nil {
				fmt.Fprintf(out, "[nightme] gateway error: %v\n", err)
			}
		}
	}
}

// channelResponder adapts a channel.Channel to the gateway.Responder
// interface so the gateway handlers can push replies without taking
// a hard dependency on the channel package.
//
// renderer is optional — when set, channelResponder dispatches user
// messages through Renderer.SendUserMessage + Renderer.MarkExecuting,
// wiring the F-25 receipt lifecycle. When nil (non-Feishu channels
// or tests), it falls back to the plain SendLongMessage path.
type channelResponder struct {
	ch       channel.Channel
	renderer *feishu.Renderer
	// userMessageFn lets the responder route user messages into
	// the session queue (F-25). nil = plain SendText fallback.
	userMessageFn func(chatID, userMsgID string, blocks []agent.ContentBlock) error
}

func (r channelResponder) Reply(ctx context.Context, chatID, text string) error {
	if r.ch == nil {
		return nil
	}
	return r.ch.SendLongMessage(ctx, chatID, text)
}

// SendUserMessage is the F-25 entry point used by the gateway to
// hand a user message to the agent. It creates a MessageReceipt
// (⏳ emoji + reply), routes the structured blocks through the
// session InputBuffer (idle=bypass / busy=queue), and on dispatch
// flips the receipt to Executing.
//
// The receipt's reply text is the user-facing caption only (built
// from blocks via the renderer's own formatter). When the renderer
// is unavailable, blocks are flattened to a single string for the
// legacy SendLongMessage path.
//
// When renderer or userMessageFn is nil, this is a no-op (the
// caller should fall back to plain SendLongMessage). We do NOT
// silently drop the message — that would surprise users.
func (r channelResponder) SendUserMessage(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) error {
	if r.renderer == nil || r.userMessageFn == nil {
		// No renderer wired — best-effort fall-through: flatten
		// the blocks to a single string and send via the channel
		// so the user isn't left without feedback. The legacy
		// SendLongMessage path remains available for channels
		// that don't support reactions (Telegram etc.).
		if r.ch != nil {
			return r.ch.SendLongMessage(ctx, chatID, feishu.BuildForwardedTextFromBlocks(blocks))
		}
		return nil
	}
	// Create the receipt first so the user sees ⏳ immediately,
	// then dispatch via the session (which decides immediate send
	// vs buffer based on InputBuffer state). The receipt's reply
	// text is the flattened block list so the user sees the
	// attachment paths inline.
	receiptText := feishu.BuildForwardedTextFromBlocks(blocks)
	_, err := r.renderer.SendUserMessage(ctx, chatID, userMsgID, receiptText)
	if err != nil {
		// Don't fail the user's message; log and fall back.
		return r.userMessageFn(chatID, userMsgID, blocks)
	}
	if err := r.userMessageFn(chatID, userMsgID, blocks); err != nil {
		// Dispatch failed — keep the receipt in Waiting so the
		// user can /flush. The receipt's note already says "⏳
		// 等待中" which is honest.
		return err
	}
	// Dispatch succeeded → mark the receipt as Executing so the
	// user sees 🔄 + heartbeat counter.
	_ = r.renderer.MarkExecuting(ctx, userMsgID)
	return nil
}

// sessionAttachments routes AgentEvents from every live session
// back to the corresponding chat via the channel renderer. It
// tracks which sessions it has already attached to so each new
// agent gets exactly one pump.
//
// When a Feishu Renderer is available, the pump delegates to it
// (which drives the F-25 receipt lifecycle — see Renderer.RenderEvent).
// Otherwise the pump falls back to direct SendMessage calls (the
// v0.1 behaviour, kept for non-Feishu channels).
type sessionAttachments struct {
	mgr      session.Manager
	ch       channel.Channel
	renderer *feishu.Renderer
	mu       sync.Mutex
	attached map[string]struct{}
	logger   *slog.Logger
}

func newSessionAttachments(mgr session.Manager, ch channel.Channel, renderer *feishu.Renderer) *sessionAttachments {
	return &sessionAttachments{
		mgr:      mgr,
		ch:       ch,
		renderer: renderer,
		attached: make(map[string]struct{}),
	}
}

// Sweep inspects the session table and starts a pump goroutine for
// every session that has a live agent but no attached pump yet.
func (s *sessionAttachments) Sweep(ctx context.Context) {
	if s.mgr == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.mgr.List() {
		if sess == nil || sess.ChatID == "" {
			continue
		}
		if sess.Status() != session.StatusRunning {
			continue
		}
		if _, ok := s.attached[sess.ID]; ok {
			continue
		}
		events := sess.Events()
		if events == nil {
			continue
		}
		s.attached[sess.ID] = struct{}{}
		if s.logger != nil {
			s.logger.Info("session created", "chat_id", sess.ChatID, "workspace", sess.Workspace, "agent", sess.Agent)
		}
		chatID := sess.ChatID
		go s.pump(ctx, chatID, events)
	}
}

// pump drains events for one session and renders them through the
// channel. It exits when the events channel closes (the agent has
// ended) or when ctx is cancelled.
func (s *sessionAttachments) pump(ctx context.Context, chatID string, events <-chan agent.AgentEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if s.ch == nil {
				continue
			}
			// Prefer the Feishu Renderer when available — it drives
			// the F-25 receipt lifecycle (heartbeat ticks,
			// state transitions). Otherwise fall back to direct
			// SendMessage for non-Feishu channels.
			if s.renderer != nil {
				if err := s.renderer.RenderEvent(ctx, chatID, ev); err != nil {
					if s.logger != nil {
						s.logger.Warn("renderer failed",
							"chat_id", chatID,
							"event_kind", ev.Kind.String(),
							"err", err)
					}
				}
				continue
			}
			switch ev.Kind {
			case agent.EventText:
				_ = s.ch.SendLongMessage(ctx, chatID, ev.Text)
			case agent.EventToolStart:
				name := "tool"
				if ev.ToolStart != nil && ev.ToolStart.Name != "" {
					name = ev.ToolStart.Name
				}
				_ = s.ch.SendMessage(ctx, chatID, "🔧 "+name+"...")
			case agent.EventToolEnd:
				name := "tool"
				if ev.ToolEnd != nil && ev.ToolEnd.Name != "" {
					name = ev.ToolEnd.Name
				}
				_ = s.ch.SendMessage(ctx, chatID, "✅ "+name+" done")
			case agent.EventDone:
				code := 0
				if ev.Done != nil {
					code = ev.Done.ExitCode
				}
				_ = s.ch.SendMessage(ctx, chatID, fmt.Sprintf("Session ended (exit %d)", code))
			case agent.EventError:
				if ev.Error != nil && ev.Error.Err != nil {
					_ = s.ch.SendMessage(ctx, chatID, "Error: "+ev.Error.Err.Error())
				}
			}
		}
	}
}

// shutdownRun stops the channel first, then either detaches or
// kills every known session depending on cleanup. The default
// policy is detach (matches v0.1 behavior); --cleanup opts into
// the kill-everything path used by CI / one-shot scripts.
func shutdownRun(out io.Writer, ch channel.Channel, mgr session.Manager, cleanup bool, loggers ...*slog.Logger) error {
	_ = out // retained in the signature for a future shutdown status line
	var logger *slog.Logger
	if len(loggers) > 0 {
		logger = loggers[0]
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var firstErr error
	if ch != nil {
		if err := ch.Stop(shutdownCtx); err != nil {
			firstErr = fmt.Errorf("run: stop channel: %w", err)
		}
	}
	if mgr == nil {
		return firstErr
	}

	if cleanup {
		killErr := killAllSessions(mgr, logger)
		if killErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("run: kill sessions: %w", killErr)
		}
	} else {
		markErr := markSessionsDetached(mgr)
		if markErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("run: detach sessions: %w", markErr)
		}
	}
	if err := mgr.Persist(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("run: persist sessions: %w", err)
	}
	return firstErr
}

type sessionDetacher interface {
	MarkDetached(string) error
}

// markSessionsDetached uses the MemoryManager's process-aware implementation
// when available. A small fallback keeps the run core useful with a test fake
// that only implements session.Manager.
func markSessionsDetached(mgr session.Manager) error {
	if detacher, ok := mgr.(sessionDetacher); ok {
		for _, sess := range mgr.List() {
			if sess == nil {
				continue
			}
			if err := detacher.MarkDetached(sess.ID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
				return err
			}
		}
		return nil
	}
	for _, sess := range mgr.List() {
		if sess != nil {
			sess.MarkDetached()
		}
	}
	return nil
}

// killAllSessions terminates every session via the manager's Kill
// helper. We collect non-fatal errors and return them joined so a
// single rogue process does not mask the rest. Already-exited
// sessions are skipped (Kill is idempotent).
func killAllSessions(mgr session.Manager, logger *slog.Logger) error {
	var firstErr error
	for _, sess := range mgr.List() {
		if sess == nil {
			continue
		}
		if err := mgr.Kill(sess.ID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
			if firstErr == nil {
				firstErr = fmt.Errorf("kill %s: %w", sess.ID, err)
			}
		} else if logger != nil {
			logger.Info("session killed", "chat_id", sess.ChatID, "pid", sess.PID)
		}
	}
	return firstErr
}
