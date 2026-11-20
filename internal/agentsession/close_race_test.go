package agentsession

import (
	"sync"
	"testing"
)

// TestClose_ConcurrentCallersDoNotPanic pins the readpumpStop
// close: three independent callers can reach Close() on the same
// session — a user /close, the daemon's idle-CLI reaper, and the
// daemon shutdown. The stop channel must be closed exactly once;
// a double close panics and takes the whole daemon down.
//
// The old check-then-close ran outside asMu, so two closers could
// both observe the channel open and both close it. The loop makes
// the window likely to be hit.
func TestClose_ConcurrentCallersDoNotPanic(t *testing.T) {
	const iterations = 50
	const callers = 8

	for range iterations {
		as := newAgentSessionRuntime("as_test", "cs_x", "claude", "/tmp", nil)
		as.SetHandle(newFakeAgentSession(4242).buildLive())
		as.startReadPump()

		release := make(chan struct{})
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-release
				_ = as.Close()
			}()
		}
		close(release)
		wg.Wait()

		as.Shutdown()
	}
}
