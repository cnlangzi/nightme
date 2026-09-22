// stream.go — single-connection Remote mux client for dsh 0.1.2-rc.1.
//
// dsh 0.1.2-rc.1 consolidated the legacy /api/events.mux and
// /api/events.host into a single /api/remote.mux endpoint with a
// multiplexed wire protocol:
//
//	client → server:  {type:"open",   streamId, endpoint, payload:{args:{...}}}
//	client → server:  {type:"cancel", streamId}                    (cancel an open stream)
//	server → client:  {type:"ready",  clientId, host:{home:"..."}} (sent on connect)
//	server → client:  {type:"item",   streamId, value}             (one event from the stream)
//	server → client:  {type:"end",    streamId}                     (stream finished)
//	server → client:  {type:"error",  streamId, error:{code,message,details}}
//
// We open two classes of logical streams on the one physical
// connection:
//
//   1. endpoint "$events"        — daemon-global Host lifecycle events
//      payload: {args:{}}
//   2. endpoint "session/follow" — per-session event stream (one per
//      active AgentSession). payload:
//      {args:{address:{kind:"session", sessionId}}}. Each item is
//      a SessionFollowFrame; we translate .event.data into the
//      bridge's legacy {method, rpcId, payload} envelope so Router
//      can stay unchanged.
//
// One physical connection serves both. Single-writer invariant
// (gorilla requires it) — only the writeLoop goroutine calls
// WriteMessage; concurrent readers are fine because only the
// readLoop goroutine reads.

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsHandshakeTimeout = 10 * time.Second
	wsFrameReadLimit   = 10 * 1024 * 1024

	// wsReadDeadline bounds how long a single frame can sit in the
	// read buffer. dsh's RemoteStreamMuxServer pings every 2s and
	// terminates the WS after 2 missed pongs (~4s); our pong
	// handler (§6.8) keeps that timer satisfied, so a generous
	// 60s ceiling is fine and only matters if dsh goes silent.
	wsReadDeadline = 60 * time.Second

	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second

	// muxPath is the dsh 0.1.2-rc.1 Remote mux endpoint.
	muxPath = "/api/remote.mux"
)

const (
	// hostEventsEndpoint is the typert endpoint name dsh uses for
	// daemon-global Host lifecycle events. Verified 2026-09-11
	// against dsh 0.1.2-rc.1 (api-gateway types/stream-protocol.js).
	hostEventsEndpoint = "$events"

	// sessionControlEndpoint is the typert endpoint name dsh uses
	// for the daemon-global live session/control stream (baseline
	// + projection / queue / jobs deltas per
	// packages/api/session-controller/src/control.ts). The
	// endpoint takes no args; dsh returns one baseline + a stream
	// of deltas. The bridge only decodes the `modelSelection`
	// projection; other projections / queues / jobs ride through
	// as RawMessage so projection schema bumps don't break us.
	sessionControlEndpoint = "session/control"

	// sessionFollowEndpoint is the typert endpoint name dsh uses
	// for per-session live event streams. Verified 2026-09-11
	// against dsh 0.1.2-rc.1 (api-session-controller
	// typert.host.js — `@deepseek-ai/dsh-api-session-controller#
	// session/follow`, mode=stream).
	sessionFollowEndpoint = "session/follow"

	// hostStreamID is the fixed streamId for the daemon-global
	// Host event stream. It's a constant so the dispatcher can
	// route Host items to onHostFrame without a registry lookup.
	hostStreamID = "host-$events"

	// controlStreamID is the fixed streamId for the daemon-global
	// session/control stream. Same fixed-id trick as hostStreamID —
	// dispatcher routes by literal ID without a registry lookup.
	controlStreamID = "host-control"
)

// sessionStream holds the per-session subscription state. The
// handler is shared across all sessions (it's StreamHub.onMuxFrame
// — Router handles per-session dispatch by extracting sessionId
// from payload.sessionId).
//
// `generation` records the connection generation this streamId was
// minted against. connectAndServe bumps generation on every dial
// and only re-mints streamIds for subs from older generations.
// New subs (added between dials via Subscribe) carry the current
// generation and keep their streamId across the next reconnect.
type sessionStream struct {
	sessionID  string
	streamID   string
	generation uint64
}

// StreamHub owns one /api/remote.mux WebSocket connection. It
// opens the Host $events stream on Start and per-session streams
// on demand via Subscribe. All frames on every logical stream
// flow back through the same pump goroutine, which dispatches by
// streamId.
type StreamHub struct {
	baseURL string
	log     *slog.Logger
	jar     http.CookieJar

	// Callbacks. onHostFrame receives Host lifecycle events
	// (decoded {method, rpcId, payload}); onMuxFrame receives
	// every session event (Router.DispatchMux then routes by
	// payload.sessionId). onControlFrame receives each item yielded
	// by the session/control stream (the stream itself is opened
	// by StreamHub on every (re)connect — drivers do not subscribe
	// to it; they watch the resolved projection via the host
	// package's Control store).
	onMuxFrame     FrameHandler
	onHostFrame    FrameHandler
	onControlFrame ControlFrameHandler

	mu         sync.RWMutex
	sessions   map[string]*sessionStream // sessionID → subscription
	byStreamID map[string]*sessionStream // streamID → subscription
	conn       *websocket.Conn
	writeCh    chan clientFrame // single-writer queue
	closed     bool
	stop       chan struct{}

	pumpWG sync.WaitGroup

	// dispatchMu + dispatchCount replaces a sync.WaitGroup for
	// in-flight handler accounting. The WaitGroup pattern has a
	// documented race: "calls with a positive delta that occur when
	// the counter is zero must happen before a Wait" — between an
	// Add(1) in the dispatch hot path and a Wait() in Close(), a
	// concurrent Done() can drop the counter to 0 with Wait already
	// observing 0, causing Close to return before the handler
	// finishes. Lock+counter+cond makes the same sequence safe
	// because both Add(1) (encoded as dispatchCount++) and Wait
	// (encoded as a cond.Wait loop) take dispatchMu and observe a
	// consistent state. Tradeoff: one extra mutex acquisition per
	// dispatched item, negligible vs the WS round-trip cost.
	dispatchMu    sync.Mutex
	dispatchCount int
	dispatchCond  *sync.Cond // initialized in NewStreamHub, not lazily

	streamSeq         atomic.Uint64 // mint unique streamIds
	currentGeneration atomic.Uint64 // bumps on every connect; subs track which generation minted them
}

// NewStreamHub constructs a hub without cookie attachment. Used by
// tests which fake the dsh server and don't auth-gate. Production
// callers must use NewStreamHubWithJar so the dsh-auth cookie
// obtained from the launch-token exchange reaches the WS upgrade.
//
// Callbacks must be non-blocking — see type doc on FrameHandler.
// Pass nil for log to use slog.Default().
//
// onControlFrame is invoked once per item yielded by the
// session/control stream (baseline + deltas). The hub opens the
// session/control stream automatically on every (re)connect —
// callers do not need to subscribe to it.
func NewStreamHub(baseURL string, log *slog.Logger, onMuxFrame, onHostFrame FrameHandler, onControlFrame ControlFrameHandler) *StreamHub {
	if log == nil {
		log = slog.Default()
	}
	h := &StreamHub{
		baseURL:        baseURL,
		log:            log,
		onMuxFrame:     onMuxFrame,
		onHostFrame:    onHostFrame,
		onControlFrame: onControlFrame,
		sessions:       make(map[string]*sessionStream),
		byStreamID:     make(map[string]*sessionStream),
		stop:           make(chan struct{}),
		writeCh:        make(chan clientFrame, 32),
	}
	h.dispatchCond = sync.NewCond(&h.dispatchMu)
	return h
}

// NewStreamHubWithJar constructs a hub that attaches jar's cookies
// to every WS upgrade. Spawned-dsh path: the cookie is minted via
// `GET /?token=<launchToken>` (see spawnAndWire.mintAuthCookie)
// before this hub is started, so the first upgrade already carries
// the dsh-auth cookie and dsh doesn't 401 mid-handshake.
//
// jar must be non-nil; passing nil is a programming error (use
// NewStreamHub for the cookie-less test path).
func NewStreamHubWithJar(baseURL string, jar http.CookieJar, log *slog.Logger, onMuxFrame, onHostFrame FrameHandler, onControlFrame ControlFrameHandler) *StreamHub {
	h := NewStreamHub(baseURL, log, onMuxFrame, onHostFrame, onControlFrame)
	h.jar = jar
	return h
}

// Subscribe registers a per-session subscription. The actual
// `session/follow` stream is opened by connectAndServe on the
// next (re)connect — this avoids double-open when Subscribe is
// called between a previous reconnect's open and the next connect.
//
// When the hub is already connected, Subscribe also enqueues an
// open frame immediately (without waiting for a reconnect). The
// streamId the sub carries is tagged with currentGeneration so
// connectAndServe knows NOT to re-mint it on the next reconnect
// (re-minting would orphan the items the server is about to push
// for the streamId we just sent).
//
// Subscribing the same sessionID twice replaces the prior
// subscription AND cancels the prior stream (last-wins) — matches
// the bridge session.go pattern of "one handler per session,
// replaced on reconnect".
func (h *StreamHub) Subscribe(sessionID string, _ FrameHandler) (unsubscribe func()) {
	if sessionID == "" {
		return func() {}
	}
	streamID := h.mintStreamID("sess")
	sub := &sessionStream{
		sessionID: sessionID,
		streamID:  streamID,
	}

	h.mu.Lock()
	// Generation is captured under h.mu so the Subscribe-vs-
	// reconnect race below can't leave us with a stale value.
	// connectAndServe bumps currentGeneration under the same lock;
	// taking it here means the open we enqueue below is consistent
	// with the generation the next toReopen loop will read.
	sub.generation = h.currentGeneration.Load()
	if old, ok := h.sessions[sessionID]; ok {
		delete(h.byStreamID, old.streamID)
		h.queueCancelLocked(old.streamID)
	}
	h.sessions[sessionID] = sub
	h.byStreamID[streamID] = sub
	// If the hub is currently connected, enqueue the open frame
	// NOW. If the hub isn't connected yet, connectAndServe will
	// pick up this session from h.sessions on the next (re)connect.
	if h.conn != nil && !h.closed {
		select {
		case h.writeCh <- clientFrame{
			Type:     "open",
			StreamID: streamID,
			Endpoint: sessionFollowEndpoint,
			Payload:  sessionOpenPayload(sessionID),
		}:
			h.log.Info("dsh.host: subscribed to session stream (immediate open)",
				"session_id", sessionID, "stream_id", streamID)
		default:
			// writeCh full — connectAndServe's toReopen on the
			// next reconnect will catch up. Drop the immediate
			// attempt to avoid blocking the caller.
			h.log.Warn("dsh.host: writeCh full; deferred session open",
				"session_id", sessionID, "stream_id", streamID)
		}
	} else {
		h.log.Info("dsh.host: subscribed to session stream (deferred open)",
			"session_id", sessionID, "stream_id", streamID)
	}
	h.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			if cur, ok := h.sessions[sessionID]; ok && cur == sub {
				delete(h.sessions, sessionID)
				delete(h.byStreamID, streamID)
				h.queueCancelLocked(streamID)
			}
			h.mu.Unlock()
			h.log.Info("dsh.host: unsubscribed session stream",
				"session_id", sessionID, "stream_id", streamID)
		})
	}
}

// Unsubscribe tears down the session/follow stream for sessionID
// (mirrors the cancel func returned by Subscribe but keyed by
// sessionId so callers that lost the cancel func — typically
// because they reached into Router.Unsubscribe directly — can
// still free the hub-side resources).
//
// No-op when sessionID is unknown to the hub.
func (h *StreamHub) Unsubscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	sub, ok := h.sessions[sessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, sessionID)
	delete(h.byStreamID, sub.streamID)
	h.queueCancelLocked(sub.streamID)
	h.mu.Unlock()
	h.log.Info("dsh.host: unsubscribed session stream",
		"session_id", sessionID, "stream_id", sub.streamID)
}

// sessionOpenPayload returns the `{args:{...}}` payload for the
// session/follow open frame.
//
// dsh's session/follow typert expects the args wrapped as
// `{request: SessionFollowRequest}`, not flat. assistantStream
// opts into process-local presentation frames; durable follow
// events no longer carry assistant/chunk, and thinking arrives
// on those frames.
func sessionOpenPayload(sessionID string) json.RawMessage {
	return mustJSON(map[string]any{
		"args": map[string]any{
			"request": map[string]any{
				"address": map[string]any{
					"kind":      "session",
					"sessionId": sessionID,
				},
				"assistantStream": true,
			},
		},
	})
}

// Start kicks off the single /api/remote.mux pump. The mux pump
// reconnects with backoff on transport loss; Start fails only if
// the very first dial fails synchronously (rare — only when
// baseURL is malformed).
//
// Per-stream `open` frames are sent by connectAndServe on every
// (re)connect — see the toReopen list there. Start does not
// pre-queue any opens; it just kicks off the loop.
func (h *StreamHub) Start(ctx context.Context) error {
	if h.baseURL == "" {
		return errors.New("dsh.host: empty baseURL")
	}
	if _, err := url.Parse(h.baseURL); err != nil {
		return fmt.Errorf("dsh.host: parse base url %q: %w", h.baseURL, err)
	}

	h.pumpWG.Add(1)
	go h.runMuxLoop(ctx)
	return nil
}

// Close stops the pump and waits for it to drain. Safe to call
// multiple times (subsequent calls are no-ops). Cancels every
// active session stream on the server side via `{type:"cancel"}`.
func (h *StreamHub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	close(h.stop)
	for streamID := range h.byStreamID {
		h.queueCancelLocked(streamID)
	}
	conn := h.conn
	h.conn = nil
	h.sessions = nil
	h.byStreamID = nil
	h.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	h.pumpWG.Wait()
	h.waitDispatchDrain()
}

// waitDispatchDrain blocks until every dispatched handler has
// finished. Replaces the old dispatchWG.Wait() which had an
// Add(1)↔Wait race. See the dispatchMu field doc for the
// motivation; same idea but with cond instead of a sync.WaitGroup.
func (h *StreamHub) waitDispatchDrain() {
	h.dispatchMu.Lock()
	defer h.dispatchMu.Unlock()
	for h.dispatchCount > 0 {
		h.dispatchCond.Wait()
	}
}

// runMuxLoop owns the physical WS connection. It dials, serves
// until disconnect, then backs off and reconnects.
func (h *StreamHub) runMuxLoop(ctx context.Context) {
	defer h.pumpWG.Done()

	delay := reconnectBaseDelay
	for {
		if h.isClosed() {
			return
		}
		if err := h.connectAndServe(ctx); err != nil {
			h.log.Warn("dsh.host: mux loop iteration failed",
				"err", err, "retry_in", delay)
		}
		select {
		case <-h.stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = nextBackoff(delay)
	}
}

// connectAndServe dials /api/remote.mux, runs the read/write
// pumps until one fails, then returns the error for the outer
// loop to schedule a reconnect. After a fresh connect it
// re-opens every active subscription so callers don't have to.
func (h *StreamHub) connectAndServe(ctx context.Context) error {
	u, err := url.Parse(h.baseURL)
	if err != nil {
		return fmt.Errorf("dsh.host: parse base url %q: %w", h.baseURL, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return fmt.Errorf("dsh.host: unsupported scheme %q in base url", u.Scheme)
	}
	u.Path = muxPath

	// Dial /api/remote.mux via websocket.NewClient so we can attach
	// the dsh-auth cookie as a real HTTP header. Go's stdlib
	// cookiejar deliberately refuses to return cookies for ws://
	// URLs (`if u.Scheme != "http" && u.Scheme != "https"`), so
	// dialer.Jar.Cookies(wsURL) is always 0 even when the matching
	// http URL has the cookie stored. The fix: pull cookies via the
	// http:// URL and stamp them onto the upgrade request header
	// ourselves.
	//
	// Note: dsh 0.1.2-rc.1 rejects any Sec-WebSocket-Protocol it
	// doesn't recognize (returns HTTP 400 "Invalid Sec-WebSocket-
	// Protocol header") so we MUST NOT set a custom subprotocol
	// here — the empty default negotiates to "no protocol" which
	// dsh accepts.
	requestHeader := http.Header{}
	if h.jar != nil {
		httpURL := *u
		httpURL.Scheme = "http"
		for _, c := range h.jar.Cookies(&httpURL) {
			// Cookie.String handles RFC 6265 §5.2 escaping for
			// values containing ; , = " whitespace etc. A raw
			// `Name+"="+Value` paste would corrupt the header
			// for any non-trivial value.
			requestHeader.Add("Cookie", c.String())
		}
	}

	netConn, err := (&net.Dialer{Timeout: wsHandshakeTimeout}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return fmt.Errorf("dsh.host: dial %s: %w", u.Host, err)
	}

	conn, resp, err := websocket.NewClient(netConn, u, requestHeader, 4096, 4096)
	if err != nil {
		netConn.Close()
		if resp != nil {
			// Drain + close the upgrade-response body so the
			// connection doesn't leak fds across reconnect attempts.
			if resp.Body != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			return fmt.Errorf("dsh.host: ws dial %s: HTTP %d: %w",
				u.String(), resp.StatusCode, err)
		}
		return fmt.Errorf("dsh.host: ws dial %s: %w", u.String(), err)
	}
	conn.SetReadLimit(wsFrameReadLimit)

	// dsh's RemoteStreamMuxServer pings every 2s and terminates
	// dsh's RemoteStreamMuxServer pings every 2s and terminates
	// the socket after 2 missed pongs. Reply to each ping with the
	// exact payload dsh sent (RFC 6455 §5.5.3) so the server's
	// missed-pong counter resets; without this the server
	// terminates the connection and the read loop sees close 1006
	// "unexpected EOF" within 4 seconds of every reconnect.
	//
	// ALSO reset the read deadline here: control frames (ping /
	// pong / close) are dispatched to the handler by gorilla
	// WITHOUT making ReadMessage return, so the data-frame reset
	// in readLoop is a no-op for idle sessions. Without this
	// reset an idle mux (no business frames, only 2s pings) hits
	// the absolute 60s deadline from line 483 / 578, ReadMessage
	// returns i/o-timeout, and the connection tears down +
	// reconnects even though dsh is perfectly healthy. Refresh on
	// every ping keeps the deadline aligned with "last network
	// activity" without leaking goroutines.
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
		return conn.WriteControl(websocket.PongMessage,
			[]byte(appData),
			time.Now().Add(time.Second))
	})
	// SetPongHandler is a no-op for now — gorilla surfaces pong
	// frames only as a signal that the server is alive, and the
	// server's missed-pong counter is what matters, not ours. We
	// could add a SetPongHandler that resets the deadline too,
	// but the server pings every 2s so a client-side pong is
	// always within the same 2s window — handling pings is
	// sufficient. (We track server liveness via the ping round-
	// trip; if we ever want client-initiated pings, add a
	// writeLoop ticker that emits Ping frames + a SetPongHandler
	// that resets the deadline here.)
	//
	// Initial read deadline — readLoop also resets this on every
	// successful data frame read; the ping handler above covers
	// the idle-but-pinging case. The deadline only bites if dsh
	// goes silent for >60s — in that case we'd rather reconnect
	// than hang forever.
	_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	h.conn = conn
	// Bump the connection generation. Subs carry the generation
	// they were minted against so a Subscribe that fires the open
	// frame between reconnects doesn't get its streamId orphaned
	// by the next toReopen loop (which mints a fresh id for stale
	// subs only).
	thisGen := h.currentGeneration.Add(1)
	// After reconnect, re-open every active subscription whose
	// streamId was minted against a previous connection. Subs
	// from the current generation were just opened by Subscribe's
	// immediate-open path; resending the open for them would make
	// the server see a duplicate streamId and 1008 the connection.
	toReopen := make([]clientFrame, 0, len(h.sessions)+1)
	for _, sub := range h.sessions {
		if sub.generation == thisGen {
			// Subscribe already sent the open for this sub; the
			// server is mid-stream on this streamId. Skip.
			continue
		}
		newID := h.mintStreamID("sess")
		delete(h.byStreamID, sub.streamID)
		sub.streamID = newID
		sub.generation = thisGen
		h.byStreamID[newID] = sub
		toReopen = append(toReopen, clientFrame{
			Type:     "open",
			StreamID: newID,
			Endpoint: sessionFollowEndpoint,
			Payload:  sessionOpenPayload(sub.sessionID),
		})
	}
	// Re-open the host stream too (its streamId is fixed).
	// Note: host stream is always at hostStreamID = "host-$events"
	// — same ID is fine across reconnects because the server
	// re-creates it on the fresh connection.
	toReopen = append(toReopen, clientFrame{
		Type:     "open",
		StreamID: hostStreamID,
		Endpoint: hostEventsEndpoint,
		Payload:  mustJSON(map[string]any{"args": map[string]any{}}),
	})
	// Re-open the session/control stream (its streamId is fixed).
	// Same rationale as hostStreamID: dsh re-creates it on the
	// fresh connection; the bridge receives a fresh baseline
	// containing every active session's projection snapshot.
	toReopen = append(toReopen, clientFrame{
		Type:     "open",
		StreamID: controlStreamID,
		Endpoint: sessionControlEndpoint,
		Payload:  mustJSON(map[string]any{"args": map[string]any{}}),
	})
	h.mu.Unlock()

	h.log.Info("dsh.host: mux stream connected", "url", u.String(),
		"reopen_count", len(toReopen))

	// Serve until either side errors out.
	readErrCh := make(chan error, 1)
	go func() { readErrCh <- h.readLoop(conn) }()
	writeErrCh := make(chan error, 1)
	go func() { writeErrCh <- h.writeLoop(ctx, conn) }()

	for _, frame := range toReopen {
		h.writeCh <- frame
	}

	select {
	case err := <-readErrCh:
		// Read side died. Closing conn unblocks writeLoop's next
		// WriteMessage (gorilla surfaces ECONNRESET immediately).
		// We deliberately do NOT wait for writeErrCh — writeLoop
		// might be blocked on an empty writeCh and never exit
		// without this conn.Close() unblocking it.
		_ = conn.Close()
		return err
	case err := <-writeErrCh:
		_ = conn.Close()
		return err
	}
}

// readLoop reads inbound WS text frames until error. Decodes
// serverFrame and dispatches by streamId.
func (h *StreamHub) readLoop(conn *websocket.Conn) error {
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			h.log.Info("dsh.host: readLoop error", "err", err)
			return err
		}
		// Reset the read deadline on every successful frame so
		// dsh's 2s ping (which arrives as a control frame and is
		// handled by SetPingHandler, NOT ReadMessage) keeps the
		// socket alive. The deadline only bites if dsh goes
		// silent for >60s — in that case we'd rather reconnect
		// than hang forever.
		_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
		h.log.Debug("dsh.host: mux read", "bytes", len(raw), "preview", truncateBytes(raw, 200))
		if len(raw) == 0 {
			continue
		}
		var f serverFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			h.log.Debug("dsh.host: invalid mux frame",
				"len", len(raw), "err", err)
			continue
		}
		h.dispatch(f)
	}
}

// dispatch fans out a server frame to the right handler. Drops
// frames addressed to unknown streamIds (they're either the very
// first `ready` frame dsh sends on connect, or stale frames for
// streams we already closed).
func (h *StreamHub) dispatch(f serverFrame) {
	switch f.Type {
	case "item":
		// Look up which handler this streamId belongs to.
		h.mu.RLock()
		if f.StreamID == hostStreamID {
			h.mu.RUnlock()
			// dsh delivers the $events ready handshake as a host
			// item: value={type:"ready", clientId, host}. Capture
			// the clientId at the dispatch site; SendPermission
			// echoes it on /api/$events/result. The host handler
			// ignores "ready" via its early-return.
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(f.Value, &head) == nil && head.Type == "ready" {
				var r struct {
					ClientID string          `json:"clientId"`
					Host     json.RawMessage `json:"host"`
				}
				_ = json.Unmarshal(f.Value, &r)
				h.log.Info("dsh.host: mux ready",
					"client_id", r.ClientID, "host", truncateBytes(r.Host, 120))
				SetHostClientID(r.ClientID)
				h.markDispatchStart()
				h.invokeOnHost("ready", "", f.Value)
				return
			}
			method, rpcID, payload := translateHostEvent(f.Value)
			if method == "" {
				h.log.Debug("dsh.host: untranslated host item",
					"value_bytes", truncateBytes(f.Value, 200))
				return
			}
			h.markDispatchStart()
			h.invokeOnHost(method, rpcID, payload)
			return
		}
		if f.StreamID == controlStreamID {
			h.mu.RUnlock()
			frame, ok := translateControlFrame(f.Value)
			if !ok {
				h.log.Debug("dsh.host: untranslated control item",
					"value_bytes", truncateBytes(f.Value, 200))
				return
			}
			h.markDispatchStart()
			h.invokeOnControl(frame)
			return
		}
		sub, ok := h.byStreamID[f.StreamID]
		h.mu.RUnlock()
		if !ok || sub == nil {
			h.log.Debug("dsh.host: item for unknown stream",
				"stream_id", f.StreamID)
			return
		}
		method, rpcID, payload := translateSessionEvent(f.Value, sub.sessionID)
		if method == "" {
			h.log.Debug("dsh.host: untranslated session item",
				"stream_id", f.StreamID,
				"session_id", sub.sessionID,
				"value_bytes", truncateBytes(f.Value, 200))
			return
		}
		h.markDispatchStart()
		h.invokeOnMux(method, rpcID, payload)

	case "end":
		h.mu.Lock()
		sub, ok := h.byStreamID[f.StreamID]
		delete(h.byStreamID, f.StreamID)
		h.mu.Unlock()
		if ok && sub != nil {
			h.log.Info("dsh.host: stream ended",
				"stream_id", f.StreamID, "session_id", sub.sessionID)
		}

	case "error":
		h.mu.Lock()
		sub, ok := h.byStreamID[f.StreamID]
		delete(h.byStreamID, f.StreamID)
		h.mu.Unlock()
		msg := "<unknown>"
		code := "<unknown>"
		if f.Error != nil {
			msg = f.Error.Message
			code = f.Error.Code
		}
		sid := ""
		if ok && sub != nil {
			sid = sub.sessionID
		}
		h.log.Error("dsh.host: stream error",
			"stream_id", f.StreamID, "session_id", sid,
			"code", code, "msg", msg)

	default:
		h.log.Debug("dsh.host: unknown mux frame type",
			"type", f.Type, "stream_id", f.StreamID)
	}
}

func (h *StreamHub) invokeOnHost(method, rpcID string, payload json.RawMessage) {
	defer h.markDispatchDone()
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("dsh.host: host dispatch handler panic",
				"method", method, "rpc_id", rpcID, "panic", r)
		}
	}()
	if h.onHostFrame != nil {
		h.onHostFrame(method, rpcID, payload)
	}
}

func (h *StreamHub) invokeOnControl(frame ControlFrame) {
	defer h.markDispatchDone()
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("dsh.host: control dispatch handler panic",
				"kind", frame.Kind, "session_id", frame.SessionID, "panic", r)
		}
	}()
	if h.onControlFrame != nil {
		h.onControlFrame(frame)
	}
}

func (h *StreamHub) invokeOnMux(method, rpcID string, payload json.RawMessage) {
	defer h.markDispatchDone()
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("dsh.host: mux dispatch handler panic",
				"method", method, "rpc_id", rpcID, "panic", r)
		}
	}()
	if h.onMuxFrame != nil {
		h.onMuxFrame(method, rpcID, payload)
	}
}

// markDispatchStart / markDispatchDone replace the old
// dispatchWG.Add(1) / Done() pair. The mutex+counter+cond pattern
// avoids the documented Add(1)↔Wait() race; see dispatchMu field
// doc for motivation. dispatchCond is initialized once in
// NewStreamHub so waitDispatchDrain never observes a nil cond.
func (h *StreamHub) markDispatchStart() {
	h.dispatchMu.Lock()
	h.dispatchCount++
	h.dispatchMu.Unlock()
}

func (h *StreamHub) markDispatchDone() {
	h.dispatchMu.Lock()
	h.dispatchCount--
	if h.dispatchCount == 0 {
		h.dispatchCond.Broadcast()
	}
	h.dispatchMu.Unlock()
}

// writeLoop drains writeCh into the WS connection. Single-writer
// invariant: only this goroutine calls WriteMessage. Exits when
// the connection closes (next WriteMessage errors).
func (h *StreamHub) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.stop:
			return nil
		case frame, ok := <-h.writeCh:
			if !ok {
				return nil
			}
			body, err := json.Marshal(frame)
			if err != nil {
				h.log.Warn("dsh.host: marshal client frame",
					"err", err, "type", frame.Type, "stream_id", frame.StreamID)
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
				return err
			}
		}
	}
}

// queueCancelLocked puts a `{type:"cancel", streamId}` frame on
// the write channel without blocking. Must be called with h.mu
// held. Drops on full channel — the pump is dead at that point
// and a frame we couldn't send is harmless (the connection tear
// down will cancel the server-side stream implicitly).
func (h *StreamHub) queueCancelLocked(streamID string) {
	select {
	case h.writeCh <- clientFrame{Type: "cancel", StreamID: streamID}:
	default:
	}
}

func (h *StreamHub) isClosed() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.closed
}

// mintStreamID returns a process-unique streamId. The prefix is
// used by logs to make grepping easier.
func (h *StreamHub) mintStreamID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, h.streamSeq.Add(1))
}

// ─── wire types ───────────────────────────────────────────────────

type serverFrame struct {
	Type     string          `json:"type"`
	StreamID string          `json:"streamId,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Error    *serverErr      `json:"error,omitempty"`
	ClientID string          `json:"clientId,omitempty"`
	Host     json.RawMessage `json:"host,omitempty"`
}

type serverErr struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

type clientFrame struct {
	Type     string          `json:"type"`
	StreamID string          `json:"streamId,omitempty"`
	Endpoint string          `json:"endpoint,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// FrameHandler is the per-stream callback. Payload is the JSON
// bytes of the method-specific object (already unmarshaled by
// the time it gets here).
type FrameHandler func(method, rpcID string, payload json.RawMessage)

// ControlFrameKind is the discriminator on a session/control item
// after translateControlFrame parses it.
type ControlFrameKind string

const (
	ControlBaseline   ControlFrameKind = "baseline"
	ControlProjection ControlFrameKind = "projection"
	ControlQueue      ControlFrameKind = "queue"
	ControlJobs       ControlFrameKind = "jobs"
)

// ControlFrame is the bridge-side decoded shape of one item on
// the session/control stream. Carry depends on Kind:
//
//   - baseline:   Models map (sessionId → resolved model id)
//   - projection: SessionID + Key + Model (when Key=="modelSelection")
//   - queue / jobs: SessionID + Raw (RawMessage for forward compat)
type ControlFrame struct {
	Kind      ControlFrameKind
	SessionID string
	Key       string
	Model     string            // only set for baseline+projection of modelSelection
	Models    map[string]string // only set for baseline
	Raw       json.RawMessage   // queue/jobs items + undecoded projection values
}

// ControlFrameHandler receives each ControlFrame yielded by the
// session/control stream. Must be non-blocking — runs on the WS
// readLoop goroutine.
type ControlFrameHandler func(frame ControlFrame)

// translateControlFrame parses one item yielded by the
// session/control stream. Returns ok=false when the item type is
// empty / unknown so the dispatcher can drop it quietly.
//
// Wire form (dsh 0.1.5-rc.1 control.ts):
//
//	{ type:"baseline",   value:{queues, jobs, projections:{<sid>:{asOfSeq,values:{modelSelection,...}}}} }
//	{ type:"projection", sessionId, key, value, seq }
//	{ type:"queue",      sessionId, items, seq }
//	{ type:"jobs",       sessionId, jobs, seq }
func translateControlFrame(raw json.RawMessage) (ControlFrame, bool) {
	var f struct {
		Type      string          `json:"type"`
		SessionID string          `json:"sessionId,omitempty"`
		Key       string          `json:"key,omitempty"`
		Seq       int64           `json:"seq,omitempty"`
		Value     json.RawMessage `json:"value,omitempty"`
		Items     json.RawMessage `json:"items,omitempty"`
		Jobs      json.RawMessage `json:"jobs,omitempty"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return ControlFrame{}, false
	}
	switch f.Type {
	case "baseline":
		var b sessionControlBaselineWire
		if err := json.Unmarshal(raw, &b); err != nil {
			return ControlFrame{}, false
		}
		// Project every session's modelSelection projection down to
		// the resolved model id (next ?? lastUsed). Sessions with no
		// recorded selection yet (both nil) map to "" — the watcher's
		// fresh callback will see "".
		models := make(map[string]string, len(b.Value.Projections))
		for sid, proj := range b.Value.Projections {
			models[sid] = proj.Values.ModelSelection.resolveModel()
		}
		return ControlFrame{
			Kind:   ControlBaseline,
			Models: models,
			Raw:    raw,
		}, true
	case "projection":
		if f.Key != "modelSelection" {
			// Forward-compat: ride through undecoded. Hub caller
			// (Client.Control.ApplyProjection) no-ops on non-
			// modelSelection keys.
			return ControlFrame{
				Kind:      ControlProjection,
				SessionID: f.SessionID,
				Key:       f.Key,
				Raw:       raw,
			}, true
		}
		var proj modelSelectionProjection
		if err := json.Unmarshal(f.Value, &proj); err != nil {
			return ControlFrame{}, false
		}
		return ControlFrame{
			Kind:      ControlProjection,
			SessionID: f.SessionID,
			Key:       f.Key,
			Model:     proj.resolveModel(),
			Raw:       f.Value,
		}, true
	case "queue":
		return ControlFrame{
			Kind:      ControlQueue,
			SessionID: f.SessionID,
			Raw:       f.Items,
		}, true
	case "jobs":
		return ControlFrame{
			Kind:      ControlJobs,
			SessionID: f.SessionID,
			Raw:       f.Jobs,
		}, true
	}
	return ControlFrame{}, false
}

// sessionControlBaselineWire is the bridge-side decode of the
// baseline frame. Defined here (not in protocol.go) because only
// the hub's translator needs it — the projection store consumes
// the decoded `Models` map directly.
//
// Wire form:
//
//	{ type:"baseline",
//	  value:{ queues, jobs,
//	          projections:{ <sid>:{ asOfSeq, values:{ modelSelection, ... } } } }
type sessionControlBaselineWire struct {
	Value struct {
		Projections map[string]struct {
			AsOfSeq int64 `json:"asOfSeq"`
			Values  struct {
				ModelSelection modelSelectionProjection `json:"modelSelection"`
			} `json:"values"`
		} `json:"projections"`
	} `json:"value"`
}

// ─── dispatch translation ─────────────────────────────────────────

// translateHostEvent decodes one item yielded by the Host $events
// async iterable. dsh 0.1.2-rc.1 ships three item shapes (per
// @deepseek-ai/dsh-api-gateway/lib/types/index.js::openRemoteEvents
// + broadcastRemoteEvent + startRemoteEvent):
//
//	{ type:"ready",     clientId, host:{home} }           // FIRST item, no dispatch
//	{ type:"emit",      event, args:[<positional args>] }  // broadcasted Cordis event
//	{ type:"waterfall", event, eventId, agentId, request } // scoped event awaiting reply
//	{ type:"cancel",    eventId }                          // server-side cancel of a waterfall
//
// The bridge's FrameHandler contract is {method, rpcID, payload}.
// We map:
//
//	emit      → method=event, rpcId=auto (no server id), payload={args:[...]}
//	waterfall → method=event, rpcId=eventId, payload={agentId, request}
//	cancel    → method="host/cancel", rpcId=eventId, payload={"eventId":...}
//	ready     → method="" (caller in dispatch() logs it; we don't translate)
//
// Verified against dsh 0.1.2-rc.1 real traffic: api-session/status
// and api-session/activity arrive as emit frames with positional
// args [sessionId, isRunning|timestamp].
func translateHostEvent(raw json.RawMessage) (method, rpcID string, payload json.RawMessage) {
	var rec struct {
		Type    string          `json:"type"`
		Event   string          `json:"event,omitempty"`
		Args    json.RawMessage `json:"args,omitempty"`
		EventID string          `json:"eventId,omitempty"`
		AgentID string          `json:"agentId,omitempty"`
		Request json.RawMessage `json:"request,omitempty"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", "", nil
	}
	switch rec.Type {
	case "emit":
		if rec.Event == "" {
			return "", "", nil
		}
		// No server-side id for emit frames; pass empty rpcId so
		// callers can still log the (method, payload) pair but won't
		// try to correlate with a /api/respond answer.
		return rec.Event, "", mustJSON(map[string]any{"args": rec.Args})
	case "waterfall":
		if rec.Event == "" {
			return "", "", nil
		}
		return rec.Event, rec.EventID, mustJSON(map[string]any{
			"agentId": rec.AgentID,
			"request": rec.Request,
		})
	case "cancel":
		return "host/cancel", rec.EventID, mustJSON(map[string]any{
			"eventId": rec.EventID,
		})
	case "":
		// Empty type means we couldn't decode — drop silently.
		return "", "", nil
	default:
		// Unknown item type — log and drop. The dispatcher's
		// "untranslated host item" path will record the bytes.
		return "", "", nil
	}
}

// translateSessionEvent decodes one SessionFollowFrame item.
//
// Wire form (from dsh typert):
//
//	{ type:"snapshot", header, cursor, records, hasMore, projections }
//	  → method = "session/snapshot"
//	{ type:"event", event:{type, seq, time, data:{...payload-fields}} }
//	  → method = event.type (the top-level per-event discriminator)
//	  → rpcId = stringified seq
//	  → payload = event.data with sessionId added
//	{ type:"assistant-stream", frame:{type:"chunk", chunk} }
//	  → method = "assistant/chunk"
//	  → rpcId = "astream-<index>" (not a session seq)
//	  → payload = {chunk, sessionId}
//	{ type:"end" } → not handled here (dispatcher handles "end" at the frame level)
func translateSessionEvent(raw json.RawMessage, sessionID string) (method, rpcID string, payload json.RawMessage) {
	var frame struct {
		Type    string          `json:"type"`
		Event   json.RawMessage `json:"event,omitempty"`
		Header  json.RawMessage `json:"header,omitempty"`
		Cursor  int64           `json:"cursor,omitempty"`
		Records json.RawMessage `json:"records,omitempty"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return "", "", nil
	}
	switch frame.Type {
	case "snapshot":
		return "session/snapshot", "", mustJSON(map[string]any{
			"header":  frame.Header,
			"cursor":  frame.Cursor,
			"records": frame.Records,
		})
	case "event":
		var ev struct {
			Type string          `json:"type"`
			Seq  int64           `json:"seq"`
			Time int64           `json:"time"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(frame.Event, &ev); err != nil || len(ev.Data) == 0 {
			return "", "", nil
		}
		// Wrap event.data with sessionId so Router.DispatchMux can
		// route frames to the right subscriber via extractSessionID.
		wrapped, err := ensureSessionID(ev.Data, sessionID)
		if err != nil {
			return "", "", nil
		}
		return ev.Type, fmt.Sprintf("seq-%d", ev.Seq), wrapped
	case "assistant-stream":
		var wrap struct {
			Frame struct {
				Type  string          `json:"type"`
				Index int             `json:"index"`
				Chunk json.RawMessage `json:"chunk"`
			} `json:"frame"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil || wrap.Frame.Type != "chunk" || len(wrap.Frame.Chunk) == 0 || string(wrap.Frame.Chunk) == "null" {
			return "", "", nil
		}
		body, err := ensureSessionID(mustJSON(map[string]any{
			"chunk": wrap.Frame.Chunk,
		}), sessionID)
		if err != nil {
			return "", "", nil
		}
		return "assistant/chunk", fmt.Sprintf("astream-%d", wrap.Frame.Index), body
	default:
		return "", "", nil
	}
}

// ensureSessionID parses data as a JSON object, sets sessionId if
// not already present, and re-marshals. Returns data verbatim if
// it's not an object (Router's extractSessionID will return ""
// for non-objects and the dispatcher will log + drop).
func ensureSessionID(data json.RawMessage, sessionID string) (json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return data, nil
	}
	if _, ok := raw["sessionId"]; !ok && sessionID != "" {
		raw["sessionId"] = json.RawMessage("\"" + sessionID + "\"")
	}
	return json.Marshal(raw)
}

// ─── small utilities ──────────────────────────────────────────────

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

func truncateBytes(s []byte, n int) string {
	if len(s) <= n {
		return string(s)
	}
	return string(s[:n]) + "…"
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return d
}
