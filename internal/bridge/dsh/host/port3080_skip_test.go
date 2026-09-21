package host_test

import (
	"net"
	"testing"
	"time"
)

// requireExclusivePort3080 skips the test when something is already
// listening on 127.0.0.1:3080. Host tests that spawn or attach on
// the pinned dsh port assume exclusive control over it; running
// them while a live dsh holds 3080 (e.g. from inside the dsh web
// GUI) makes attach succeed instead of spawn, and the spawn-fallback
// path would reclaim (kill) the live dsh. Skip rather than fail or
// kill.
func requireExclusivePort3080(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:3080", 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Skipf("port 3080 is already held (live dsh?); test requires exclusive 3080 control")
	}
}
