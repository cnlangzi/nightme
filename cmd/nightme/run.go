// Package main — `nightme run` daemon command.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/config"
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
	signals      <-chan os.Signal
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
	}
}

// newRunCmd builds the long-running Feishu daemon command.
func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Start the Feishu daemon",
		Long: "run starts the Feishu WebSocket channel and restores persisted " +
			"sessions. M2 PR #4 only verifies incoming messages; Gateway routing " +
			"and agent round-trip handling land in PR #5.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd)
		},
	}
}

// runRun is the cobra entrypoint. The split makes the daemon easy to drive
// with fake channels and managers in unit tests.
func runRun(cmd *cobra.Command) error {
	return runRunWith(cmd, defaultRunDeps())
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

	sigCh := deps.signals
	if sigCh == nil {
		owned := make(chan os.Signal, 2)
		signal.Notify(owned, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(owned)
		sigCh = owned
	}
	return runDaemon(cmd.Context(), cmd.OutOrStdout(), deps, sigCh)
}

// runDaemon wires the channel and session manager, then waits for an incoming
// message or a shutdown signal. It intentionally does not route messages to a
// Gateway yet: PR #5 owns that integration.
func runDaemon(ctx context.Context, out io.Writer, deps runDeps, sigCh <-chan os.Signal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}

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
	mgr := deps.newManager(agents, reg, nil)
	if mgr == nil {
		return errors.New("run: session manager is nil")
	}
	if err := mgr.Restore(ctx); err != nil {
		return fmt.Errorf("run: restore sessions: %w", err)
	}

	ch, err := deps.newChannel(cfg)
	if err != nil {
		return fmt.Errorf("run: create Feishu channel: %w", err)
	}
	if ch == nil {
		return errors.New("run: Feishu channel is nil")
	}
	if err := ch.Start(ctx); err != nil {
		return fmt.Errorf("run: start Feishu channel: %w", err)
	}
	fmt.Fprintln(out, "Feishu WebSocket connected")

	incoming := ch.Incoming()
	if incoming == nil {
		return shutdownRun(out, ch, mgr)
	}
	for {
		select {
		case <-ctx.Done():
			return shutdownRun(out, ch, mgr)
		case sig, ok := <-sigCh:
			if ok && sig != nil {
				fmt.Fprintf(out, "[nightme] received %s\n", sig)
			}
			return shutdownRun(out, ch, mgr)
		case msg, ok := <-incoming:
			if !ok {
				return shutdownRun(out, ch, mgr)
			}
			fmt.Fprintf(out, "received: %s\n", msg.Text)
		}
	}
}

// shutdownRun stops the channel first, then marks every known session
// detached. It never calls Manager.Kill: the v0.1 policy leaves CLI children
// alive across a daemon restart.
func shutdownRun(out io.Writer, ch channel.Channel, mgr session.Manager) error {
	_ = out // retained in the signature for a future shutdown status line
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

	markErr := markSessionsDetached(mgr)
	if markErr != nil && firstErr == nil {
		firstErr = fmt.Errorf("run: detach sessions: %w", markErr)
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
