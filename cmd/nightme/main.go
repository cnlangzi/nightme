// Package main is the nightme daemon entrypoint.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/logging"
)

func init() {
	// Disable Cobra's Windows mousetrap. By default Cobra checks
	// whether the parent process is explorer.exe (mousetrap.StartedByExplorer)
	// on every root.Execute() and, if so, prints
	//   "This is a command line tool.
	//    You need to open cmd.exe and run it from there."
	// then sleeps 5s and os.Exit(1).
	//
	// nightme's bare `nightme.exe` invocation enters the REPL
	// (cmd/nightme/repl.go) — that path never calls root.Execute(),
	// so a freshly double-clicked binary shows the banner fine. But
	// the first command dispatched from inside the REPL (`login
	// feishu`, `start`, `status`, …) goes through root.Execute(),
	// which fires the mousetrap because the parent is still
	// explorer.exe. Result: the REPL appears to work, then any
	// command silently exits 5s later — confusing.
	//
	// The mousetrap is designed for CLIs that have no interactive
	// mode and want to nudge the user toward cmd.exe. nightme's
	// REPL is the intended interaction surface for explorer-launched
	// binaries, so the nudge is counter-productive here. Clearing
	// MousetrapHelpText to "" turns the check off entirely (see
	// cobra.go:68-75 — empty string disables the message).
	cobra.MousetrapHelpText = ""
}

func main() {
	cfg, err := config.LoadDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	// The forked daemon child has no console: its stdout is
	// /dev/null and its stderr is the crash-capture file
	// (daemon_stderr.go). Teeing the log stream there would bury
	// the panic stack that file exists for under a full duplicate
	// of nightme.log, so the child logs to the file only.
	newLogger := logging.New
	if isDaemonChild(os.Args) {
		newLogger = logging.NewQuiet
	}
	var logger *slog.Logger
	if l, err := newLogger(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	} else {
		logger = l
	}
	// F-46 debug: install logger as slog.Default so all
	// downstream code (handlers_gtw.go, gateway.go, action.go,
	// chatsession.go) that calls slog.Default().Warn(...) lands
	// in the same MultiWriter sink as the plumbed logger. Without
	// this, our F-46 debug logs are silently dropped because
	// slog.Default() returns Go's no-op default logger.
	slog.SetDefault(logger)
	defer func() { _ = logging.Close(logger) }()
	Execute(logger)
}
