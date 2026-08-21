//go:build unix

// Regression coverage for the tray Restart click handler. The
// daemon's onRestartRequestDefault spawns a detached child to
// perform the stop+start protocol via the daemon IPC socket;
// this test pins the spawn shape so a future refactor that
// regresses to a bare `_daemon` argv (which runDaemonChild
// rejects) is caught at unit-test time rather than at the
// user's desk when the tray click silently degrades to a
// plain stop.
package main

import (
	"context"
	"testing"
)

func TestBuildRestartCmd_ArgvIsRestart(t *testing.T) {
	cmd := buildRestartCmd(context.Background(), "/fake/path/nightme")
	if got := len(cmd.Args); got < 2 {
		t.Fatalf("argv len = %d, want >= 2: %v", got, cmd.Args)
	}
	if got := cmd.Args[1]; got != "restart" {
		t.Fatalf("argv[1] = %q, want \"restart\" — a bare `_daemon` here "+
			"silently fails (runDaemonChild requires the fd-inheritance env vars "+
			"that only startDaemon sets up)", got)
	}
}