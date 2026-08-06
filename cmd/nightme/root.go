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
	root.SetVersionTemplate(version.String() + "\n")
	root.Version = version.Version
	root.AddCommand(newTestCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newAgentsCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newStartCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newRestartCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newDebugCmd())
	return root
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
