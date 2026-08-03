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
	newGateway     func(gateway.Gateway, *agent.Registry, gatewaycmd.Responder)
	signals        <-chan os.Signal
	cleanup        bool
	skipFeishuAuth bool
	onReady        func()
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
		newGateway: func(gw gateway.Gateway, agents *agent.Registry, resp gatewaycmd.Responder) {
			gatewaycmd.RegisterDefaultCommands(gw, agents, resp)
		},
	}
}

// newRuntimeTestCmd exposes the in-process daemon runtime to unit tests.
func newRuntimeTestCmd() *cobra.Command {
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

	// v1.1 (commit 3 + 4): the Gateway owns the chat → session
	// binding table and the per-userMessage receipt FSM. We
	// construct it now so the slash command handlers can use it.
	responder := channelResponder{ch: ch}

	gw := gateway.New(nil, mgr)
	if gw == nil {
		return errors.New("run: gateway is nil")
	}

	// Register the session-manager-backed helpers the slash
	// commands need (create detached record, kill by ID). The
	// runtime provides these via the gateway's gatewaycmd
	// package so handlers don't have to depend on session
	// directly.
	gatewaycmd.RegisterSessionOps(
		func(ctx context.Context, workspace, agentName string, args []string) (*session.Session, error) {
			return mgr.Register(ctx, session.CreateRequest{
				Workspace: workspace, Agent: agentName, Args: args,
			})
		},
		func(sid string) error { return mgr.Kill(sid) },
	)

	deps.newGateway(gw, agents, responder)

	// Wrap the gateway's Handle() so the fallback we pass in
	// (closing over coordinator) sees the same logic. We replace
	// the default fallback with one that forwards to the live session.
	//
	// F-25 integration: the fallback routes through the
	// channelResponder.SendUserMessage path, which (when the renderer
	// v1.1 (commits 3 + 4): the fallback drives the receipt FSM:
//  1. Look up the session via gateway.LookupSessionByChat.
//  2. Build the receipt via gateway.CreateReceipt.
//  3. Queue to session.InputBuffer (decides dispatch vs buffer).
//  4. UpdateReceipt(Executing) on immediate dispatch (Idle path).
//     On the Busy path the InputBuffer.onFlush hook (installed
//     below) flips queued receipts to Executing when the buffer
//     actually flushes.
	fallback := func(ctx context.Context, msg *gateway.InboundMessage) error {
		sess, err := gw.LookupSessionByChat(msg.ChatID)
		if err != nil {
			return responder.Reply(ctx, msg.ChatID, "", "no workspace set, send /cwd <path> first")
		}
		if sess.Status() != session.StatusRunning {
			return responder.Reply(ctx, msg.ChatID, "", "CLI not running, send /run <agent> to start")
		}

		blocks := feishu.BuildBlocks(msg.Text, msg.Attachments)
		userMsgID := msg.MessageID
		if userMsgID == "" {
			userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}

		if _, err := gw.CreateReceipt(ctx, msg.ChatID, userMsgID, blocks); err != nil {
			// Receipt creation failed (channel offline?) — fall
			// back to a plain OutText send so the message isn't
			// silently dropped.
			return responder.Reply(ctx, msg.ChatID, userMsgID, msg.Text)
		}

		// Queue to session.InputBuffer. The buffer decides
		// dispatch (Idle) vs buffer (Busy).
		//
		// For Busy (buffered), the receipt stays Pending. The
		// InputBuffer's onFlush hook (installed below by the
		// runtime via session.InputBuffer.OnFlush) flips queued
		// receipts to Executing when the agent finishes the
		// current turn.
		if err := sess.QueueUserMessage(blocks, userMsgID); err != nil {
			_ = gw.DisposeReceipt(ctx, userMsgID)
			return err
		}
		return nil
	}

	// Rebuild the gateway with the fallback closure.
	gw = gateway.New(fallback, mgr)
	deps.newGateway(gw, agents, responder)
	mgr.SetEventCallback(gw.OnSessionEvent)

	// Install the InputBuffer.onFlush hook so queued receipts flip
	// to Executing when the buffer actually flushes. This is the
	// F-25 integration: the session knows nothing about receipts
	// (it's a pure process domain object); the gateway installs
	// the hook via a small wrapper.
	//
	// (We rebuild gwImpl here after the second gateway.New so the
	// type-assertion below matches the freshly-built instance.)
	_ = gw

	if err := mgr.Restore(ctx); err != nil {
		return fmt.Errorf("run: restore sessions: %w", err)
	}

	if err := ch.Start(ctx); err != nil {
		logger.Error("channel disconnected", "reason", err)
		return fmt.Errorf("run: start Feishu channel: %w", err)
	}
	logger.Info("channel connected", "channel", "feishu")
	fmt.Fprintln(out, "Feishu WebSocket connected")

	if fa, ok := ch.(*feishu.Adapter); ok {
		fa.SetLogger(logger)
	}

	gwImpl := gw.(*gateway.Router)
	gwImpl.AttachChannels(ch)
	if err := gwImpl.Start(ctx); err != nil {
		return fmt.Errorf("run: start gateway: %w", err)
	}
	defer func() { _ = gwImpl.Stop(context.Background()) }()

	if deps.onReady != nil {
		deps.onReady()
	}

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

func (r channelResponder) Reply(ctx context.Context, chatID, userMsgID, text string) error {
	if r.ch == nil {
		return nil
	}
	// Slash-command / runtime-error replies use OutCommandReply
	// so the Feishu adapter sends a plain text message instead
	// of routing through the receipt rolling-log card. No
	// ReplyTo, no in-place update, no receipt creation. See
	// internal/gateway/messages.go for the kind definition.
	//
	// The userMsgID arg threads the user message we couldn't
	// reach (e.g. the receipt was never created on the
	// "no workspace set" / "CLI not running" fallbacks). The
	// Responder interface carries it for parity with the
	// successful CreateReceipt path; OutCommandReply itself
	// doesn't thread it.
	_ = userMsgID
	return r.ch.Send(ctx, gateway.OutboundMessage{ChatID: chatID, Kind: gateway.OutCommandReply, Text: text})
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
			logger.Info("session killed", "session_id", sess.ID, "pid", sess.PID)
		}
	}
	return firstErr
}
