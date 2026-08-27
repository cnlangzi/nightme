//go:build darwin && notray

// Package main — no-op tray replacement for the cross-compiled
// darwin default binary.
//
// Build tag: `darwin && notray`. Compiled in only when the
// release workflow builds darwin/amd64 with `-tags notray`
// (CGO_ENABLED=0). See tray.go for the full rationale, but in
// short: getlantern/systray has no non-CGo fallback, and
// macos-latest (Apple Silicon) cannot cross-compile CGo to
// darwin/amd64 — Go silently disables CGo on cross-compile, and
// the systray import then fails with "undefined: nativeLoop,
// registerSystray, …". Building with CGO_ENABLED=0 + `notray`
// drops the systray import entirely, the binary links cleanly,
// and the daemon is still fully controllable over the
// daemoncontrol Unix socket — the only loss is the menu-bar
// tray icon.
//
// Symmetric with tray_stub.go (Linux `linux && !gui`). The
// `_daemon` lifecycle, REPL, and IPC are unaffected; the only
// behavioural delta vs the tray-enabled default is "no menu-bar
// icon".
//
// Signature matches tray_gui.go's runTrayOwning so
// daemon_lifecycle_unix.go needs no build-tag awareness.

package main

import (
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/runtime"
)

func runTrayOwning(cmd *cobra.Command, runDeps runtime.Deps, opts trayOptions) error {
	if opts.logger != nil {
		opts.logger.Info("system tray not compiled in (cross-compiled darwin binary, -tags notray); running runtime loop directly",
			"hint", "use the darwin/arm64 native artifact for the menu-bar tray icon")
	}
	return runRunWith(cmd, runDeps)
}
