// Package main — `nightme run` long-running Feishu daemon (CLI shell).
//
// The wiring, event-bus subscription, restore, shutdown, and the
// 7-step runtime orchestrator all live in internal/runtime. This
// file is a thin cobra adapter:
//
//   - parse --channel
//   - build runtime.Deps
//   - install signal handling via runtime.RunOptions
//   - call runtime.Runner.Run(ctx)
//
// The cmd/nightme tests that touch the runtime internals
// directly (newEventHandler, wireRuntimeCallbacksAndRestore,
// markPromptDone, shutdownRun) import internal/runtime as
// `runtime` and call the exported equivalents there.

package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/runtime"
)

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
	deps := runtime.DefaultDeps()
	deps, err := runtime.WithChannel(deps, channelName)
	if err != nil {
		return err
	}
	return runRunWith(cmd, deps)
}

// runRunWith is the CLI shell's bridge into runtime.Runner.Run.
// It installs signal handling (delegated to runtime.RunOptions)
// and routes cmd's stdout to the runtime's Out writer.
func runRunWith(cmd *cobra.Command, deps runtime.Deps) error {
	if cmd == nil {
		return errCmdRequired
	}
	out := cmd.OutOrStdout()
	logger := loggerFromContext(cmd.Context())

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	runner := runtime.RunWith(deps, runtime.RunOptions{
		Out:    out,
		Logger: logger,
		SigCh:  sigCh,
	})
	return runner.Run(cmd.Context())
}

// errCmdRequired is returned when runRunWith is called without
// a cobra command (production callers always pass one).
var errCmdRequired = errors.New("run: command is required")