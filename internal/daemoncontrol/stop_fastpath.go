// Cross-platform helper for the "stop" RPC handler's starting-state
// fast-path. Kept in its own file (no build tags) so the unix and
// windows servers share one definition — drift between the two
// caused the original "daemon did not stop within 15s" bug when
// unix-only nuance (no implicit send-buffer flush on os.Exit) was
// not mirrored to the windows path. Any future change to the
// fast-path must happen here and apply to both platforms.
package daemoncontrol

import (
	"io"
	"log"
	"os"
)

// osExit is overridable so tests can intercept the process exit
// triggered by the starting-state fast-path. In production it is
// os.Exit. The fast-path fires when runDaemon has not yet entered
// its wait select — cancel() would be unconsumed, so we exit hard
// to release the daemon flock for stopDaemon's TryLock poll. Tests
// override this var to verify the path without killing the test
// runner.
var osExit = os.Exit

// stopLogf is the diagnostic logger for the fast-path. Tests may
// override it to silence the daemon's stderr noise during in-process
// exercises of the path. Default writes to stderr via the stdlib
// log package; the daemon captures stderr into daemon-stderr.log
// via openDaemonStderrOrDevNull, so the messages are available
// post-mortem when the operator investigates a hung restart.
var stopLogf = log.Printf

// writeCloseCloser is the subset satisfied by both *net.UnixConn
// (unix) and *pipeInstance (windows). Using it lets this file stay
// cross-platform without build tags.
type writeCloseCloser interface {
	io.Writer
	io.Closer
}

// startingStateStopAck writes a stop acknowledgment to conn, then
// closes it, then calls osExit(0). It is the fast-path taken by the
// "stop" RPC handler when the daemon's runtime hasn't reached its
// wait select yet (state=="starting"); in that window cancel()
// would be unconsumed and the daemon would hang until ch.Start
// returned — past stopDaemon's 15s budget.
//
// Errors from WriteResult and Close are logged but not propagated:
// we are exiting immediately, and the worst case (write or close
// fails) is "client sees EOF", which stopDaemon already handles
// by polling TryLock(DaemonLock). Logging gives the operator a
// breadcrumb in daemon-stderr.log if a restart keeps timing out
// in the same place.
//
// CRITICAL: explicit conn.Close() before osExit. Unix domain
// sockets do NOT flush their send buffer on process exit the way
// TCP does — without this close, the client's ReadResponse sees
// EOF (response lost) and stopDaemon aborts before it ever polls
// DaemonLock. Verified with a direct socket-flush test (see
// /tmp/restart_repro/socktest).
func startingStateStopAck(conn writeCloseCloser) {
	if err := WriteResult(conn, struct{}{}); err != nil {
		stopLogf("daemoncontrol: stop fast-path: write response failed: %v", err)
	}
	if err := conn.Close(); err != nil {
		stopLogf("daemoncontrol: stop fast-path: close conn failed: %v", err)
	}
	osExit(0)
}
