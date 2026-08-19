// Package tray contains the platform-agnostic glue that wires the
// nightme daemon to a system-tray icon. The actual native event
// loop lives behind a build-tagged adapter (see
// cmd/nightme/tray.go); the pieces in this package are pure Go
// and unit-testable without a GUI.
//
// The two responsibilities in this file are:
//
//  1. Re-dispatching a registered cobra command from a tray menu
//     click — the equivalent of typing its name on the command
//     line, but synchronous, with output silenced (see Invoke).
//  2. (Reserved for future openrepl.) Spawning a fresh terminal
//     window that runs the REPL when the user clicks "Open REPL".
//
// Both are kept out of cmd/nightme so the click handling in the
// daemon child can be tested without spinning up a real tray.
package tray

import (
	"io"

	"github.com/spf13/cobra"
)

// Invoke re-dispatches a single cobra command synchronously, as if
// the user had typed its name on the command line. Returns the
// command's own error (or nil).
//
// Why this exists: cmdRegistry's TrayItems() carries a pointer to
// each registered *cobra.Command, and the tray menu's click handler
// needs a one-liner to "run that command now". The handler must
// not call root.Execute() because (a) root's args + flag state are
// global and not safe across the tray event loop goroutine and the
// daemon's main goroutine, and (b) the tray has no use for flag
// parsing — menu items are clickable verbs, not flag vectors.
//
// Going through cmd.RunE directly satisfies both: no global state
// mutates, and the click fires the same RunE the user would have
// hit on the command line or in the REPL.
//
// Output handling: stdout/stderr on the *cobra.Command are
// temporarily redirected to io.Discard, then restored. The tray
// has nowhere to display command output, and a long-running
// command that writes to stdout would corrupt native menu state
// on some platforms (Cocoa's NSMenu in particular). If a future
// feature wants the output back, it should plumb a per-call
// writer through this function — keeping io.Discard as the
// default is the conservative choice.
func Invoke(cmd *cobra.Command) error {
	if cmd == nil {
		return nil
	}
	if cmd.RunE == nil {
		// Leaf commands without a RunE (e.g. _daemon) are not
		// invokable from the tray anyway — the registry filters
		// them out via Hidden — but guard against future drift
		// where a subcommand registers a PreRun but no RunE.
		return nil
	}
	origOut, origErr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	defer func() {
		cmd.SetOut(origOut)
		cmd.SetErr(origErr)
	}()
	return cmd.RunE(cmd, nil)
}
