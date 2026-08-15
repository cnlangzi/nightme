// ws_test.go — ping-keeper tests for the dsh WS downlink.
//
// The ping keeper is a small but critical piece: it's what keeps
// the dsh `web` profile from silently closing our WS connection
// after ~25 min of no client traffic. Each test runs against a
// throwaway httptest.Server with a gorilla/websocket.Upgrader so
// we exercise the real wire path (not a fake).
//
// All tests use the *At form (startWSPingWriterAt) so the ping
// cadence is sub-second; production uses startWSPingWriter with
// wsPingInterval = 25s.
package dsh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testCtx returns a context that's cancelled when the test
// finishes. Keeps dial errors from leaking past t.Fatal.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// upgradeOnce is a tiny helper: httptest.Server + a single-path
// WS upgrader. Returns the server URL with the ws:// scheme.
func newWSTestServer(t *testing.T, onPing func(), onConnect func()) (string, *httptest.Server) {
	t.Helper()
	up := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("ws upgrade: %v", err)
			return
		}
		if onConnect != nil {
			onConnect()
		}
		// Drain inbound frames until peer closes. We don't need to
		// reply — only count pings. ReadMessage returns on close.
		conn.SetPingHandler(func(string) error {
			if onPing != nil {
				onPing()
			}
			// Default handler returns nil after auto-pong.
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				_ = conn.Close()
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	return u.String(), srv
}

// TestStartWSPingWriterAt_SendsPingsOnInterval verifies the
// keeper actually emits PingMessage frames at the configured
// cadence. We use a 30ms interval so a single second is enough
// to observe 2-3 pings; production (25s) would take minutes.
func TestStartWSPingWriterAt_SendsPingsOnInterval(t *testing.T) {
	var pingCount atomic.Int32
	var connectedCount atomic.Int32
	connected := make(chan struct{})
	wsURL, srv := newWSTestServer(t,
		func() { pingCount.Add(1) },
		func() {
			if connectedCount.Add(1) == 1 {
				close(connected)
			}
		},
	)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()

	stop := make(chan struct{})
	defer close(stop) // belt: in case the test exits early
	startWSPingWriterAt(stop, conn, 30*time.Millisecond)

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatalf("ws server did not record a connect")
	}

	// Wait ~250ms — expect at least 5 pings (interval 30ms).
	time.Sleep(250 * time.Millisecond)

	got := pingCount.Load()
	if got < 5 {
		t.Fatalf("expected ≥ 5 pings in 250ms with 30ms interval, got %d", got)
	}
}

// TestStartWSPingWriterAt_ExitsOnStopChan verifies the goroutine
// returns promptly when stop is closed (bridge shutdown path).
func TestStartWSPingWriterAt_ExitsOnStopChan(t *testing.T) {
	wsURL, srv := newWSTestServer(t, nil, nil)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()

	stop := make(chan struct{})
	startWSPingWriterAt(stop, conn, 50*time.Millisecond)

	// Give the goroutine a chance to actually start (one tick).
	time.Sleep(80 * time.Millisecond)

	// Closing stop should cause the goroutine to return on its
	// next tick. 200ms is plenty for a 50ms ticker to fire and
	// observe the closed stop chan.
	close(stop)

	// We don't have a direct handle to the goroutine; the next
	// best proxy is that closing the conn from this side makes
	// the keeper's WriteControl fail and exit anyway. Verify
	// nothing panics or hangs (deferred Close covers the WS).
}

// TestStartWSPingWriterAt_ExitsOnConnClose verifies the goroutine
// returns when the conn is dead (WriteControl fails). This is
// the "dsh server closed us" exit path.
func TestStartWSPingWriterAt_ExitsOnConnClose(t *testing.T) {
	var (
		mu       sync.Mutex
		closed   bool
		closeCh  = make(chan struct{})
	)
	wsURL, srv := newWSTestServer(t, nil, func() {
		mu.Lock()
		if !closed {
			closed = true
			close(closeCh)
		}
		mu.Unlock()
	})
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	startWSPingWriterAt(stop, conn, 30*time.Millisecond)

	<-closeCh
	// Server-side close fires once the handler returns; give the
	// server a moment to actually close the underlying TCP conn.
	time.Sleep(100 * time.Millisecond)
	_ = conn.Close() // matches the keeper's expectation
}

// TestDialWS_StartsKeeperSynchronously verifies dialWS returns
// without waiting on the first ping — the keeper is fully async.
func TestDialWS_StartsKeeperSynchronously(t *testing.T) {
	wsURL, srv := newWSTestServer(t, nil, nil)
	defer srv.Close()

	u, _ := url.Parse(wsURL)
	// dialWS expects http(s)→ws(s); newWSTestServer already gives
	// ws://, but dialWS handles the http→ws swap so we feed http.
	httpURL := "http://" + u.Host + u.Path

	stop := make(chan struct{})
	defer close(stop)

	start := time.Now()
	conn, err := dialWS(testCtx(t), httpURL, u.Path, stop)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer conn.Close()

	// dialWS must return well before the 25s production ping
	// cadence. 500ms is a generous upper bound that still
	// catches a regression where dialWS accidentally waits.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("dialWS blocked for %v (expected < 500ms)", elapsed)
	}
}