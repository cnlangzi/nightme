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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/gateway"
	gatewaycmd "github.com/cnlangzi/nightme/internal/gateway/cmd"
	"github.com/cnlangzi/nightme/internal/registry"
	"github.com/cnlangzi/nightme/internal/session"
)

// runDeps contains the construction seams for the daemon. The production
// command uses the defaults below; tests replace them with in-process fakes.
type runDeps struct {
	loadConfig     func() (*config.Config, error)
	openRegistry   func(*config.Config) (*registry.File, error)
	buildAgents    func(*config.Config) *agent.Registry
	newChannel     func(*config.Config) (channel.Channel, error)
	newManager     func(*agent.Registry, *registry.File, session.EventCallback) session.Manager
	newGateway     func(session.Manager, *agent.Registry, gatewaycmd.Responder) gateway.Gateway
	signals        <-chan os.Signal
	cleanup        bool
	skipFeishuAuth bool
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
		newGateway: func(mgr session.Manager, agents *agent.Registry, resp gatewaycmd.Responder) gateway.Gateway {
			gw := gateway.New(nil)
			gatewaycmd.RegisterDefaultCommands(gw, mgr, agents, resp)
			return gw
		},
	}
}

// newRunCmd builds the long-running Feishu daemon command.
func newRunCmd() *cobra.Command {
	var cleanup bool
	var channelName string
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
			"CI or one-shot runs.\n\n" +
			"Pass --channel=echo to run the daemon with the echo channel " +
			"(a no-network stub that prints outbound messages to stdout). " +
			"Useful for smoke tests and for exercising the v0.3 hub-and-" +
			"spoke architecture without Feishu credentials.",
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

// runRun is the cobra entrypoint. The split makes the daemon easy to drive
// with fake channels and managers in unit tests.
func runRun(cmd *cobra.Command, cleanup bool, channelName string) error {
	if channelName != "" && channelName != "feishu" && channelName != "echo" {
		return fmt.Errorf("run: unknown channel %q (want feishu or echo)", channelName)
	}
	deps := withChannel(defaultRunDeps(), channelName)
	return runRunWith(cmd, withCleanup(deps, cleanup))
}

// withCleanup returns deps with the cleanup flag applied. Exported
// to tests so they can drive both shutdown paths without parsing
// cobra flags.
func withCleanup(deps runDeps, cleanup bool) runDeps {
	deps.cleanup = cleanup
	return deps
}

// withChannel overrides defaultRunDeps.newChannel based on the
// --channel flag. The default ("feishu") is unchanged; "echo"
// swaps in the no-network stub for smoke tests.
func withChannel(deps runDeps, channelName string) runDeps {
	switch channelName {
	case "feishu", "":
		// default — feishu.NewAdapter
	case "echo":
		deps.skipFeishuAuth = true
		deps.newChannel = func(*config.Config) (channel.Channel, error) {
			return echo.New("echo", os.Stdout), nil
		}
	default:
		// Unknown: leave defaultRunDeps to fail with the existing
		// Feishu credential check, so the user sees a clear error.
	}
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
	if !deps.skipFeishuAuth && (strings.TrimSpace(cfg.Feishu.AppID) == "" || strings.TrimSpace(cfg.Feishu.AppSecret) == "") {
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
	// Stage 3: the Feishu adapter owns the rolling-log receipt
	// logic (Channel.Send is the display strategy). The runtime
	// just hands user messages to the adapter and lets the
	// session queue them; the gateway's pumpOutbound then routes
	// every event through the same receipt.
	responder := channelResponder{ch: ch}
	responder.userMessageFn = func(chatID, userMsgID string, blocks []agent.ContentBlock) error {
		sess, err := mgr.GetByChat(chatID)
		if err != nil {
			return fmt.Errorf("no session for chat: %w", err)
		}
		return sess.QueueUserMessage(blocks, userMsgID)
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
	fallback := func(ctx context.Context, msg *gateway.InboundMessage) error {
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
			userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}
		// Stage 3: route through responder.SendUserMessage which
		// creates the Feishu receipt (when channel is a Feishu
		// adapter) or falls back to a flat OutText send.
		return responder.SendUserMessage(ctx, msg.ChatID, userMsgID, blocks)
	}
	gw = gateway.New(fallback)
	gatewaycmd.RegisterDefaultCommands(gw, mgr, agents, responder)

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

	// Route outgoing Feishu messages through the same structured
	// logger so the CLI surface shows both halves of every
	// conversation (the inbound "received:" line is logged below
	// by the channel pump). Without this, the user sees only the
	// messages they sent to nightme and not the replies / cards /
	// reactions nightme sent back.
	if fa, ok := ch.(*feishu.Adapter); ok {
		fa.SetLogger(logger)
	}

	// Spawn per-session pumps so live agent events are rendered
	// back to the chat. We (re)attach every time the session
	// table changes by polling on each incoming message.

	// Stage 3: drive the gateway's dispatch runtime. The gateway
	// owns the per-channel inbound pump and the per-session
	// outbound pump; the Feishu adapter's Channel.Send is the
	// rolling-log display strategy. No filter needed — every
	// channel goes through the gateway.
	gwImpl := gw.(*gateway.Router)
	gwImpl.AttachChannels(ch)
	gwImpl.AttachSweeper(func() []gateway.OutboundSource {
		var out []gateway.OutboundSource
		for _, sess := range mgr.List() {
			if sess == nil || sess.ChatID == "" {
				continue
			}
			if sess.Status() != session.StatusRunning {
				continue
			}
			out = append(out, gateway.OutboundSource{
				SessionID: sess.ID,
				ChatID:    sess.ChatID,
				Events:    sess.Events(),
			})
		}
		return out
	})
	if err := gwImpl.Start(ctx); err != nil {
		return fmt.Errorf("run: start gateway: %w", err)
	}
	defer func() { _ = gwImpl.Stop(context.Background()) }()

	// Block on signal or context cancellation. All the per-channel
	// and per-session goroutines are owned by the gateway and
	// exit on ctx.Done(); we only need to wait here.
	select {
	case <-ctx.Done():
	case sig, ok := <-sigCh:
		if ok && sig != nil {
			fmt.Fprintf(out, "[nightme] received %s\n", sig)
		}
	}
	return shutdownRun(out, ch, mgr, deps.cleanup, logger)
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
	ch channel.Channel
	// userMessageFn lets the responder route user messages into
	// the session queue (F-25). nil = plain SendText fallback.
	userMessageFn func(chatID, userMsgID string, blocks []agent.ContentBlock) error
}

func (r channelResponder) Reply(ctx context.Context, chatID, text string) error {
	if r.ch == nil {
		return nil
	}
	return r.ch.Send(ctx, gateway.OutboundMessage{ChatID: chatID, Kind: gateway.OutText, Text: text})
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
	if r.ch == nil {
		return nil
	}
	// Stage 3: if the channel is a Feishu adapter, use the receipt
	// flow (adapter.SendUserMessage creates the rolling-log
	// receipt and posts the ⏳ reply). Otherwise fall back to a
	// flat OutText send.
	feishuCh, isFeishu := r.ch.(*feishu.Adapter)
	if !isFeishu {
		return r.ch.Send(ctx, gateway.OutboundMessage{
			ChatID: chatID,
			Kind:   gateway.OutText,
			Text:   feishu.BuildForwardedTextFromBlocks(blocks),
		})
	}
	receiptText := feishu.BuildForwardedTextFromBlocks(blocks)
	if _, err := feishuCh.SendUserMessage(ctx, chatID, userMsgID, receiptText); err != nil {
		// Don't fail the user's message; log and fall back.
		return r.userMessageFn(chatID, userMsgID, blocks)
	}
	if err := r.userMessageFn(chatID, userMsgID, blocks); err != nil {
		// Dispatch failed — keep the receipt in Waiting so the
		// user can /flush.
		return err
	}
	// Dispatch succeeded → mark the receipt as Executing.
	_ = feishuCh.MarkExecuting(ctx, userMsgID)
	return nil
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
