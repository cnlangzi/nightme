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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/version"
	"github.com/gorilla/websocket"
)

const (
	wsHandshakeTimeout = 10 * time.Second
	wsFrameReadLimit   = 10 * 1024 * 1024

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
)

// sessionStream holds the per-session subscription state. The
// handler is shared across all sessions (it's StreamHub.onMuxFrame
// — Router handles per-session dispatch by extracting sessionId
// from payload.sessionId).
type sessionStream struct {
	sessionID string
	streamID  string
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
	// payload.sessionId).
	onMuxFrame  FrameHandler
	onHostFrame FrameHandler

	mu         sync.RWMutex
	sessions   map[string]*sessionStream // sessionID → subscription
	byStreamID map[string]*sessionStream // streamID → subscription
	conn       *websocket.Conn
	writeCh    chan clientFrame // single-writer queue
	closed     bool
	stop       chan struct{}

	pumpWG     sync.WaitGroup
	dispatchWG sync.WaitGroup

	streamSeq atomic.Uint64 // mint unique streamIds
}

// NewStreamHub constructs a hub without cookie attachment. Used by
// tests which fake the dsh server and don't auth-gate. Production
// callers must use NewStreamHubWithJar so the dsh-auth cookie
// obtained from the launch-token exchange reaches the WS upgrade.
//
// Callbacks must be non-blocking — see type doc on FrameHandler.
// Pass nil for log to use slog.Default().
func NewStreamHub(baseURL string, log *slog.Logger, onMuxFrame, onHostFrame FrameHandler) *StreamHub {
	if log == nil {
		log = slog.Default()
	}
	return &StreamHub{
		baseURL:     baseURL,
		log:         log,
		onMuxFrame:  onMuxFrame,
		onHostFrame: onHostFrame,
		sessions:    make(map[string]*sessionStream),
		byStreamID:  make(map[string]*sessionStream),
		stop:        make(chan struct{}),
		writeCh:     make(chan clientFrame, 32),
	}
}

// NewStreamHubWithJar constructs a hub that attaches jar's cookies
// to every WS upgrade. Spawned-dsh path: the cookie is minted via
// `GET /?token=<launchToken>` (see spawnAndWire.mintAuthCookie)
// before this hub is started, so the first upgrade already carries
// the dsh-auth cookie and dsh doesn't 401 mid-handshake.
//
// jar must be non-nil; passing nil is a programming error (use
// NewStreamHub for the cookie-less test path).
func NewStreamHubWithJar(baseURL string, jar http.CookieJar, log *slog.Logger, onMuxFrame, onHostFrame FrameHandler) *StreamHub {
	h := NewStreamHub(baseURL, log, onMuxFrame, onHostFrame)
	h.jar = jar
	return h
}

// Subscribe registers a per-session subscription. The actual
// `session/follow` stream is opened by connectAndServe on the
// next (re)connect — this avoids double-open when Subscribe is
// called between a previous reconnect's open and the next connect.
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
	sub := &sessionStream{sessionID: sessionID, streamID: streamID}

	h.mu.Lock()
	if old, ok := h.sessions[sessionID]; ok {
		delete(h.byStreamID, old.streamID)
		h.queueCancelLocked(old.streamID)
	}
	h.sessions[sessionID] = sub
	h.byStreamID[streamID] = sub
	h.mu.Unlock()

	h.log.Info("dsh.host: subscribed to session stream (deferred open)",
		"session_id", sessionID, "stream_id", streamID)

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

// sessionOpenPayload returns the `{args:{...}}` payload for the
// session/follow open frame.
func sessionOpenPayload(sessionID string) json.RawMessage {
	return mustJSON(map[string]any{
		"args": map[string]any{
			"address": map[string]any{
				"kind":      "session",
				"sessionId": sessionID,
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
	h.dispatchWG.Wait()
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
	requestHeader := http.Header{}
	requestHeader.Set("Sec-WebSocket-Protocol", "nightme.bridge/v"+version.Version)
	if h.jar != nil {
		httpURL := *u
		httpURL.Scheme = "http"
		for _, c := range h.jar.Cookies(&httpURL) {
			requestHeader.Add("Cookie", c.Name+"="+c.Value)
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
			return fmt.Errorf("dsh.host: ws dial %s: HTTP %d: %w",
				u.String(), resp.StatusCode, err)
		}
		return fmt.Errorf("dsh.host: ws dial %s: %w", u.String(), err)
	}
	conn.SetReadLimit(wsFrameReadLimit)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	h.conn = conn
	// After reconnect, re-open every active subscription. The
	// server-side streamIds from the previous session are
	// meaningless on the new connection — mint fresh ones.
	toReopen := make([]clientFrame, 0, len(h.sessions)+1)
	for _, sub := range h.sessions {
		newID := h.mintStreamID("sess")
		delete(h.byStreamID, sub.streamID)
		sub.streamID = newID
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
	h.mu.Unlock()

	h.log.Info("dsh.host: mux stream connected", "url", u.String())

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
			return err
		}
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
	case "ready":
		// dsh sends one {type:"ready", clientId, host:{home:"..."}}
		// frame right after the WS upgrade completes. Nothing to
		// do with it; just log so we can correlate dsh-side
		// connection logs.
		h.log.Info("dsh.host: mux ready",
			"client_id", f.ClientID, "host", string(f.Host))
		return

	case "item":
		// Look up which handler this streamId belongs to.
		h.mu.RLock()
		if f.StreamID == hostStreamID {
			h.mu.RUnlock()
			method, rpcID, payload := translateHostEvent(f.Value)
			if method == "" {
				h.log.Debug("dsh.host: untranslated host item",
					"value_bytes", truncateBytes(f.Value, 200))
				return
			}
			h.dispatchWG.Add(1)
			h.invokeOnHost(method, rpcID, payload)
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
		h.dispatchWG.Add(1)
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
	defer h.dispatchWG.Done()
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

func (h *StreamHub) invokeOnMux(method, rpcID string, payload json.RawMessage) {
	defer h.dispatchWG.Done()
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

// queueOpen puts a frame onto the write channel without blocking.
// Safe to call before runMuxLoop starts (the frames queue up).
func (h *StreamHub) queueOpen(frame clientFrame) {
	select {
	case h.writeCh <- frame:
	default:
		h.log.Warn("dsh.host: writeCh full; dropping open",
			"stream_id", frame.StreamID, "endpoint", frame.Endpoint)
	}
}

// queueCancelLocked must be called with h.mu held.
func (h *StreamHub) queueCancelLocked(streamID string) {
	select {
	case h.writeCh <- clientFrame{Type: "cancel", StreamID: streamID}:
	default:
		// see queueOpen — pump is dead, dropping is the right call.
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

// ─── dispatch translation ─────────────────────────────────────────

// translateHostEvent decodes one RemoteEventRecord item.
//
// Wire form (from dsh source):
//
//	{ id?, event: "<discriminator>", ...eventFields }
//
// The "event" field IS the method discriminator. id becomes rpcId.
func translateHostEvent(raw json.RawMessage) (method, rpcID string, payload json.RawMessage) {
	var rec struct {
		ID    string          `json:"id"`
		Event string          `json:"event"`
		Rest  json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Event == "" {
		return "", "", nil
	}
	body, err := json.Marshal(struct {
		ID    string          `json:"id,omitempty"`
		Event string          `json:"event"`
		Rest  json.RawMessage `json:"-"`
	}{ID: rec.ID, Event: rec.Event, Rest: rec.Rest})
	if err != nil {
		return "", "", nil
	}
	return rec.Event, rec.ID, body
}

// translateSessionEvent decodes one SessionFollowFrame item.
//
// Wire form (from dsh typert):
//
//	{ type:"snapshot", header, cursor, records, hasMore, projections }
//	  → method = "session/snapshot"
//	{ type:"event", event:{type, seq, time, data:{type, ...payload-fields}} }
//	  → method = event.data.type (the per-event discriminator)
//	  → rpcId = stringified seq
//	  → payload = event.data with sessionId added
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
