// ws.go — WebSocket downlink client for `dsh --profile web`.
//
// Two independent WebSocket connections per bridge session:
//   - /api/events.mux   (serverFrame{MuxFrame…}) — primary event stream
//   - /api/events.host  (serverFrame{HostFrame…}) — lifecycle events
//
// Both are downlink-only: the server pushes serverRequest frames
// and the client may NOT write back. Approval / question answers go
// through the HTTP /api/respond call (see respond.go), NOT through
// the WS connection.
//
// We use gorilla/websocket (already a transitive dep of nightme).
package dsh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cnlangzi/nightme/internal/agent"
)

// wsHandshakeTimeout bounds the WebSocket upgrade. dsh web cold-starts
// in ~1.5s on a real machine; 10s is generous slack for slow CI
// without blocking tests too long.
const wsHandshakeTimeout = 10 * time.Second

// wsFrameReadLimit caps an individual WS message in bytes.
// 10 MiB matches codex / pi / claudecode and is comfortably above any
// expected SessionEvent payload (large tool outputs are streaming
// not single-frame; the worst case is one big tool result which
// dsh breaks into multiple events anyway).
const wsFrameReadLimit = 10 * 1024 * 1024

// wsPingInterval is the cadence at which the dialed WS gets a
// client-side ping. dsh web's `web` profile silently closes WS
// connections that look idle from the *server's* view after
// roughly 25-30 min of no client traffic; without a client-side
// ping the bridge's gorilla/websocket conn never emits anything,
// dsh detects "client gone", closes the WS, and ~5 s later the
// lifecycle watchdog SIGKILLs the cmd — surfacing as "dsh bridge
// died (signal-killed)". 25 s is comfortably below any plausible
// server-side idle timeout while keeping write pressure
// negligible (< 5 B per ping).
const wsPingInterval = 25 * time.Second

// wsPingWriteTimeout caps how long a single PingMessage control
// frame can take. gorilla's WriteControl respects an absolute
// deadline — without one a wedged peer would block the ping
// goroutine forever. 5 s is the recommended default in the
// gorilla/websocket docs.
const wsPingWriteTimeout = 5 * time.Second

// startWSPingWriter launches a watchdog goroutine that sends a
// WebSocket ping frame every wsPingInterval. The goroutine exits
// on the first of these three signals:
//
//  1. stop is closed       — bridge is shutting down
//  2. WriteControl fails   — conn is gone (server closed, network
//                             drop, etc.); no point retrying
//  3. WriteControl blocks  — bounded by wsPingWriteTimeout so the
//                             goroutine cannot wedge forever
//
// Wrapped in agent.SafeGo so a panic (e.g. nil conn deref) is
// recovered at the daemon level and the bridge keeps serving.
//
// MUST be called from dialWS only — the goroutine captures the
// bare conn pointer and assumes single-owner semantics.
func startWSPingWriter(stop <-chan struct{}, conn *websocket.Conn) {
	startWSPingWriterAt(stop, conn, wsPingInterval)
}

// startWSPingWriterAt is the testable form: takes the ping
// interval explicitly so unit tests can drive the loop with
// sub-second cadence. Production callers should use
// startWSPingWriter (which fixes the interval to wsPingInterval).
func startWSPingWriterAt(stop <-chan struct{}, conn *websocket.Conn, interval time.Duration) {
	agent.SafeGo("dsh:ws-ping", func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := conn.WriteControl(
					websocket.PingMessage, nil,
					time.Now().Add(wsPingWriteTimeout),
				); err != nil {
					// Conn is dead (close / reset / EOF). Silent
					// exit — the read pump on the same conn will
					// already be observing the same error and the
					// lifecycle watchdog owns the dsh shutdown.
					return
				}
			}
		}
	})
}

// dialMux opens the /api/events.mux WebSocket. Returns the live
// connection ready for readPump; caller is responsible for calling
// Close on shutdown.
//
// stop is the bridge's close-signal chan; the per-WS ping keeper
// goroutine launched by dialWS exits as soon as stop is closed.
func dialMux(ctx context.Context, baseURL string, stop <-chan struct{}) (*websocket.Conn, error) {
	return dialWS(ctx, baseURL, "/api/events.mux", stop)
}

// dialHost opens the /api/events.host WebSocket. See dialMux.
//
// stop is the bridge's close-signal chan; the per-WS ping keeper
// goroutine launched by dialWS exits as soon as stop is closed.
func dialHost(ctx context.Context, baseURL string, stop <-chan struct{}) (*websocket.Conn, error) {
	return dialWS(ctx, baseURL, "/api/events.host", stop)
}

// dialWS is the shared upgrade routine. `path` must start with "/"
// and is joined to baseURL after swapping http(s)→ws(s).
//
// stop is the bridge's close-signal chan. dialWS launches a
// dedicated ping-keeper goroutine on the returned conn (see
// startWSPingWriter) so the wire stays alive across multi-minute
// idle gaps — dsh web closes the WS after ~25 min of no client
// traffic, which would otherwise surface as "dsh bridge died
// (signal-killed)" via the lifecycle watchdog.
func dialWS(ctx context.Context, baseURL string, path string, stop <-chan struct{}) (*websocket.Conn, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("dsh: parse base url %q: %w", baseURL, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("dsh: unsupported scheme %q in base url", u.Scheme)
	}
	u.Path = path

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = wsHandshakeTimeout

	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("dsh: ws dial %s: %w", u.String(), err)
	}
	conn.SetReadLimit(wsFrameReadLimit)
	// Default ping handler auto-pongs inbound pings. Combined with
	// the outbound ping writer below, the WS link stays "alive"
	// from both peers' perspective. The writer is needed because
	// gorilla's default ping handler is read-loop-driven — if dsh
	// stays silent on the wire, we'd never emit anything and the
	// server-side idle detector would close us.
	startWSPingWriter(stop, conn)
	return conn, nil
}

// readMuxPump reads serverRequest frames from `conn` and hands them
// to `onFrame` for decoding + dispatch. Returns when the connection
// closes (server-side EOF, network error, or explicit Close).
//
// onFrame is called sequentially; long handlers block subsequent
// reads. translate.go is fast (no I/O), so this is fine.
//
// `label` is a debug-only tag for log lines ("mux" or "host").
func readMuxPump(conn *websocket.Conn, label string, onFrame func(method string, rpcID string, payload json.RawMessage)) {
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// Close / EOF / protocol violation all land here.
			// We don't surface the error upward because the lifecycle
			// goroutine owns the "dsh is done" signal via cmd.Wait.
			dLog("dsh: %s ws read err: %v", label, err)
			return
		}
		if len(raw) == 0 {
			continue
		}
		var frame serverFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			dLog("dsh: %s ws frame decode: %v (len=%d)", label, err, len(raw))
			continue
		}
		if frame.Type != "server-request" {
			dLog("dsh: %s ws frame unexpected type=%q", label, frame.Type)
			continue
		}
		onFrame(frame.Method, frame.RPCID, frame.Payload)
	}
}

// drainStreamCapBytes caps the bytes we buffer per stream chunk.
// At 4 KiB, even a chatty dsh web session can never push our
// log line past a sane size; if dsh goes really wild we just log
// each chunk separately and the runtime's log shipper truncates.
// Matches codex's stderrCapBytes so cross-bridge log shapes stay
// uniform.
const drainStreamCapBytes = 4 * 1024

// drainStream reads from `r` in 4 KiB chunks and dLog's each chunk
// until `r` errors (EOF / pipe close / closed-bridge wake-up). Used
// by drainStdout and drainStderr to keep dsh's pipes from filling
// while not exposing unbounded log lines.
//
// `label` is a debug-only tag ("stderr", "stdout (post-url)", ...).
// `done` is the driver's closed chan — when it's closed (bridge
// shutting down) we return even if the stream is still readable.
//
// `sink` is an OPTIONAL ring buffer that receives every chunk we
// read. Used by drainStderr to mirror bytes into d.stderrTail for
// diagnostic capture on bridge exit; nil for callers that just want
// to drain (drainStdout — we don't keep a stdout ring).
//
// Implementation note: we use io.ReadAll-style accumulation per
// chunk (not bufio.Scanner) because dsh streams are often
// unbuffered newlines, and we're not parsing — just draining.
func drainStream(r io.ReadCloser, label string, done <-chan struct{}, sink *agent.StderrRingBuffer) {
	buf := make([]byte, drainStreamCapBytes)
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := r.Read(buf)
		if n > 0 {
			if sink != nil {
				_, _ = sink.Write(buf[:n])
			}
			dLog("dsh: %s: %s", label, strings.TrimRight(string(buf[:n]), "\n"))
		}
		if err != nil {
			return
		}
	}
}
