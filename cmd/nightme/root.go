// Package main is the nightme daemon entrypoint.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/version"
)

type loggerContextKey struct{}

func withLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

func loggerFromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok && logger != nil {
			return logger
		}
	}
	return slog.Default()
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use: "nightme", Short: "Bridge AI Coding CLIs to IM channels",
		Long: "nightme is a single-process daemon that bridges AI Coding\n" +
			"CLIs (Claude Code / Codex / OpenCode) to IM channels (Feishu /\n" +
			"WhatsApp / Web UI). v0.1 ships the Local Bridge test mode —\n" +
			"use `nightme test` to spawn an agent in a PTY and `nightme list`\n" +
			"to inspect persisted sessions. See docs/SPEC.md.",
		SilenceUsage: true, SilenceErrors: true,
	}
	root.SetVersionTemplate(bannerWithVersion() + "\n")
	root.Version = version.Version
	root.AddCommand(newTestCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newKillCmd())
	root.AddCommand(newAgentsCmd())
	root.AddCommand(newLoginCmd())
	root.AddCommand(newNameCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newDebugCmd())
	addLifecycleCommands(root)
	addUnixOnlyCommands(root)
	return root
}

// addLifecycleCommands registers the cross-platform daemon
// lifecycle commands: start / stop / restart / status / _daemon.
// All five are defined in daemon_lifecycle.go (no build tag);
// their platform-specific behaviour (fork + fd inheritance vs
// CreateProcess + LockFileEx; AF_UNIX socket vs named pipe) lives
// in daemon_lifecycle_unix.go / daemon_lifecycle_windows.go and
// the matching files in internal/daemoncontrol.
//
// Kept here in root.go so a single edit point covers both Unix
// and Windows; root_unix.go and root_windows.go only differ in
// what they add on top (Unix: doctor; Windows: nothing extra).
func addLifecycleCommands(root *cobra.Command) {
	root.AddCommand(newStartCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newRestartCmd())
	root.AddCommand(newDaemonCmd())
}

func Execute(logger *slog.Logger) {
	root := newRootCmd()
	// Bare invocation (no args) routes into the REPL instead of
	// silently exiting. Anything else flows through the existing
	// cobra path unchanged.
	if len(os.Args) == 1 {
		root.SetContext(withLogger(context.Background(), logger))
		Recover(root, logger)
		if err := runREPL(root, logger); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(nmerrors.ExitCode(err))
		}
		return
	}

	root.SetContext(withLogger(context.Background(), logger))
	Recover(root, logger)
	if logger != nil {
		logger.Info("command started", "args", os.Args[1:])
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(nmerrors.ExitCode(err))
	}
}
