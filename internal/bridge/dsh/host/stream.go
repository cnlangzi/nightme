// stream.go — single-connection mux+host WebSocket pumps with reconnect.
//
// One pair of WS connections per nightme daemon:
//   - /api/events.mux   — session-scoped frames for ALL attached sessions
//   - /api/events.host  — daemon-global lifecycle frames
//
// Both pumps run in their own goroutine, auto-reconnect with backoff
// on connection loss, and exit cleanly on Close. The mux stream's
// "one stream N sessions" semantic (dsh-api.md §3.4.1) is what makes
// the 1:N architecture possible — every (re)connect triggers a fresh
// session/subscribed frame for every attached session, so the router
// can rebuild its view of the world without state-loss after a
// transient network blip.
//
// Frame flow:
//   readPump → serverFrame decoded → onFrame callback
//
// onFrame callbacks must be NON-BLOCKING and FAST — they run in
// the pump goroutine and a slow handler stalls all subsequent
// reads (and therefore all sessions on this mux stream).

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Wire constants. Mirrors dsh/ws.go so concurrent calls in both
// layers see the same timeouts and limits.

const (
	// wsHandshakeTimeout bounds the WebSocket upgrade. dsh web
	// cold-starts in ~1.5 s on a real machine; 10 s leaves slack
	// for slow CI without blocking tests too long.
	wsHandshakeTimeout = 10 * time.Second

	// wsFrameReadLimit caps an individual WS message in bytes.
	// 10 MiB matches codex / pi / claudecode and is comfortably
	// above any expected SessionEvent payload.
	wsFrameReadLimit = 10 * 1024 * 1024

	// reconnectBaseDelay is the initial wait between reconnect
	// attempts after a stream disconnect. Doubles up to
	// reconnectMaxDelay. Matches the opencode bridge's choice so
	// retry pressure looks uniform across bridges.
	reconnectBaseDelay = 1 * time.Second

	// reconnectMaxDelay caps the exponential backoff.
	reconnectMaxDelay = 30 * time.Second
)

// serverFrame is the wire envelope every mux+host frame uses.
// Identical to dsh/protocol.go's unexported serverFrame; duplicated
// here to keep this package standalone (Phase 3 will consolidate).
type serverFrame struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

// FrameHandler is the per-stream callback invoked once per decoded
// serverFrame. Payload is the JSON bytes of the method-specific
// object (already unmarshaled by the time it gets here — decode
// the method-specific shape at the call site).
type FrameHandler func(method, rpcID string, payload json.RawMessage)

// StreamHub owns one mux + one host WebSocket connection. Both
// connections auto-reconnect with backoff. The hub does NOT know
// about sessionId or dispatch semantics — those live in Router.
//
// Concurrency: Start / Close are safe for concurrent use; the
// underlying conn pointers are guarded by mu so a reconnect can
// safely swap them while Close is reading.
type StreamHub struct {
	baseURL string
	log     *slog.Logger

	onMuxFrame  FrameHandler
	onHostFrame FrameHandler

	mu       sync.Mutex
	muxConn  *websocket.Conn
	hostConn *websocket.Conn
	closed   bool
	stop     chan struct{}

	pumpWG sync.WaitGroup
}

// NewStreamHub constructs a hub. Callbacks must be non-blocking —
// see type doc on FrameHandler. Pass nil for log to use slog.Default().
func NewStreamHub(baseURL string, log *slog.Logger, onMuxFrame, onHostFrame FrameHandler) *StreamHub {
	if log == nil {
		log = slog.Default()
	}
	return &StreamHub{
		baseURL:     baseURL,
		log:         log,
		onMuxFrame:  onMuxFrame,
		onHostFrame: onHostFrame,
		stop:        make(chan struct{}),
	}
}

// Start kicks off the mux + host pumps. Both pumps auto-reconnect
// with backoff; Start only fails if the initial dial fails
// synchronously (rare — only when baseURL is malformed).
//
// The pumps run until Close is called. They observe ctx.Done()
// between reconnect attempts but NOT during an active read
// (websocket.ReadMessage has no ctx-aware variant in gorilla; we
// rely on Close to interrupt in-flight reads).
func (h *StreamHub) Start(ctx context.Context) error {
	if h.baseURL == "" {
		return errors.New("dsh.host: empty baseURL")
	}
	if _, err := url.Parse(h.baseURL); err != nil {
		return fmt.Errorf("dsh.host: parse base url %q: %w", h.baseURL, err)
	}

	h.pumpWG.Add(2)
	go h.runPump(ctx, "/api/events.mux", "mux", func(conn *websocket.Conn) {
		h.mu.Lock()
		h.muxConn = conn
		h.mu.Unlock()
		h.readUntilClose(conn, "mux", h.onMuxFrame)
	})
	go h.runPump(ctx, "/api/events.host", "host", func(conn *websocket.Conn) {
		h.mu.Lock()
		h.hostConn = conn
		h.mu.Unlock()
		h.readUntilClose(conn, "host", h.onHostFrame)
	})
	return nil
}

// Close stops both pumps and waits for them to drain. Safe to
// call multiple times (subsequent calls are no-ops).
//
// On the first call: closes stop chan, closes both WS conns (which
// unblocks any in-flight ReadMessage with an error), waits for
// pumpWG. Subsequent calls: no-op (idempotent).
func (h *StreamHub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	close(h.stop)
	if h.muxConn != nil {
		_ = h.muxConn.Close()
	}
	if h.hostConn != nil {
		_ = h.hostConn.Close()
	}
	h.mu.Unlock()

	h.pumpWG.Wait()
}

// runPump is the per-stream loop: dial → run handler until it
// returns (connection closed) → backoff → repeat. Exits on Close
// (stop chan) or ctx done.
func (h *StreamHub) runPump(ctx context.Context, path, label string, run func(*websocket.Conn)) {
	defer h.pumpWG.Done()

	delay := reconnectBaseDelay
	for {
		if h.isClosed() {
			return
		}
		conn, err := h.dial(ctx, path)
		if err != nil {
			h.log.Warn("dsh.host: dial failed",
				"stream", label, "err", err, "retry_in", delay)
			if !sleepCtx(ctx, h.stop, delay) {
				return
			}
			delay = nextBackoff(delay)
			continue
		}
		// Successful dial — reset backoff so the NEXT failure
		// (if any) starts from baseDelay.
		delay = reconnectBaseDelay

		h.log.Info("dsh.host: stream connected", "stream", label, "url", path)
		run(conn) // returns when conn closes

		if h.isClosed() {
			return
		}
		h.log.Info("dsh.host: stream disconnected, reconnecting",
			"stream", label, "retry_in", delay)
		if !sleepCtx(ctx, h.stop, delay) {
			return
		}
		delay = nextBackoff(delay)
	}
}

// dial upgrades the HTTP baseURL to WS(S) and opens the connection.
// Returns the live conn or a transport error.
func (h *StreamHub) dial(ctx context.Context, path string) (*websocket.Conn, error) {
	u, err := url.Parse(h.baseURL)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: parse base url %q: %w", h.baseURL, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("dsh.host: unsupported scheme %q in base url", u.Scheme)
	}
	u.Path = path

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = wsHandshakeTimeout

	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: ws dial %s: %w", u.String(), err)
	}
	conn.SetReadLimit(wsFrameReadLimit)
	return conn, nil
}

// readUntilClose pumps frames from conn until the connection
// closes (server-side EOF, network error, or explicit Close).
// Decoded serverFrames go to onFrame. Bad JSON and unexpected
// envelope types are logged at debug and dropped (one bad frame
// must never kill the pump).
func (h *StreamHub) readUntilClose(conn *websocket.Conn, label string, onFrame FrameHandler) {
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// Close / EOF / protocol violation all land here.
			// We don't surface the error upward — the runPump
			// loop owns the reconnect decision and will log
			// the disconnect at info level.
			if !h.isClosed() {
				h.log.Debug("dsh.host: stream read err",
					"stream", label, "err", err)
			}
			return
		}
		if len(raw) == 0 {
			continue
		}
		var frame serverFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			h.log.Debug("dsh.host: frame decode",
				"stream", label, "err", err, "len", len(raw))
			continue
		}
		if frame.Type != "server-request" {
			h.log.Debug("dsh.host: unexpected frame type",
				"stream", label, "type", frame.Type)
			continue
		}
		onFrame(frame.Method, frame.RPCID, frame.Payload)
	}
}

// isClosed reports whether Close has been called. Cheap non-blocking
// check via mu lock — fine to call from the pump hot path.
func (h *StreamHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// nextBackoff doubles the delay, capped at reconnectMaxDelay.
// Pulled out as a helper so tests can pin the policy without
// reaching into the loop body.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return d
}

// sleepCtx blocks for d, or returns false on stop / ctx done.
// Used by runPump between reconnect attempts.
func sleepCtx(ctx context.Context, stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-stop:
		return false
	case <-ctx.Done():
		return false
	}
}