// Package main is the nightme daemon entrypoint.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	// Register channel Builders via init() so the channel.Registry
	// sees every available channel. Drop the blank assignment if
	// the test build needs to disable one; production runtime
	// always uses both.
	_ "github.com/cnlangzi/nightme/internal/channel/feishu"
	_ "github.com/cnlangzi/nightme/internal/channel/telegram"

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

// newRootCmd builds the cobra command tree AND the REPL banner list
// in one pass via cmdRegistry. Every subcommand that should appear in
// the REPL's "Common:" section must go through reg.add(); the cobra
// tree and the banner can no longer drift apart.
//
// Returns (root, reg). The reg is consumed by the REPL banner
// renderer (buildBanner in repl.go); pass it through if you build a
// different entry point that needs the banner.
func newRootCmd() (*cobra.Command, *cmdRegistry) {
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

	reg := newCmdRegistry(root)

	// User-facing commands. Banner entries are aligned to column 16
	// (cmd-use + spaces + one-line description). Adding a new
	// subcommand is exactly one reg.add() call — the cobra tree,
	// the REPL banner, and the tray Commands submenu stay in
	// lockstep.
	//
	// addNoTray entries below are the commands that, if triggered
	// from a tray menu click, would either block the tray event
	// loop (anything that needs a TTY or stdin) or duplicate an
	// already-on-the-tray item. The barrier for "is this safe
	// from a tray click" is: "does this command exit within
	// seconds without user input and without claiming a TTY?".
	// If yes, reg.add; if no, reg.addNoTray.
	reg.addNoTray(newTestCmd(),   "test ...        spawn CLI in PTY (Ctrl-C to end)")
	reg.add(newListCmd(),         "list            list sessions")
	reg.add(newKillCmd(),         "kill            terminate agent processes")
	reg.add(newAgentsCmd(),       "agents          list registered agents")
	reg.addNoTray(newLoginCmd(),  "login feishu    QR Feishu registration")
	reg.addNoTray(newLogsCmd(),   "logs [--lines N] tail daemon log (Ctrl-C to exit)")
	reg.add(newCleanCmd(),        "clean           truncate logs + remove attachments")
	reg.add(newConfigCmd(), "config          interactive configuration (name, agents)")
	reg.add(newVersionCmd(),      "version         version info")
	reg.add(newUpdateCmd(), "update          check for a newer release")
	reg.addNoTray(newDebugCmd(),  "debug           exercise reaction/action flow")

	addLifecycleCommands(reg)
	addUnixOnlyCommands(reg)
	return root, reg
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
//
// All four lifecycle commands go through addNoTray: the tray has
// its own Restart / Stop / Status items that operate on the
// running daemon via direct signal/IPC, and exposing the same
// verbs as "Commands" submenu entries would invite a double-click
// race (tray Restart + Commands/restart at the same time). status
// is also on the tray as a non-clickable info row.
func addLifecycleCommands(reg *cmdRegistry) {
	reg.addNoTray(newStartCmd(),   "start           start daemon in the background")
	reg.addNoTray(newStatusCmd(),  "status          show daemon status")
	reg.addNoTray(newStopCmd(),    "stop            gracefully stop daemon")
	reg.addNoTray(newRestartCmd(), "restart         gracefully replace daemon")
	// _daemon is the forked child process entry point — internal,
	// not user-facing. Registered in the cobra tree so the binary
	// can dispatch into it, but hidden from help and the REPL banner.
	// reg is passed in so the daemon child can wire its systray
	// "Commands" submenu off the same registry the REPL banner
	// renders from — one source of truth for both surfaces.
	reg.addHidden(newDaemonCmd(reg))
}

func Execute(logger *slog.Logger) {
	root, reg := newRootCmd()
	// Bare invocation (no args) routes into the REPL instead of
	// silently exiting. Anything else flows through the existing
	// cobra path unchanged.
	if len(os.Args) == 1 {
		root.SetContext(withLogger(context.Background(), logger))
		Recover(root, logger)
		if err := runREPL(root, reg, logger); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(nmerrors.ExitCode(err))
		}
		return
	}

	root.SetContext(withLogger(context.Background(), logger))
	Recover(root, logger)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(nmerrors.ExitCode(err))
	}
}