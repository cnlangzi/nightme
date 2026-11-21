// Package main is the nightme daemon entrypoint.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
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
	// cobra.go — empty string disables the message).
	cobra.MousetrapHelpText = ""
}

func main() {
	cfg, loadErr := loadConfigOrDefaults(config.DefaultPath())
	if loadErr != nil && !isDaemonChild(os.Args) {
		// A config file that does not parse is what `nightme doctor`
		// exists to report: the process has to reach the command. The
		// daemon child keeps its stderr quiet — that stream is the
		// crash capture.
		fmt.Fprintln(os.Stderr, "Warning:", loadErr)
	}
	// The agent CLIs nightme spawns keep their client telemetry off
	// unless the config says otherwise. See internal/bridge/telemetry.
	telemetry.Allow(cfg.AgentTelemetry)
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

// loadConfigOrDefaults loads the config at path. A file that cannot be
// parsed is not the end of the road: the shipped defaults stand in and
// the error that made them necessary comes back with them. The
// commands that need a valid config load it themselves and fail with
// the parse error; the ones that only look at it — `nightme doctor`
// above all — report it and carry on.
//
// The config is loaded, not loaded-and-defaulted: LoadDefault probes
// the built-in agents and writes the one it finds into the user's
// config file, and a diagnostic must not change the thing it
// diagnoses. The commands that need a primary agent (the daemon start,
// the daemon child, `nightme run`, `nightme login`) load the config
// themselves and persist the agent they picked.
func loadConfigOrDefaults(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	// Load("") applies the shipped defaults and reads no file, so it
	// cannot fail.
	fallback, _ := config.Load("")
	return fallback, err
}
