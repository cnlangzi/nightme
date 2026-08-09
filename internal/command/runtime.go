package command

import (
	"log/slog"
	"time"
)

// RuntimeServices aggregates the dependencies a slash command
// receives at Handle() time. The runtime (cmd/nightme/run.go)
// builds this once at startup; the Commander passes it to every
// dispatched Handle() call.
//
// Commands that need per-chat state hold *chatsession.Manager
// directly in their Factory. The remaining fields are shared
// interfaces with multiple implementations or cross-cutting concerns.
type RuntimeServices struct {
	// Config provides cross-command read-only configuration.
	// Currently exposes only Primary (default agent name).
	Config Config

	// Logger is the structured logger used for diagnostic output.
	// May be nil; commands should fall back to slog.Default() in
	// that case.
	Logger *slog.Logger

	// Clock returns the current time. May be nil; commands
	// fall back to time.Now in that case. Test code overrides
	// for deterministic timestamps.
	Clock func() time.Time
}

// Config is the read-only configuration slice RuntimeServices
// exposes to commands. Currently just Primary; grows as commands
// need more shared config.
type Config struct {
	// Primary is the default agent name (cmdline
	// `nightme --primary` or cfg.Primary). Previously each
	// Factory received this directly as `defaultPrimary`; now
	// it lives in rt.Config.Primary and Factories no longer
	// carry the field.
	Primary string
}