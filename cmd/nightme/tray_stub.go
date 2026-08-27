//go:build linux && !gui

// Package main — no-op tray replacement for the default Linux
// build.
//
// Build tag: `linux && !gui`. This is the DEFAULT on Linux; the
// systray-backed implementation in tray_gui.go requires an
// explicit `-tags gui`.
//
// Rationale (short version — see tray.go for the full story):
// importing github.com/getlantern/systray on Linux forces the
// linker to record libayatana-appindicator3.so.1 + libgtk-3.so.0
// as DT_NEEDED entries. On a host without the GTK3 runtime, ld.so
// then refuses to start the process at all:
//
//	error while loading shared libraries:
//	libayatana-appindicator3.so.1: cannot open shared object file
//
// Linux hosts are overwhelmingly servers with no GUI stack, so the
// default build excludes the import entirely and the daemon runs
// its runtime loop directly on the calling thread. With systray
// gone, the binary has no CGo dependency at all, which is what
// lets the release pipeline build it with CGO_ENABLED=0 as a fully
// static executable.
//
// Nothing user-facing is lost except the tray icon itself. The
// daemon is controlled over the daemoncontrol Unix socket, so
// `nightme start` / `stop` / `restart` / `status` / `logs` and the
// REPL all behave identically. Users who want the tray install the
// `-gui` release artifact.

package main

import (
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/runtime"
)

// runTrayOwning runs the daemon runtime loop on the calling
// thread. Signature matches the systray-backed implementation in
// tray_gui.go so daemon_lifecycle_unix.go needs no build-tag
// awareness.
//
// This mirrors the headless short-circuit that the GUI build takes
// when isHeadless() reports true (tray_gui.go) — the difference is
// that here the decision is made at compile time, which is the
// only place it CAN be made when the failure mode is a missing
// shared library.
//
// opts is accepted but only its logger is used: there is no menu
// to wire, so reg / onStopRequest / onRestartRequest have no
// consumer. They stay in the signature because
// daemon_lifecycle_unix.go populates them for the `-tags gui`
// build from the same call site.
func runTrayOwning(cmd *cobra.Command, runDeps runtime.Deps, opts trayOptions) error {
	if opts.logger != nil {
		opts.logger.Info("system tray not compiled in (built without -tags gui); running runtime loop directly",
			"hint", "install the linux -gui release artifact for a tray icon")
	}
	return runRunWith(cmd, runDeps)
}
